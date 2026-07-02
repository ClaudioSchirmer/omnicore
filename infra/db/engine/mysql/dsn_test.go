//go:build mysql

package mysql

import (
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

func TestEnsureDSNParams(t *testing.T) {
	t.Run("app pool forces parseTime/clientFoundRows and multiStatements OFF", func(t *testing.T) {
		out, err := EnsureDSNParams("user:pass@tcp(localhost:3306)/db")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := gomysql.ParseDSN(out)
		if err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		if cfg.MultiStatements {
			t.Error("multiStatements must be OFF on the application pool")
		}
		if !cfg.ParseTime {
			t.Error("parseTime not forced")
		}
		if !cfg.ClientFoundRows {
			t.Error("clientFoundRows not forced")
		}
	})

	t.Run("app pool forces multiStatements OFF even if operator set it on", func(t *testing.T) {
		out, err := EnsureDSNParams("user:pass@tcp(localhost:3306)/db?multiStatements=true&parseTime=false&clientFoundRows=false")
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := gomysql.ParseDSN(out)
		if cfg.MultiStatements {
			t.Error("operator multiStatements=true must be overridden OFF on the app pool")
		}
		if !cfg.ParseTime || !cfg.ClientFoundRows {
			t.Errorf("conflicting values not overridden: parseTime=%v clientFoundRows=%v", cfg.ParseTime, cfg.ClientFoundRows)
		}
	})

	t.Run("migration variant forces multiStatements ON", func(t *testing.T) {
		out, err := EnsureMigrationDSNParams("user:pass@tcp(localhost:3306)/db?multiStatements=false")
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := gomysql.ParseDSN(out)
		if !cfg.MultiStatements {
			t.Error("migration runner needs multiStatements ON for the flattened framework migration")
		}
		if !cfg.ParseTime || !cfg.ClientFoundRows {
			t.Errorf("migration variant must also force parseTime/clientFoundRows: parseTime=%v clientFoundRows=%v", cfg.ParseTime, cfg.ClientFoundRows)
		}
	})

	t.Run("preserves unrelated params", func(t *testing.T) {
		out, err := EnsureDSNParams("user:pass@tcp(localhost:3306)/db?charset=utf8mb4&loc=UTC")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "charset=utf8mb4") {
			t.Errorf("unrelated param dropped: %q", out)
		}
	})

	t.Run("invalid dsn errors", func(t *testing.T) {
		if _, err := EnsureDSNParams("::::not a dsn"); err == nil {
			t.Fatal("expected parse error")
		}
	})
}
