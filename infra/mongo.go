package infra

import (
	"context"
	"fmt"

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

func NewMongoDB(ctx context.Context, uri, dbName string) (*MongoDB, error) {
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
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(ctx)
		return nil, err
	}
	m := &MongoDB{client: client, db: client.Database(dbName)}
	m.collFn = func(name string) mongoColl { return m.db.Collection(name) }
	return m, nil
}

func (m *MongoDB) Close(ctx context.Context) error {
	return m.client.Disconnect(ctx)
}

func (m *MongoDB) Collection(name string) *mongo.Collection {
	return m.db.Collection(name)
}

func (m *MongoDB) Upsert(ctx context.Context, collection, id string, doc bson.M) error {
	col := m.collFn(collection)
	filter := bson.M{"_id": id}
	update := bson.M{"$set": doc}
	opts := options.UpdateOne().SetUpsert(true)
	_, err := col.UpdateOne(ctx, filter, update, opts)
	return err
}

func (m *MongoDB) Delete(ctx context.Context, collection, id string) error {
	col := m.collFn(collection)
	_, err := col.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// FindManyByField returns every document in collection where field == value.
// Used by the composer's Mongo dispatch when it follows an external FromSchema embed:
// the field is the joinKey declared via .On(...) and the value comes from
// the parent document. Empty slice when nothing matches — the caller is
// expected to handle "no embed" by simply omitting the field, identical to
// the PG fetchWhere path.
func (m *MongoDB) FindManyByField(ctx context.Context, collection, field string, value any) ([]bson.M, error) {
	col := m.collFn(collection)
	cur, err := col.Find(ctx, bson.M{field: value})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []bson.M
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
func (m *MongoDB) FindIDsByField(ctx context.Context, collection, field string, value any) ([]string, error) {
	col := m.collFn(collection)
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
func (m *MongoDB) UpdateFields(ctx context.Context, collection, id string, fields bson.M) error {
	if len(fields) == 0 {
		return nil
	}
	col := m.collFn(collection)
	_, err := col.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": fields})
	return err
}
