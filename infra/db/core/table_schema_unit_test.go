package core

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// These exercise the boot-time panic guards of TableSchema construction and the
// type-less (external) read/write helpers that take the index<0 branch — all
// pure, no database.

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	fn()
}

func TestNewTableSchema_NonStructPanics(t *testing.T) {
	mustPanic(t, "NewTableSchema[int]", func() { NewTableSchema[int]("bad") })
}

func TestField_MissingFieldPanics(t *testing.T) {
	mustPanic(t, "unknown field", func() {
		NewTableSchema[*builderTestEntity]("t").ID("id").Field("Nope", "nope")
	})
}

func TestField_DuplicateGoFieldPanics(t *testing.T) {
	mustPanic(t, "duplicate go field", func() {
		NewTableSchema[*builderTestEntity]("t").ID("id").
			Field("Name", "name").Field("Name", "other")
	})
}

func TestField_DuplicateColumnPanics(t *testing.T) {
	mustPanic(t, "duplicate column", func() {
		NewTableSchema[*builderTestEntity]("t").ID("id").
			Field("Name", "shared").Field("Email", "shared")
	})
}

func TestField_LabelKeyOnTypeAnchoredPanics(t *testing.T) {
	mustPanic(t, "labelKey external-only", func() {
		NewTableSchema[*builderTestEntity]("t").ID("id").
			Field("Name", "name", "SomeLabelKey")
	})
}

func TestField_TooManyLabelKeysPanics(t *testing.T) {
	mustPanic(t, "two labelKeys", func() {
		NewExternalSchema("ext").ID("id").Field("Name", "name", "a", "b")
	})
}

func TestEnsureColumnFree_EmptyColumnIsNoop(t *testing.T) {
	// SoftDelete("") drives ensureColumnFree's empty-column early return without
	// claiming any column.
	s := NewTableSchema[*builderTestEntity]("t").ID("id").Field("Name", "name")
	s.ensureColumnFree("", "SoftDelete") // must not panic
	if _, ok := s.softDeleteColumn(); ok {
		t.Error("no soft-delete should be set")
	}
}

func TestEnsureColumnFree_UpdatedAtCollisionPanics(t *testing.T) {
	mustPanic(t, "updated_at collision", func() {
		// UpdatedAt claims "ts"; declaring SoftDelete on the same column must
		// trip the UpdatedAt collision arm of ensureColumnFree.
		NewTableSchema[*builderTestEntity]("t").ID("id").
			UpdatedAt("ts").SoftDelete("ts")
	})
}

func TestChild_RequiresTypeAnchoredPanics(t *testing.T) {
	mustPanic(t, "external child", func() {
		NewTableSchema[*builderTestEntity]("cov").ID("id").Field("Name", "name").
			Child(NewExternalSchema("cov_children").ID("id").ParentID("cov_agg_id"))
	})
}

// External (type-less) schemas carry fields with index < 0, so writeFields and
// ScanPlan take the index<0 skip branch — exercised here via direct calls.
func TestExternalSchema_WriteFieldsSkipsUnindexed(t *testing.T) {
	ext := NewExternalSchema("users").ID("id").Field("Name", "name")
	out := ext.writeFields(struct{ Name string }{Name: "x"})
	if len(out) != 0 {
		t.Errorf("external writeFields must skip unindexed fields, got %v", out)
	}
}

func TestExternalSchema_ScanPlanSkipsUnindexed(t *testing.T) {
	ext := NewExternalSchema("users").ID("id").Field("Name", "name")
	cols, byCol := ext.ScanPlan()
	// idIndex is -1 for a type-less schema, and each field index is -1, so the
	// scan plan is empty.
	if len(cols) != 0 || len(byCol) != 0 {
		t.Errorf("external ScanPlan must be empty, got cols=%v byCol=%v", cols, byCol)
	}
}

func TestValidateModes_ArchiveWithoutSoftDeletePanics(t *testing.T) {
	noSD := NewTableSchema[*builderTestEntity]("t").ID("id").Field("Name", "name")
	mustPanic(t, "ValidateModes", func() {
		noSD.ValidateModes([]domain.EntityMode{domain.ModeArchive})
	})
}
