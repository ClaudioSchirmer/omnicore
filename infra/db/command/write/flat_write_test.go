package write

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
)

// These white-box tests drive the shared flat write path on BaseEngine through a
// recording fake WriteTx + WriteBeginner — the unit-level twin of what the
// integration suites exercise against a real database. They cover the control
// flow + error branches (begin/exec/commit failures, update-not-found, hook
// firing, the missing-SoftDelete guard) without a live backend; the real SQL
// behavior remains the integration suites' contract.

// recTx records the statements the write path emits and exposes injectable
// failures + a programmable rows-affected count.
type recTx struct {
	count      int64            // ExecCount return value (matched rows)
	execErrSub string           // Exec/ExecCount fails when the SQL contains this substring
	execErr    error            // the injected failure (errRecExec when unset) — e.g. a *pgconn.PgError for the FK-veto branch
	execErrs   map[string]error // multi-injection: substring → error (first match wins), for chained failures
	commitErr  error
	execs      []string
	execArgs   [][]any // args of each recorded statement, aligned with execs
	committed  bool
	rolledBack bool
	queryFn    func(sql string, args []any) (Rows, error) // drives Query (shared-base existence probe)
}

// fakeRows is a minimal core.Rows for the write-side existence probe.
type fakeRows struct {
	remaining int
	scan      func(dest []any) error
}

func (r *fakeRows) Next() bool {
	if r.remaining <= 0 {
		return false
	}
	r.remaining--
	return true
}
func (r *fakeRows) Scan(dest ...any) error {
	if r.scan != nil {
		return r.scan(dest)
	}
	return nil
}
func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

type recTxHandle struct{ persistence.SealedTxHandle }

var errRecExec = errors.New("rec exec error")

func (t *recTx) failsOn(sql string) bool {
	return t.execErrSub != "" && strings.Contains(sql, t.execErrSub)
}

func (t *recTx) injectedErr(sql string) error {
	for sub, err := range t.execErrs {
		if strings.Contains(sql, sub) {
			return err
		}
	}
	if t.failsOn(sql) {
		if t.execErr != nil {
			return t.execErr
		}
		return errRecExec
	}
	return nil
}

func (t *recTx) Exec(_ context.Context, sql string, args ...any) error {
	t.execs = append(t.execs, sql)
	t.execArgs = append(t.execArgs, args)
	return t.injectedErr(sql)
}
func (t *recTx) ExecCount(_ context.Context, sql string, args ...any) (int64, error) {
	t.execs = append(t.execs, sql)
	t.execArgs = append(t.execArgs, args)
	if err := t.injectedErr(sql); err != nil {
		return 0, err
	}
	return t.count, nil
}
func (t *recTx) Query(_ context.Context, sql string, args ...any) (Rows, error) {
	if t.queryFn != nil {
		return t.queryFn(sql, args)
	}
	return nil, nil
}
func (t *recTx) QueryRow(context.Context, string, ...any) Row { return nil }
func (t *recTx) Commit(context.Context) error                 { t.committed = true; return t.commitErr }
func (t *recTx) Rollback(context.Context) error               { t.rolledBack = true; return nil }
func (t *recTx) Handle() persistence.TxHandle                 { return &recTxHandle{} }
func (t *recTx) Dialect() Dialect                             { return testPGDialect{} }

type recBeginner struct {
	tx       *recTx
	beginErr error
}

