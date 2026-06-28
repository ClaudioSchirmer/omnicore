//go:build mysql

package mysql

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// On an in-TX hook failure the MySQL engine must emit the best-effort
// persistence.hook.error observability line (verb / hookSlot / entityType /
// threadId / error) and return the error verbatim — the mirror of the Postgres
// path and CLAUDE.md invariant #12. The hook fires before any *sql.Tx use, so a
// nil tx is harmless here (the test hook ignores it).
func TestFireHook_LogsAndPropagatesError(t *testing.T) {
	cases := []struct {
		name string
		slot string
		fire func(e *Engine, ctx persistence.RequestContext, hook db.WriteHook, hctx db.HookContext) error
	}{
		{
			name: "afterBegin",
			slot: "afterBegin",
			fire: func(e *Engine, ctx persistence.RequestContext, hook db.WriteHook, hctx db.HookContext) error {
				return e.FireAfterBegin(ctx, mysqlTx{}, nil, hook, hctx)
			},
		},
		{
			name: "beforeCommit",
			slot: "beforeCommit",
			fire: func(e *Engine, ctx persistence.RequestContext, hook db.WriteHook, hctx db.HookContext) error {
				return e.FireBeforeCommit(ctx, mysqlTx{}, nil, domain.NewID("00000000-0000-0000-0000-000000000001"), hook, hctx)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := &Engine{}
			e.ConfigureAudit(nil, slog.New(slog.NewJSONHandler(&buf, nil)), nil)
			ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
			hctx := db.HookContext{Verb: "Update", EntityType: "User"}
			boom := errors.New("hook boom")

			hook := db.WriteHook{
				AfterBegin: func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error {
					return boom
				},
				BeforeCommit: func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
					return boom
				},
			}

			err := tc.fire(e, ctx, hook, hctx)
			if !errors.Is(err, boom) {
				t.Fatalf("fire returned %v, want the hook error verbatim", err)
			}

			var rec map[string]any
			if e := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); e != nil {
				t.Fatalf("log not valid JSON: %v (%q)", e, buf.String())
			}
			if rec["msg"] != "persistence.hook.error" {
				t.Errorf("msg = %v, want persistence.hook.error", rec["msg"])
			}
			if rec["verb"] != "Update" || rec["entityType"] != "User" || rec["hookSlot"] != tc.slot {
				t.Errorf("fields drifted: verb=%v entityType=%v hookSlot=%v", rec["verb"], rec["entityType"], rec["hookSlot"])
			}
			if rec["threadId"] != ctx.ID().String() {
				t.Errorf("threadId = %v, want %v", rec["threadId"], ctx.ID().String())
			}
			if rec["error"] != "hook boom" {
				t.Errorf("error = %v, want \"hook boom\"", rec["error"])
			}
		})
	}
}

// A nil hook fires nothing and emits no log line (the common, no-hook path).
func TestFireHook_NilHookIsSilent(t *testing.T) {
	var buf bytes.Buffer
	e := &Engine{}
	e.ConfigureAudit(nil, slog.New(slog.NewJSONHandler(&buf, nil)), nil)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	hctx := db.HookContext{Verb: "Insert", EntityType: "User"}

	if err := e.FireAfterBegin(ctx, mysqlTx{}, nil, db.WriteHook{}, hctx); err != nil {
		t.Fatalf("nil AfterBegin should be a no-op, got %v", err)
	}
	if err := e.FireBeforeCommit(ctx, mysqlTx{}, nil, domain.NewID("x"), db.WriteHook{}, hctx); err != nil {
		t.Fatalf("nil BeforeCommit should be a no-op, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("no hook configured must emit no log, got %q", buf.String())
	}
}
