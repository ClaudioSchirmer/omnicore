//go:build mysql

package mysql

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestMySQLDialect_BuildUpsert_DoUpdate locks the MySQL upsert shape: a row
// alias (AS new) + ON DUPLICATE KEY UPDATE with new.col for the "set to new
// value" mode and the TABLE-qualified column for the bump mode (with the row
// alias present, a bare right-hand-side column is ambiguous — errno 1052).
// The conflict columns are NOT named — MySQL keys off the existing unique index.
func TestMySQLDialect_BuildUpsert_DoUpdate(t *testing.T) {
	got := mysqlDialect{}.BuildUpsert(
		"omnicore_integration_failures",
		[]string{"consumer_group", "event_id", "error"},
		[]string{"consumer_group", "event_id"},
		[]core.UpsertSet{
			{Col: "error", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetBump},
		},
	)
	for _, want := range []string{
		"INSERT INTO `omnicore_integration_failures` (`consumer_group`, `event_id`, `error`)",
		"VALUES (?, ?, ?)",
		"AS new ON DUPLICATE KEY UPDATE",
		"`error` = new.`error`",
		"`attempt` = `omnicore_integration_failures`.`attempt` + 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mysql upsert missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ON CONFLICT") {
		t.Errorf("mysql upsert must not render ON CONFLICT:\n%s", got)
	}
}

// TestMySQLDialect_BuildUpsert_DoNothing: empty sets render a no-op assignment on
// the first conflict column (the precise equivalent of PG's DO NOTHING — never
// INSERT IGNORE, which would swallow unrelated errors).
func TestMySQLDialect_BuildUpsert_DoNothing(t *testing.T) {
	got := mysqlDialect{}.BuildUpsert(
		"omnicore_integration_processed",
		[]string{"event_id", "consumer_group"},
		[]string{"event_id", "consumer_group"},
		nil,
	)
	// No-op via new.<col> (a bare `col = col` is ambiguous under the AS new
	// alias — MySQL 8.4 errno 1052); new.<col> is unambiguous AND a true no-op
	// because the conflicting row matched on that key.
	if !strings.Contains(got, "ON DUPLICATE KEY UPDATE `event_id` = new.`event_id`") {
		t.Errorf("empty sets must render a no-op update, got:\n%s", got)
	}
	if strings.Contains(got, "INSERT IGNORE") {
		t.Errorf("must not use INSERT IGNORE:\n%s", got)
	}
}

// A conflict-ONLY assignment (write.OnUpdate) binds a placeholder of its own.
// MySQL's are positional and untyped, so the ordering is all that matters — the
// clause follows the VALUES list, exactly where the caller appends its args.
func TestMySQLDialect_BuildUpsert_ArgBindsItsOwnPlaceholder(t *testing.T) {
	got := mysqlDialect{}.BuildUpsert(
		"authentication_attempts",
		[]string{"id", "identity", "last_ip"},
		[]string{"identity"},
		[]core.UpsertSet{
			{Col: "last_ip", Mode: core.UpsertSetNew},
			{Col: "repeated_at", Mode: core.UpsertSetArg},
		},
	)
	if !strings.Contains(got, "`repeated_at` = ?") {
		t.Errorf("mysql upsert missing the conflict-only placeholder in:\n%s", got)
	}
}
