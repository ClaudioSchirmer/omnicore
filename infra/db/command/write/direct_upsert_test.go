package write

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Upsert is the one Direct write keyed on something other than the identity, and
// the one that cannot be archive-gated. These tests pin the rendered statement,
// the four kinds of column an upsert has, and the refusals that exist because a
// silent wrong answer here is invisible in production.

type attemptRow struct {
	ID              domain.ID
	Identity        string
	IdentityKind    string
	Outcome         string
	TotalCount      int64
	CurrentCount    int64
	WindowStartedAt time.Time
	LastAt          *time.Time
	LastIP          string
	IdentityExisted bool
	TotalBlocked    int64
}

func attemptSchema() *TableSchema {
	return core.NewDirectSchema[attemptRow]("authentication_attempts").
		ID("id").
		Field("Identity", "identity").
		Field("IdentityKind", "identity_kind").
		Field("Outcome", "outcome").
		Field("WindowStartedAt", "window_started_at").
		Field("LastIP", "last_ip").
		Field("IdentityExisted", "identity_existed").
		StampedCounterField("TotalCount", "total_count").
		StampedCounterField("CurrentCount", "current_count").
		StampedTimeField("LastAt", "last_at")
}

func attemptKey() UpsertOption {
	return OnConflict("Identity", "IdentityKind", "Outcome")
}

func newUpsertWriter(t *testing.T) (*DirectWriter, *fakeWriteTx) {
	t.Helper()
	tx := &fakeWriteTx{n: 1}
	return NewDirectWriter(&directTestEngine{tx: tx}, attemptSchema(), "AuthAttempt"), tx
}

// The whole statement, once: the four column kinds and where each one lands.
func TestUpsert_RendersTheFourColumnKinds(t *testing.T) {
	w, tx := newUpsertWriter(t)
	err := w.Upsert(context.Background(), Values{
		"Identity":        "bob",
		"IdentityKind":    "USERNAME",
		"Outcome":         "FAILURE",
		"TotalCount":      Stamp,
		"CurrentCount":    Stamp,
		"WindowStartedAt": OnInsert(testStamp()),
		"LastAt":          Stamp,
		"LastIP":          "10.0.0.1",
		"IdentityExisted": true,
	}, attemptKey())
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	sql := tx.lastSQL

	// Every column is written on the INSERT — including the ones that never
	// change afterwards.
	for _, col := range []string{"id", "identity", "identity_kind", "outcome", "total_count",
		"current_count", "window_started_at", "last_at", "last_ip", "identity_existed"} {
		if !strings.Contains(sql, col) {
			t.Fatalf("column %q missing from the INSERT: %s", col, sql)
		}
	}
	head, tail, ok := strings.Cut(sql, "ON CONFLICT")
	if !ok {
		t.Fatalf("expected an ON CONFLICT clause: %s", sql)
	}
	_ = head

	// Counters increment server-side; plain values take the proposed row.
	for _, want := range []string{
		"total_count = authentication_attempts.total_count + 1",
		"current_count = authentication_attempts.current_count + 1",
		"last_at = EXCLUDED.last_at",
		"last_ip = EXCLUDED.last_ip",
		"identity_existed = EXCLUDED.identity_existed",
	} {
		if !strings.Contains(tail, want) {
			t.Fatalf("conflict clause missing %q:\n%s", want, tail)
		}
	}
	// Insert-only and the key itself are absent from the conflict clause: the
	// window is not restarted, and a column the row MATCHED on is never assigned.
	for _, absent := range []string{"window_started_at =", "identity =", "identity_kind =", "outcome ="} {
		if strings.Contains(tail, absent) {
			t.Fatalf("conflict clause must not assign %q:\n%s", absent, tail)
		}
	}
}

