package write

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// Optimistic concurrency: every ROOT update pins the revision the entity was
// loaded with, so a write built on a stale read is refused instead of reverting
// whatever landed in between. Zero rows then has two causes and one probe tells
// them apart — 404 when the row is gone, 409 when it moved.

func notificationKeys(t *testing.T, err error) []string {
	t.Helper()
	carrier, ok := errAsCarrier(err)
	if !ok {
		t.Fatalf("expected a NotificationCarrier, got %T (%v)", err, err)
	}
	var keys []string
	for _, ctx := range carrier.NotificationContexts() {
		for _, m := range ctx.Messages() {
			keys = append(keys, domain.NotificationKey(m.Notification))
		}
	}
	return keys
}

func hasKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// loadedUpdatable builds an Updatable whose entity carries `revision` exactly as
// the relational loader would have stamped it.
func loadedUpdatable(t *testing.T, revision int64) domain.Updatable {
	t.Helper()
	e := &builderTestEntity{Name: "a", Email: "a@x"}
	e.SetID(domain.NewID(uuid.NewString()))
	if revision > 0 && !domain.SetManagedColumns(e, revision, nil, nil, nil) {
		t.Fatal("SetManagedColumns did not reach the entity's Managed carrier")
	}
	upd, err := domain.GetUpdatable(e, func(*builderTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return upd
}

// ---------- statement shape ----------

func TestBuildUpdate_PinsTheLoadedRevision(t *testing.T) {
	sql, args := buildUpdate(testPGDialect{}, "users", "id", "u1",
		domain.Fields{"name": "Ana"}, []string{"updated_at"}, testNow, "revision", 7)

	want := "UPDATE users SET name = $1, updated_at = $2, revision = revision + 1 WHERE id = $3 AND revision = $4"
	if sql != want {
		t.Fatalf("sql = %q, want %q", sql, want)
	}
	if len(args) != 4 || args[3] != int64(7) {
		t.Fatalf("the loaded revision must be bound last, got args = %v", args)
	}
}

func TestBuildUpdate_UnguardedWhenEntityNeverCameFromTheLoader(t *testing.T) {
	sql, args := buildUpdate(testPGDialect{}, "users", "id", "u1",
		domain.Fields{"name": "Ana"}, nil, testNow, "revision", 0)

	if strings.Contains(sql, "AND") {
		t.Errorf("revision 0 means unknown provenance — the write must not be guarded: %q", sql)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want the field + the id only", args)
	}
}

// A child row declares no revision of its own (the schema builder rejects it):
// even handed a revision, the statement must stay unguarded.
func TestBuildUpdate_UnguardedWithoutARevisionColumn(t *testing.T) {
	sql, _ := buildUpdate(testPGDialect{}, "addresses", "id", "c1",
		domain.Fields{"street": "Main"}, nil, testNow, "", 7)

	if strings.Contains(sql, "AND") {
		t.Errorf("a schema without a revision column has nothing to pin: %q", sql)
	}
}

// The capability is probed, never required: anything that cannot answer a
// revision writes unguarded instead of panicking the write path.
func TestLoadedRevision_WithoutTheCarrierDegradesToZero(t *testing.T) {
	if got := loadedRevision(nil); got != 0 {
		t.Errorf("loadedRevision(nil) = %d, want 0", got)
	}
}

// ---------- the 404 / 409 split ----------

func TestExecExpectingRow_GuardedZeroRows_RowStillThere_IsConflict(t *testing.T) {
	tx := &recTx{count: 0, queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}

	err := execExpectingRow(context.Background(), tx, testPGDialect{},
		"UPDATE users …", nil, "users", "User", "id", "u1", 7)

	if keys := notificationKeys(t, err); !hasKey(keys, "ConcurrentModificationNotification") {
		t.Errorf("a row that still exists after a guarded miss is a conflict, got %v", keys)
	}
}

func TestExecExpectingRow_GuardedZeroRows_RowGone_IsNotFound(t *testing.T) {
	tx := &recTx{count: 0, queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 0}, nil
	}}

	err := execExpectingRow(context.Background(), tx, testPGDialect{},
		"UPDATE users …", nil, "users", "User", "id", "u1", 7)

	if keys := notificationKeys(t, err); !hasKey(keys, "RecordNotFoundNotification") {
		t.Errorf("a vanished row is still a 404, not a conflict, got %v", keys)
	}
}

