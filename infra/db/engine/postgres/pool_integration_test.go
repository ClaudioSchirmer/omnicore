//go:build integration && postgres

package postgres

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestNewPostgres_AppliesPool proves EngineConfig.Pool reaches the live pgxpool:
// an explicit MaxOpenConns maps to pgxpool's MaxConns (which otherwise defaults to
// max(4, NumCPU)).
func TestNewPostgres_AppliesPool(t *testing.T) {
	ctx := context.Background()
	eng, err := NewPostgres(ctx, pgAdminDSN(), WithPool(core.PoolConfig{MaxOpenConns: 6}))
	if err != nil {
		t.Skipf("Postgres not reachable (%v) — start devops/docker-compose.yml", err)
	}
	defer eng.Close()
	if got := eng.Pool().Config().MaxConns; got != 6 {
		t.Fatalf("pgxpool MaxConns = %d, want 6 (pool config not applied)", got)
	}
}
