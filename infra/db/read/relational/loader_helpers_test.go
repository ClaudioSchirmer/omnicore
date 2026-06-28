package relational

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
		limit  int64
		want   string
	}{
		{"empty", "", "", 0, ""},
		{"whereOnly", "WHERE x = $1", "", 0, " WHERE x = $1"},
		{"orderOnly", "", "ORDER BY x", 0, " ORDER BY x"},
		{"limitOnly", "", "", 5, " LIMIT 5"},
		{"all", "WHERE x = $1", "ORDER BY x DESC", 10, " WHERE x = $1 ORDER BY x DESC LIMIT 10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tailClause(c.clause, c.order, c.limit); got != c.want {
				t.Errorf("tailClause = %q, want %q", got, c.want)
			}
		})
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
