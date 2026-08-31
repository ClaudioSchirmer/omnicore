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
// to new value" mode and the target-alias-qualified column for the bump mode
// (the table is aliased, so the alias is the only valid qualifier), WITHOUT a
// statement terminator (the driver rejects a trailing semicolon on plain SQL).
func TestOracleDialect_BuildUpsert_DoUpdate(t *testing.T) {
	got := oracleDialect{}.BuildUpsert(
		"omnicore_integration_failures",
		[]string{"consumer_group", "event_id", "error"},
		[]string{"consumer_group", "event_id"},
		[]core.UpsertSet{
			{Col: "error", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetBump},
		},
	)
	for _, want := range []string{
		`MERGE INTO "OMNICORE_INTEGRATION_FAILURES" target`,
		`USING (SELECT :1 AS "CONSUMER_GROUP", :2 AS "EVENT_ID", :3 AS "ERROR" FROM dual) source`,
		// NULL-safe ON: '' binds as NULL on Oracle, so a plain `=` would never
		// match an empty-string conflict column and every retry would die on
		// ORA-00001 instead of incrementing the attempt.
		`ON ((target."CONSUMER_GROUP" = source."CONSUMER_GROUP" OR (target."CONSUMER_GROUP" IS NULL AND source."CONSUMER_GROUP" IS NULL)) AND (target."EVENT_ID" = source."EVENT_ID" OR (target."EVENT_ID" IS NULL AND source."EVENT_ID" IS NULL)))`,
		`WHEN MATCHED THEN UPDATE SET "ERROR" = source."ERROR", "ATTEMPT" = target."ATTEMPT" + 1`,
		`WHEN NOT MATCHED THEN INSERT ("CONSUMER_GROUP", "EVENT_ID", "ERROR") VALUES (source."CONSUMER_GROUP", source."EVENT_ID", source."ERROR")`,
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
	if !strings.Contains(got, `WHEN NOT MATCHED THEN INSERT ("EVENT_ID", "CONSUMER_GROUP") VALUES (source."EVENT_ID", source."CONSUMER_GROUP")`) {
		t.Errorf("do-nothing upsert must still insert on miss, got:\n%s", got)
	}
	if !strings.Contains(got, "FROM dual) source ON (") {
		t.Errorf("MERGE source must select FROM dual with a parenthesized ON, got:\n%s", got)
	}
}

// A conflict-ONLY assignment (write.OnUpdate) binds a placeholder of its own,
// numbered after the source columns: WHEN MATCHED is rendered after the USING
// SELECT, so the arguments appended behind the insert ones line up.
func TestOracleDialect_BuildUpsert_ArgContinuesTheNumbering(t *testing.T) {
	got := oracleDialect{}.BuildUpsert(
		"authentication_attempts",
		[]string{"id", "identity", "last_ip"},
		[]string{"identity"},
		[]core.UpsertSet{
			{Col: "last_ip", Mode: core.UpsertSetNew},
			{Col: "repeated_at", Mode: core.UpsertSetArg},
		},
	)
	if !strings.Contains(got, `WHEN MATCHED THEN UPDATE SET "LAST_IP" = source."LAST_IP", "REPEATED_AT" = :4`) {
		t.Errorf("oracle upsert missing the conflict-only placeholder in:\n%s", got)
	}
}
