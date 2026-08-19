package write

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The base revision contract: EVERY write that touches a shared identity
// advances the base's revision, so the read side can totally order the
// identity's closure (shared scalars, base children, remnant pick, role rows).
// INSERT/UPDATE bump inside upsertSharedBase's own statement (covered by the
// upsert tests); the verbs below did not touch the base at all before this
// contract and are the ones bumpBaseRevision covers.

const revisionBumpSQL = "UPDATE pessoa SET revision = revision + 1"

func hasRevisionBump(execs []string) bool {
	for _, s := range execs {
		if strings.HasPrefix(s, revisionBumpSQL) {
			return true
		}
	}
	return false
}

func roleTestArchivable(t *testing.T) domain.Archivable {
	t.Helper()
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	ar, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	return ar
}

func roleTestUnarchivable(t *testing.T) domain.Unarchivable {
	t.Helper()
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	return un
}

// A role ARCHIVE with other roles still active: the base has no lifecycle
// transition (archiveBaseIfNoActiveRole finds an active sibling and does
// nothing) — yet the base revision must advance: the archive changed the
// identity's closure (the person document's segment pick).
func TestArchiveRole_NoBaseTransition_StillBumpsBaseRevision(t *testing.T) {
	// The anyActiveRole probe finds an active sibling → no base archive.
	tx := &recTx{count: 1, queryFn: rowsFound()}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), roleTestArchivable(t), roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !hasRevisionBump(tx.execs) {
		t.Errorf("a role archive must advance the base revision even without a base transition, got %v", tx.execs)
	}
	for _, s := range tx.execs {
		if strings.HasPrefix(s, "UPDATE pessoa SET deleted_at") {
			t.Errorf("an active sibling must keep the base un-archived, got %q", s)
		}
	}
}

// A role UNARCHIVE when the base is already active: reactivateBaseIfArchived is
// a no-op — the base revision still advances.
func TestUnarchiveRole_BaseAlreadyActive_StillBumpsBaseRevision(t *testing.T) {
	// Probes: the unarchive sibling veto finds nothing; baseIsArchived scans 0
	// (active). fakeRows scan writes nothing → archived stays 0.
	tx := &recTx{count: 1, queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	// The sibling veto reads rows too — remaining:1 would veto. Script per-SQL:
	tx.queryFn = func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "deleted_at IS NULL") && strings.Contains(sql, "FROM aluno") {
			return &fakeRows{remaining: 0}, nil // no active sibling → no veto
		}
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}
	if err := be.Unarchive(newBuilderCtx(), roleTestUnarchivable(t), roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if !hasRevisionBump(tx.execs) {
		t.Errorf("a role unarchive must advance the base revision even when the base is already active, got %v", tx.execs)
	}
}

