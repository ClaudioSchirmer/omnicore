//go:build oracle

package migration

import (
	"strings"
	"testing"
)

// TestSplitOracleStatements locks the statement splitter the hand-rolled
// Oracle migrate driver feeds executes through: Oracle's protocol takes one
// statement per call and rejects the trailing semicolon, so the migration
// body is cut on TOP-LEVEL semicolons only — never inside string literals,
// quoted identifiers, or comments.
func TestSplitOracleStatements(t *testing.T) {
	t.Run("plain statements split and lose the terminator", func(t *testing.T) {
		got := splitOracleStatements("CREATE TABLE a (x NUMBER(10));\nCREATE INDEX a_x ON a (x);\n")
		if len(got) != 2 || got[0] != "CREATE TABLE a (x NUMBER(10))" || got[1] != "CREATE INDEX a_x ON a (x)" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("semicolon inside a string literal does not split", func(t *testing.T) {
		got := splitOracleStatements("INSERT INTO t (v) VALUES ('a;b');")
		if len(got) != 1 || got[0] != "INSERT INTO t (v) VALUES ('a;b')" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("escaped quote ('') stays inside the string", func(t *testing.T) {
		got := splitOracleStatements("INSERT INTO t (v) VALUES ('it''s;fine'); SELECT 1 FROM dual;")
		if len(got) != 2 || got[0] != "INSERT INTO t (v) VALUES ('it''s;fine')" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("semicolon in -- and /* */ comments does not split", func(t *testing.T) {
		body := "-- header; with semicolon\nCREATE TABLE a (x NUMBER(10)); /* mid; comment */ CREATE TABLE b (y NUMBER(10));"
		got := splitOracleStatements(body)
		if len(got) != 2 {
			t.Fatalf("got %d statements: %q", len(got), got)
		}
		if !strings.HasPrefix(got[0], "-- header") || !strings.Contains(got[0], "CREATE TABLE a") {
			t.Errorf("statement 0 lost its leading comment or body: %q", got[0])
		}
		if !strings.Contains(got[1], "CREATE TABLE b") {
			t.Errorf("statement 1 = %q", got[1])
		}
	})

	t.Run("quoted identifier with a semicolon does not split", func(t *testing.T) {
		got := splitOracleStatements(`CREATE TABLE "weird;name" (x NUMBER(10));`)
		if len(got) != 1 || got[0] != `CREATE TABLE "weird;name" (x NUMBER(10))` {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("comment-only and whitespace-only segments are dropped", func(t *testing.T) {
		body := "CREATE TABLE a (x NUMBER(10));\n-- trailing banner\n/* and a block */\n   \n"
		got := splitOracleStatements(body)
		if len(got) != 1 {
			t.Fatalf("got %d statements: %q", len(got), got)
		}
	})

	t.Run("the framework migration splits into its full statement set", func(t *testing.T) {
		up, err := frameworkMigrations.ReadFile("embedded/oracle/0001_framework.up.sql")
		if err != nil {
			t.Fatalf("read embedded up: %v", err)
		}
		stmts := splitOracleStatements(string(up))
		// 6 CREATE TABLE + 9 CREATE INDEX (audit_events carries only its ID and
		// the entity-timeline index; the four forensic indexes are devops-added;
		// omnicore_upstream_failures left with the unified failure ledger — its
		// rows live in 0003's omnicore_projection_failures now).
		if len(stmts) != 15 {
			t.Fatalf("framework up split into %d statements, want 15", len(stmts))
		}
		for _, s := range stmts {
			if !strings.Contains(s, "CREATE TABLE") && !strings.Contains(s, "CREATE INDEX") {
				t.Errorf("unexpected statement: %.80q", s)
			}
			if strings.HasSuffix(strings.TrimSpace(s), ";") {
				t.Errorf("statement kept its terminator: %.80q", s)
			}
		}
	})

	t.Run("the framework down migration splits into 6 drops", func(t *testing.T) {
		down, err := frameworkMigrations.ReadFile("embedded/oracle/0001_framework.down.sql")
		if err != nil {
			t.Fatalf("read embedded down: %v", err)
		}
		stmts := splitOracleStatements(string(down))
		if len(stmts) != 6 {
			t.Fatalf("framework down split into %d statements, want 6", len(stmts))
		}
	})
}

// TestOracleDriver_LockName locks the DBMS_LOCK namespace derivation: the
// migration lock is framework-namespaced (ALLOCATE_UNIQUE names are
// database-global) and distinct per tracking table.
func TestOracleDriver_LockName(t *testing.T) {
	fw := (&oracleDriver{table: frameworkTrackingTbl}).lockName()
	svc := (&oracleDriver{table: serviceTrackingTbl}).lockName()
	if !strings.HasPrefix(fw, "omcmig_") || !strings.HasPrefix(svc, "omcmig_") {
		t.Fatalf("lock names must carry the omcmig_ namespace: %q / %q", fw, svc)
	}
	if fw == svc {
		t.Fatal("framework and service migration locks must not collide")
	}
	if len(fw) > 128 || len(svc) > 128 {
		t.Fatalf("lock name exceeds DBMS_LOCK's 128-char limit: %q / %q", fw, svc)
	}
}

// TestNewOracleDriver_RejectsUnsafeTable: the tracking-table name is
// framework-owned, but the identifier gate holds all the same.
func TestNewOracleDriver_RejectsUnsafeTable(t *testing.T) {
	if _, err := newOracleDriver(nil, "bad;name"); err == nil {
		t.Fatal("expected an error for an unsafe tracking-table name")
	}
}
