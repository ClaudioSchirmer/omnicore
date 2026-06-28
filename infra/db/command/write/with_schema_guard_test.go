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
	schema := NewTableSchema[*flatArchivable]("flats").PK("id") // no SoftDelete
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
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").SoftDelete("deleted_at")
	repo.WithSchema(schema)
	if repo.Schema != schema {
		t.Error("WithSchema must bind the schema on the happy path")
	}
}

func TestBaseRepositoryWithSchema_NilFactory_Panics(t *testing.T) {
	repo := &BaseRepository[*flatArchivable]{} // NewEntity nil
	schema := NewTableSchema[*flatArchivable]("flats").PK("id").SoftDelete("deleted_at")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic: nil NewEntity surfaced at WithSchema construction")
		}
	}()
	repo.WithSchema(schema)
}
