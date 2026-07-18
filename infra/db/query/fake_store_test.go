package query

import "context"

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

	updates []map[string]any // captured Upsert / UpdateFields docs, each {"$set": doc}
	deletes []any            // captured Delete ids
	upd     int64            // unused at the port level; kept for source compatibility
}

type fakeStore struct {
	fn func(name string) *fakeColl
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
	c := s.fn(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updates = append(c.updates, map[string]any{"$set": doc})
	return nil
}

func (s *fakeStore) BulkUpsert(_ context.Context, collection PhysicalCollection, docs []IdentifiedDocument) error {
	c := s.fn(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	for _, d := range docs {
		c.updates = append(c.updates, map[string]any{"$set": d.Doc})
	}
	return nil
}

func (s *fakeStore) UpdateFields(_ context.Context, collection PhysicalCollection, _ string, fields Document) error {
	c := s.fn(collection.String())
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updates = append(c.updates, map[string]any{"$set": fields})
	return nil
}

func (s *fakeStore) Delete(_ context.Context, collection PhysicalCollection, id string) error {
	c := s.fn(collection.String())
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.deletes = append(c.deletes, id)
	return nil
}

func (s *fakeStore) FindManyByField(_ context.Context, collection PhysicalCollection, _ string, _ any) ([]Document, error) {
	c := s.fn(collection.String())
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

func (s *fakeStore) FindIDsByField(_ context.Context, collection PhysicalCollection, _ string, _ any) ([]string, error) {
	c := s.fn(collection.String())
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
	c := s.fn(collection.String())
	if c.countErr != nil {
		return false, c.countErr
	}
	return c.count > 0, nil
}

func (s *fakeStore) ObservedFieldNames(_ context.Context, collection PhysicalCollection) (map[string]struct{}, error) {
	c := s.fn(collection.String())
	if c.findErr != nil {
		return nil, c.findErr
	}
	return map[string]struct{}{}, nil
}

func (s *fakeStore) UnsetFields(_ context.Context, _ PhysicalCollection, _ []string) error {
	return nil
}

func (s *fakeStore) SnapshotDocumentIDs(_ context.Context, collection PhysicalCollection) (map[string]struct{}, error) {
	c := s.fn(collection.String())
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
	c := s.fn(collection.String())
	if c.deleteErr != nil {
		return 0, c.deleteErr
	}
	for _, id := range ids {
		c.deletes = append(c.deletes, id)
	}
	return len(ids), nil
}
