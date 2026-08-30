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

// An external schema is the one that never writes, so it is the one that cannot
// stamp.
func TestStampedTimeField_RefusedOnAnExternalSchema(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("an external schema never writes — the declaration must be refused")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "never writes") {
			t.Fatalf("the panic must name the reason, got %v", r)
		}
	}()
	NewExternalSchema("upstream_orders").StampedTimeField("PaidAt", "paid_at")
}

// A SHARED BASE may declare a stamped field: it is type-less, but its columns map
// to the ROLE's Go fields, so the role's entity is what carries the request. The
// consequence is that the TYPE check cannot run at declaration — it is deferred
// to .SharedBase(role), where a struct finally exists.
func TestStampedTimeField_SharedBaseDefersTheTypeCheck(t *testing.T) {
	base := func() *TableSchema {
		return NewSharedBaseSchema("people").
			ID("id").
			Revision("revision").
			NaturalID("document").
			Field("Document", "document").
			StampedTimeField("VerifiedAt", "verified_at")
	}
	// Declaring it is fine on its own — there is nothing to check against yet.
	b := base()
	if !b.IsStampedField("VerifiedAt") {
		t.Fatal("a shared base must accept a stamped declaration")
	}

	// A role whose field is the right type resolves cleanly.
	type goodRole struct {
		ID         domain.ID
		Document   string
		VerifiedAt *time.Time
	}
	NewTableSchema[*goodRole]("employees").ID("id").SharedBase(base(), "person_id")

	// A role whose field is the WRONG type fails at the point the type appears.
	type badRole struct {
		ID         domain.ID
		Document   string
		VerifiedAt time.Time // not a pointer
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("the deferred type check must fire when the role anchors the type")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "*time.Time") {
			t.Fatalf("the panic must teach the declaration, got %v", r)
		}
	}()
	NewTableSchema[*badRole]("contractors").ID("id").SharedBase(base(), "person_id")
}

// Two declarations of one base must agree on which columns are stamped —
// otherwise the behavior would depend on which instance the write path held.
func TestSharedBaseEquivalence_ComparesTheStampedFlag(t *testing.T) {
	stamped := NewSharedBaseSchema("people").
		ID("id").Revision("revision").NaturalID("document").
		Field("Document", "document").
		StampedTimeField("VerifiedAt", "verified_at")
	plain := NewSharedBaseSchema("people").
		ID("id").Revision("revision").NaturalID("document").
		Field("Document", "document").
		Field("VerifiedAt", "verified_at")

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("a base declared stamped in one place and plain in another must be refused")
		}
	}()
	AssertSharedBaseEquivalent(stamped, plain)
}

// An aggregate child carries stamp requests like any other node: it embeds
// domain.Managed, and the value the domain hands to the aggregate map travels
// with whatever its rule asked for. Attaching one is not refused.
type stampDeclLine struct {
	domain.Managed
	Label     string
	ShippedAt *time.Time
}

func (l stampDeclLine) IsSameBusinessIdentity(other domain.AggregateValueObject) bool {
	return domain.IsSameByBusinessFields(l, other)
}
func (stampDeclLine) CollectionName() string                           { return "Lines" }
func (stampDeclLine) BuildRules(string, domain.Service, *domain.Rules) {}

