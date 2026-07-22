package write

import (
	"strings"
	"testing"

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

// The Batch role verbs go through fillBaseMeta → the base revision advances there too
// (a batch UPDATE of a role never touches the base otherwise).
func TestBatchRoleUpdate_BumpsBaseRevision(t *testing.T) {
	tx := &recTx{count: 1, queryFn: rowsFKMatch()}
	be := newFlatBE(&recBeginner{tx: tx})
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M2"}
	e.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(e, func(*roleTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	batch := domain.NewBatch([]domain.ValidEntity{upd})
	if _, err := be.Batch(newBuilderCtx(), batch, []*TableSchema{roleTestSchema()}); err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if !hasRevisionBump(tx.execs) {
		t.Errorf("a batch role update must advance the base revision, got %v", tx.execs)
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

// The BATCH Deletable member captures the row's last revision BEFORE the
// DELETE (the row answers 0 afterwards) and stamps it on the DELETED payload —
// the same tombstone contract as the standalone hard delete — while the base
// half still advances the identity's revision after the row is gone.
func TestBatchRoleDelete_PayloadCarriesLastRevisionAndBumpsBase(t *testing.T) {
	tx := &recTx{count: 1, queryFn: func(sql string, _ []any) (Rows, error) {
		if strings.Contains(sql, "SELECT revision FROM aluno") {
			return &fakeRows{remaining: 1, scan: func(dest []any) error {
				if p, ok := dest[0].(*int64); ok {
					*p = 9
				}
				return nil
			}}, nil
		}
		return &fakeRows{remaining: 1, scan: func([]any) error { return nil }}, nil
	}}
	be := newFlatBE(&recBeginner{tx: tx})
	e := &roleTestEntity{Name: "Ana", Document: "D1", Matricula: "M1"}
	e.SetID(domain.NewID(uuid.NewString()))
	del, err := domain.GetDeletable(e, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	batch := domain.NewBatch([]domain.ValidEntity{del})
	if _, err := be.Batch(newBuilderCtx(), batch, []*TableSchema{roleTestSchema()}); err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if !hasRevisionBump(tx.execs) {
		t.Errorf("a batch role delete must advance the identity's base revision, got %v", tx.execs)
	}
	var payload string
	for i, s := range tx.execs {
		if strings.HasPrefix(s, "INSERT INTO outbox") {
			for _, a := range tx.execArgs[i] {
				if b, ok := a.([]byte); ok && strings.Contains(string(b), `"_ids"`) {
					payload = string(b)
				}
				if str, ok := a.(string); ok && strings.Contains(str, `"_ids"`) {
					payload = str
				}
			}
		}
	}
	if !strings.Contains(payload, `"revision":9`) {
		t.Errorf("the batch DELETED payload must carry the last revision, got %s", payload)
	}
}
