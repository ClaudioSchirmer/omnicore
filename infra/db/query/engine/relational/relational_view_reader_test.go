package relational

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// guardEnt is a minimal entity — enough to build an AggregateLoader and a schema
// so the boot guard can be exercised without a database (BoundTable reads only
// the loader's WithSchema table).
type guardEnt struct {
	domain.BaseEntity
	Name string
}

func (e *guardEnt) Modes() []domain.EntityMode                     { return []domain.EntityMode{domain.ModeInsert} }
func (e *guardEnt) BuildRules(string, domain.Service, *domain.Rules) {}

func guardSchema(table string) *core.TableSchema {
	return core.NewTableSchema[*guardEnt](table).ID("id").Field("Name", "name")
}

func guardLoader(table string) query.RelationalReader {
	return read.NewAggregateLoader[*guardEnt](nil, func() *guardEnt { return &guardEnt{} }).WithSchema(guardSchema(table))
}

// TestNewRelationalViewReader_WrongLoaderTablePanics is the boot guard: a view
// handed a loader bound to a different entity's table fails the boot loudly,
// naming both tables — never silently serving the wrong aggregate.
func TestNewRelationalViewReader_WrongLoaderTablePanics(t *testing.T) {
	vdef := query.View("v").Schema(guardSchema("gadgets")).Version(1).RelationalSource(guardLoader("users"))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a boot panic for a loader bound to the wrong table")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "users") || !strings.Contains(msg, "gadgets") {
			t.Errorf("panic must name both tables, got %q", msg)
		}
	}()
	NewRelationalViewReader([]*query.ViewDefinition{vdef})
}

// TestNewRelationalViewReader_MatchingLoaderRegisters confirms the happy path:
// a loader bound to the view's own table registers the relational view.
func TestNewRelationalViewReader_MatchingLoaderRegisters(t *testing.T) {
	vdef := query.View("v").Schema(guardSchema("gadgets")).Version(1).RelationalSource(guardLoader("gadgets"))
	r := NewRelationalViewReader([]*query.ViewDefinition{vdef})
	if r.Empty() {
		t.Fatal("a matching relational view must be registered")
	}
}

// TestNewRelationalViewReader_MongoViewSkipped confirms a view without the marker
// is left to the Mongo reader — the relational reader indexes nothing.
func TestNewRelationalViewReader_MongoViewSkipped(t *testing.T) {
	vdef := query.View("v").Schema(guardSchema("gadgets")).Version(1)
	r := NewRelationalViewReader([]*query.ViewDefinition{vdef})
	if !r.Empty() {
		t.Fatal("a Mongo-backed view must not be indexed by the relational reader")
	}
}
