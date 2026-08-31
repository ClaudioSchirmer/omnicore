//go:build sqlserver

package sqlserver

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestSQLServerDialect_BuildUpsert_DoUpdate locks the SQL Server upsert shape:
// a single MERGE … WITH (HOLDLOCK) statement (HOLDLOCK closes the
// match-then-insert race ON CONFLICT/ON DUPLICATE KEY close natively on the
// other engines), source.col for the "set to new value" mode and the
// target-alias-qualified column for the bump mode (the table is aliased, so
// the alias is the only valid qualifier), terminated with the semicolon MERGE
// requires.
func TestSQLServerDialect_BuildUpsert_DoUpdate(t *testing.T) {
	got := sqlserverDialect{}.BuildUpsert(
		"omnicore_integration_failures",
		[]string{"consumer_group", "event_id", "error"},
		[]string{"consumer_group", "event_id"},
		[]core.UpsertSet{
			{Col: "error", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetBump},
		},
	)
	for _, want := range []string{
		"MERGE [omnicore_integration_failures] WITH (HOLDLOCK) AS target",
		"USING (SELECT @p1 AS [consumer_group], @p2 AS [event_id], @p3 AS [error]) AS source",
		"ON target.[consumer_group] = source.[consumer_group] AND target.[event_id] = source.[event_id]",
		"WHEN MATCHED THEN UPDATE SET [error] = source.[error], [attempt] = target.[attempt] + 1",
		"WHEN NOT MATCHED THEN INSERT ([consumer_group], [event_id], [error]) VALUES (source.[consumer_group], source.[event_id], source.[error]);",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sqlserver upsert missing %q in:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"ON CONFLICT", "ON DUPLICATE KEY"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("sqlserver upsert must not render %s:\n%s", forbidden, got)
		}
	}
	if !strings.HasSuffix(got, ";") {
		t.Errorf("MERGE requires the statement terminator, got:\n%s", got)
	}
}

// TestSQLServerDialect_BuildUpsert_DoNothing: empty sets omit the WHEN MATCHED
// clause entirely — a true do-nothing on conflict (the MERGE-native equivalent
// of PG's DO NOTHING; no no-op assignment is needed, unlike MySQL).
func TestSQLServerDialect_BuildUpsert_DoNothing(t *testing.T) {
	got := sqlserverDialect{}.BuildUpsert(
		"omnicore_integration_processed",
		[]string{"event_id", "consumer_group"},
		[]string{"event_id", "consumer_group"},
		nil,
	)
	if strings.Contains(got, "WHEN MATCHED") {
		t.Errorf("empty sets must omit WHEN MATCHED, got:\n%s", got)
	}
	if !strings.Contains(got, "WHEN NOT MATCHED THEN INSERT ([event_id], [consumer_group]) VALUES (source.[event_id], source.[consumer_group]);") {
		t.Errorf("do-nothing upsert must still insert on miss, got:\n%s", got)
	}
	if !strings.Contains(got, "WITH (HOLDLOCK)") {
		t.Errorf("HOLDLOCK is mandatory on every MERGE upsert, got:\n%s", got)
	}
}

// A conflict-ONLY assignment (write.OnUpdate) binds a placeholder of its own,
// numbered after the source columns: WHEN MATCHED is rendered after the USING
// SELECT, so the arguments appended behind the insert ones line up.
func TestSQLServerDialect_BuildUpsert_ArgContinuesTheNumbering(t *testing.T) {
	got := sqlserverDialect{}.BuildUpsert(
		"authentication_attempts",
		[]string{"id", "identity", "last_ip"},
		[]string{"identity"},
		[]core.UpsertSet{
			{Col: "last_ip", Mode: core.UpsertSetNew},
			{Col: "repeated_at", Mode: core.UpsertSetArg},
		},
	)
	if !strings.Contains(got, "WHEN MATCHED THEN UPDATE SET [last_ip] = source.[last_ip], [repeated_at] = @p4") {
		t.Errorf("sqlserver upsert missing the conflict-only placeholder in:\n%s", got)
	}
}
