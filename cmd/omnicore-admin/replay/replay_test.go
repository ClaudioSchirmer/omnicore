package replay

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// --- quoteIdent -------------------------------------------------------------

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("users"); got != `"users"` {
		t.Errorf("quoteIdent(users) = %q, want %q", got, `"users"`)
	}
	if got := quoteIdent("order"); got != `"order"` {
		t.Errorf("quoteIdent(order) = %q, want %q", got, `"order"`)
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

// --- scanRowsToMaps ---------------------------------------------------------

// fakeRows is a minimal pgx.Rows over positional column values, exercising the
// FieldDescriptions / Next / Values / Err / Close surface scanRowsToMaps uses.
type fakeRows struct {
	cols      []string
	data      [][]any
	idx       int
	valuesErr error
	errAfter  error
	closed    bool
}

func (r *fakeRows) Close()                        { r.closed = true }
func (r *fakeRows) Err() error                    { return r.errAfter }
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription {
	out := make([]pgconn.FieldDescription, len(r.cols))
	for i, c := range r.cols {
		out[i] = pgconn.FieldDescription{Name: c}
	}
	return out
}
func (r *fakeRows) RawValues() [][]byte { return nil }
func (r *fakeRows) Conn() *pgx.Conn     { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	return errors.New("scanRowsToMaps must use Values(), not Scan()")
}
func (r *fakeRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}
func (r *fakeRows) Values() ([]any, error) {
	if r.valuesErr != nil {
		return nil, r.valuesErr
	}
	return r.data[r.idx-1], nil
}

func TestScanRowsToMaps_Success(t *testing.T) {
	id := uuid.New()
	// pgx hands UUID columns back as a raw [16]byte; NormalizeSQLValue
	// canonicalizes that shape (a uuid.UUID named value would pass through).
	raw := [16]byte(id)
	rows := &fakeRows{
		cols: []string{"id", "name"},
		data: [][]any{
			{raw, "Alice"},
			{"plain-id", "Bob"},
		},
	}
	out, err := scanRowsToMaps(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 maps, got %d", len(out))
	}
	// UUID column is normalized to its canonical string form.
	if out[0]["id"] != id.String() {
		t.Errorf("uuid not normalized: got %v want %v", out[0]["id"], id.String())
	}
	if out[0]["name"] != "Alice" {
		t.Errorf("name drifted: %v", out[0]["name"])
	}
	if out[1]["id"] != "plain-id" || out[1]["name"] != "Bob" {
		t.Errorf("row 2 drifted: %+v", out[1])
	}
	if !rows.closed {
		t.Error("scanRowsToMaps must Close() the rows")
	}
}

func TestScanRowsToMaps_Empty(t *testing.T) {
	rows := &fakeRows{cols: []string{"id"}, data: nil}
	out, err := scanRowsToMaps(rows)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil slice for no rows, got %v", out)
	}
}

func TestScanRowsToMaps_ValuesError(t *testing.T) {
	rows := &fakeRows{
		cols:      []string{"id"},
		data:      [][]any{{"x"}},
		valuesErr: errors.New("values boom"),
	}
	if _, err := scanRowsToMaps(rows); err == nil {
		t.Fatal("expected Values() error to surface")
	}
}

func TestScanRowsToMaps_RowsErr(t *testing.T) {
	rows := &fakeRows{
		cols:     []string{"id"},
		data:     [][]any{{"x"}},
		errAfter: errors.New("late rows err"),
	}
	if _, err := scanRowsToMaps(rows); err == nil {
		t.Fatal("expected rows.Err() to surface")
	}
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
		err := Run(nil, []string{})
		if err == nil || !strings.Contains(err.Error(), "--aggregate is required") {
			t.Fatalf("expected aggregate-required error, got %v", err)
		}
	})
	t.Run("bad-batch-size", func(t *testing.T) {
		err := Run(nil, []string{"--aggregate", "users", "--batch-size", "0"})
		if err == nil || !strings.Contains(err.Error(), "--batch-size must be >= 1") {
			t.Fatalf("expected batch-size error, got %v", err)
		}
	})
	t.Run("help-returns-nil", func(t *testing.T) {
		if err := Run(nil, []string{"-h"}); err != nil {
			t.Fatalf("help must return nil, got %v", err)
		}
	})
	t.Run("unknown-flag", func(t *testing.T) {
		if err := Run(nil, []string{"--nope"}); err == nil {
			t.Fatal("expected parse error for unknown flag")
		}
	})
}
