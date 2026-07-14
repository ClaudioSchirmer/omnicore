package replay

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// --- buildWhere -------------------------------------------------------------

func TestBuildWhere(t *testing.T) {
	cases := []struct {
		name            string
		includeArchived bool
		filter          string
		want            string
	}{
		{"active-no-filter", false, "", " WHERE deleted_at IS NULL"},
		{"active-with-filter", false, "active = true", " WHERE deleted_at IS NULL AND (active = true)"},
		{"archived-no-filter", true, "", ""},
		{"archived-with-filter", true, "tenant_id = 'acme'", " WHERE (tenant_id = 'acme')"},
		{"active-blank-filter", false, "   ", " WHERE deleted_at IS NULL"},
		{"archived-blank-filter", true, "  ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildWhere(tc.includeArchived, tc.filter); got != tc.want {
				t.Errorf("buildWhere(%v, %q) = %q, want %q", tc.includeArchived, tc.filter, got, tc.want)
			}
		})
	}
}

// --- stringField ------------------------------------------------------------

func TestStringField(t *testing.T) {
	row := map[string]any{
		"id":      "abc-123",
		"raw":     []byte("byte-id"),
		"num":     42,
		"nilcol":  nil,
		"present": "",
	}
	t.Run("string", func(t *testing.T) {
		got, ok := stringField(row, "id")
		if !ok || got != "abc-123" {
			t.Fatalf("got (%q,%v)", got, ok)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		got, ok := stringField(row, "raw")
		if !ok || got != "byte-id" {
			t.Fatalf("got (%q,%v)", got, ok)
		}
	})
	t.Run("non-canonical-quoted", func(t *testing.T) {
		got, ok := stringField(row, "num")
		if !ok || got != `"42"` {
			t.Fatalf("got (%q,%v), want quoted 42", got, ok)
		}
	})
	t.Run("missing-key", func(t *testing.T) {
		if got, ok := stringField(row, "absent"); ok || got != "" {
			t.Fatalf("missing key must be ('',false), got (%q,%v)", got, ok)
		}
	})
	t.Run("nil-value", func(t *testing.T) {
		if got, ok := stringField(row, "nilcol"); ok || got != "" {
			t.Fatalf("nil value must be ('',false), got (%q,%v)", got, ok)
		}
	})
	t.Run("present-empty-string", func(t *testing.T) {
		// An empty string is still "present" — ok true, value "".
		got, ok := stringField(row, "present")
		if !ok || got != "" {
			t.Fatalf("got (%q,%v), want ('',true)", got, ok)
		}
	})
}

// --- execute (backend-neutral seam) -----------------------------------------

// fakeDB implements both core.Querier and core.Dialect so execute can be driven
// without a real database. It records the SQL it is handed (to assert the
// generated statements are dialect-quoted + placeholder-rendered) and captures
// each outbox Exec. execute uses only Placeholder + QuoteIdent of the Dialect
// and QueryRow/QueryMaps/Exec of the Querier; the rest are inert stubs.
type fakeDB struct {
	count    int64
	rows     []map[string]any
	queryErr error
	execErr  error

	seenSQL []string
	inserts []outboxInsert
}

type outboxInsert struct {
	sql  string
	args []any
}

// core.Querier
func (f *fakeDB) QueryRow(_ context.Context, sql string, _ ...any) core.Row {
	f.seenSQL = append(f.seenSQL, sql)
	return fakeRow{count: f.count}
}
func (f *fakeDB) Query(_ context.Context, _ string, _ ...any) (core.Rows, error) {
	return nil, errors.New("Query is not used by execute")
}
func (f *fakeDB) QueryMaps(_ context.Context, sql string, _ ...any) ([]map[string]any, error) {
	f.seenSQL = append(f.seenSQL, sql)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}
func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) error {
	if f.execErr != nil {
		return f.execErr
	}
	f.inserts = append(f.inserts, outboxInsert{sql: sql, args: args})
	return nil
}

// core.Dialect — only Placeholder + QuoteIdent are exercised.
func (f *fakeDB) Placeholder(n int) string                   { return fmt.Sprintf("$%d", n) }
func (f *fakeDB) QuoteIdent(name string) string              { return `"` + name + `"` }
func (f *fakeDB) EncodeArg(v any) any                        { return v }
func (f *fakeDB) DecodeID(raw string) (string, error)        { return raw, nil }
func (f *fakeDB) ILikeClause(col, ph string) string          { return col + " ILIKE " + ph }
func (f *fakeDB) NowExpr() string { return "NOW()" }
func (f *fakeDB) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}
func (f *fakeDB) IsUniqueViolation(error) (string, bool)     { return "", false }
func (f *fakeDB) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (f *fakeDB) BuildUpsert(string, []string, []string, []core.UpsertSet) string {
	return ""
}

type fakeRow struct{ count int64 }

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*int64); ok {
			*p = r.count
		}
	}
	return nil
}

