package query

import "context"

// IdentifiedDocument pairs a composed document with its _id for batch writes.
// The rebuild loop accumulates a slice of these and hands it to BulkUpsert so
// the whole batch crosses the store boundary in one call.
type IdentifiedDocument struct {
	ID  string
	Doc Document
}

// ReadModelStore is the backend-neutral port the read-model machinery
// (composer, SyncEngine, rebuild, drift, upstream subscriber) depends on to
// read and write composed documents. The Mongo adapter (*MongoDB) implements
// it; a future backend (Elasticsearch, …) implements the same contract and
// drops in at the composition root without touching the generic read-model
// logic. Every document crosses the boundary as Document (map[string]any), so
// the port carries no Mongo driver types.
// Every method names its target as a PhysicalCollection, never a raw string, so
// a caller must route a logical view name through a ViewResolver first; a bare
// view name cannot reach the store as a collection by accident (it will not
// compile). The id/field/value arguments stay plain strings.
type ReadModelStore interface {
	// Upsert replaces (or inserts) the document keyed by id in collection.
	Upsert(ctx context.Context, collection PhysicalCollection, id string, doc Document) error
	// BulkUpsert applies a batch of upserts in as few round trips as the
	// backend allows (one unordered bulk write on Mongo). Semantically
	// identical to calling Upsert once per element and order-independent; the
	// rebuild loop uses it where per-document round trips dominate cost. An
	// empty batch is a no-op.
	BulkUpsert(ctx context.Context, collection PhysicalCollection, docs []IdentifiedDocument) error
	// Delete removes the document keyed by id; missing target is not an error.
	Delete(ctx context.Context, collection PhysicalCollection, id string) error
	// FindManyByField returns every document where field == value.
	FindManyByField(ctx context.Context, collection PhysicalCollection, field string, value any) ([]Document, error)
	// FindIDsByField returns only the _id of every document where field == value.
	FindIDsByField(ctx context.Context, collection PhysicalCollection, field string, value any) ([]string, error)
	// UpdateFields applies a partial $set to the document keyed by id; missing
	// target is not an error.
	UpdateFields(ctx context.Context, collection PhysicalCollection, id string, fields Document) error
	// HasDocuments reports whether the collection holds at least one document.
	HasDocuments(ctx context.Context, collection PhysicalCollection) (bool, error)
	// ObservedFieldNames returns the union of every top-level field name across
	// all documents in the collection (drives orphan-field cleanup on rebuild).
	ObservedFieldNames(ctx context.Context, collection PhysicalCollection) (map[string]struct{}, error)
	// UnsetFields removes the given top-level fields from every document.
	UnsetFields(ctx context.Context, collection PhysicalCollection, fields []string) error
	// SnapshotDocumentIDs returns the set of every document _id in the collection.
	SnapshotDocumentIDs(ctx context.Context, collection PhysicalCollection) (map[string]struct{}, error)
	// DeleteByIDs removes the documents whose _id is in ids; returns the count.
	DeleteByIDs(ctx context.Context, collection PhysicalCollection, ids []string) (int, error)
}
