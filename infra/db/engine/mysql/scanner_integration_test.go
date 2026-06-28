//go:build integration && mysql

package mysql

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// Phase 4 item 3 integration test: a MANUAL root scanner runs on MySQL. Manual
// scanners used to be Postgres-only (handed pgx rows); now they receive the
// backend-neutral db.Row and the loader runs them through the engine's
// Querier, so they work on any engine — the consumer owns the dialect-specific
// column decoding (here, BINARY(16) → uuid string).
//
//	go test -tags=integration,mysql ./infra/db/mysql/ -run ManualRootScanner -count=1

type scanProbe struct {
	domain.BaseEntity
	Label string
}

func (*scanProbe) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeDisplay} }
func (*scanProbe) BuildRules(string, domain.Service, *domain.Rules) {}

func scanProbeSchema() *db.TableSchema {
	return db.NewTableSchema[*scanProbe]("scan_probe").
		PK("id").
		Field("Label", "label")
}

func TestMySQLLoader_ManualRootScanner(t *testing.T) {
	eng, raw := setup(t)
	ctx := context.Background()

	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS scan_probe`); err != nil {
		t.Fatalf("drop scan_probe: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE scan_probe (
		id    BINARY(16)   PRIMARY KEY,
		label VARCHAR(100) NOT NULL
	)`); err != nil {
		t.Fatalf("create scan_probe: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS scan_probe`) })

	id := uuid.New()
	if _, err := raw.ExecContext(ctx, `INSERT INTO scan_probe (id, label) VALUES (?, ?)`, id[:], "manual"); err != nil {
		t.Fatalf("insert probe: %v", err)
	}

	// Manual root scanner: receives db.Row, decodes the BINARY(16) id to a
	// uuid string itself, and sets it (FindOne requires the scanner to populate
	// the id). This is the exact shape that used to be impossible on MySQL.
	loader := db.NewAggregateLoader[*scanProbe](eng, func() *scanProbe { return &scanProbe{} }).
		WithSchema(scanProbeSchema()).
		WithRootScanner(func(row db.Row) (*scanProbe, error) {
			var idBytes []byte
			var label string
			if err := row.Scan(&idBytes, &label); err != nil {
				return nil, err
			}
			u, err := uuid.FromBytes(idBytes)
			if err != nil {
				return nil, err
			}
			p := &scanProbe{Label: label}
			p.SetID(domain.NewID(u.String()))
			return p, nil
		})

	got, err := loader.FindOne(ctx, criteria.Where(criteria.Eq("Label", "manual")))
	if err != nil {
		t.Fatalf("FindOne with manual root scanner: %v", err)
	}
	if got.GetID() == nil || got.GetID().Value() != id.String() {
		t.Fatalf("manual scanner id = %v, want %s", got.GetID(), id.String())
	}
	if got.Label != "manual" {
		t.Fatalf("manual scanner label = %q, want manual", got.Label)
	}
}
