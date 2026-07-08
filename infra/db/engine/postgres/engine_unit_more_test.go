//go:build postgres

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// Unit-reachable engine surface: the boot-chaining wrappers and the
// NewPostgres construction errors (bad DSN, unreachable server). The live
// pool/lock/read paths remain the integration suites' contract.

func TestPostgres_ConfigChaining(t *testing.T) {
	p := &Postgres{}
	if got := p.WithAudit(nil, nil, nil); got != core.RelationalEngine(p) {
		t.Error("WithAudit must return the receiver for chaining")
	}
	if got := p.WithEventPublisher(events.NewSlogPublisher(nil)); got != core.RelationalEngine(p) {
		t.Error("WithEventPublisher must return the receiver for chaining")
	}
}

func TestNewPostgres_ParseConfigError(t *testing.T) {
	if _, err := NewPostgres(context.Background(), "://not-a-dsn"); err == nil {
		t.Fatal("expected the DSN parse error")
	}
}

func TestNewPostgres_PingFailureClosesPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Unroutable port; the options exercise the tracer + pool knobs.
	_, err := NewPostgres(ctx,
		"postgres://u:p@127.0.0.1:1/db?connect_timeout=1",
		WithPgxTracing(true),
		WithPool(core.PoolConfig{MaxOpenConns: 2, ConnMaxLifetime: time.Minute}),
	)
	if err == nil {
		t.Fatal("expected the ping failure")
	}
	if strings.Contains(err.Error(), "parse") {
		t.Fatalf("must fail at ping, not parse: %v", err)
	}
}
