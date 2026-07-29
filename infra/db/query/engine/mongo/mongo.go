package mongo

import (
	"context"
	"fmt"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
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
	m.collFn = func(name string) mongoColl {
		// EVERY read-side write requires MAJORITY acknowledgment. A record
		// acknowledged by the primary alone can be ROLLED BACK on a failover:
		// the write was confirmed to us and then withdrawn beneath us, so the
		// event that produced it was legitimately complete and nothing on the
		// delivery path will ever re-issue it.
		//
		// The projection-state registry needed this first — it is the fiduciary
		// of the write-then-check handshakes (base-revision stamps, tombstones)
		// and a rolled-back record silently dissolves the ordering premise those
		// proofs rest on. View collections were left on the deployment default
		// with the stated reason that "their writes reconverge through event
		// redelivery + guards". That premise was false: the projection loop was
		// at-most-once, and an insert-once aggregate has no later write to
		// re-stamp its document. The redelivery half is fixed now, but redelivery
		// does not help here either — the write SUCCEEDED and was then undone.
		//
		// Stated here rather than inherited: on MongoDB 5.0+ a replica set
		// already defaults to majority, so this is usually a no-op today, and
		// that is precisely the point — a correctness invariant must not rest on
		// a default that can be changed outside this repository, nor silently
		// lapse on an arbiter topology or an older server. On a standalone node
		// majority degrades to the primary ack, which is why the QA bench cannot
		// observe either the cost or the failover behaviour.
		//
		// No configuration knob: a switch that disables a correctness invariant
		// is a trap, and the need is unmeasured. Where this is genuinely
		// expensive is a cross-region replica set or a sharded cluster; the
		// framework cannot assume the consumer's topology, so what it owes them
		// is the measured number for a stated topology, not a claim that the
		// cost is negligible.
		return m.db.Collection(name, options.Collection().SetWriteConcern(writeconcern.Majority()))
	}
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

// ApplyProjection runs an aggregation-pipeline update keyed by _id, upserting
// when the document does not exist yet — the payload-direct projection's one
// atomic write (conditional shared-base sets + surgical child-array edits are
// pipeline stages evaluated server-side).
func (m *MongoDB) ApplyProjection(ctx context.Context, collection query.PhysicalCollection, id string, stages []query.Document, upsert bool) error {
	col := m.collFn(collection.String())
	pipeline := make(bson.A, 0, len(stages))
	for _, st := range stages {
		pipeline = append(pipeline, bson.M(st))
	}
	opts := options.UpdateOne().SetUpsert(upsert)
	_, err := col.UpdateOne(ctx, bson.M{"_id": id}, pipeline, opts)
	return err
}

func (m *MongoDB) Delete(ctx context.Context, collection query.PhysicalCollection, id string) error {
	col := m.collFn(collection.String())
	_, err := col.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// DeleteGuarded removes the document keyed by id only when its stored revision
// watermark is <= rev — or when it carries no watermark at all (a
// watermark-less document counts as older, matching the projection guards'
// $ifNull treatment). A document a fresher writer already advanced past rev
// survives; a missing document is a no-op, like Delete.
// createdAtUnixMs > 0 adds the incarnation scope: the document dies only when its
// stored created_at equals the tombstone's created_at instant (BSON datetimes
// compare at millisecond grain — the same grain the caller renders).
func (m *MongoDB) DeleteGuarded(ctx context.Context, collection query.PhysicalCollection, id string, rev int64, createdAtUnixMs int64) error {
	col := m.collFn(collection.String())
	filter := bson.M{
		"_id": id,
		"$or": bson.A{
			bson.M{query.DocRevisionField: bson.M{"$lte": rev}},
			bson.M{query.DocRevisionField: bson.M{"$exists": false}},
		},
	}
	if createdAtUnixMs > 0 {
		// Same incarnation OR no birth certificate at all:
		//   - the incarnation match is a TWO-SECOND range centered on the
		//     tombstone's value, not equality: that value is read back from
		//     the relational column, whose precision is engine-DDL-dependent —
		//     a MySQL DATETIME without fractional digits ROUNDS to the nearest
		//     second (12.7 stores as 13), other engines truncate — while the
		//     document's created_at came from the payload at full precision.
		//     [T-1s, T+1s) covers both behaviors. Two incarnations of one
		//     deterministic id born within that window are indistinguishable
		//     (unreachable through API round-trips).
		//   - a document WITHOUT created_at under a tombstoned id can only be
		//     a zombie: an UPDATED carries no created_at, so a redelivered
		//     update re-materializing the document after the delete produces
		//     exactly that shape. The legitimately REBORN document always
		//     materializes through its own INSERTED (serialized before its
		//     updates on the aggregate's partition), which stamps the NEW
		//     created_at — outside the range, never absent.
		floor := time.UnixMilli(createdAtUnixMs).UTC().Truncate(time.Second)
		filter["$and"] = bson.A{bson.M{"$or": bson.A{
			bson.M{"created_at": bson.M{"$gte": floor.Add(-time.Second), "$lt": floor.Add(time.Second)}},
			bson.M{"created_at": bson.M{"$exists": false}},
		}}}
	}
	_, err := col.DeleteOne(ctx, filter)
	return err
}

// BulkApplyProjection applies a batch of upserting pipeline updates in a single
// unordered bulk write — ApplyProjection's batched companion, driven by the
// rebuild/verify backfill with revision-guarded stages. Unordered so an
// individual document failure does not stop the rest of the batch (the error
// still surfaces; the rebuild aborts on it). An empty batch is a no-op — the
// driver rejects a zero-length model slice, so the guard is required.
func (m *MongoDB) BulkApplyProjection(ctx context.Context, collection query.PhysicalCollection, items []query.IdentifiedStages) error {
	if len(items) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(items))
	for _, it := range items {
		pipeline := make(bson.A, 0, len(it.Stages))
		for _, st := range it.Stages {
			pipeline = append(pipeline, bson.M(st))
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": it.ID}).
			SetUpdate(pipeline).
			SetUpsert(true))
	}
	col := m.collFn(collection.String())
	_, err := col.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

// EnsureProjectionState provisions the projection-state registry: the TTL
// index that expires document tombstones by their "at" stamp after
// query.TombstoneTTL. Base-revision documents carry no "at" field, so the TTL
// sweep never touches them (Mongo's TTL monitor skips documents missing the
// indexed field). CreateOne is idempotent for an identical existing index.
func (m *MongoDB) EnsureProjectionState(ctx context.Context) error {
	col := m.db.Collection(query.ProjectionStateCollectionName)
	_, err := col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(query.TombstoneTTL.Seconds())),
	})
	return err
}

