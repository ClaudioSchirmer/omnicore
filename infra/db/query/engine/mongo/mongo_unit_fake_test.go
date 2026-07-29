package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Aliases for the driver's option-lister variadics, so the fakeColl method
// signatures match the mongoColl interface exactly while staying readable.
type (
	countOpt   = options.Lister[options.CountOptions]
	findOpt    = options.Lister[options.FindOptions]
	findOneOpt = options.Lister[options.FindOneOptions]
	updateOpt  = options.Lister[options.UpdateOneOptions]
	deleteOpt  = options.Lister[options.DeleteOneOptions]
	bulkOpt    = options.Lister[options.BulkWriteOptions]
)

// This file provides a hand-rolled, in-process fake of the unexported
// mongoColl seam (mongo.go), so the Mongo read side (MongoViewReader) and the
// document CRUD helpers (MongoDB.Upsert/Delete/UpdateFields/FindManyByField/
// FindIDsByField) and the UpstreamSubscriber ripple run without a live
// MongoDB. Cursors and single results are built with the driver's own
// in-memory constructors (mongo.NewCursorFromDocuments /
// NewSingleResultFromDocument), so the decode path is the real one. The
// integration suite (//go:build integration) remains the source of truth for
// real server behavior; these fakes verify the Go control flow, filter/sort/
// projection assembly, pagination, and error propagation around the driver.

// fakeColl implements mongoColl. find/findOne return programmable documents;
// the *Err fields force the matching failure branch; update/delete capture
// what the caller emitted.
type fakeColl struct {
	docs      []any // documents returned by Find (cursor) and FindOne (first)
	count     int64 // value returned by CountDocuments
	notFound  bool  // FindOne returns mongo.ErrNoDocuments
	findErr   error // forced error from Find
	countErr  error // forced error from CountDocuments
	updateErr error // forced error from UpdateOne
	deleteErr error // forced error from DeleteOne

	updates   []bson.M // captured UpdateOne update documents
	pipelines []bson.A // captured UpdateOne aggregation-pipeline updates
	deletes []any    // captured DeleteOne filters
	upd     int64    // ModifiedCount/MatchedCount to report

	bulkErr    error // forced error from BulkWrite
	bulkModels int   // total write models seen across BulkWrite calls
	bulkCalls  int   // number of BulkWrite invocations
}

func (c *fakeColl) CountDocuments(ctx context.Context, filter any, opts ...countOpt) (int64, error) {
	if c.countErr != nil {
		return 0, c.countErr
	}
	return c.count, nil
}

func (c *fakeColl) Find(ctx context.Context, filter any, opts ...findOpt) (*mongo.Cursor, error) {
	if c.findErr != nil {
		return nil, c.findErr
	}
	return mongo.NewCursorFromDocuments(append([]any{}, c.docs...), nil, nil)
}

func (c *fakeColl) FindOne(ctx context.Context, filter any, opts ...findOneOpt) *mongo.SingleResult {
	if c.notFound || len(c.docs) == 0 {
		return mongo.NewSingleResultFromDocument(bson.M{}, mongo.ErrNoDocuments, nil)
	}
	return mongo.NewSingleResultFromDocument(c.docs[0], nil, nil)
}

func (c *fakeColl) UpdateOne(ctx context.Context, filter, update any, opts ...updateOpt) (*mongo.UpdateResult, error) {
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	if u, ok := update.(bson.M); ok {
		c.updates = append(c.updates, u)
	}
	if p, ok := update.(bson.A); ok {
		c.pipelines = append(c.pipelines, p)
	}
	return &mongo.UpdateResult{MatchedCount: c.upd, ModifiedCount: c.upd}, nil
}

func (c *fakeColl) BulkWrite(ctx context.Context, models []mongo.WriteModel, opts ...bulkOpt) (*mongo.BulkWriteResult, error) {
	if c.bulkErr != nil {
		return nil, c.bulkErr
	}
	c.bulkCalls++
	c.bulkModels += len(models)
	return &mongo.BulkWriteResult{UpsertedCount: int64(len(models))}, nil
}

func (c *fakeColl) DeleteOne(ctx context.Context, filter any, opts ...deleteOpt) (*mongo.DeleteResult, error) {
	if c.deleteErr != nil {
		return nil, c.deleteErr
	}
	c.deletes = append(c.deletes, filter)
	return &mongo.DeleteResult{DeletedCount: 1}, nil
}

// newFakeMongo builds a MongoDB whose collFn always returns coll. The field
// is unexported but reachable here because the test file is in package infra.
func newFakeMongo(coll mongoColl) *MongoDB {
	return &MongoDB{collFn: func(string) mongoColl { return coll }}
}

// newFakeMongoFunc builds a MongoDB whose collFn dispatches by collection
// name — useful when a single flow touches more than one collection (e.g. the
// upstream-subscriber ripple).
func newFakeMongoFunc(fn func(name string) mongoColl) *MongoDB {
	return &MongoDB{collFn: fn}
}
