package query

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// Shared infra-root test harness: a scriptable core.RelationalEngine + core.Querier
// + core.Rows/core.Row + core.Dialect, plus the small entity/VO fixtures the composer,
// drift, rebuild, and upstream-failure tests drive. The Mongo-view control plane
// (composer/sync/rebuild/drift/upstream) reaches the relational backend ONLY
// through the engine seam now, so these fakes stand in for a live database
// without any pgx dependency.

var errFake = errors.New("fake infra error")

// assertPanics runs fn and fails unless it panics — a shared infra-root test
// helper (the schema-validation tests that used to host it moved to infra/db).
func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

// --- entity fixtures -------------------------------------------------------

// builderTestEntity is the flat root entity the composer/rebuild schemas anchor.
type builderTestEntity struct {
	domain.BaseEntity
	Name  string
	Email string
}

func (e *builderTestEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (e *builderTestEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// builderTestSchema is the flat schema anchoring builderTestEntity in the view
// and composer tests.
var builderTestSchema = core.NewTableSchema[*builderTestEntity]("builder_test_entities").
	PK("id").
	Field("Name", "name").
	Field("Email", "email").
	SoftDelete("deleted_at").
	CreatedAt("created_at").
	UpdatedAt("updated_at")

// schemaSample is a flat fixture for the view-schema tests (mirrors the db
// package's own schemaSample — fixtures do not cross a package boundary).
type schemaSample struct {
	ID      string
	Name    string
	Created string
	Updated string
	Removed string
}

// fakeVO is a value-object fixture for the composer embed schemas.
type fakeVO struct {
	ID    string
	Label string
}

func (v fakeVO) GetID() domain.ID                                 { return domain.NewID(v.ID) }
func (v fakeVO) BuildRules(string, domain.Service, *domain.Rules) {}

func newBuilderCtx() persistence.RequestContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

// --- fake engine -----------------------------------------------------------

// fakeEngine is a scriptable core.RelationalEngine. q drives the neutral read
// surface (the composer's QueryMaps, the rebuild SELECT-id Query, the registry
// Exec/QueryRow). acquireErr forces AcquireRebuildLock to fail; lockHeld makes
// the returned lock report "held by someone else" (Acquired()==false).
type fakeEngine struct {
	q          core.Querier
	acquireErr error
	lockHeld   bool
	lockHolder string
}

func newFakeEngine(q core.Querier) *fakeEngine { return &fakeEngine{q: q} }

func (fakeEngine) Insert(persistence.RequestContext, domain.Insertable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (fakeEngine) Update(persistence.RequestContext, domain.Updatable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (fakeEngine) Archive(persistence.RequestContext, domain.Archivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (fakeEngine) Unarchive(persistence.RequestContext, domain.Unarchivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (fakeEngine) Delete(persistence.RequestContext, domain.Deletable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (e *fakeEngine) Querier() core.Querier { return e.q }
func (*fakeEngine) Dialect() core.Dialect   { return fakeDialect{} }
func (e *fakeEngine) WithAudit(*audit.Config, *slog.Logger, []string) core.RelationalEngine {
	return e
}
func (e *fakeEngine) WithEventPublisher(events.Publisher) core.RelationalEngine { return e }
func (e *fakeEngine) AcquireRebuildLock(context.Context, string) (core.RebuildLock, error) {
	if e.acquireErr != nil {
		return nil, e.acquireErr
	}
	return &fakeRebuildLock{q: e.q, held: e.lockHeld, holder: e.lockHolder}, nil
}
func (*fakeEngine) Close() {}

// fakeRebuildLock is the scriptable core.RebuildLock: held=true means another
// session owns the mutex (Acquired()==false); its Querier is the engine's seam.
type fakeRebuildLock struct {
	q      core.Querier
	held   bool
	holder string
}

func (l *fakeRebuildLock) Acquired() bool              { return !l.held }
func (l *fakeRebuildLock) Holder() string              { return l.holder }
func (l *fakeRebuildLock) Querier() core.Querier       { return l.q }
func (*fakeRebuildLock) Release(context.Context) error { return nil }

// --- fake dialect (Postgres-shaped) ----------------------------------------

type fakeDialect struct{}

func (fakeDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (fakeDialect) QuoteIdent(name string) string {
	if !core.SafeIdentifier(name) {
		panic(fmt.Sprintf("test: invalid SQL identifier %q", name))
	}
	return name
}
func (fakeDialect) EncodeArg(val any) any {
	if id, ok := val.(domain.ID); ok {
		return id.Value()
	}
	return val
}
func (fakeDialect) DecodeID(raw string) (string, error)        { return raw, nil }
func (fakeDialect) ILikeClause(col, ph string) string          { return col + " ILIKE " + ph }
func (fakeDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (fakeDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }

// BuildUpsert mirrors the Postgres flavor (INSERT INTO … VALUES (…) ON CONFLICT
// (…) DO UPDATE/NOTHING) so the control-plane upsert SQL the failure-registry
// helpers generate is shape-asserted faithfully without importing the pg engine.
func (d fakeDialect) BuildUpsert(table string, cols, conflictCols []string, sets []core.UpsertSet) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(d.QuoteIdent(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(") VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.Placeholder(i + 1))
	}
	b.WriteString(") ON CONFLICT (")
	for i, c := range conflictCols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(c))
	}
	b.WriteString(")")
	if len(sets) == 0 {
		b.WriteString(" DO NOTHING")
		return b.String()
	}
	b.WriteString(" DO UPDATE SET ")
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(d.QuoteIdent(s.Col))
		b.WriteString(" = ")
		if s.Mode == core.UpsertSetNew {
			b.WriteString("EXCLUDED." + d.QuoteIdent(s.Col))
		} else {
			b.WriteString(s.Expr)
		}
	}
	return b.String()
}

// --- fake querier / rows / row ---------------------------------------------

// fakeQuerier is a scriptable core.Querier. Each verb has an optional hook; unset
// hooks default to an empty success (zero rows / no-op exec).
type fakeQuerier struct {
	queryFn     func(sql string, args []any) (core.Rows, error)
	queryMapsFn func(sql string, args []any) ([]map[string]any, error)
	queryRowFn  func(sql string, args []any) core.Row
	execFn      func(sql string, args []any) error
}

func (q *fakeQuerier) Query(_ context.Context, sql string, args ...any) (core.Rows, error) {
	if q.queryFn != nil {
		return q.queryFn(sql, args)
	}
	return &fakeRows{}, nil
}
func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) core.Row {
	if q.queryRowFn != nil {
		return q.queryRowFn(sql, args)
	}
	return fakeRow{}
}
func (q *fakeQuerier) Exec(_ context.Context, sql string, args ...any) error {
	if q.execFn != nil {
		return q.execFn(sql, args)
	}
	return nil
}
func (q *fakeQuerier) QueryMaps(_ context.Context, sql string, args ...any) ([]map[string]any, error) {
	if q.queryMapsFn != nil {
		return q.queryMapsFn(sql, args)
	}
	return nil, nil
}

// fakeRows is a programmable core.Rows. rows drives Next; scan (when set) populates
// the destinations per row; nextErr is returned by Err().
type fakeRows struct {
	rows    int
	pos     int
	scan    func(idx int, dest []any) error
	nextErr error
}

func (r *fakeRows) Next() bool {
	if r.pos >= r.rows {
		return false
	}
	r.pos++
	return true
}
func (r *fakeRows) Scan(dest ...any) error {
	if r.scan != nil {
		return r.scan(r.pos-1, dest)
	}
	return nil
}
func (r *fakeRows) Err() error   { return r.nextErr }
func (r *fakeRows) Close() error { return nil }

// fakeRow is a programmable core.Row: id is scanned into the first *string dest;
// err short-circuits Scan.
type fakeRow struct {
	id  string
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if p, ok := d.(*string); ok {
			*p = r.id
			return nil
		}
	}
	return nil
}

// mapsFromColsData builds the column-keyed maps the engine's QueryMaps returns
// from a column list + positional row data — the dynamic-shape read the composer
// consumes.
func mapsFromColsData(cols []string, data [][]any) []map[string]any {
	out := make([]map[string]any, len(data))
	for i, row := range data {
		m := make(map[string]any, len(cols))
		for j, c := range cols {
			m[c] = row[j]
		}
		out[i] = m
	}
	return out
}
