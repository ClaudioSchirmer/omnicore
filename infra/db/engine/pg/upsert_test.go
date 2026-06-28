package pg

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestPgDialect_BuildUpsert_DoUpdate locks the Postgres upsert shape: an
// ON CONFLICT … DO UPDATE with EXCLUDED.col for the "set to new value" mode and
// a verbatim expression (bare column refers to the existing row) for the rest.
func TestPgDialect_BuildUpsert_DoUpdate(t *testing.T) {
	got := pgDialect{}.BuildUpsert(
		"omnicore_upstream_failures",
		[]string{"topic", "view", "error"},
		[]string{"topic", "view"},
		[]core.UpsertSet{
			{Col: "error", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetExpr, Expr: "attempt + 1"},
		},
	)
	for _, want := range []string{
		"INSERT INTO omnicore_upstream_failures (topic, view, error)",
		"VALUES ($1, $2, $3)",
		"ON CONFLICT (topic, view) DO UPDATE SET",
		"error = EXCLUDED.error",
		"attempt = attempt + 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pg upsert missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "DO NOTHING") {
		t.Errorf("non-empty sets must not render DO NOTHING:\n%s", got)
	}
}

// TestPgDialect_BuildUpsert_DoNothing: empty sets render ON CONFLICT … DO NOTHING.
func TestPgDialect_BuildUpsert_DoNothing(t *testing.T) {
	got := pgDialect{}.BuildUpsert(
		"omnicore_integration_processed",
		[]string{"event_id", "consumer_group"},
		[]string{"event_id", "consumer_group"},
		nil,
	)
	if !strings.Contains(got, "ON CONFLICT (event_id, consumer_group) DO NOTHING") {
		t.Errorf("empty sets must render DO NOTHING, got:\n%s", got)
	}
}
