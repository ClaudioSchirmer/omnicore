package query

import "context"

// ReadModelStore is the backend-neutral port the read-model machinery
// (composer, SyncEngine, rebuild, drift, upstream subscriber) depends on to
// read and write composed documents. The Mongo adapter (*MongoDB) implements
// it; a future backend (Elasticsearch, …) implements the same contract and
// drops in at the composition root without touching the generic read-model
// logic. Every document crosses the boundary as Document (map[string]any), so
// the port carries no Mongo driver types.
type ReadModelStore interface {
	// Upsert replaces (or inserts) the document keyed by id in collection.
	Upsert(ctx context.Context, collection, id string, doc Document) error
	// Delete removes the document keyed by id; missing target is not an error.
	Delete(ctx context.Context, collection, id string) error
	// FindManyByField returns every document where field == value.
	FindManyByField(ctx context.Context, collection, field string, value any) ([]Document, error)
	// FindIDsByField returns only the _id of every document where field == value.
	FindIDsByField(ctx context.Context, collection, field string, value any) ([]string, error)
	// UpdateFields applies a partial $set to the document keyed by id; missing
	// target is not an error.
	UpdateFields(ctx context.Context, collection, id string, fields Document) error
	// HasDocuments reports whether the collection holds at least one document.
	HasDocuments(ctx context.Context, collection string) (bool, error)
	// ObservedFieldNames returns the union of every top-level field name across
	// all documents in the collection (drives orphan-field cleanup on rebuild).
	ObservedFieldNames(ctx context.Context, collection string) (map[string]struct{}, error)
	// UnsetFields removes the given top-level fields from every document.
	UnsetFields(ctx context.Context, collection string, fields []string) error
	// SnapshotDocumentIDs returns the set of every document _id in the collection.
	SnapshotDocumentIDs(ctx context.Context, collection string) (map[string]struct{}, error)
	// DeleteByIDs removes the documents whose _id is in ids; returns the count.
	DeleteByIDs(ctx context.Context, collection string, ids []string) (int, error)
}
