//go:build postgres

package postgres

import (
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TestWithPool_SetsOption asserts WithPool records the pool config on the option
// struct NewPostgres reads. The actual pgxpool.Config mapping (MaxConns /
// MaxConnLifetime) needs a live pool and is exercised by the integration suite.
func TestWithPool_SetsOption(t *testing.T) {
	var o postgresOptions
	WithPool(core.PoolConfig{MaxOpenConns: 9, MaxIdleConns: 3, ConnMaxLifetime: time.Minute})(&o)

	if o.pool.MaxOpenConns != 9 {
		t.Errorf("MaxOpenConns = %d, want 9", o.pool.MaxOpenConns)
	}
	if o.pool.ConnMaxLifetime != time.Minute {
		t.Errorf("ConnMaxLifetime = %s, want 1m", o.pool.ConnMaxLifetime)
	}
}
