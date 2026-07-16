//go:build oracle

package oracle

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestOracleDialect_BuildUpsert_DoUpdate locks the Oracle upsert shape: a
// single MERGE INTO … USING (SELECT … FROM dual) statement (Oracle has no
// HOLDLOCK equivalent — the concurrent-MERGE ORA-00001 is classified as a
// unique violation instead; see tasks/oracle.md D2), source.col for the "set
// to new value" mode and a verbatim expression (bare column = target row,
// identical semantics to the other dialects) for the rest, WITHOUT a statement
// terminator (the driver rejects a trailing semicolon on plain SQL).
func TestOracleDialect_BuildUpsert_DoUpdate(t *testing.T) {
	got := oracleDialect{}.BuildUpsert(
		"omnicore_integration_failures",
		[]string{"consumer_group", "event_id", "error"},
		[]string{"consumer_group", "event_id"},
		[]core.UpsertSet{
			{Col: "error", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetExpr, Expr: "attempt + 1"},
		},
	)
	for _, want := range []string{
		"MERGE INTO omnicore_integration_failures target",
		"USING (SELECT :1 AS consumer_group, :2 AS event_id, :3 AS error FROM dual) source",
		"ON (target.consumer_group = source.consumer_group AND target.event_id = source.event_id)",
		"WHEN MATCHED THEN UPDATE SET error = source.error, attempt = attempt + 1",
		"WHEN NOT MATCHED THEN INSERT (consumer_group, event_id, error) VALUES (source.consumer_group, source.event_id, source.error)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("oracle upsert missing %q in:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"ON CONFLICT", "ON DUPLICATE KEY", "HOLDLOCK"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("oracle upsert must not render %s:\n%s", forbidden, got)
		}
	}
	if strings.HasSuffix(got, ";") {
		t.Errorf("the driver rejects a trailing semicolon on plain SQL, got:\n%s", got)
	}
}

// TestOracleDialect_BuildUpsert_DoNothing: empty sets omit the WHEN MATCHED
// clause entirely — a true do-nothing on conflict (the MERGE-native equivalent
// of PG's DO NOTHING; no no-op assignment is needed, unlike MySQL).
func TestOracleDialect_BuildUpsert_DoNothing(t *testing.T) {
	got := oracleDialect{}.BuildUpsert(
		"omnicore_integration_processed",
		[]string{"event_id", "consumer_group"},
		[]string{"event_id", "consumer_group"},
		nil,
	)
	if strings.Contains(got, "WHEN MATCHED") {
		t.Errorf("empty sets must omit WHEN MATCHED, got:\n%s", got)
	}
	if !strings.Contains(got, "WHEN NOT MATCHED THEN INSERT (event_id, consumer_group) VALUES (source.event_id, source.consumer_group)") {
		t.Errorf("do-nothing upsert must still insert on miss, got:\n%s", got)
	}
	if !strings.Contains(got, "FROM dual) source ON (") {
		t.Errorf("MERGE source must select FROM dual with a parenthesized ON, got:\n%s", got)
	}
}
