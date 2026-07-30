package mongo

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The reader promises Go vocabulary — names AND values. A BSON datetime must
// reach every consumer (typed projection, tabular export, RawDoc handlers) as
// time.Time, recursively through the nested child collections.
func TestNormalizeBSONValues_DatetimeToTimeRecursively(t *testing.T) {
	ts := time.Date(2015, 3, 10, 0, 0, 0, 0, time.UTC)
	doc := map[string]any{
		"name":       "Ana",
		"birth_date": bson.NewDateTimeFromTime(ts),
		"children": bson.A{
			bson.M{"hired_at": bson.NewDateTimeFromTime(ts), "job_title": "Engineer"},
		},
	}
	normalizeBSONValues(doc)

	got, ok := doc["birth_date"].(time.Time)
	if !ok || !got.Equal(ts) {
		t.Fatalf("root datetime must become time.Time(%v), got %T %v", ts, doc["birth_date"], doc["birth_date"])
	}
	items, ok := doc["children"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("bson.A must normalize to []any, got %T", doc["children"])
	}
	child, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("bson.M must normalize to map[string]any, got %T", items[0])
	}
	if nested, ok := child["hired_at"].(time.Time); !ok || !nested.Equal(ts) {
		t.Errorf("nested datetime must become time.Time, got %T %v", child["hired_at"], child["hired_at"])
	}
	if child["job_title"] != "Engineer" || doc["name"] != "Ana" {
		t.Errorf("non-datetime values must pass through untouched: %v", doc)
	}
}

// projectionTouchesField drives the child DeletedAt auto-include: a
// projection narrowing any child subfield (or the whole child) must pull the
// child's DeletedAt column so the archived-entry strip can see it.
func TestProjectionTouchesField(t *testing.T) {
	proj := map[string]int{"name": 1, "Dependents.name": 1}
	if !projectionTouchesField(proj, "Dependents") {
		t.Error("subfield projection must touch the child")
	}
	if projectionTouchesField(proj, "JobHistories") {
		t.Error("unprojected child must not be touched")
	}
	whole := map[string]int{"Dependents": 1}
	if !projectionTouchesField(whole, "Dependents") {
		t.Error("whole-child projection must touch the child")
	}
}
