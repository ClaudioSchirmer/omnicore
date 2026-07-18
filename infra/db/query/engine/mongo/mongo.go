package mongo

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoColl is the minimal collection surface the read side and the
// document CRUD helpers consume. It is unexported and never leaves the
// infra package, so the abstraction does not cross a layer boundary.
// *mongo.Collection satisfies it in production; a fake satisfies it in unit
// tests, letting the view reader and the upstream-subscriber ripple run
// without a live MongoDB. Index/collection management (IndexView, createCol)
// stays on the concrete *mongo.Database handle and remains integration-only.
type mongoColl interface {
	CountDocuments(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error)
	Find(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*mongo.Cursor, error)
	FindOne(ctx context.Context, filter any, opts ...options.Lister[options.FindOneOptions]) *mongo.SingleResult
	UpdateOne(ctx context.Context, filter, update any, opts ...options.Lister[options.UpdateOneOptions]) (*mongo.UpdateResult, error)
	DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (*mongo.DeleteResult, error)
	BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*mongo.BulkWriteResult, error)
}

type MongoDB struct {
	client *mongo.Client
	db     *mongo.Database
	// collFn resolves a collection name to the unexported mongoColl seam.
	// Defaults to db.Collection in production; a unit test swaps it for a
	// fake. The public Collection accessor keeps returning the concrete
	// *mongo.Collection for consumer repositories.
	collFn func(name string) mongoColl
}

// MongoOption tunes NewMongoDB. Variadic so existing callers
// (NewMongoDB(ctx, uri, db)) keep compiling unchanged.
type MongoOption func(*mongoOptions)

type mongoOptions struct {
	trace bool
}

// WithMongoTracing installs a CommandMonitor that spans every Mongo command.
// bootstrap passes tracing.Instruments(SubMongo); false (the default) leaves
// the client untraced and pays nothing.
func WithMongoTracing(enabled bool) MongoOption {
	return func(o *mongoOptions) { o.trace = enabled }
}

func NewMongoDB(ctx context.Context, uri, dbName string, opts ...MongoOption) (*MongoDB, error) {
	var o mongoOptions
	for _, opt := range opts {
		opt(&o)
	}
	// DefaultDocumentM normalizes the decoder so nested documents inside
	// arrays land as bson.M (= map[string]any), matching the top-level
	// shape. Without it the driver decodes array-of-doc elements as bson.D
	// (an ordered []KV), which breaks any consumer that walks the doc by
	// key — including fwresponses.AutoFromDoc and any custom FromDoc. The
	// read side's downstream contract (ViewReader returns map[string]any)
	// already implies "documents are maps"; this flag enforces it at the
	// boundary so the trap stops leaking into projector code.
	clientOpts := options.Client().
		ApplyURI(uri).
		SetBSONOptions(&options.BSONOptions{DefaultDocumentM: true})
	if o.trace {
		clientOpts.SetMonitor(newMongoCommandMonitor())
	}
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}
	m := &MongoDB{client: client, db: client.Database(dbName)}
	m.collFn = func(name string) mongoColl { return m.db.Collection(name) }
	return m, nil
}

func (m *MongoDB) Close(ctx context.Context) error {
	return m.client.Disconnect(ctx)
}

// Ping verifies the connection to the Mongo deployment is alive — the read-store
// leg of the readiness probe. It runs the driver's ping under the caller's
// context (the probe passes a short deadline), so a wedged server fails the
// probe fast instead of hanging it.
func (m *MongoDB) Ping(ctx context.Context) error {
	return m.client.Ping(ctx, nil)
}

func (m *MongoDB) Collection(name string) *mongo.Collection {
	return m.db.Collection(name)
}

// DropCollection drops the named collection — the retired slot reclaimed one
// lease after a blue-green flip. A missing collection is not an error (Mongo's
// drop is idempotent), matching Delete's "missing target is fine" posture.
func (m *MongoDB) DropCollection(ctx context.Context, collection query.PhysicalCollection) error {
	return m.db.Collection(collection.String()).Drop(ctx)
}

// ProvisionSlot brings target to view's declared Mongo shape (create with
// collation/capped/time-series/validator + every declared index), reusing the
// boot-time apply sequence against an explicit collection.
func (m *MongoDB) ProvisionSlot(ctx context.Context, view *query.ViewDefinition, target query.PhysicalCollection) error {
	return provisionSlot(ctx, m, view, target)
}

func (m *MongoDB) Upsert(ctx context.Context, collection query.PhysicalCollection, id string, doc query.Document) error {
	col := m.collFn(collection.String())
	filter := bson.M{"_id": id}
	update := bson.M{"$set": doc}
	opts := options.UpdateOne().SetUpsert(true)
	_, err := col.UpdateOne(ctx, filter, update, opts)
	return err
}

