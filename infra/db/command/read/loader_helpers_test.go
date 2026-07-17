package read

import "testing"

// Pure helper tests for the aggregate loader's SQL-tail assembly and identifier
// quoting. They exercise db-internal functions directly, so they live with the
// relational model; the loader's live-query / criteria-compile branches (which
// need an engine) are covered from the pg package.

func TestTailClause_AllParts(t *testing.T) {
	cases := []struct {
		name   string
		clause string
		order  string
		want   string
	}{
		{"empty", "", "", ""},
		{"whereOnly", "WHERE x = $1", "", " WHERE x = $1"},
		{"orderOnly", "", "ORDER BY x", " ORDER BY x"},
		{"all", "WHERE x = $1", "ORDER BY x DESC", " WHERE x = $1 ORDER BY x DESC"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tailClause(c.clause, c.order); got != c.want {
				t.Errorf("tailClause = %q, want %q", got, c.want)
			}
		})
	}
}

// The row cap left tailClause on purpose: the caller applies it over the
// COMPLETE statement via Dialect.ApplyLimit, so an engine whose cap is not a
// tail clause can rewrite the statement.
func TestTailClause_CapViaDialectApplyLimit(t *testing.T) {
	sql := "SELECT * FROM t" + tailClause("WHERE x = $1", "ORDER BY x DESC")
	if got := (testPGDialect{}).ApplyLimit(sql, 10); got != "SELECT * FROM t WHERE x = $1 ORDER BY x DESC LIMIT 10" {
		t.Errorf("ApplyLimit = %q", got)
	}
}

// applyWindow renders the row cap (offset-free) or the windowed page (offset > 0)
// over the COMPLETE statement, delegating to the dialect. The offset contract —
// a positive Limit AND an ORDER BY — is enforced here, not silently dropped.
func TestApplyWindow(t *testing.T) {
	d := testPGDialect{}
	base := "SELECT * FROM t WHERE x = $1"
	ordered := base + " ORDER BY id ASC"

	// No offset, no limit → statement unchanged (the unordered default listing).
	if got, err := applyWindow(d, base, 0, 0, ""); err != nil || got != base {
		t.Fatalf("no window: got %q err %v", got, err)
	}
	// Limit only → plain cap, no ORDER BY required.
	if got, err := applyWindow(d, base, 10, 0, ""); err != nil || got != base+" LIMIT 10" {
		t.Fatalf("limit only: got %q err %v", got, err)
	}
	// Limit + offset over an ordered statement → windowed page.
	if got, err := applyWindow(d, ordered, 10, 20, "ORDER BY id ASC"); err != nil || got != ordered+" LIMIT 10 OFFSET 20" {
		t.Fatalf("window: got %q err %v", got, err)
	}
}

func TestApplyWindow_OffsetRequiresPositiveLimit(t *testing.T) {
	if _, err := applyWindow(testPGDialect{}, "SELECT * FROM t ORDER BY id ASC", 0, 20, "ORDER BY id ASC"); err == nil {
		t.Fatal("expected an error: an offset without a positive limit is an open-ended skip")
	}
}

func TestApplyWindow_OffsetRequiresOrder(t *testing.T) {
	if _, err := applyWindow(testPGDialect{}, "SELECT * FROM t", 10, 20, ""); err == nil {
		t.Fatal("expected an error: an offset without an ORDER BY is non-deterministic")
	}
}

func TestQuoteIdentifiers(t *testing.T) {
	got := quoteIdentifiers([]string{"id", "name", "deleted_at"}, testPGDialect{})
	want := []string{"id", "name", "deleted_at"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestQuoteIdentifiers_PanicsOnBadIdentifier(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on invalid identifier")
		}
	}()
	quoteIdentifiers([]string{"bad; DROP TABLE"}, testPGDialect{})
}
