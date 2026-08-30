package core

import (
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The DECLARATION half of a stamped field: what a schema accepts, what it
// refuses, and the guards that exist because a wrong declaration would fail
// silently at write time instead of loudly at boot.

type stampDeclOrder struct {
	ID       domain.ID
	Status   string
	PaidAt   *time.Time
	SignedAt time.Time
}

func stampDeclSchema() *TableSchema {
	return NewTableSchema[*stampDeclOrder]("orders").
		ID("id").
		Field("Status", "status").
		StampedTimeField("PaidAt", "paid_at")
}

func TestStampedTimeField_JoinsTheBijectionLikeAnyField(t *testing.T) {
	s := stampDeclSchema()
	if col, ok := s.ColumnOf("PaidAt"); !ok || col != "paid_at" {
		t.Fatalf("a stamped field maps like any other: %q %v", col, ok)
	}
	if !s.IsStampedField("PaidAt") || s.IsStampedField("Status") {
		t.Fatal("IsStampedField must tell the two kinds apart")
	}
	if !s.HasStampedFields() {
		t.Fatal("HasStampedFields must report the declaration")
	}
	// It resolves for criteria too — filtering and ordering are unaffected.
	if rf, ok := s.Resolve("PaidAt"); !ok || rf.Column != "paid_at" {
		t.Fatalf("a stamped field must stay filterable/orderable, got %+v %v", rf, ok)
	}
}

// *time.Time is the semantics, not a nullability preference: before the fact
// happens the column has no value, and a zero time.Time would report year 1 as
// the moment something was signed.
func TestStampedTimeField_RequiresAPointerTime(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a non-pointer time must be refused at declaration")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "*time.Time") {
			t.Fatalf("the panic must teach the declaration, got %v", r)
		}
	}()
	NewTableSchema[*stampDeclOrder]("orders").ID("id").StampedTimeField("SignedAt", "signed_at")
}

func TestStampedTimeField_RefusedOnASibling(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a sibling carries no managed columns — the declaration must be refused")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "sibling") {
			t.Fatalf("the panic must name the refusal, got %v", r)
		}
	}()
	NewSiblingSchema[*stampDeclOrder]("order_terms").StampedTimeField("PaidAt", "paid_at")
}

// A type-less schema has no entity to carry the request. Both kinds are refused,
// each with its own reason.
func TestStampedTimeField_RefusedOnTypelessSchemas(t *testing.T) {
	for _, c := range []struct {
		name   string
		build  func() *TableSchema
		expect string
	}{
		{"external", func() *TableSchema { return NewExternalSchema("upstream_orders") }, "never writes"},
		{"shared base", func() *TableSchema { return NewSharedBaseSchema("people") }, "ROLE schema"},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("a type-less schema must refuse a stamped field")
				}
				if msg, _ := r.(string); !strings.Contains(msg, c.expect) {
					t.Fatalf("the panic must explain THIS schema's reason (%q), got %v", c.expect, r)
				}
			}()
			c.build().StampedTimeField("PaidAt", "paid_at")
		})
	}
}

// An aggregate child is a VALUE in the aggregate map: a rule calling
// item.Stamp(...) would mutate a copy and lose the request. Refuse at the attach
// point rather than let it fail silently at runtime.
func TestStampedTimeField_RefusedOnAnAggregateChild(t *testing.T) {
	child := NewTableSchema[*stampDeclOrder]("order_lines").
		ID("id").
		ParentID("order_id").
		StampedTimeField("PaidAt", "paid_at")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a child's stamp request would be lost on a value copy — attaching it must be refused")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "silently lost") {
			t.Fatalf("the panic must explain why, got %v", r)
		}
	}()
	NewTableSchema[*stampDeclOrder]("orders").ID("id").Child(child)
}

func TestStampColumns_TranslatesAndRefuses(t *testing.T) {
	s := stampDeclSchema()

	cols, err := s.StampColumns([]string{"PaidAt"})
	if err != nil || len(cols) != 1 || cols[0] != "paid_at" {
		t.Fatalf("StampColumns = %v, %v", cols, err)
	}
	if cols, err := s.StampColumns(nil); cols != nil || err != nil {
		t.Fatalf("no request means no column: %v, %v", cols, err)
	}
	if _, err := s.StampColumns([]string{"Nope"}); err == nil ||
		!strings.Contains(err.Error(), "no stamped field") {
		t.Fatalf("an unknown field must be refused, got %v", err)
	}
	if _, err := s.StampColumns([]string{"Status"}); err == nil ||
		!strings.Contains(err.Error(), "plain field") {
		t.Fatalf("a plain field must be refused with its own diagnostic, got %v", err)
	}
}

// A target that carries no domain.Managed simply has no requests — the seam is
// structural, so infra never has to know what it was handed.
func TestRequestedStamps_NonCarrierHasNoRequests(t *testing.T) {
	if got := domain.RequestedStamps(&stampDeclOrder{}); got != nil {
		t.Fatalf("a type embedding no carrier requests nothing, got %v", got)
	}
}
