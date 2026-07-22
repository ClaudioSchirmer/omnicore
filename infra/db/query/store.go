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
	// FindManyByFieldIn returns every document where field ∈ values — the
	// set-based companion of FindManyByField (one {field: {$in: values}} query).
	// The composer's batched embed resolution uses it to fetch a whole batch of
	// parents' external embeds in one round trip per embed source instead of one
	// FindManyByField per parent. An empty values slice returns no documents;
	// duplicate values are tolerated. Documents come back in no guaranteed order —
	// the caller groups them by field.
	FindManyByFieldIn(ctx context.Context, collection PhysicalCollection, field string, values []any) ([]Document, error)
	// FindIDsByField returns only the _id of every document where field == value.
	FindIDsByField(ctx context.Context, collection PhysicalCollection, field string, value any) ([]string, error)
	// UpdateFields applies a partial $set to the document keyed by id; missing
	// target is not an error.
	UpdateFields(ctx context.Context, collection PhysicalCollection, id string, fields Document) error
	// ApplyProjection applies an aggregation-pipeline update to the document
	// keyed by id — the payload-direct projection's atomic write:
	// unconditional own-field sets, revision-guarded shared-base sets and
	// surgical child-array edits execute server-side in ONE operation, so
	// interleaved writers (role events, shared-base fan-out) never
	// read-modify-write. stages are standard update-pipeline documents.
	// upsert=true materializes a missing document; upsert=false makes a
	// missing document a no-op — required wherever the target id comes from a
	// snapshot another writer may have invalidated (the recompose-ripple's
	// surgical embed edits, the shared-base fan-out), or a write racing a
	// concurrent document delete would resurrect a skeleton holding nothing
	// but the edited fields.
	ApplyProjection(ctx context.Context, collection PhysicalCollection, id string, stages []Document, upsert bool) error
	// HasDocuments reports whether the collection holds at least one document.
	HasDocuments(ctx context.Context, collection PhysicalCollection) (bool, error)
	// SnapshotDocumentIDs returns the set of every document _id in the collection.
	SnapshotDocumentIDs(ctx context.Context, collection PhysicalCollection) (map[string]struct{}, error)
	// DeleteByIDs removes the documents whose _id is in ids; returns the count.
	DeleteByIDs(ctx context.Context, collection PhysicalCollection, ids []string) (int, error)
	// ProvisionSlot brings target to the view's declared shape (indexes,
	// validator, collation/capped/time-series) — the blue-green driver calls it
	// on a shadow slot before backfilling.
	ProvisionSlot(ctx context.Context, view *ViewDefinition, target PhysicalCollection) error
	// DropCollection drops the collection — the retired slot reclaimed after a
	// flip. A missing collection is not an error.
	DropCollection(ctx context.Context, collection PhysicalCollection) error
}
