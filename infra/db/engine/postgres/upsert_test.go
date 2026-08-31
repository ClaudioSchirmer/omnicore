//go:build postgres

package postgres

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestPgDialect_BuildUpsert_DoUpdate locks the Postgres upsert shape: an
// ON CONFLICT … DO UPDATE with EXCLUDED.col for the "set to new value" mode and
// the TABLE-qualified column for the bump mode — inside DO UPDATE a bare column
// is ambiguous against EXCLUDED and fails with SQLSTATE 42702, which is exactly
// how the failure ledgers' park INSERT was broken on Postgres until the
// projection_resilience suite executed one for real.
func TestPgDialect_BuildUpsert_DoUpdate(t *testing.T) {
	got := pgDialect{}.BuildUpsert(
		"omnicore_upstream_failures",
		[]string{"topic", "view", "error"},
		[]string{"topic", "view"},
		[]core.UpsertSet{
			{Col: "error", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetBump},
		},
	)
	for _, want := range []string{
		"INSERT INTO omnicore_upstream_failures (topic, view, error)",
		"VALUES ($1, $2, $3)",
		"ON CONFLICT (topic, view) DO UPDATE SET",
		"error = EXCLUDED.error",
		"attempt = omnicore_upstream_failures.attempt + 1",
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

// A conflict-ONLY assignment (write.OnUpdate) binds a placeholder of its own,
// numbered after the inserted columns: DO UPDATE is rendered after the VALUES
// list, so the arguments the caller appends behind the insert ones line up.
func TestPgDialect_BuildUpsert_ArgContinuesTheNumbering(t *testing.T) {
	got := pgDialect{}.BuildUpsert(
		"authentication_attempts",
		[]string{"id", "identity", "last_ip"},
		[]string{"identity"},
		[]core.UpsertSet{
			{Col: "last_ip", Mode: core.UpsertSetNew},
			{Col: "repeated_at", Mode: core.UpsertSetArg},
			{Col: "repeated_by", Mode: core.UpsertSetArg},
		},
	)
	for _, want := range []string{"repeated_at = $4", "repeated_by = $5"} {
		if !strings.Contains(got, want) {
			t.Errorf("pg upsert missing %q in:\n%s", want, got)
		}
	}
}
