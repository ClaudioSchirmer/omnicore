package query

import (
	"context"
	"fmt"
)

// fakeColl is the port-level recorder the read-model unit tests drive instead
// of a live Mongo collection. It mirrors the field surface of the old
// mongoColl fake (docs/count/*Err/updates/deletes) so the test bodies port
// across the package split with minimal change, but it records at the
// ReadModelStore boundary (Upsert/Delete/UpdateFields/Find*) rather than the
// driver's UpdateOne/DeleteOne. fakeStore routes per-collection through fn.
type fakeColl struct {
	docs      []any // documents returned by FindManyByField / FindIDsByField / SnapshotDocumentIDs
	count     int64 // value reported by HasDocuments (count > 0)
	notFound  bool  // unused at the port level; kept for source compatibility
	findErr   error // forced error from FindManyByField / FindIDsByField
	countErr  error // forced error from HasDocuments
	updateErr error // forced error from Upsert / UpdateFields
	deleteErr error // forced error from Delete

	updates        []map[string]any // captured Upsert / UpdateFields docs, each {"$set": doc}
	deletes        []any            // captured Delete ids (guarded deletes append here too)
	guardedDeletes []map[string]any // captured DeleteGuarded calls, each {"_id": id, "revision": rev}
	upd            int64            // unused at the port level; kept for source compatibility
}

type fakeStore struct {
	fn func(name string) *fakeColl
	// state is the projection-state registry's dedicated recorder: the
	// registry is not view data (it lives outside the slots in production), so
	// the fake isolates it from the per-view routing too — registry traffic
	// (base-revision stamps, tombstones) never leaks into a view collection's
	// recorders, and tests seed/inspect it directly.
	state *fakeColl
	// stateEnsured counts EnsureProjectionState calls.
	stateEnsured int
	// blue-green slot ops: recorders + forced errors.
	provisioned  []string
	dropped      []string
	provisionErr error
	dropErr      error
}

// coll routes a collection name to its recorder — the registry to the
// dedicated state recorder, everything else through fn.
func (s *fakeStore) coll(name string) *fakeColl {
	if name == ProjectionStateCollectionName {
		if s.state == nil {
			s.state = &fakeColl{}
		}
		return s.state
	}
	return s.fn(name)
}

func newFakeMongo(coll *fakeColl) *fakeStore {
	return &fakeStore{fn: func(string) *fakeColl { return coll }}
}

func newFakeMongoFunc(fn func(name string) *fakeColl) *fakeStore {
	return &fakeStore{fn: fn}
}

var _ ReadModelStore = (*fakeStore)(nil)

// pc builds a PhysicalCollection from a raw name for tests in this package (the
// resolver is the production path; tests short-circuit it). identityResolver is a
// ViewResolver with no registry backing whose Active/Shadow resolve to the bare
// name — handed to the constructors that now require one.
func pc(name string) PhysicalCollection { return PhysicalCollection{name: name} }

var identityResolver = NewViewResolver(nil)