// The DELETED payload carries the row's LAST revision (read before the DELETE):
// the read side turns it into the document tombstone. The flat batch path pins
// the same contract.
func TestHardDelete_PayloadCarriesLastRevision(t *testing.T) {
	var revRead bool
	tx := &recTx{queryFn: func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "SELECT revision FROM aluno") {
			revRead = true
			return &fakeRows{remaining: 1, scan: func(dest []any) error {
				if p, ok := dest[0].(*int64); ok {
					*p = 7
				}
				return nil
			}}, nil
		}
		return &fakeRows{remaining: 0}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Delete(newBuilderCtx(), roleTestDeletable(t), roleTestSchema(), firingHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !revRead {
		t.Fatal("the role's own revision must be read before the DELETE")
	}
	// The outbox row is the last-but-one exec's payload; assert the _ids block
	// carries revision 7 by inspecting the recorded outbox args.
	var payload string
	for i, s := range tx.execs {
		if strings.HasPrefix(s, "INSERT INTO outbox") {
			for _, a := range tx.execArgs[i] {
				if b, ok := a.([]byte); ok && strings.Contains(string(b), `"_ids"`) {
					payload = string(b)
				}
				if s, ok := a.(string); ok && strings.Contains(s, `"_ids"`) {
					payload = s
				}
			}
		}
	}
	if payload == "" {
		t.Fatal("no outbox payload captured")
	}
	if !strings.Contains(payload, `"revision":7`) {
		t.Errorf("the DELETED payload must carry the last revision, got %s", payload)
	}
}

// The created_at scan tolerates every driver shape (time.Time, string, []byte)
// and degrades to zero on an unknown form — the tombstone then falls back to
// revision-only rather than carrying a wrong discriminator.
func TestNormalizeCreatedAt_DriverShapes(t *testing.T) {
	want := time.Date(2026, 7, 22, 18, 42, 12, 731000000, time.UTC)
	cases := []struct {
		name string
		in   any
		want time.Time
	}{
		{"time.Time", want, want},
		{"rfc3339", "2026-07-22T18:42:12.731Z", want},
		{"mysql naive", []byte("2026-07-22 18:42:12.731"), want},
		{"seconds only", "2026-07-22 18:42:12", want.Truncate(time.Second)},
		{"garbage", "not-a-time", time.Time{}},
		{"nil", nil, time.Time{}},
		{"int", 42, time.Time{}},
	}
	for _, c := range cases {
		if got := normalizeCreatedAt(c.in); !got.Equal(c.want) {
			t.Errorf("%s: normalizeCreatedAt(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// insertCreatedAt hands the operation stamp only when the schema declares a
// CreatedAt column — no column, no discriminator to carry.
func TestInsertCreatedAt_RequiresDeclaredColumn(t *testing.T) {
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	withCol := NewTableSchema[*roleTestEntity]("t1").ID("id").Revision("revision").
		Field("Matricula", "m").CreatedAt("created_at")
	if got := insertCreatedAt(withCol, now); !got.Equal(now) {
		t.Errorf("declared CreatedAt must carry the stamp, got %v", got)
	}
	without := NewTableSchema[*roleTestEntity]("t2").ID("id").Revision("revision").
		Field("Matricula", "m")
	if got := insertCreatedAt(without, now); !got.IsZero() {
		t.Errorf("no CreatedAt column → zero discriminator, got %v", got)
	}
}

// readRevisionCreatedAt reads both values in ONE statement and answers zeros
// for a vanished row — the DELETED of a row already gone tombstones nothing.
func TestReadRevisionCreatedAt_OneStatementAndVanishedRow(t *testing.T) {
	var sqls []string
	tx := &recTx{queryFn: func(sql string, _ []any) (Rows, error) {
		sqls = append(sqls, sql)
		return &fakeRows{remaining: 0}, nil // vanished row
	}}
	rev, created, err := readRevisionCreatedAt(context.Background(), tx, testPGDialect{},
		"aluno", "revision", "created_at", "id", "x")
	if err != nil || rev != 0 || !created.IsZero() {
		t.Fatalf("vanished row must answer zeros, got rev=%d created=%v err=%v", rev, created, err)
	}
	if len(sqls) != 1 || !strings.Contains(sqls[0], "revision, created_at") {
		t.Errorf("both values must ride ONE statement, got %v", sqls)
	}
}

// readRevisionCreatedAt error branches: a failing query propagates; a scan
// error propagates; the createdCol=="" form scans revision alone.
func TestReadRevisionCreatedAt_ErrorAndRevOnlyBranches(t *testing.T) {
	// Query error propagates.
	tx := &recTx{queryFn: func(string, []any) (Rows, error) { return nil, errRecExec }}
	if _, _, err := readRevisionCreatedAt(context.Background(), tx, testPGDialect{},
		"aluno", "revision", "created_at", "id", "x"); err == nil {
		t.Error("a query error must propagate")
	}
	// Scan error propagates (both-column form).
	tx = &recTx{queryFn: func(string, []any) (Rows, error) {
		return &fakeRows{remaining: 1, scan: func([]any) error { return errRecExec }}, nil
	}}
	if _, _, err := readRevisionCreatedAt(context.Background(), tx, testPGDialect{},
		"aluno", "revision", "created_at", "id", "x"); err == nil {
		t.Error("a scan error must propagate")
	}
	// Revision-only form (no CreatedAt column declared) scans one value.
	tx = &recTx{queryFn: func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "created_at") {
			t.Errorf("createdCol=\"\" must not select created_at: %s", sql)
		}
		return &fakeRows{remaining: 1, scan: func(dest []any) error {
			if p, ok := dest[0].(*int64); ok {
				*p = 6
			}
			return nil
		}}, nil
	}}
	rev, created, err := readRevisionCreatedAt(context.Background(), tx, testPGDialect{},
		"aluno", "revision", "", "id", "x")
	if err != nil || rev != 6 || !created.IsZero() {
		t.Errorf("rev-only form: got rev=%d created=%v err=%v", rev, created, err)
	}
}

// A failing base-revision bump inside fillBaseMeta fails the verb — the
// payload must never carry a base revision the bump did not produce.
func TestFillBaseMeta_BumpErrorPropagates(t *testing.T) {
	tx := &recTx{count: 1, execErrSub: "UPDATE pessoa SET revision", queryFn: rowsFKMatch()}
	be := newFlatBE(&recBeginner{tx: tx})
	if err := be.Archive(newBuilderCtx(), roleTestArchivable(t), roleTestSchema(), firingHook); err == nil {
		t.Fatal("a failing identity-revision bump must fail the verb")
	}
	if tx.committed {
		t.Error("the TX must not commit after a failed bump")
	}
}
