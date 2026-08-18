package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
)

// SQL contract tests pin the wire shape so a refactor cannot silently
// drop the audit_events reference or drop a column the row needs.

// The statement must begin AT "SELECT " with no leading whitespace: some
// dialects cap a read by rewriting the statement HEAD (SQL Server turns
// `SELECT …` into `SELECT TOP n …`) and can only find it at position zero.
// A leading newline here compiles, passes every fake-backed test, and then
// panics against a real SQL Server — so the contract is pinned in the unit
// suite, where it costs nothing to catch.
func TestSelectAuditEventCols_StartsAtSelectForHeadRewritingDialects(t *testing.T) {
	if !strings.HasPrefix(selectAuditEventCols, "SELECT ") {
		t.Errorf("selectAuditEventCols must start at %q (Dialect.ApplyLimit head rewrite), got %q",
			"SELECT ", selectAuditEventCols[:min(20, len(selectAuditEventCols))])
	}
}

func TestSelectAuditEventCols_ReferencesAuditEventsAndColumns(t *testing.T) {
	if !strings.Contains(selectAuditEventCols, "audit_events") {
		t.Error("selectAuditEventCols missing reference to audit_events table")
	}
	for _, col := range []string{
		"id", "entity_type", "aggregate_id", "verb", "action_name", "kind",
		"actor", "actor_issuer", "tenant_id", "thread_id", "trace_id",
		"occurred_at", "payload",
	} {
		if !strings.Contains(selectAuditEventCols, col) {
			t.Errorf("selectAuditEventCols missing column %q", col)
		}
	}
}

// ─── scanAuditRow ────────────────────────────────────────────────────────────