// A second shape of the same table — a different column set and a different
// counter — is just a different Values map.
func TestUpsert_BlockedShapeIsAnotherValuesMap(t *testing.T) {
	schema := attemptSchema().StampedCounterField("TotalBlocked", "total_blocked")
	tx := &fakeWriteTx{n: 1}
	w := NewDirectWriter(&directTestEngine{tx: tx}, schema, "AuthAttempt")

	if err := w.Upsert(context.Background(), Values{
		"Identity": "bob", "IdentityKind": "USERNAME", "Outcome": "BLOCKED",
		"TotalBlocked": Stamp,
		"LastAt":       Stamp,
		"LastIP":       OnInsert("10.0.0.1"),
	}, attemptKey()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	tail := tx.lastSQL[strings.Index(tx.lastSQL, "ON CONFLICT"):]
	if !strings.Contains(tail, "total_blocked = authentication_attempts.total_blocked + 1") {
		t.Fatalf("the blocked counter must bump: %s", tail)
	}
	if strings.Contains(tail, "last_ip =") {
		t.Fatalf("an OnInsert column must not be revised on conflict: %s", tail)
	}
	if strings.Contains(tx.lastSQL, "total_count") {
		t.Fatalf("a column absent from Values must not be in the statement: %s", tx.lastSQL)
	}
}

// The conflict key is the statement's premise; the framework will not guess it.
func TestUpsert_RequiresAConflictKey(t *testing.T) {
	w, _ := newUpsertWriter(t)
	err := w.Upsert(context.Background(), Values{"Identity": "bob"})
	if err == nil || !strings.Contains(err.Error(), "OnConflict") {
		t.Fatalf("an Upsert with no key must be refused, got %v", err)
	}
}

func TestUpsert_KeyMustBePartOfTheRow(t *testing.T) {
	w, _ := newUpsertWriter(t)
	err := w.Upsert(context.Background(), Values{"LastIP": "10.0.0.1"}, attemptKey())
	if err == nil || !strings.Contains(err.Error(), "carries no value") {
		t.Fatalf("a key field absent from Values must be refused, got %v", err)
	}
}

func TestUpsert_UnknownConflictFieldIsRefused(t *testing.T) {
	w, _ := newUpsertWriter(t)
	err := w.Upsert(context.Background(), Values{"Identity": "bob"}, OnConflict("Identitty"))
	if err == nil || !strings.Contains(err.Error(), "unknown conflict field") {
		t.Fatalf("a misspelled conflict field must be refused, got %v", err)
	}
}

// A stamped column may be asked for, never dictated.
func TestUpsert_ValueInAStampedSlotIsRefused(t *testing.T) {
	w, _ := newUpsertWriter(t)
	err := w.Upsert(context.Background(), Values{
		"Identity": "bob", "IdentityKind": "USERNAME", "Outcome": "FAILURE",
		"LastAt": time.Now(),
	}, attemptKey())
	if err == nil || !strings.Contains(err.Error(), "write.Stamp") {
		t.Fatalf("binding a value to a stamped field must be refused, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The archive policy — the reason an upsert cannot be gated
// ---------------------------------------------------------------------------

func archivableSchema() *TableSchema {
	return core.NewDirectSchema[attemptRow]("authentication_attempts").
		ID("id").
		Field("Identity", "identity").
		Field("LastIP", "last_ip").
		StampedCounterField("TotalCount", "total_count").
		DeletedAt("deleted_at")
}

func newArchivableWriter(t *testing.T) (*DirectWriter, *fakeWriteTx) {
	t.Helper()
	tx := &fakeWriteTx{n: 1}
	return NewDirectWriter(&directTestEngine{tx: tx}, archivableSchema(), "AuthAttempt"), tx
}

// Every other verb gates on deleted_at IS NULL. An upsert cannot, so the caller
// must say what happens — there is no defensible default.
func TestUpsert_ArchivePolicyIsMandatoryWhenDeletedAtIsDeclared(t *testing.T) {
	w, _ := newArchivableWriter(t)
	err := w.Upsert(context.Background(), Values{"Identity": "bob", "TotalCount": Stamp},
		OnConflict("Identity"))
	if err == nil {
		t.Fatal("a schema with DeletedAt must not upsert without an archive policy")
	}
	for _, want := range []string{"UnarchiveOnConflict", "KeepArchiveStateOnConflict", "deleted_at IS NULL"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the diagnostic must mention %q, got: %v", want, err)
		}
	}
}

func TestUpsert_UnarchiveOnConflictClearsTheColumn(t *testing.T) {
	w, tx := newArchivableWriter(t)
	if err := w.Upsert(context.Background(), Values{"Identity": "bob", "TotalCount": Stamp},
		OnConflict("Identity"), UnarchiveOnConflict()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !strings.Contains(tx.lastSQL, "deleted_at = NULL") {
		t.Fatalf("UnarchiveOnConflict must clear the archive column: %s", tx.lastSQL)
	}
}

func TestUpsert_KeepArchiveStateLeavesTheColumnAlone(t *testing.T) {
	w, tx := newArchivableWriter(t)
	if err := w.Upsert(context.Background(), Values{"Identity": "bob", "TotalCount": Stamp},
		OnConflict("Identity"), KeepArchiveStateOnConflict()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if strings.Contains(tx.lastSQL, "deleted_at") {
		t.Fatalf("KeepArchiveStateOnConflict must not touch the archive column: %s", tx.lastSQL)
	}
	// The rest of the upsert is unchanged by the policy.
	if !strings.Contains(tx.lastSQL, "total_count = authentication_attempts.total_count + 1") {
		t.Fatalf("the counter must still bump: %s", tx.lastSQL)
	}
}

// A schema with no archive column needs no policy at all.
func TestUpsert_NoDeletedAtNeedsNoPolicy(t *testing.T) {
	w, _ := newUpsertWriter(t)
	if err := w.Upsert(context.Background(), Values{
		"Identity": "bob", "IdentityKind": "USERNAME", "Outcome": "FAILURE", "LastIP": "10.0.0.1",
	}, attemptKey()); err != nil {
		t.Fatalf("a schema without DeletedAt must upsert with no policy: %v", err)
	}
}

// A fresh row starts every counter at 1 — bound as an ordinary argument, since
// there is no existing value to add to yet.
func TestUpsert_InsertSideStartsCountersAtOne(t *testing.T) {
	w, _ := newUpsertWriter(t)
	plan, err := w.upsertPlan(Values{
		"Identity": "bob", "IdentityKind": "USERNAME", "Outcome": "FAILURE", "TotalCount": Stamp,
	}, upsertConfig{conflictGo: []string{"Identity", "IdentityKind", "Outcome"}})
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	_, args, err := w.renderUpsert(testPGDialect{}, plan, "018f0000-0000-7000-8000-000000000001", testStamp())
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	var ones int
	for _, a := range args {
		if n, ok := a.(int64); ok && n == 1 {
			ones++
		}
	}
	if ones != 1 {
		t.Fatalf("the counter must bind 1 on the insert side, args = %v", args)
	}
}
