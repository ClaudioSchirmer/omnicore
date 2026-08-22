package relational

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// TestBuildDocument_SharedBaseFieldsFlattened proves the read-side doc a
// RelationalSource view over a shared-base ROLE serves carries the BASE's business
// fields flattened under their physical columns — read off the loaded role entity
// via SharedBaseScanPlan — alongside the role's own fields. Without this the reader
// could filter by a base field but never return it (the bug the account/user
// relational twins surfaced). baseEnt + sharedBaseSchema are declared in
// relational_view_reader_test.go.
func TestBuildDocument_SharedBaseFieldsFlattened(t *testing.T) {
	e := &baseEnt{HolderName: "Ada", DisplayName: "ACME Corp"}
	e.SetID(domain.NewID("11111111-1111-1111-1111-111111111111"))

	doc := BuildDocument(sharedBaseSchema("holders"), e)

	if got := doc["display_name"]; got != "ACME Corp" {
		t.Errorf("shared-base field not flattened into the doc: display_name=%v, want %q", got, "ACME Corp")
	}
	if got := doc["holder_name"]; got != "Ada" {
		t.Errorf("role's own field missing/wrong: holder_name=%v, want %q", got, "Ada")
	}
	if _, ok := doc["id"]; !ok {
		t.Error("the shared id must be present on the doc")
	}
}

// TestBuildDocument_PlainAggregateHasNoBaseMerge is the negative control: a plain
// aggregate (no shared base) is unchanged by the shared-base merge — only its own
// columns land, so the plain-view doc shape (and the Composer parity) is untouched.
func TestBuildDocument_PlainAggregateHasNoBaseMerge(t *testing.T) {
	e := &guardEnt{Name: "widget", Material: "steel"}
	e.SetID(domain.NewID("22222222-2222-2222-2222-222222222222"))

	// guardSchema owns only Name (+ id) — no sibling, no shared base declared.
	doc := BuildDocument(guardSchema("gadgets"), e)

	if got := doc["name"]; got != "widget" {
		t.Errorf("root field: name=%v, want %q", got, "widget")
	}
	// Material belongs to a sibling guardSchema does NOT declare, so it must not
	// leak into the plain doc.
	if _, ok := doc["material"]; ok {
		t.Error("a plain aggregate doc must not carry an undeclared sibling column")
	}
}