func TestExecute_NeutralReplay(t *testing.T) {
	f := &fakeDB{
		count: 2,
		rows: []map[string]any{
			{"id": "id-a", "name": "Alice"},
			{"id": "id-b", "name": "Bob"},
		},
	}
	var buf strings.Builder
	err := execute(context.Background(), f, f, executeOptions{
		Aggregate: "users", BatchSize: 1000, Out: &buf,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The generated SELECT must be dialect-quoted + placeholder-rendered — proof
	// the replay no longer emits hardcoded PG SQL.
	gotSelect := f.seenSQL[len(f.seenSQL)-1]
	for _, want := range []string{`"users"`, `ORDER BY "id"`, "LIMIT $1 OFFSET $2"} {
		if !strings.Contains(gotSelect, want) {
			t.Errorf("select %q missing %q", gotSelect, want)
		}
	}
	if len(f.inserts) != 2 {
		t.Fatalf("expected 2 outbox inserts, got %d", len(f.inserts))
	}
	ins := f.inserts[0]
	if !strings.Contains(ins.sql, "INSERT INTO outbox") || strings.Contains(ins.sql, "::jsonb") ||
		strings.Contains(ins.sql, "gen_random_uuid") {
		t.Errorf("outbox insert is not neutral: %q", ins.sql)
	}
	if len(ins.args) != 4 || ins.args[0] != "users" || ins.args[1] != "INSERTED" || ins.args[2] != "id-a" {
		t.Errorf("outbox insert args drifted: %v", ins.args)
	}
}

func TestExecute_EmptyAndDryRun(t *testing.T) {
	t.Run("no-rows", func(t *testing.T) {
		f := &fakeDB{count: 0}
		var buf strings.Builder
		if err := execute(context.Background(), f, f, executeOptions{Aggregate: "users", BatchSize: 10, Out: &buf}); err != nil {
			t.Fatal(err)
		}
		if len(f.inserts) != 0 {
			t.Errorf("no rows must write nothing, got %d", len(f.inserts))
		}
		if !strings.Contains(buf.String(), "nothing to do") {
			t.Errorf("missing nothing-to-do message: %q", buf.String())
		}
	})
	t.Run("dry-run", func(t *testing.T) {
		f := &fakeDB{count: 5, rows: []map[string]any{{"id": "x"}}}
		var buf strings.Builder
		if err := execute(context.Background(), f, f, executeOptions{Aggregate: "users", BatchSize: 10, DryRun: true, Out: &buf}); err != nil {
			t.Fatal(err)
		}
		if len(f.inserts) != 0 {
			t.Errorf("dry-run must write nothing, got %d", len(f.inserts))
		}
	})
}

func TestExecute_Errors(t *testing.T) {
	t.Run("bad-aggregate", func(t *testing.T) {
		f := &fakeDB{}
		if err := execute(context.Background(), f, f, executeOptions{Aggregate: "bad name", BatchSize: 10, Out: &strings.Builder{}}); err == nil {
			t.Fatal("expected invalid-identifier error")
		}
	})
	t.Run("missing-id", func(t *testing.T) {
		f := &fakeDB{count: 1, rows: []map[string]any{{"name": "no-id"}}}
		if err := execute(context.Background(), f, f, executeOptions{Aggregate: "users", BatchSize: 10, Out: &strings.Builder{}}); err == nil {
			t.Fatal("expected missing-id error")
		}
	})
	t.Run("query-error", func(t *testing.T) {
		f := &fakeDB{count: 1, queryErr: errors.New("boom")}
		if err := execute(context.Background(), f, f, executeOptions{Aggregate: "users", BatchSize: 10, Out: &strings.Builder{}}); err == nil {
			t.Fatal("expected query error to surface")
		}
	})
}

// --- newUsage ---------------------------------------------------------------

func TestNewUsage(t *testing.T) {
	fs := flag.NewFlagSet("replay-all-as-events", flag.ContinueOnError)
	var buf strings.Builder
	fs.SetOutput(&buf)
	fs.String("aggregate", "", "agg")
	newUsage(fs)()
	out := buf.String()
	for _, want := range []string{
		"replay-all-as-events",
		"Usage:",
		"--aggregate",
		"APP_PROFILE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q in:\n%s", want, out)
		}
	}
}

// --- Run argument-parsing branches (no DB reached) --------------------------

func TestRun_ArgValidation(t *testing.T) {
	t.Run("missing-aggregate", func(t *testing.T) {
		err := Run(context.Background(), []string{})
		if err == nil || !strings.Contains(err.Error(), "--aggregate is required") {
			t.Fatalf("expected aggregate-required error, got %v", err)
		}
	})
	t.Run("bad-batch-size", func(t *testing.T) {
		err := Run(context.Background(), []string{"--aggregate", "users", "--batch-size", "0"})
		if err == nil || !strings.Contains(err.Error(), "--batch-size must be >= 1") {
			t.Fatalf("expected batch-size error, got %v", err)
		}
	})
	t.Run("help-returns-nil", func(t *testing.T) {
		if err := Run(context.Background(), []string{"-h"}); err != nil {
			t.Fatalf("help must return nil, got %v", err)
		}
	})
	t.Run("unknown-flag", func(t *testing.T) {
		if err := Run(context.Background(), []string{"--nope"}); err == nil {
			t.Fatal("expected parse error for unknown flag")
		}
	})
}
