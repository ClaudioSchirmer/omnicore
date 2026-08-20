//go:build postgres || mysql || sqlserver || oracle || sqlite

package bootstrap

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/gofiber/fiber/v3"
)

// searchGuardReader is a non-nil RelationalReader — RelationalSource marks the
// view by the reader it stores, so the stub has to be a real value.
type searchGuardReader struct{}

func (searchGuardReader) FindAllEntities(context.Context, *criteria.Query) ([]domain.Entity, error) {
	return nil, nil
}
func (searchGuardReader) CountEntities(context.Context, *criteria.Query) (int64, error) {
	return 0, nil
}
func (searchGuardReader) BoundTable() string { return "guard_rows" }

type searchGuardEntity struct {
	ID   string
	Name string
}

func searchGuardSchema() *core.TableSchema {
	return core.NewTableSchema[searchGuardEntity]("guard_rows").
		ID("id").
		Field("Name", "name")
}

// searchGuardFeature contributes one view and mounts nothing — the guard reads
// the declarations, not the routes.
type searchGuardFeature struct{ views []*query.ViewDefinition }

func (f *searchGuardFeature) Mount(*fiber.App, Deps)         {}
func (f *searchGuardFeature) Views() []*query.ViewDefinition { return f.views }

func withOptIn(t *testing.T, view string) {
	t.Helper()
	queryschema.ResetSearchOptIns()
	queryschema.RecordSearchOptIn(view, "requests.FindGuardRowsRequest")
	t.Cleanup(queryschema.ResetSearchOptIns)
}

func TestVerifySearchIndexes_FailsWhenTheViewDeclaresNoTextIndex(t *testing.T) {
	withOptIn(t, "guard_rows")
	feat := &searchGuardFeature{views: []*query.ViewDefinition{
		query.View("guard_rows").Schema(searchGuardSchema()).
			Indexes(query.Index("name")),
	}}

	err := verifySearchIndexes([]Feature{feat})
	if err == nil {
		t.Fatal("an endpoint accepting ?search= over an index-less view must fail the boot")
	}
	for _, want := range []string{"guard_rows", "FindGuardRowsRequest", "TextIndex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diagnostic must mention %q: %v", want, err)
		}
	}
}

func TestVerifySearchIndexes_PassesWhenTheIndexIsDeclared(t *testing.T) {
	withOptIn(t, "guard_rows")
	feat := &searchGuardFeature{views: []*query.ViewDefinition{
		query.View("guard_rows").Schema(searchGuardSchema()).
			Indexes(query.TextIndex("name")),
	}}

	if err := verifySearchIndexes([]Feature{feat}); err != nil {
		t.Fatalf("a declared text index satisfies the guard: %v", err)
	}
}

// Free text over the SoR is a declared capability boundary answered with a
// typed 400, not a misconfiguration — and a DTO shared between a Mongo view and
// its relational twin is the canonical shape.
func TestVerifySearchIndexes_SkipsARelationalView(t *testing.T) {
	withOptIn(t, "guard_rows")
	feat := &searchGuardFeature{views: []*query.ViewDefinition{
		query.View("guard_rows").Schema(searchGuardSchema()).
			RelationalSource(searchGuardReader{}),
	}}

	if err := verifySearchIndexes([]Feature{feat}); err != nil {
		t.Fatalf("a relational view must not fail the boot: %v", err)
	}
}

// The registry is process-wide, so a name this composition root does not
// declare is not its business.
func TestVerifySearchIndexes_IgnoresAForeignViewName(t *testing.T) {
	withOptIn(t, "someone_elses_view")
	feat := &searchGuardFeature{views: []*query.ViewDefinition{
		query.View("guard_rows").Schema(searchGuardSchema()),
	}}

	if err := verifySearchIndexes([]Feature{feat}); err != nil {
		t.Fatalf("an unknown view name must be ignored: %v", err)
	}
}

// TestVerifySearchIndexes_DrainsTheRegistry — the registry is process-wide, so
// a boot that did not clear it would hand its declarations to the NEXT one. Two
// composition roots in one binary (tests, a multi-app process) that happen to
// share a view name would then make the second boot fail over the first's
// Request DTO. Each boot verifies exactly what was recorded since the previous.
func TestVerifySearchIndexes_DrainsTheRegistry(t *testing.T) {
	withOptIn(t, "guard_rows")
	indexed := &searchGuardFeature{views: []*query.ViewDefinition{
		query.View("guard_rows").Schema(searchGuardSchema()).
			Indexes(query.TextIndex("name")),
	}}
	if err := verifySearchIndexes([]Feature{indexed}); err != nil {
		t.Fatalf("the declared pair must pass: %v", err)
	}
	// Second boot, same binary, same view name — but WITHOUT a text index and
	// without any declaration of its own. The first boot's record is gone, so
	// there is nothing to fail over.
	bare := &searchGuardFeature{views: []*query.ViewDefinition{
		query.View("guard_rows").Schema(searchGuardSchema()),
	}}
	if err := verifySearchIndexes([]Feature{bare}); err != nil {
		t.Fatalf("a later boot must not inherit an earlier one's declarations: %v", err)
	}
}

// The recorder every read surface calls is the one seat that decides whether a
// declaration enters the registry: a DTO that does not declare `query:"search"`
// contributes nothing, and neither does a handler that cannot name its view.
func TestRecordSearchDeclaration_OnlyRecordsTheDeclaredPair(t *testing.T) {
	queryschema.ResetSearchOptIns()
	t.Cleanup(queryschema.ResetSearchOptIns)

	type searching struct {
		Search *string `query:"search"`
	}
	type silent struct {
		First *int64 `query:"first"`
	}

	searchingSchema := queryschema.ExtractRequestSchema(reflectTypeOf(searching{}))
	silentSchema := queryschema.ExtractRequestSchema(reflectTypeOf(silent{}))

	// No `query:"search"` → nothing recorded, whatever the handler is.
	queryschema.RecordSearchDeclaration(silentSchema, "silent", namedViewHandler{view: "guard_rows"})
	// Declares it, but the handler does not name a view → not covered.
	queryschema.RecordSearchDeclaration(searchingSchema, "anonymous", struct{}{})
	if got := queryschema.SearchOptIns(); len(got) != 0 {
		t.Fatalf("neither shape may enter the registry, got %v", got)
	}

	queryschema.RecordSearchDeclaration(searchingSchema, "requests.FindGuardRowsRequest",
		namedViewHandler{view: "guard_rows"})
	got := queryschema.SearchOptIns()
	if len(got) != 1 || got[0].View != "guard_rows" || got[0].Request != "requests.FindGuardRowsRequest" {
		t.Fatalf("the declared pair must be recorded, got %v", got)
	}
}

type namedViewHandler struct{ view string }

func (h namedViewHandler) ViewName() string { return h.view }

func TestVerifySearchIndexes_NoDeclarationsIsANoOp(t *testing.T) {
	queryschema.ResetSearchOptIns()
	t.Cleanup(queryschema.ResetSearchOptIns)
	if err := verifySearchIndexes(nil); err != nil {
		t.Fatalf("nothing declared, nothing to check: %v", err)
	}
}

// reflectTypeOf keeps the schema-extraction calls above readable.
func reflectTypeOf(v any) reflect.Type { return reflect.TypeOf(v) }