func TestStampedTimeField_AllowedOnAnAggregateChild(t *testing.T) {
	child := NewTableSchema[stampDeclLine]("order_lines").
		ID("id").
		ParentID("order_id").
		Field("Label", "label").
		StampedTimeField("ShippedAt", "shipped_at")

	root := NewTableSchema[*stampDeclOrder]("orders").ID("id").Child(child)
	got := root.ChildSchema("stampDeclLine")
	if got == nil || !got.IsStampedField("ShippedAt") {
		t.Fatal("the child's stamped declaration must survive attachment")
	}
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

// The deferred check must know WHICH kind it is deferring: the two members of
// the family fix different Go types, and validating a counter as a timestamp
// would refuse a correct declaration (the bug this pins).
func TestStampedCounterField_SharedBaseDefersItsOwnTypeCheck(t *testing.T) {
	base := func() *TableSchema {
		return NewSharedBaseSchema("people").
			ID("id").
			Revision("revision").
			NaturalID("document").
			Field("Document", "document").
			StampedTimeField("VerifiedAt", "verified_at").
			StampedCounterField("SeenCount", "seen_count")
	}
	// Both kinds on one base resolve cleanly against a role that types them right.
	type goodRole struct {
		ID         domain.ID
		Document   string
		VerifiedAt *time.Time
		SeenCount  int64
	}
	NewTableSchema[*goodRole]("members").ID("id").SharedBase(base(), "person_id")

	// And the counter's own check still fires when the role types it wrong.
	type badRole struct {
		ID         domain.ID
		Document   string
		VerifiedAt *time.Time
		SeenCount  *int64 // a counter is never a pointer
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a mistyped counter on a base must be refused when the role anchors it")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "int64") {
			t.Fatalf("the panic must be the COUNTER's, got %v", r)
		}
	}()
	NewTableSchema[*badRole]("contractors").ID("id").SharedBase(base(), "person_id")
}

// ApplyStamps is the write-back, and every guard in it exists because the
// framework must never fail a good write over a reporting detail: the statement
// is already correct, so an unreachable field is skipped rather than reported.
func TestApplyStamps_GuardsSkipInsteadOfFailing(t *testing.T) {
	s := stampDeclSchema()
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	// The happy path: a settable pointer field takes the instant.
	o := &stampDeclOrder{}
	s.ApplyStamps(o, []string{"PaidAt"}, now)
	if o.PaidAt == nil || !o.PaidAt.Equal(now) {
		t.Fatalf("the stamped field must carry the instant, got %v", o.PaidAt)
	}

	// No request → nothing happens, and no reflection is attempted.
	p := &stampDeclOrder{}
	s.ApplyStamps(p, nil, now)
	if p.PaidAt != nil {
		t.Fatal("an empty request must write nothing")
	}

	// A non-pointer target is not addressable, so there is nothing to set. The
	// statement was still built correctly, so this is a skip, not an error.
	s.ApplyStamps(stampDeclOrder{}, []string{"PaidAt"}, now) // must not panic
	s.ApplyStamps((*stampDeclOrder)(nil), []string{"PaidAt"}, now)
	s.ApplyStamps(nil, []string{"PaidAt"}, now)

	// A name the schema does not declare stamped is skipped here too — the
	// REFUSAL belongs to StampColumns, which runs before this.
	q := &stampDeclOrder{Status: "NEW"}
	s.ApplyStamps(q, []string{"Status", "Nope"}, now)
	if q.Status != "NEW" {
		t.Fatalf("a plain field must not be written by the write-back, got %q", q.Status)
	}
}

// A counter is never written back: its new value is the server's and the
// framework does not read it back, so there is nothing honest to put on the
// entity.
func TestApplyStamps_LeavesCountersAlone(t *testing.T) {
	type counted struct {
		ID         domain.ID
		TotalCount int64
	}
	s := NewTableSchema[*counted]("hits").ID("id").StampedCounterField("TotalCount", "total_count")
	c := &counted{TotalCount: 7}
	s.ApplyStamps(c, []string{"TotalCount"}, time.Now())
	if c.TotalCount != 7 {
		t.Fatalf("a counter must not be written back, got %d", c.TotalCount)
	}
}

// A stamped counter is refused on a sibling for the same reason a stamped time
// is: the row is a slice of the owner's and carries no framework-owned column.
func TestStampedCounterField_RefusedOnSiblingAndExternal(t *testing.T) {
	for _, c := range []struct {
		name   string
		build  func() *TableSchema
		expect string
	}{
		{"sibling", func() *TableSchema { return NewSiblingSchema[*stampDeclOrder]("order_terms") }, "sibling"},
		{"external", func() *TableSchema { return NewExternalSchema("upstream") }, "never writes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("the declaration must be refused")
				}
				if msg, _ := r.(string); !strings.Contains(msg, c.expect) {
					t.Fatalf("the panic must name the reason (%q), got %v", c.expect, r)
				}
			}()
			c.build().StampedCounterField("TotalCount", "total_count")
		})
	}
}