// FindManyByField returns every document in collection where field == value.
// Used by the composer's Mongo dispatch when it follows an external JoinUpstream embed:
// the field is the joinKey declared via Source.ParentID(...) and the value comes from
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

// FindManyByFieldIn returns every document in collection where field ∈ values —
// the set-based companion of FindManyByField (a single {field: {$in: values}}
// query). The composer's batched embed path drives it: one round trip resolves a
// whole batch of parents' external embeds instead of one FindManyByField per
// parent. An empty values slice is a no-op (an empty $in matches nothing; the
// guard skips the round trip). Empty slice on no match, matching FindManyByField.
func (m *MongoDB) FindManyByFieldIn(ctx context.Context, collection query.PhysicalCollection, field string, values []any) ([]query.Document, error) {
	if len(values) == 0 {
		return nil, nil
	}
	col := m.collFn(collection.String())
	cur, err := col.Find(ctx, bson.M{field: bson.M{"$in": values}})
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
// RevisionsByIDs projects only _id + the revision watermark for the requested
// ids — an index-covered read, so a reconciliation sweep costs a key scan rather
// than fetching documents it has no intention of reading.
func (m *MongoDB) RevisionsByIDs(ctx context.Context, collection query.PhysicalCollection, ids []string) (map[string]int64, error) {
	out := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	col := m.collFn(collection.String())
	cur, err := col.Find(ctx,
		bson.M{"_id": bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{"_id": 1, query.DocRevisionField: 1}))
	if err != nil {
		return nil, fmt.Errorf("revisions by ids on %q: %w", collection, err)
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		id, ok := doc["_id"]
		if !ok {
			continue
		}
		// A document written before the watermark existed decodes to 0, which is
		// still PRESENT — distinct from absent, which is what the caller acts on.
		out[fmt.Sprintf("%v", id)] = toInt64(doc[query.DocRevisionField])
	}
	if err := cur.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// toInt64 normalizes a BSON numeric to int64; a non-numeric or absent watermark
// answers 0 (treated as "older than anything", matching the projection guards'
// $ifNull handling).
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

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