func (b *recBeginner) Begin(context.Context) (WriteTx, error) {
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

// newFlatBE builds a BaseEngine wired to beginner with audit on both
// destinations (so the in-TX audit row + post-commit echo branches run) and a
// discard logger.
func newFlatBE(beginner WriteBeginner) *BaseEngine {
	be := &BaseEngine{}
	be.SetBeginner(beginner)
	be.ConfigureAudit(
		&audit.Config{Destinations: []audit.Destination{audit.DestinationDatabase, audit.DestinationSlog}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		[]string{"tenant_id"},
	)
	_ = be.AuditClaims()
	return be
}

func flatInsertable(t *testing.T) domain.Insertable {
	t.Helper()
	ins, err := domain.GetInsertable(&builderTestEntity{Name: "a", Email: "a@x"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	return ins
}

func flatUpdatable(t *testing.T) domain.Updatable {
	t.Helper()
	e := &builderTestEntity{Name: "a", Email: "a@x"}
	e.SetID(domain.NewID(uuid.NewString()))
	upd, err := domain.GetUpdatable(e, func(*builderTestEntity) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	return upd
}

// firingHook fires both slots successfully, covering the FireAfterBegin /
// FireBeforeCommit success branches (incl. tx.Handle()).
var firingHook = WriteHook{
	AfterBegin:   func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error { return nil },
	BeforeCommit: func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error { return nil },
}

func TestBaseEngine_Insert_HappyPath(t *testing.T) {
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	res, err := be.Insert(newBuilderCtx(), flatInsertable(t), builderTestSchema, firingHook)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if res.ID == "" {
		t.Error("expected a generated id")
	}
	if !tx.committed {
		t.Error("expected commit")
	}
	// data INSERT + outbox INSERT + audit INSERT.
	if len(tx.execs) != 3 {
		t.Errorf("expected 3 statements (data+outbox+audit), got %d: %v", len(tx.execs), tx.execs)
	}
}

func TestBaseEngine_Insert_ErrorBranches(t *testing.T) {
	// begin failure.
	be := newFlatBE(&recBeginner{beginErr: errors.New("begin boom")})
	if _, err := be.Insert(newBuilderCtx(), flatInsertable(t), builderTestSchema, WriteHook{}); err == nil {
		t.Error("expected begin error")
	}

	// data-write failure → rollback, no commit.
	tx := &recTx{execErrSub: "INSERT INTO builder_test_entities"}
	be = newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), flatInsertable(t), builderTestSchema, WriteHook{}); !errors.Is(err, errRecExec) {
		t.Errorf("expected exec error, got %v", err)
	}
	if tx.committed {
		t.Error("must not commit on data-write failure")
	}

	// commit failure.
	tx = &recTx{commitErr: errors.New("commit boom")}
	be = newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Insert(newBuilderCtx(), flatInsertable(t), builderTestSchema, WriteHook{}); err == nil {
		t.Error("expected commit error")
	}

	// afterBegin hook failure short-circuits before any write.
	tx = &recTx{}
	be = newFlatBE(&recBeginner{tx: tx})
	hook := WriteHook{AfterBegin: func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error {
		return errors.New("hook boom")
	}}
	if _, err := be.Insert(newBuilderCtx(), flatInsertable(t), builderTestSchema, hook); err == nil {
		t.Error("expected afterBegin hook error")
	}
	if len(tx.execs) != 0 {
		t.Errorf("no statement should run when afterBegin fails, got %v", tx.execs)
	}
}

func TestBaseEngine_Update_FoundAndNotFound(t *testing.T) {
	// matched row.
	tx := &recTx{count: 1}
	be := newFlatBE(&recBeginner{tx: tx})
	if _, err := be.Update(newBuilderCtx(), flatUpdatable(t), builderTestSchema, firingHook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !tx.committed {
		t.Error("expected commit")
	}

	// zero rows → RecordNotFoundNotification carrier, no commit.
	tx = &recTx{count: 0}
	be = newFlatBE(&recBeginner{tx: tx})
	_, err := be.Update(newBuilderCtx(), flatUpdatable(t), builderTestSchema, WriteHook{})
	if _, ok := errAsCarrier(err); !ok {
		t.Fatalf("expected NotFound carrier, got %T (%v)", err, err)
	}
	if tx.committed {
		t.Error("must not commit when the row is absent")
	}
}

func TestBaseEngine_ArchiveUnarchiveDelete(t *testing.T) {
	for _, verb := range []string{"archive", "unarchive", "delete"} {
		tx := &recTx{}
		be := newFlatBE(&recBeginner{tx: tx})
		var err error
		switch verb {
		case "archive":
			e := &builderTestEntity{Name: "a", Email: "a@x"}
			e.SetID(domain.NewID(uuid.NewString()))
			a, _ := domain.GetArchivable(e, nil, "GetArchivable")
			err = be.Archive(newBuilderCtx(), a, builderTestSchema, firingHook)
		case "unarchive":
			e := &builderTestEntity{Name: "a", Email: "a@x"}
			e.SetID(domain.NewID(uuid.NewString()))
			u, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")
			err = be.Unarchive(newBuilderCtx(), u, builderTestSchema, firingHook)
		case "delete":
			e := &builderTestEntity{Name: "a", Email: "a@x"}
			e.SetID(domain.NewID(uuid.NewString()))
			d, _ := domain.GetDeletable(e, nil, "GetDeletable")
			err = be.Delete(newBuilderCtx(), d, builderTestSchema, firingHook)
		}
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if !tx.committed {
			t.Errorf("%s: expected commit", verb)
		}
		// soft-write/delete + outbox + audit.
		if len(tx.execs) != 3 {
			t.Errorf("%s: expected 3 statements, got %d", verb, len(tx.execs))
		}
	}
}

func TestBaseEngine_Archive_MissingSoftDeleteIsError(t *testing.T) {
	noSD := NewTableSchema[*builderTestEntity]("nsd").PK("id").Field("Name", "name").Field("Email", "email")
	tx := &recTx{}
	be := newFlatBE(&recBeginner{tx: tx})
	e := &builderTestEntity{Name: "a", Email: "a@x"}
	e.SetID(domain.NewID(uuid.NewString()))
	a, _ := domain.GetArchivable(e, nil, "GetArchivable")
	if err := be.Archive(newBuilderCtx(), a, noSD, WriteHook{}); err == nil {
		t.Fatal("expected an error archiving a schema without SoftDelete")
	}
}

func errAsCarrier(err error) (domain.NotificationCarrier, bool) {
	var c domain.NotificationCarrier
	ok := errors.As(err, &c)
	return c, ok
}

// TestBaseEngine_HookError_NilLoggerFallsBack covers the hook-dispatch error
// path on a BaseEngine with NO logger configured: logHookError must fall back to
// slog.Default() (via b.log()) without panicking, and the hook error propagates
// verbatim. The dialect engines inherit this through the promoted FireAfterBegin.
func TestBaseEngine_HookError_NilLoggerFallsBack(t *testing.T) {
	be := &BaseEngine{} // no ConfigureAudit → nil logger
	hook := WriteHook{AfterBegin: func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error {
		return errors.New("boom")
	}}
	err := be.FireAfterBegin(newBuilderCtx(), &recTx{}, &builderTestEntity{}, hook, HookContext{Verb: "Insert", EntityType: "x"})
	if err == nil {
		t.Fatal("expected the hook error to propagate")
	}
}
