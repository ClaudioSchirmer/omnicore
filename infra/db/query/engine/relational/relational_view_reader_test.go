package relational

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
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

// assertRelationalCapability400 checks that a capability the relational reader
// cannot serve surfaces as a NotificationCarrier whose single notification is a
// RelationalCapabilityNotification with SemanticSchema — the wire mapping turns
// that into a 400 (not a generic 500), and the offending field/capability rides
// through as the notification's field name.
func assertRelationalCapability400(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an unsupported-capability error, got nil")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("error must be a NotificationCarrier (maps to a typed status), got %T: %v", err, err)
	}
	ctxs := carrier.NotificationContexts()
	if len(ctxs) != 1 || len(ctxs[0].Messages()) != 1 {
		t.Fatalf("expected exactly one notification, got contexts=%d", len(ctxs))
	}
	msg := ctxs[0].Messages()[0]
	if got := reflect.TypeOf(msg.Notification).Name(); got != "RelationalCapabilityNotification" {
		t.Errorf("notification = %q, want RelationalCapabilityNotification", got)
	}
	if got := msg.Notification.Semantic(); got != domain.SemanticSchema {
		t.Errorf("semantic = %v, want SemanticSchema (→400)", got)
	}
	if msg.ResolveFieldName() != wantField && msg.FieldName != wantField {
		t.Errorf("field = %q, want the offending capability %q", msg.ResolveFieldName(), wantField)
	}
}

// TestUnsupportedChildFilter_MapsTo400 covers a filter pushed at a child (dotted)
// field: a root SELECT cannot express it, so the reader rejects it as a 400.
func TestUnsupportedChildFilter_MapsTo400(t *testing.T) {
	_, err := toExpr(map[string]any{"Addresses.ZipCode": "12345"})
	assertRelationalCapability400(t, err, "Addresses.ZipCode")
}

// TestUnsupportedChildSort_MapsTo400 covers a sort on a child (dotted) field:
// a root ORDER BY cannot express it, so the reader rejects it as a 400.
func TestUnsupportedChildSort_MapsTo400(t *testing.T) {
	err := applySort(criteria.Where(nil), []queries.SortField{{Field: "Addresses.ZipCode"}})
	assertRelationalCapability400(t, err, "Addresses.ZipCode")
}
