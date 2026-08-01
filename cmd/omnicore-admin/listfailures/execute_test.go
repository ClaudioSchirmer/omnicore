package listfailures

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// execute() runs the read + render through the backend-neutral seam, so a
// scriptable engine drives every branch without a live database: topic vs
// full listing, the view filter, the truncation hint, both formats, and the
// list-error surface.

var errFakeList = errors.New("fake list error")

type lfRows struct {
	rows []fakeFailureRow
	pos  int
}

type fakeFailureRow struct {
	kind, topic, view, upstreamID string
}

func (r *lfRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *lfRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	// Column order mirrors selectProjectionFailures: id, kind, consumer_group,
	// topic, aggregate_type, event_type*, aggregate_id, stage*, local_id*,
	// traceparent*, payload*, error, attempt, first_seen, last_attempt
	// (* = nullable, scanned into **string).
	set := func(i int, v string) {
		if p, ok := dest[i].(*string); ok {
			*p = v
		}
	}
	setPtr := func(i int, v string) {
		if p, ok := dest[i].(**string); ok {
			s := v
			*p = &s
		}
	}
	set(0, fmt.Sprintf("row-%d", r.pos))
	set(1, row.kind)
	set(2, "svc-sync")
	set(3, row.topic)
	set(4, row.view)
	set(6, row.upstreamID)
	setPtr(7, "discover")
	setPtr(8, "l1")
	set(11, "boom")
	if p, ok := dest[12].(*int); ok {
		*p = 2
	}
	return nil
}
func (r *lfRows) Err() error   { return nil }
func (r *lfRows) Close() error { return nil }

type lfQuerier struct {
	rows    []fakeFailureRow
	err     error
	lastSQL string
}

func (q *lfQuerier) Query(_ context.Context, sql string, _ ...any) (core.Rows, error) {
	q.lastSQL = sql
	if q.err != nil {
		return nil, q.err
	}
	return &lfRows{rows: q.rows}, nil
}
func (q *lfQuerier) QueryRow(context.Context, string, ...any) core.Row { return nil }
func (q *lfQuerier) Exec(context.Context, string, ...any) error        { return nil }
func (q *lfQuerier) QueryMaps(context.Context, string, ...any) ([]map[string]any, error) {
	return nil, nil
}

type lfDialect struct{}

func (lfDialect) Placeholder(n int) string            { return fmt.Sprintf("$%d", n) }
func (lfDialect) QuoteIdent(name string) string       { return name }
func (lfDialect) EncodeArg(v any) any                 { return v }
func (lfDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (lfDialect) ILikeClause(col, ph string) string   { return col + " ILIKE " + ph }
func (lfDialect) LikeClause(col, ph string) string    { return col + " LIKE " + ph }
func (lfDialect) NowExpr() string                     { return "NOW()" }
func (lfDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}
func (lfDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}
func (lfDialect) Savepoint(name string) string               { return "SAVEPOINT " + name }
func (lfDialect) RollbackToSavepoint(name string) string     { return "ROLLBACK TO SAVEPOINT " + name }
func (lfDialect) ReleaseSavepoint(name string) string        { return "RELEASE SAVEPOINT " + name }
func (lfDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (lfDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (lfDialect) BuildUpsert(string, []string, []string, []core.UpsertSet) string {
	return ""
}

type lfEngine struct{ q *lfQuerier }

func (lfEngine) Insert(persistence.RequestContext, domain.Insertable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (lfEngine) Update(persistence.RequestContext, domain.Updatable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (lfEngine) Archive(persistence.RequestContext, domain.Archivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (lfEngine) Unarchive(persistence.RequestContext, domain.Unarchivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (lfEngine) Delete(persistence.RequestContext, domain.Deletable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (e lfEngine) Querier() core.Querier                                                 { return e.q }
func (lfEngine) Dialect() core.Dialect                                                   { return lfDialect{} }
func (e lfEngine) WithAudit(*audit.Config, *slog.Logger, []string) core.RelationalEngine { return e }
func (e lfEngine) WithEventPublisher(events.Publisher) core.RelationalEngine             { return e }
func (lfEngine) AcquireRebuildLock(context.Context, string) (core.RebuildLock, error) {
	return nil, errors.New("not used")
}
func (lfEngine) Close() {}

func failureRows() []fakeFailureRow {
	return []fakeFailureRow{
		{kind: "ripple", topic: "t1", view: "users", upstreamID: "u1"},
		{kind: "event", topic: "t1", view: "orders", upstreamID: "u2"},
		{kind: "ripple", topic: "t2", view: "users", upstreamID: "u3"},
	}
}

func TestExecute_TextListing(t *testing.T) {
	q := &lfQuerier{rows: failureRows()}
	var out bytes.Buffer
	if err := execute(context.Background(), lfEngine{q: q}, executeOptions{Group: "svc-sync", Format: formatText, Out: &out}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(q.lastSQL, "consumer_group =") {
		t.Errorf("the group scope must reach the SQL, got %q", q.lastSQL)
	}
	if !strings.Contains(out.String(), "u1") || !strings.Contains(out.String(), "u3") {
		t.Errorf("text output missing rows:\n%s", out.String())
	}
}

func TestExecute_KindAndTopicFiltersAndJSON(t *testing.T) {
	q := &lfQuerier{rows: failureRows()}
	var out bytes.Buffer
	err := execute(context.Background(), lfEngine{q: q}, executeOptions{Group: "svc-sync", Kind: "event", Topic: "t1", Format: formatJSON, Out: &out})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, `"u2"`) || strings.Contains(s, `"u1"`) || strings.Contains(s, `"u3"`) {
		t.Errorf("kind+topic filters must keep exactly u2:\n%s", s)
	}
}

func TestExecute_ViewFilterAndLimit(t *testing.T) {
	q := &lfQuerier{rows: failureRows()}
	var out bytes.Buffer
	err := execute(context.Background(), lfEngine{q: q}, executeOptions{Group: "svc-sync", View: "users", Limit: 1, Format: formatText, Out: &out})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "u1") || strings.Contains(s, "u2") || strings.Contains(s, "u3") {
		t.Errorf("view filter + limit must keep exactly u1:\n%s", s)
	}
	if !strings.Contains(s, "truncated") {
		t.Errorf("the truncation hint must surface:\n%s", s)
	}
}

func TestExecute_ListErrorPropagates(t *testing.T) {
	q := &lfQuerier{err: errFakeList}
	var out bytes.Buffer
	if err := execute(context.Background(), lfEngine{q: q}, executeOptions{Format: formatText, Out: &out}); err == nil {
		t.Fatal("expected the list error")
	}
}
