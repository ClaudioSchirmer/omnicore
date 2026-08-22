//go:build integration && postgres

package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	query "github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// A by-id read has no wire `?fields=`, so a Projection reaching the reader came
// from the Query's ToCriteria — most often ReadCriteria.Restrict, the
// field-level access-control seam, which implements the removal by writing an
// exclusion into that projection.
//
// ReadByID used to build its FindOne with no options at all, so the exclusion
// was dropped on the floor: the restricted field came back on a Mongo-backed
// view while the relational reader honored it. These tests pin the fix against
// a REAL server, because the bug lived exactly in the options the driver was
// (not) handed — a fake collection would have agreed with either version.

type rbpUser struct{ ID, Name, Phone string }

func rbpView() *query.ViewDefinition {
	schema := core.NewTableSchema[rbpUser]("rbp_users").
		ID("id").
		Field("Name", "name").
		Field("Phone", "phone").
		DeletedAt("deleted_at")
	return query.View("rbp_users").Version(1).Schema(schema)
}

func seedRBPUser(t *testing.T, m *MongoDB) {
	t.Helper()
	doc := map[string]any{
		"_id":        "u1",
		"name":       "Alice",
		"phone":      "+5511999998888",
		"deleted_at": nil,
	}
	if err := m.Upsert(context.Background(), pc("rbp_users"), "u1", doc); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestReadByID_HonorsRestrictExclusion — a passive Restrict (the caller never
// named the field) scrubs it from the read. On the by-id route too.
func TestReadByID_HonorsRestrictExclusion(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	seedRBPUser(t, m)
	r := NewMongoViewReader(m, testResolver).SetViews([]*query.ViewDefinition{rbpView()})

	crit := queries.ReadCriteria{Filter: map[string]any{}}
	if err := crit.Restrict("Phone"); err != nil {
		t.Fatalf("a passive Restrict must not error: %v", err)
	}

	doc, found, err := r.ReadByID(context.Background(), "rbp_users", "u1", crit)
	if err != nil || !found {
		t.Fatalf("read: err=%v found=%v", err, found)
	}
	if _, leaked := doc["Phone"]; leaked {
		t.Errorf("the restricted field must not reach the caller, got %v", doc)
	}
	if doc["Name"] != "Alice" {
		t.Errorf("the rest of the document must still be served, got %v", doc)
	}
}

// TestReadByID_HonorsInclusionProjection — the other projection mode: an
// explicit inclusion narrows the document to what ToCriteria asked for.
func TestReadByID_HonorsInclusionProjection(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	seedRBPUser(t, m)
	r := NewMongoViewReader(m, testResolver).SetViews([]*query.ViewDefinition{rbpView()})

	doc, found, err := r.ReadByID(context.Background(), "rbp_users", "u1",
		queries.ReadCriteria{Filter: map[string]any{}, Projection: queries.ProjectOnlyPaths("Name")})
	if err != nil || !found {
		t.Fatalf("read: err=%v found=%v", err, found)
	}
	if doc["Name"] != "Alice" {
		t.Errorf("the included field must be served, got %v", doc)
	}
	if _, leaked := doc["Phone"]; leaked {
		t.Errorf("a field outside the inclusion must not be served, got %v", doc)
	}
}

// TestReadByID_NoProjectionServesTheWholeDocument — the default is unchanged:
// nothing declared, nothing narrowed.
func TestReadByID_NoProjectionServesTheWholeDocument(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	seedRBPUser(t, m)
	r := NewMongoViewReader(m, testResolver).SetViews([]*query.ViewDefinition{rbpView()})

	doc, found, err := r.ReadByID(context.Background(), "rbp_users", "u1",
		queries.ReadCriteria{Filter: map[string]any{}})
	if err != nil || !found {
		t.Fatalf("read: err=%v found=%v", err, found)
	}
	for _, want := range []string{"Name", "Phone"} {
		if _, ok := doc[want]; !ok {
			t.Errorf("an unprojected by-id read must serve %q, got %v", want, doc)
		}
	}
}
