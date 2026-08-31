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
	// The two scoping wrappers need a column on each half: one dated when the
	// row is created, one dated only when something collided with it.
	WindowOpenedAt *time.Time
	RepeatedAt     *time.Time
	RepeatCount    int64
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

// The remaining refusals and shapes upsertPlan owes, each raised before a
// transaction exists.
func TestUpsertPlan_RemainingRefusals(t *testing.T) {
	w, _ := newUpsertWriter(t)
	key := upsertConfig{conflictGo: []string{"Identity", "IdentityKind", "Outcome"}}

	if _, err := w.upsertPlan(Values{}, key); err == nil ||
		!strings.Contains(err.Error(), "at least one value") {
		t.Fatalf("an empty Values must be refused, got %v", err)
	}
	if _, err := w.upsertPlan(Values{
		"Identity": "b", "IdentityKind": "U", "Outcome": "F", "ID": "x",
	}, key); err == nil || !strings.Contains(err.Error(), "minted") {
		t.Fatalf("an explicit id must be refused, got %v", err)
	}
	if _, err := w.upsertPlan(Values{
		"Identity": "b", "IdentityKind": "U", "Outcome": "F", "Nope": 1,
	}, key); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an unknown field must be refused, got %v", err)
	}
}

// An upsert on a schema with NO archive column needs no policy, and none of the
// archive machinery appears in its statement.
func TestUpsert_NoArchiveColumnEmitsNoArchiveClause(t *testing.T) {
	w, tx := newUpsertWriter(t)
	if err := w.Upsert(context.Background(), Values{
		"Identity": "bob", "IdentityKind": "U", "Outcome": "F", "LastIP": "1.1.1.1",
	}, attemptKey()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if strings.Contains(tx.lastSQL, "deleted_at") {
		t.Fatalf("a schema with no DeletedAt must not mention it: %s", tx.lastSQL)
	}
}

// The managed timestamps ride the upsert like they ride every other verb:
// created_at on the insert side only, updated_at on both.
func TestUpsert_ManagedTimestamps(t *testing.T) {
	// A dedicated schema: the fixture above deliberately declares no managed
	// timestamps, which is what lets its assertions pin the exact column list.
	schema := core.NewDirectSchema[attemptRow]("authentication_attempts").
		ID("id").
		Field("Identity", "identity").
		Field("IdentityKind", "identity_kind").
		Field("Outcome", "outcome").
		Field("LastIP", "last_ip").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	tx := &fakeWriteTx{n: 1}
	w := NewDirectWriter(&directTestEngine{tx: tx}, schema, "AuthAttempt")

	if err := w.Upsert(context.Background(), Values{
		"Identity": "bob", "IdentityKind": "U", "Outcome": "F", "LastIP": "1.1.1.1",
	}, attemptKey()); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	head, tail, _ := strings.Cut(tx.lastSQL, "ON CONFLICT")
	if !strings.Contains(head, "created_at") || !strings.Contains(head, "updated_at") {
		t.Fatalf("both managed columns belong on the INSERT: %s", head)
	}
	if !strings.Contains(tail, "updated_at = EXCLUDED.updated_at") {
		t.Fatalf("updated_at must be revised on conflict: %s", tail)
	}
	if strings.Contains(tail, "created_at =") {
		t.Fatalf("a row's creation is never revised: %s", tail)
	}
}

// ---------------------------------------------------------------------------
// OnInsert / OnUpdate — the two halves of an upsert, and the stamp verbs scoped
// to one of them. The point of the pairing is that the VALUE stays the
// framework's while the caller says on which path it is written.
// ---------------------------------------------------------------------------

// scopedSchema declares a stamped column for each half plus a counter, so one
// fixture covers every bucket a scoped stamp plan can carry.
func scopedSchema() *TableSchema {
	return core.NewDirectSchema[attemptRow]("authentication_attempts").
		ID("id").
		Field("Identity", "identity").
		Field("LastIP", "last_ip").
		StampedTimeField("WindowOpenedAt", "window_opened_at").
		StampedTimeField("RepeatedAt", "repeated_at").
		StampedCounterField("RepeatCount", "repeat_count")
}

func newScopedWriter(t *testing.T) (*DirectWriter, *fakeWriteTx) {
	t.Helper()
	tx := &fakeWriteTx{n: 1}
	return NewDirectWriter(&directTestEngine{tx: tx}, scopedSchema(), "AuthAttempt"), tx
}

const testUpsertID = "018f0000-0000-7000-8000-000000000001"

// The ask this whole pairing exists for: a window opened with the framework's
// own instant, and never reopened. The caller cannot compute that instant, and a
// bare write.Stamp would refresh it on every conflict.
func TestUpsert_OnInsertStampDatesTheCreationOnly(t *testing.T) {
	w, _ := newScopedWriter(t)
	plan, err := w.upsertPlan(Values{
		"Identity":       "bob",
		"WindowOpenedAt": OnInsert(Stamp),
		"LastIP":         "10.0.0.1",
	}, upsertConfig{conflictGo: []string{"Identity"}})
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, ok := strings.Cut(sql, "ON CONFLICT")
	if !ok {
		t.Fatalf("expected an ON CONFLICT clause: %s", sql)
	}
	if !strings.Contains(head, "window_opened_at") {
		t.Fatalf("the created row must be dated: %s", head)
	}
	if strings.Contains(tail, "window_opened_at") {
		t.Fatalf("a window is opened once, never reopened: %s", tail)
	}
	// The instant bound is the operation's own — the whole reason the marker
	// exists rather than a time the caller computed.
	var stamped bool
	for _, a := range args {
		if a == testNow {
			stamped = true
		}
	}
	if !stamped {
		t.Fatalf("the operation instant must be bound on the insert half, args = %v", args)
	}
}

// The mirror: a column that describes the COLLISION is absent from the row being
// created, and binds an argument of its own on the conflict path — there is no
// proposed row to read it back from.
func TestUpsert_OnUpdateStampBindsItsOwnArgument(t *testing.T) {
	w, _ := newScopedWriter(t)
	plan, err := w.upsertPlan(Values{
		"Identity":   "bob",
		"LastIP":     "10.0.0.1",
		"RepeatedAt": OnUpdate(Stamp),
	}, upsertConfig{conflictGo: []string{"Identity"}})
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ := strings.Cut(sql, "ON CONFLICT")
	if strings.Contains(head, "repeated_at") {
		t.Fatalf("a collision column has no place in the row being created: %s", head)
	}
	// id, identity and last_ip are the inserted columns, so the conflict-only
	// value takes the placeholder right after them.
	if !strings.Contains(tail, "repeated_at = $4") {
		t.Fatalf("the conflict-only stamp must bind its own placeholder: %s", tail)
	}
	if len(args) != 4 || args[3] != testNow {
		t.Fatalf("the fourth argument must be the operation instant, args = %v", args)
	}
}

// An ordinary value scopes the same way a stamp does.
func TestUpsert_OnUpdateValueIsWrittenOnlyOnConflict(t *testing.T) {
	w, _ := newScopedWriter(t)
	plan, err := w.upsertPlan(Values{
		"Identity": "bob",
		"LastIP":   OnUpdate("10.0.0.1"),
	}, upsertConfig{conflictGo: []string{"Identity"}})
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ := strings.Cut(sql, "ON CONFLICT")
	if strings.Contains(head, "last_ip") {
		t.Fatalf("an OnUpdate column must not be in the proposed row: %s", head)
	}
	if !strings.Contains(tail, "last_ip = $3") {
		t.Fatalf("an OnUpdate column binds its own placeholder: %s", tail)
	}
	if len(args) != 3 || args[2] != "10.0.0.1" {
		t.Fatalf("the value must be bound last, args = %v", args)
	}
}

// A counter says itself differently on each half: 1 in the row being created,
// `col + 1` against the row that was already there. Scoping picks one of the two.
func TestUpsert_ScopedCounters(t *testing.T) {
	w, _ := newScopedWriter(t)
	key := upsertConfig{conflictGo: []string{"Identity"}}

	plan, err := w.upsertPlan(Values{
		"Identity": "bob", "LastIP": "1.1.1.1", "RepeatCount": OnInsert(Stamp),
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, _, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ := strings.Cut(sql, "ON CONFLICT")
	if !strings.Contains(head, "repeat_count") || strings.Contains(tail, "repeat_count") {
		t.Fatalf("an insert-only counter starts at 1 and never bumps: %s", sql)
	}

	plan, err = w.upsertPlan(Values{
		"Identity": "bob", "LastIP": "1.1.1.1", "RepeatCount": OnUpdate(Stamp),
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, _, err = w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ = strings.Cut(sql, "ON CONFLICT")
	if strings.Contains(head, "repeat_count") {
		t.Fatalf("a collision counter is not stated by the row being created: %s", head)
	}
	if !strings.Contains(tail, "repeat_count = authentication_attempts.repeat_count + 1") {
		t.Fatalf("a collision counter increments the existing row: %s", tail)
	}
}

// The clearing verbs scope too. On the insert half they are bound values (the
// proposed row pairs one column with one argument); on the conflict half NULL
// and 0 are literals and the zero instant binds an argument of its own.
func TestUpsert_ScopedClearingVerbs(t *testing.T) {
	w, _ := newScopedWriter(t)
	key := upsertConfig{conflictGo: []string{"Identity"}}

	plan, err := w.upsertPlan(Values{
		"Identity": "bob", "LastIP": "1.1.1.1", "RepeatedAt": OnInsert(StampNull),
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ := strings.Cut(sql, "ON CONFLICT")
	if !strings.Contains(head, "repeated_at") || strings.Contains(tail, "repeated_at") {
		t.Fatalf("an insert-only absence is stated once, on the row created: %s", sql)
	}
	if len(args) != 4 || args[3] != nil {
		t.Fatalf("the absence must be bound on the insert half, args = %v", args)
	}

	plan, err = w.upsertPlan(Values{
		"Identity":    "bob",
		"LastIP":      "1.1.1.1",
		"RepeatedAt":  OnUpdate(StampEmpty),
		"RepeatCount": OnUpdate(StampEmpty),
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err = w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ = strings.Cut(sql, "ON CONFLICT")
	if strings.Contains(head, "repeated_at") || strings.Contains(head, "repeat_count") {
		t.Fatalf("neither reset belongs to the row being created: %s", head)
	}
	if !strings.Contains(tail, "repeat_count = 0") {
		t.Fatalf("a counter reset is a literal: %s", tail)
	}
	if !strings.Contains(tail, "repeated_at = $4") {
		t.Fatalf("the zero instant binds its own placeholder: %s", tail)
	}
	if len(args) != 4 || !args[3].(time.Time).IsZero() {
		t.Fatalf("the zero instant must be the last argument, args = %v", args)
	}
	// NULL on the conflict half is a literal too — nothing is bound for it.
	plan, err = w.upsertPlan(Values{
		"Identity": "bob", "LastIP": "1.1.1.1", "RepeatedAt": OnUpdate(StampNull),
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err = w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	if _, tail, _ = strings.Cut(sql, "ON CONFLICT"); !strings.Contains(tail, "repeated_at = NULL") {
		t.Fatalf("a conflict-only absence is a literal: %s", tail)
	}
	if len(args) != 3 {
		t.Fatalf("a literal binds nothing of its own, args = %v", args)
	}
}

// Both halves at once, with the managed timestamps riding along: the placeholder
// numbering is the contract that has to hold, since the conflict clause is
// rendered after the values it follows.
func TestUpsert_BothHalvesKeepThePlaceholderNumbering(t *testing.T) {
	schema := scopedSchema().UpdatedAt("updated_at")
	tx := &fakeWriteTx{n: 1}
	w := NewDirectWriter(&directTestEngine{tx: tx}, schema, "AuthAttempt")

	plan, err := w.upsertPlan(Values{
		"Identity":       "bob",
		"LastIP":         "1.1.1.1",
		"WindowOpenedAt": OnInsert(Stamp),
		"RepeatedAt":     OnUpdate(Stamp),
		"RepeatCount":    Stamp,
	}, upsertConfig{conflictGo: []string{"Identity"}})
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ := strings.Cut(sql, "ON CONFLICT")
	// id, identity, last_ip, repeat_count, window_opened_at, updated_at.
	if !strings.Contains(head, "VALUES ($1, $2, $3, $4, $5, $6)") {
		t.Fatalf("the proposed row must bind six columns: %s", head)
	}
	if !strings.Contains(tail, "repeated_at = $7") {
		t.Fatalf("the conflict-only stamp continues the numbering: %s", tail)
	}
	if len(args) != 7 || args[6] != testNow {
		t.Fatalf("the seventh argument is the conflict-only instant, args = %v", args)
	}
	if !strings.Contains(tail, "repeat_count = authentication_attempts.repeat_count + 1") ||
		!strings.Contains(tail, "updated_at = EXCLUDED.updated_at") {
		t.Fatalf("the unscoped columns keep behaving exactly as before: %s", tail)
	}
}

// The refusals the two wrappers own. Each is raised before a transaction exists.
func TestUpsert_ScopedRefusals(t *testing.T) {
	w, _ := newScopedWriter(t)
	key := upsertConfig{conflictGo: []string{"Identity"}}

	if _, err := w.upsertPlan(Values{
		"Identity": "bob", "WindowOpenedAt": OnInsert(OnUpdate(Stamp)),
	}, key); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("nested wrappers must be refused, got %v", err)
	}
	if _, err := w.upsertPlan(Values{
		"Identity": OnInsert("bob"), "LastIP": "1.1.1.1",
	}, key); err == nil || !strings.Contains(err.Error(), "matched on") {
		t.Fatalf("a wrapped conflict key must be refused, got %v", err)
	}
	if _, err := w.upsertPlan(Values{
		"Identity": "bob", "LastIP": OnUpdate(Stamp),
	}, key); err == nil || !strings.Contains(err.Error(), "plain field") {
		t.Fatalf("stamping a plain field must be refused, got %v", err)
	}
	if _, err := w.upsertPlan(Values{
		"Identity": "bob", "RepeatedAt": OnInsert(testStamp()),
	}, key); err == nil || !strings.Contains(err.Error(), "write.Stamp") {
		t.Fatalf("dictating a stamped column must be refused, got %v", err)
	}
}

// The two halves are an upsert's, and no other verb has two of them.
func TestScopedValueIsRefusedOutsideUpsert(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    Values
		want string
	}{
		{"OnInsert", Values{"LastIP": OnInsert("1.1.1.1")}, "write.OnInsert"},
		{"OnUpdate", Values{"LastIP": OnUpdate("1.1.1.1")}, "write.OnUpdate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := resolveValues(scopedSchema(), tc.v)
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "UPSERT") {
				t.Fatalf("a scoped value must be refused off the upsert path, got %v", err)
			}
		})
	}
}

// The clearing verbs UNSCOPED, which is what they have always been: an upsert
// states each one twice — bound in the row it proposes, literal (or read back
// from that row) in the conflict clause.
func TestUpsert_ClearingVerbsSayThemselvesOnBothHalves(t *testing.T) {
	w, _ := newScopedWriter(t)
	key := upsertConfig{conflictGo: []string{"Identity"}}

	plan, err := w.upsertPlan(Values{
		"Identity": "bob", "LastIP": "1.1.1.1",
		"RepeatedAt": StampNull, "RepeatCount": StampEmpty,
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, _, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	head, tail, _ := strings.Cut(sql, "ON CONFLICT")
	if !strings.Contains(head, "repeated_at") || !strings.Contains(head, "repeat_count") {
		t.Fatalf("the proposed row states both: %s", head)
	}
	if !strings.Contains(tail, "repeated_at = NULL") || !strings.Contains(tail, "repeat_count = 0") {
		t.Fatalf("the conflict clause says both as literals: %s", tail)
	}

	// The zero instant is the exception: it is already in the proposed row, so
	// the conflict path takes it from there rather than binding it twice.
	plan, err = w.upsertPlan(Values{
		"Identity": "bob", "LastIP": "1.1.1.1", "RepeatedAt": StampEmpty,
	}, key)
	if err != nil {
		t.Fatalf("upsertPlan: %v", err)
	}
	sql, args, err := w.renderUpsert(testPGDialect{}, plan, testUpsertID, testNow)
	if err != nil {
		t.Fatalf("renderUpsert: %v", err)
	}
	if _, tail, _ = strings.Cut(sql, "ON CONFLICT"); !strings.Contains(tail, "repeated_at = EXCLUDED.repeated_at") {
		t.Fatalf("the zero instant is read back from the proposed row: %s", tail)
	}
	if len(args) != 4 || !args[3].(time.Time).IsZero() {
		t.Fatalf("the zero instant is bound once, args = %v", args)
	}
}
