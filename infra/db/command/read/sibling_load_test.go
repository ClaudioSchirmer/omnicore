package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// White-box coverage for AggregateLoader.hydrateSiblings (A3b): an owner
// sibling's columns are loaded into the SAME entity struct by the shared PK,
// through the neutral read seam. Loading the sibling is what lets a later UPDATE
// see the row's real sibling values rather than clobbering them with zeros.

type sibLoadEntity struct {
	domain.BaseEntity
	Name     string
	UserName string // sibling field (mapped to the "usuario" table)
}

func (e *sibLoadEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate}
}
func (e *sibLoadEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func sibLoadSchema() *TableSchema {
	return NewTableSchema[*sibLoadEntity]("pessoa").
		PK("id").
		Field("Name", "name").
		Sibling(NewSiblingSchema[*sibLoadEntity]("usuario").Field("UserName", "user_name"))
}

// A present sibling row populates the owner's sibling field (no JOIN: the SELECT
// is against the sibling table alone, keyed by the shared PK).
func TestHydrateSiblings_PopulatesOwnerField(t *testing.T) {
	query := func(sql string, args []any) (Rows, error) {
		if !strings.Contains(sql, "FROM usuario") {
			t.Fatalf("unexpected SQL: %q", sql)
		}
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			// ScanLeadingKey targets: dest[0]=&leadingKey(pk), dest[1]=&UserName.
			if p, ok := dest[0].(*string); ok {
				*p = "r1"
			}
			if p, ok := dest[1].(*string); ok {
				*p = "alice"
			}
			return nil
		}}, nil
	}
	l := NewAggregateLoader[*sibLoadEntity](fakeEngine(query), func() *sibLoadEntity { return &sibLoadEntity{} }).
		WithSchema(sibLoadSchema())

	e := &sibLoadEntity{Name: "x"}
	e.SetID(domain.NewID("r1"))
	if err := l.hydrateSiblings(context.Background(), []*sibLoadEntity{e}, []string{"r1"}); err != nil {
		t.Fatalf("hydrateSiblings: %v", err)
	}
	if e.UserName != "alice" {
		t.Errorf("sibling field not hydrated: UserName=%q, want \"alice\"", e.UserName)
	}
}

// An absent sibling row leaves the owner's sibling field at its zero value (no
// row scanned), and is not an error.
func TestHydrateSiblings_AbsentRowLeavesZero(t *testing.T) {
	query := func(string, []any) (Rows, error) { return &fakeDBRows{rows: 0}, nil }
	l := NewAggregateLoader[*sibLoadEntity](fakeEngine(query), func() *sibLoadEntity { return &sibLoadEntity{} }).
		WithSchema(sibLoadSchema())

	e := &sibLoadEntity{Name: "x", UserName: ""}
	e.SetID(domain.NewID("r1"))
	if err := l.hydrateSiblings(context.Background(), []*sibLoadEntity{e}, []string{"r1"}); err != nil {
		t.Fatalf("hydrateSiblings: %v", err)
	}
	if e.UserName != "" {
		t.Errorf("absent sibling must leave the field zero, got %q", e.UserName)
	}
}

// A schema without siblings is a no-op (and never queries).
func TestHydrateSiblings_NoSiblingsNoQuery(t *testing.T) {
	query := func(string, []any) (Rows, error) {
		t.Fatal("hydrateSiblings must not query when no siblings are declared")
		return nil, nil
	}
	schema := NewTableSchema[*sibLoadEntity]("pessoa").PK("id").Field("Name", "name")
	l := NewAggregateLoader[*sibLoadEntity](fakeEngine(query), func() *sibLoadEntity { return &sibLoadEntity{} }).
		WithSchema(schema)

	e := &sibLoadEntity{Name: "x"}
	e.SetID(domain.NewID("r1"))
	if err := l.hydrateSiblings(context.Background(), []*sibLoadEntity{e}, []string{"r1"}); err != nil {
		t.Fatalf("hydrateSiblings: %v", err)
	}
}
