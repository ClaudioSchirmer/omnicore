package core

import (
	"reflect"
	"testing"
)

// ReadColumns is the explicit physical column list a read issues instead of
// SELECT *. These tests lock its completeness (nothing SELECT * would return is
// dropped) and its deterministic order (a stable prepared-statement cache key).

// TestReadColumns_FlatRoot — ID, business fields (declaration order), then the
// managed columns created_at, updated_at, deleted_at, revision.
func TestReadColumns_FlatRoot(t *testing.T) {
	s := NewTableSchema[schemaSample]("t").
		ID("id").
		Revision("revision").
		Field("Name", "name").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		DeletedAt("deleted_at")
	got := s.ReadColumns()
	want := []string{"id", "name", "created_at", "updated_at", "deleted_at", "revision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadColumns() = %v, want %v", got, want)
	}
}

// TestReadColumns_AggregateChild — a child carries its ParentID to the root and no
// managed/revision columns unless declared; the ParentID follows the business fields.
func TestReadColumns_AggregateChild(t *testing.T) {
	child := NewTableSchema[schemaSample]("addresses").
		ID("id").
		ParentID("user_id").
		Field("Name", "label")
	got := child.ReadColumns()
	want := []string{"id", "label", "user_id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadColumns() = %v, want %v", got, want)
	}
}

// TestReadColumns_SharedBaseRole — a role table carries the ParentID linking it to its
// shared base; ReadColumns must include it (it lives in sharedBaseLink, not the
// field set, so a naive ID+fields+managed list would silently drop it).
func TestReadColumns_SharedBaseRole(t *testing.T) {
	base := NewSharedBaseSchema("persons").
		Revision("revision").
		ID("id").
		Field("Removed", "removed").
		NaturalID("removed")
	role := NewTableSchema[schemaSample]("employees").
		Revision("revision").
		ID("id").
		Field("Name", "name").
		DeletedAt("deleted_at").
		SharedBase(base, "person_id")
	got := role.ReadColumns()
	want := []string{"id", "name", "person_id", "deleted_at", "revision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadColumns() = %v, want %v", got, want)
	}
}

// TestReadColumns_DeduplicatesPKEqualsFK — when a role links to its base through
// the ID column itself (ID == ParentID), the column appears once.
func TestReadColumns_DeduplicatesPKEqualsFK(t *testing.T) {
	base := NewSharedBaseSchema("persons").
		Revision("revision").
		ID("id").
		Field("Removed", "removed").
		NaturalID("removed")
	role := NewTableSchema[schemaSample]("employees").
		Revision("revision").
		ID("id").
		Field("Name", "name").
		SharedBase(base, "id") // ParentID == ID
	got := role.ReadColumns()
	want := []string{"id", "name", "revision"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadColumns() = %v, want %v", got, want)
	}
}