// BulkUpsert applies a batch of upsert-by-_id operations in a single unordered
// bulk write — one round trip for the whole batch instead of one UpdateOne per
// document. The rebuild loop drives it: on a 10M-row projection this collapses
// 10M individual round trips into one per batch. Unordered so an individual
// document failure does not stop the rest of the batch (the error still
// surfaces, matching Upsert's per-call semantics; the rebuild aborts on it).
// Each operation carries the same {$set: doc}, upsert:true shape as Upsert, so
// the two paths write identical documents. An empty batch is a no-op — the
// driver rejects a zero-length model slice, so the guard is required.
func (m *MongoDB) BulkUpsert(ctx context.Context, collection query.PhysicalCollection, docs []query.IdentifiedDocument) error {
	if len(docs) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(docs))
	for _, d := range docs {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": d.ID}).
			SetUpdate(bson.M{"$set": d.Doc}).
			SetUpsert(true))
	}
	col := m.collFn(collection.String())
	_, err := col.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

func (m *MongoDB) Delete(ctx context.Context, collection query.PhysicalCollection, id string) error {
	col := m.collFn(collection.String())
	_, err := col.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// FindManyByField returns every document in collection where field == value.
// Used by the composer's Mongo dispatch when it follows an external FromSchema embed:
// the field is the joinKey declared via Source.FK(...) and the value comes from
// the parent document. Empty slice when nothing matches — the caller is
// expected to handle "no embed" by simply omitting the field, identical to
// the PG fetchWhere path.
func (m *MongoDB) FindManyByField(ctx context.Context, collection query.PhysicalCollection, field string, value any) ([]query.Document, error) {
	col := m.collFn(collection.String())
	cur, err := col.Find(ctx, bson.M{field: value})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []query.Document
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FindIDsByField returns only the _id of every document in collection where
// field == value. Consumed by the UpstreamSubscriber's recompose-ripple:
// after an upsert/delete on the upstream Mongo collection, the subscriber
// asks every dependent view "which of your local docs referenced the
// changed upstream id?", and only the _id is needed to drive
// composer.Compose + mongo.Upsert. The projection {_id:1} keeps the round
// trip lean — boot guard §8.1 enforces a covering index on the join field
// so the query is index-only.
//
// _id is normalized to a string when the underlying BSON type permits
// (string is the canonical shape SyncEngine + UpstreamSubscriber use for
// aggregate_id). Non-string _ids (rare in framework-managed collections)
// fall through via fmt.Sprintf so the caller still gets a usable key.
func (m *MongoDB) FindIDsByField(ctx context.Context, collection query.PhysicalCollection, field string, value any) ([]string, error) {
	col := m.collFn(collection.String())
	opts := options.Find().SetProjection(bson.M{"_id": 1})
	cur, err := col.Find(ctx, bson.M{field: value}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		raw, ok := d["_id"]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case string:
			out = append(out, v)
		default:
			out = append(out, fmt.Sprintf("%v", raw))
			_ = v
		}
	}
	return out, nil
}

// UpdateFields applies a partial $set to the document keyed by id in
// collection. Used by the UpstreamSubscriber's GDPR anonymize path: each
// field listed in subscription.AnonymizeFields lands as a nil value in the
// fields map. The Mongo driver writes the nils as BSON nulls, matching the
// design's "blank the field" semantic. Returns nil (no error) when the
// document does not exist — anonymize is idempotent against a missing
// target, same as Delete.
func (m *MongoDB) UpdateFields(ctx context.Context, collection query.PhysicalCollection, id string, fields query.Document) error {
	if len(fields) == 0 {
		return nil
	}
	col := m.collFn(collection.String())
	_, err := col.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields})
	return err
}

// HasDocuments reports whether collection holds at least one document. Drives
// the drift detector's "Mongo has user data" branch.
func (m *MongoDB) HasDocuments(ctx context.Context, collection query.PhysicalCollection) (bool, error) {
	col := m.collFn(collection.String())
	count, err := col.CountDocuments(ctx, bson.M{}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("count documents on %q: %w", collection, err)
	}
	return count > 0, nil
}

// SnapshotDocumentIDs reads every doc _id into a set. Blue-green verify uses it
// for the shadow's completeness passes; the leftover ids are the orphans.
// The {_id:1} projection keeps the scan to the id column — without it the
// server streams every full document over the wire only to discard all but
// the _id, which on a large projection is the difference between a lean
// index-only scan and shipping the whole collection.
func (m *MongoDB) SnapshotDocumentIDs(ctx context.Context, collection query.PhysicalCollection) (map[string]struct{}, error) {
	col := m.Collection(collection.String())
	cur, err := col.Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, fmt.Errorf("snapshot ids on %q: %w", collection, err)
	}
	defer cur.Close(ctx)

	set := make(map[string]struct{})
	for cur.Next(ctx) {
		var doc struct {
			ID any `bson:"_id"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		set[fmt.Sprintf("%v", doc.ID)] = struct{}{}
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// DeleteByIDs removes the documents whose _id is in ids (single deleteMany).
// Returns the deleted count. No-op (0) for an empty list.
func (m *MongoDB) DeleteByIDs(ctx context.Context, collection query.PhysicalCollection, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	col := m.Collection(collection.String())
	res, err := col.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return 0, fmt.Errorf("delete by ids on %q: %w", collection, err)
	}
	return int(res.DeletedCount), nil
}

// Compile-time proof the Mongo adapter implements the backend-neutral port.
var _ query.ReadModelStore = (*MongoDB)(nil)
