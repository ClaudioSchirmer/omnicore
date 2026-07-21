package write

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- #6: BaseRepository.WithSchema validates the flat path -------------------

// flatArchivable is a non-aggregate entity declaring Archive in Modes(). The
// validated WithSchema must reject a schema with no SoftDelete column at
// construction.
type flatArchivable struct {
	domain.BaseEntity
	Name string
}

func (e *flatArchivable) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeArchive, domain.ModeUnarchive}
}
func (e *flatArchivable) BuildRules(string, domain.Service, *domain.Rules) {}

func TestBaseRepositoryWithSchema_ModesVsSoftDelete_Panics(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{NewEntity: func() *flatArchivable { return &flatArchivable{} }}
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").Revision("revision") // no SoftDelete
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic: Archive in Modes() without SoftDelete on the flat path")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "SoftDelete") {
			t.Errorf("panic must mention SoftDelete, got %q", msg)
		}
	}()
	repo.WithSchema(schema)
}

func TestBaseRepositoryWithSchema_Valid_SetsSchema(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{NewEntity: func() *flatArchivable { return &flatArchivable{} }}
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").Revision("revision").SoftDelete("deleted_at")
	repo.WithSchema(schema)
	if repo.Schema != schema {
		t.Error("WithSchema must bind the schema on the happy path")
	}
}

// TestBaseRepositoryWithSchema_ExternalRoot_Panics asserts the write binding
// rejects a type-less (NewExternalSchema) schema at construction: a write-backed
// root must be anchored to a Go type. An external schema is a view-embed source
// only — without a struct the persister cannot build INSERT/UPDATE and the
// composer cannot restore boolean fidelity (BoolColumns) when materializing the
// Mongo view, so a type-less root that named a real local table would compose
// relationally and silently lose bools on MySQL. The guard turns that into a
// loud boot failure.
func TestBaseRepositoryWithSchema_ExternalRoot_Panics(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{NewEntity: func() *flatArchivable { return &flatArchivable{} }}
	external := NewExternalSchema("flats").PK("id").Field("Name", "name")
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic: external/type-less schema bound to a write repository")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "type-anchored") {
			t.Errorf("panic must mention type-anchored, got %q", msg)
		}
	}()
	repo.WithSchema(external)
}

func TestBaseRepositoryWithSchema_NilFactory_Panics(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{} // NewEntity nil
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").Revision("revision").SoftDelete("deleted_at")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic: nil NewEntity surfaced at WithSchema construction")
		}
	}()
	repo.WithSchema(schema)
}
