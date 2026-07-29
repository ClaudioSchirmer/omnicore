package write

import "testing"

// mustPanicRepo asserts fn panics; the schema-construction panic guards that
// run through the write binding (BaseRepository.WithSchema) live here in db,
// while the pure-schema guards moved to the core package alongside TableSchema.
func mustPanicRepo(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	fn()
}

// TestWithSchema_NoPKPanics asserts the write binding rejects a ID-less schema
// at WithSchema (boot) rather than at first write. The schema itself lives in
// core; the BaseRepository guard is a db-side concern, so the test stays here.
func TestWithSchema_NoPKPanics(t *testing.T) {
	repo := &BaseRepository[*builderTestEntity]{
		NewEntity: func() *builderTestEntity { return &builderTestEntity{} },
	}
	noPK := NewTableSchema[*builderTestEntity]("t").Field("Name", "name")
	mustPanicRepo(t, "WithSchema no ID", func() { repo.WithSchema(noPK) })
}