// Unguarded, zero rows can only mean the row is gone — the probe must not run.
func TestExecExpectingRow_UnguardedZeroRows_SkipsTheProbe(t *testing.T) {
	probed := false
	tx := &recTx{count: 0, queryFn: func(string, []any) (Rows, error) {
		probed = true
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}

	err := execExpectingRow(context.Background(), tx, testPGDialect{},
		"UPDATE users …", nil, "users", "User", "id", "u1", 0)

	if probed {
		t.Error("an unguarded statement must not pay the disambiguation probe")
	}
	if keys := notificationKeys(t, err); !hasKey(keys, "RecordNotFoundNotification") {
		t.Errorf("expected RecordNotFoundNotification, got %v", keys)
	}
}

func TestExecExpectingRow_ProbeErrorPropagates(t *testing.T) {
	boom := errors.New("probe failed")
	tx := &recTx{count: 0, queryFn: func(string, []any) (Rows, error) { return nil, boom }}

	err := execExpectingRow(context.Background(), tx, testPGDialect{},
		"UPDATE users …", nil, "users", "User", "id", "u1", 7)

	if !errors.Is(err, boom) {
		t.Fatalf("a failing probe must surface, not be swallowed into a 404: %v", err)
	}
}

// ---------- through the engine ----------

func TestBaseEngine_Update_GuardRidesTheUpdateStatement(t *testing.T) {
	// The happy path still reads the outbox meta (revision + created_at); what it
	// must NOT do is pay the existence probe, which is a bare SELECT 1.
	tx := &recTx{count: 1, queryFn: func(sql string, _ []any) (Rows, error) {
		if strings.HasPrefix(sql, "SELECT 1 ") {
			t.Errorf("the happy path must not run the disambiguation probe, got %q", sql)
		}
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})

	if _, err := be.Update(newBuilderCtx(), loadedUpdatable(t, 7), builderTestSchema, firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var updateSQL string
	var updateArgs []any
	for i, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE builder_test_entities") {
			updateSQL, updateArgs = s, tx.execArgs[i]
		}
	}
	if updateSQL == "" {
		t.Fatal("no root UPDATE was emitted")
	}
	if !strings.Contains(updateSQL, "revision = $") {
		t.Errorf("the root UPDATE must pin the loaded revision, got %q", updateSQL)
	}
	if len(updateArgs) == 0 || updateArgs[len(updateArgs)-1] != int64(7) {
		t.Errorf("the loaded revision must be bound, got args = %v", updateArgs)
	}
}

// The guard reads the LIVE entity, never Old(): the old-state ghost is a JSON
// clone and domain.Managed is unexported, so the snapshot carries revision 0.
// Reading it there would silently disable the guard on every write.
func TestBaseEngine_Update_GuardReadsTheLiveEntityNotTheSnapshot(t *testing.T) {
	upd := loadedUpdatable(t, 7)
	if old := upd.Source().Old(); old == nil {
		t.Fatal("the fixture must carry an old-state snapshot")
	} else if rc, ok := any(old).(interface{ GetRevision() int64 }); !ok || rc.GetRevision() != 0 {
		t.Fatal("premise changed: the snapshot now carries a revision — reread which side the guard should use")
	}
	if got := loadedRevision(upd.Source()); got != 7 {
		t.Fatalf("loadedRevision must read the live entity, got %d want 7", got)
	}
}

func TestBaseEngine_Update_StaleRevisionIsRefusedNotApplied(t *testing.T) {
	tx := &recTx{count: 0, queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})

	_, err := be.Update(newBuilderCtx(), loadedUpdatable(t, 7), builderTestSchema, WriteHook{})

	if keys := notificationKeys(t, err); !hasKey(keys, "ConcurrentModificationNotification") {
		t.Errorf("a stale write must be refused with a conflict, got %v", keys)
	}
	if tx.committed {
		t.Error("a refused write must not commit")
	}
	if !tx.rolledBack {
		t.Error("a refused write must roll back")
	}
}

// An entity the framework never loaded still writes — the guard degrades rather
// than making every hand-rolled path fail.
func TestBaseEngine_Update_UnloadedEntityWritesUnguarded(t *testing.T) {
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})

	if _, err := be.Update(newBuilderCtx(), loadedUpdatable(t, 0), builderTestSchema, firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE builder_test_entities") && strings.Contains(s, "AND") {
			t.Errorf("revision 0 must write unguarded, got %q", s)
		}
	}
}
