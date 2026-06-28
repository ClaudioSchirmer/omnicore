package write

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Audit-builder pure-function branches. Relocated from the former infra-root
// coverage grab-bag once audit_builder.go moved to package db.

func TestGoFieldValues_EdgeCases(t *testing.T) {
	if got := (*core.TableSchema)(nil).GoFieldValues(&builderTestEntity{}); len(got) != 0 {
		t.Errorf("nil schema → %v, want empty", got)
	}
	// Non-struct value (after deref) → empty map.
	if got := builderTestSchema.GoFieldValues(42); len(got) != 0 {
		t.Errorf("non-struct → %v, want empty", got)
	}
	// Happy path keyed by Go field name.
	got := builderTestSchema.GoFieldValues(&builderTestEntity{Name: "alice", Email: "a@x.com"})
	if got["Name"] != "alice" || got["Email"] != "a@x.com" {
		t.Errorf("goFieldValues = %v", got)
	}
}

func TestExtractTenantID_Shapes(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"nil", nil, ""},
		{"absent", map[string]any{"x": "y"}, ""},
		{"string", map[string]any{"tenant_id": "acme"}, "acme"},
		{"sliceString", map[string]any{"tenant_id": []string{"acme"}}, "acme"},
		{"sliceStringMulti", map[string]any{"tenant_id": []string{"a", "b"}}, ""},
		{"sliceAny", map[string]any{"tenant_id": []any{"acme"}}, "acme"},
		{"sliceAnyNonString", map[string]any{"tenant_id": []any{42}}, ""},
		{"unsupported", map[string]any{"tenant_id": 99}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractTenantID(c.claims); got != c.want {
				t.Errorf("extractTenantID = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOldFieldsOf_Branches(t *testing.T) {
	// nil entity → nil.
	if got := oldFieldsOf(builderTestSchema, nil); got != nil {
		t.Errorf("nil entity → %v, want nil", got)
	}
	// Entity with no Old (insert path) → nil.
	fresh := &builderTestEntity{Name: "alice"}
	if got := oldFieldsOf(builderTestSchema, fresh); got != nil {
		t.Errorf("no-Old entity → %v, want nil", got)
	}
	// Entity carrying Old (update path) → pre-mutation snapshot. The entity needs
	// an ID + the mapped fields populated so GetUpdatable's validation passes.
	e := &builderTestEntity{Name: "alice", Email: "a@x.com"}
	e.SetID(domain.NewID(uuid.NewString()))
	u, err := domain.GetUpdatable(e, func(x *builderTestEntity) error { x.Name = "bob"; return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	got := oldFieldsOf(builderTestSchema, u.Source())
	if got == nil || got["Name"] != "alice" {
		t.Errorf("old snapshot = %v, want Name=alice", got)
	}
}

// childEventOf's remaining branches via direct construction of AggregateItem.
func TestChildEventOf_RemainingBranches(t *testing.T) {
	child := covAggSchema.ChildSchema("covChild")
	mk := func(status domain.AggregateItemStatus) domain.AggregateItem[domain.AggregateValueObject] {
		return domain.NewAggregateItem[domain.AggregateValueObject](covChild{ID: "c1", Label: "x"}, status)
	}

	// update + Changed → updated + changes.
	prev := map[string]map[string]map[string]any{
		"covChild": {"c1": {"Label": "old"}},
	}
	ev, ok := childEventOf(mk(domain.StatusChanged), child, "covChild", "update", prev)
	if !ok || ev.Op != "updated" {
		t.Errorf("update/Changed → %+v ok=%v, want updated", ev, ok)
	}

	// update + Constructor → skipped (default).
	if _, ok := childEventOf(mk(domain.StatusConstructor), child, "covChild", "update", nil); ok {
		t.Error("update/Constructor should be skipped")
	}

	// archive + Removed → skipped.
	if _, ok := childEventOf(mk(domain.StatusRemoved), child, "covChild", "archive", nil); ok {
		t.Error("archive/Removed should be skipped")
	}
	// unarchive + Removed → skipped.
	if _, ok := childEventOf(mk(domain.StatusRemoved), child, "covChild", "unarchive", nil); ok {
		t.Error("unarchive/Removed should be skipped")
	}
	// delete + Removed → skipped.
	if _, ok := childEventOf(mk(domain.StatusRemoved), child, "covChild", "delete", nil); ok {
		t.Error("delete/Removed should be skipped")
	}
	// unknown verb → skipped.
	if _, ok := childEventOf(mk(domain.StatusAdded), child, "covChild", "bogus", nil); ok {
		t.Error("unknown verb should be skipped")
	}
}