func (s *fakeStore) Upsert(_ context.Context, collection PhysicalCollection, _ string, doc Document) error {
	c := s.coll(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updates = append(c.updates, map[string]any{"$set": doc})
	return nil
}

func (s *fakeStore) BulkUpsert(_ context.Context, collection PhysicalCollection, docs []IdentifiedDocument) error {
	c := s.coll(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	for _, d := range docs {
		c.updates = append(c.updates, map[string]any{"$set": d.Doc})
	}
	return nil
}

func (s *fakeStore) UpdateFields(_ context.Context, collection PhysicalCollection, _ string, fields Document) error {
	c := s.coll(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updates = append(c.updates, map[string]any{"$set": fields})
	return nil
}

func (s *fakeStore) ApplyProjection(_ context.Context, collection PhysicalCollection, id string, stages []Document, upsert bool) error {
	c := s.coll(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updates = append(c.updates, map[string]any{"$pipeline": stages, "_id": id, "$upsert": upsert})
	return nil
}

func (s *fakeStore) Delete(_ context.Context, collection PhysicalCollection, id string) error {
	c := s.coll(collection.String())
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.deletes = append(c.deletes, id)
	return nil
}

func (s *fakeStore) FindManyByField(_ context.Context, collection PhysicalCollection, _ string, _ any) ([]Document, error) {
	c := s.coll(collection.String())
	if c.findErr != nil {
		return nil, c.findErr
	}
	out := make([]Document, 0, len(c.docs))
	for _, d := range c.docs {
		if m, ok := d.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// FindManyByFieldIn returns docs whose field value (stringified) is in values —
// unlike FindManyByField (which the fakes leave unfiltered), this one filters so
// the batched-embed grouping tests can assert docs land under the RIGHT parent.
func (s *fakeStore) FindManyByFieldIn(_ context.Context, collection PhysicalCollection, field string, values []any) ([]Document, error) {
	c := s.coll(collection.String())
	if c.findErr != nil {
		return nil, c.findErr
	}
	want := make(map[string]struct{}, len(values))
	for _, v := range values {
		want[fmt.Sprintf("%v", v)] = struct{}{}
	}
	out := make([]Document, 0)
	for _, d := range c.docs {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		fv, ok := m[field]
		if !ok {
			continue
		}
		if _, hit := want[fmt.Sprintf("%v", fv)]; hit {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *fakeStore) FindIDsByField(_ context.Context, collection PhysicalCollection, _ string, _ any) ([]string, error) {
	c := s.coll(collection.String())
	if c.findErr != nil {
		return nil, c.findErr
	}
	ids := make([]string, 0, len(c.docs))
	for _, d := range c.docs {
		if m, ok := d.(map[string]any); ok {
			if id, ok := m["_id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (s *fakeStore) HasDocuments(_ context.Context, collection PhysicalCollection) (bool, error) {
	c := s.coll(collection.String())
	if c.countErr != nil {
		return false, c.countErr
	}
	return c.count > 0, nil
}

func (s *fakeStore) ProvisionSlot(_ context.Context, _ *ViewDefinition, target PhysicalCollection) error {
	s.provisioned = append(s.provisioned, target.String())
	return s.provisionErr
}

func (s *fakeStore) DropCollection(_ context.Context, collection PhysicalCollection) error {
	s.dropped = append(s.dropped, collection.String())
	return s.dropErr
}

func (s *fakeStore) SnapshotDocumentIDs(_ context.Context, collection PhysicalCollection) (map[string]struct{}, error) {
	c := s.coll(collection.String())
	if c.findErr != nil {
		return nil, c.findErr
	}
	set := make(map[string]struct{})
	for _, d := range c.docs {
		if m, ok := d.(map[string]any); ok {
			if id, ok := m["_id"].(string); ok {
				set[id] = struct{}{}
			}
		}
	}
	return set, nil
}

func (s *fakeStore) DeleteByIDs(_ context.Context, collection PhysicalCollection, ids []string) (int, error) {
	c := s.coll(collection.String())
	if c.deleteErr != nil {
		return 0, c.deleteErr
	}
	for _, id := range ids {
		c.deletes = append(c.deletes, id)
	}
	return len(ids), nil
}

// DeleteGuarded records the id like Delete (so existing assertions keep
// reading c.deletes) and the guard revision in guardedDeletes for the tests
// that fix the tombstone discipline.
func (s *fakeStore) DeleteGuarded(_ context.Context, collection PhysicalCollection, id string, rev int64) error {
	c := s.coll(collection.String())
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.deletes = append(c.deletes, id)
	c.guardedDeletes = append(c.guardedDeletes, map[string]any{"_id": id, "revision": rev})
	return nil
}

func (s *fakeStore) BulkApplyProjection(_ context.Context, collection PhysicalCollection, items []IdentifiedStages) error {
	c := s.coll(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	for _, it := range items {
		c.updates = append(c.updates, map[string]any{"$pipeline": it.Stages, "_id": it.ID, "$upsert": true})
	}
	return nil
}

func (s *fakeStore) EnsureProjectionState(_ context.Context) error {
	s.stateEnsured++
	return nil
}

// effectiveDoc flattens one captured update into the document it would
// materialize on an empty target — the content-level view tests assert on,
// independent of the write FORM. A plain Upsert answers its $set document; a
// guarded pipeline (consultGuardedStages / ApplyProjection) merges every
// stage's $set, unwrapping the guard shells: {$cond: [newer, v, "$field"]}
// takes the incoming branch, {$literal: v} unwraps to v. Watermark fields
// ($_-prefixed guard state) come through as their incoming revision — callers
// that don't care simply don't assert them.
func effectiveDoc(update map[string]any) Document {
	if doc, ok := update["$set"].(Document); ok {
		return unwrapDoc(doc)
	}
	out := Document{}
	stages, _ := update["$pipeline"].([]Document)
	for _, st := range stages {
		set, _ := st["$set"].(Document)
		for k, v := range unwrapDoc(set) {
			out[k] = v
		}
	}
	return out
}

// unwrapDoc strips the pipeline value shells from every entry of a $set body.
func unwrapDoc(set Document) Document {
	out := Document{}
	for k, v := range set {
		out[k] = unwrapExpr(v)
	}
	return out
}

// unwrapExpr resolves one pipeline value expression to the value it applies on
// an empty/older target: $cond takes the "newer wins" branch, $literal unwraps,
// $ifNull takes its first non-fallback operand resolution.
func unwrapExpr(v any) any {
	m, ok := v.(Document)
	if !ok {
		return v
	}
	if lit, ok := m["$literal"]; ok {
		return lit
	}
	if cond, ok := m["$cond"].([]any); ok && len(cond) == 3 {
		return unwrapExpr(cond[1])
	}
	return v
}