func TestScanAuditRow_PopulatesEveryFieldOnHappyPath(t *testing.T) {
	id := uuid.New()
	aggID := uuid.New()
	threadID := uuid.New()
	actor := "user-42"
	issuer := "https://idp.example"
	tenant := "acme"
	trace := "4bf92f3577b34da6a3ce929d0e0e4736"
	occurred := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"snapshot":{"name":"alice"},"changes":[{"field":"name","from":"a","to":"b"}]}`)

	got, err := scanAuditRow(func(dest ...any) error {
		assign(t, dest[0], id)
		assign(t, dest[1], "User")
		assign(t, dest[2], aggID)
		assign(t, dest[3], "update")
		assign(t, dest[4], "GetUpdatable")
		assign(t, dest[5], "delta")
		assign(t, dest[6], &actor)
		assign(t, dest[7], &issuer)
		assign(t, dest[8], &tenant)
		assign(t, dest[9], threadID)
		assign(t, dest[10], &trace)
		assign(t, dest[11], occurred)
		assign(t, dest[12], payload)
		return nil
	})
	if err != nil {
		t.Fatalf("scanAuditRow: %v", err)
	}
	if got.EntityType != "User" || got.Verb != "update" || got.Kind != "delta" {
		t.Errorf("top-level fields wrong: %+v", got)
	}
	if got.EntityID != aggID.String() {
		t.Errorf("EntityID = %q, want %q", got.EntityID, aggID.String())
	}
	if got.ThreadID != threadID.String() {
		t.Errorf("ThreadID = %q, want %q", got.ThreadID, threadID.String())
	}
	if got.Actor != "user-42" || got.ActorIssuer != "https://idp.example" || got.TenantID != "acme" {
		t.Errorf("actor/issuer/tenant wrong: %+v", got)
	}
	// The pivot to the producing request's trace: written by the persister and
	// read back here, so an audit row can be jumped to its trace.
	if got.TraceID != trace {
		t.Errorf("TraceID = %q, want %q", got.TraceID, trace)
	}
	if got.Snapshot == nil || got.Snapshot["name"] != "alice" {
		t.Errorf("Snapshot lost on payload unmarshal: %v", got.Snapshot)
	}
	if len(got.Changes) != 1 || got.Changes[0].Field != "name" {
		t.Errorf("Changes lost on payload unmarshal: %v", got.Changes)
	}
}

func TestScanAuditRow_NullableActorIssuerTenantBecomeEmptyStrings(t *testing.T) {
	id := uuid.New()
	aggID := uuid.New()
	threadID := uuid.New()
	occurred := time.Now()

	got, err := scanAuditRow(func(dest ...any) error {
		assign(t, dest[0], id)
		assign(t, dest[1], "User")
		assign(t, dest[2], aggID)
		assign(t, dest[3], "insert")
		assign(t, dest[4], "GetInsertable")
		assign(t, dest[5], "snapshot")
		// NULL columns: actor / actor_issuer / tenant_id / trace_id (pgx scans
		// into **string=nil). trace_id is NULL whenever tracing is off.
		var nilStr *string
		assign(t, dest[6], nilStr)
		assign(t, dest[7], nilStr)
		assign(t, dest[8], nilStr)
		assign(t, dest[9], threadID)
		assign(t, dest[10], nilStr)
		assign(t, dest[11], occurred)
		assign(t, dest[12], []byte(`{}`))
		return nil
	})
	if err != nil {
		t.Fatalf("scanAuditRow: %v", err)
	}
	if got.Actor != "" || got.ActorIssuer != "" || got.TenantID != "" || got.TraceID != "" {
		t.Errorf("NULL columns must become empty strings, got: actor=%q issuer=%q tenant=%q trace=%q",
			got.Actor, got.ActorIssuer, got.TenantID, got.TraceID)
	}
}

func TestScanAuditRow_EmptyPayloadKeepsBlocksNil(t *testing.T) {
	id := uuid.New()
	aggID := uuid.New()
	threadID := uuid.New()

	got, err := scanAuditRow(func(dest ...any) error {
		assign(t, dest[0], id)
		assign(t, dest[1], "User")
		assign(t, dest[2], aggID)
		assign(t, dest[3], "archive")
		assign(t, dest[4], "GetArchivable")
		assign(t, dest[5], "transition")
		var nilStr *string
		assign(t, dest[6], nilStr)
		assign(t, dest[7], nilStr)
		assign(t, dest[8], nilStr)
		assign(t, dest[9], threadID)
		assign(t, dest[10], nilStr)
		assign(t, dest[11], time.Now())
		// kind=transition payload is `{}` — buildAuditPayload elides every block.
		assign(t, dest[12], []byte(`{}`))
		return nil
	})
	if err != nil {
		t.Fatalf("scanAuditRow: %v", err)
	}
	if got.Snapshot != nil {
		t.Errorf("transition event must keep Snapshot nil, got: %v", got.Snapshot)
	}
	if got.Changes != nil {
		t.Errorf("transition event must keep Changes nil, got: %v", got.Changes)
	}
	if got.Children != nil {
		t.Errorf("transition event must keep Children nil, got: %v", got.Children)
	}
}

func TestScanAuditRow_PropagatesPayloadUnmarshalError(t *testing.T) {
	id := uuid.New()
	_, err := scanAuditRow(func(dest ...any) error {
		assign(t, dest[0], id)
		assign(t, dest[1], "User")
		assign(t, dest[2], uuid.New())
		assign(t, dest[3], "update")
		assign(t, dest[4], "GetUpdatable")
		assign(t, dest[5], "delta")
		var nilStr *string
		assign(t, dest[6], nilStr)
		assign(t, dest[7], nilStr)
		assign(t, dest[8], nilStr)
		assign(t, dest[9], uuid.New())
		assign(t, dest[10], nilStr)
		assign(t, dest[11], time.Now())
		assign(t, dest[12], []byte(`{not-json`))
		return nil
	})
	if err == nil {
		t.Fatal("expected unmarshal error on malformed payload")
	}
}

func TestScanAuditRow_PropagatesScannerError(t *testing.T) {
	want := errors.New("driver explosion")
	got, err := scanAuditRow(func(dest ...any) error { return want })
	if got != nil {
		t.Errorf("event must be nil on scan failure, got: %+v", got)
	}
	if !errors.Is(err, want) {
		t.Errorf("scan error not propagated: %v", err)
	}
}

// ─── FindByID / FindByAggregate boundary checks (nil querier, no DB) ────────

// A reader built with no query surface fails fast on either read rather than
// dereferencing nil — the guard the engine-built reader can never hit but a
// hand-rolled NewReader(nil, …) can.

func TestFindByID_RejectsNilQuerier(t *testing.T) {
	_, err := testReader(nil).FindByID(context.Background(), uuid.New())
	if err == nil || !strings.Contains(err.Error(), "nil querier") {
		t.Errorf("expected nil-querier error, got: %v", err)
	}
}

func TestFindByAggregate_RejectsNilQuerier(t *testing.T) {
	_, err := testReader(nil).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err == nil || !strings.Contains(err.Error(), "nil querier") {
		t.Errorf("expected nil-querier error, got: %v", err)
	}
}

func TestFindByAggregate_RejectsEmptyArguments(t *testing.T) {
	cases := []struct {
		name, et, aid string
	}{
		{"empty entityType", "", uuid.NewString()},
		{"empty aggregateID", "User", ""},
		{"both empty", "", ""},
	}
	for _, c := range cases {
		// A non-nil querier so the empty-arg guard (not the nil guard) is what
		// rejects; the SQL must never reach Query.
		_, err := testReader(&fakeQueryer{}).FindByAggregate(context.Background(), c.et, c.aid, 20)
		if err == nil || !strings.Contains(err.Error(), "non-empty") {
			t.Errorf("[%s] expected validation error, got: %v", c.name, err)
		}
	}
}

// The cap is the read's contract with the database, so a caller that supplies
// none is refused rather than served an unbounded timeline.
func TestFindByAggregate_RejectsNonPositiveLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		q := &fakeQueryer{}
		_, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), limit)
		if err == nil || !strings.Contains(err.Error(), "positive limit") {
			t.Errorf("limit=%d: expected a positive-limit rejection, got: %v", limit, err)
		}
		if q.lastSQL != "" {
			t.Errorf("limit=%d: the statement must never reach the querier, got %q", limit, q.lastSQL)
		}
	}
}

func TestErrAuditNotFound_HasStableMessage(t *testing.T) {
	// errors.Is callers and external tools both branch on the sentinel; pin
	// the user-visible message so a refactor cannot silently change the
	// observable contract.
	if appaudit.ErrAuditNotFound == nil || appaudit.ErrAuditNotFound.Error() != "audit: event not found" {
		t.Errorf("appaudit.ErrAuditNotFound = %v", appaudit.ErrAuditNotFound)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// assign writes src into the location *dest. dest is always a pointer
// matching the pgx scan target shape; src can be the value itself OR a
// `*string` for nullable columns where the test wants to inject NULL.
//
// pgx's real Scan sets the pointer's destination directly; the closure
// mirrors that semantic without a reflective dance.
func assign(t *testing.T, dest any, src any) {
	t.Helper()
	switch d := dest.(type) {
	case *uuid.UUID:
		*d = src.(uuid.UUID)
	case *string:
		*d = src.(string)
	case **string:
		*d = src.(*string)
	case *time.Time:
		*d = src.(time.Time)
	case *[]byte:
		*d = src.([]byte)
	default:
		t.Fatalf("assign: unsupported dest type %T", dest)
	}
}
