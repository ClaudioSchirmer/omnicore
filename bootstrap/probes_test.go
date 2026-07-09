package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// probeRow is a core.Row whose Scan returns a configured error, letting the
// readiness test drive the relational leg without a live backend.
type probeRow struct{ err error }

func (r probeRow) Scan(dest ...any) error { return r.err }

// probeQuerier is a core.Querier whose QueryRow returns a probeRow. Only
// QueryRow is exercised by readiness.check; the rest satisfy the interface.
type probeQuerier struct{ scanErr error }

func (q probeQuerier) Query(context.Context, string, ...any) (core.Rows, error) { return nil, nil }
func (q probeQuerier) QueryRow(context.Context, string, ...any) core.Row {
	return probeRow{err: q.scanErr}
}
func (q probeQuerier) Exec(context.Context, string, ...any) error { return nil }
func (q probeQuerier) QueryMaps(context.Context, string, ...any) ([]map[string]any, error) {
	return nil, nil
}

// probeEngine embeds the minimal boot fake and overrides Querier so the
// readiness relational ping (SELECT 1 → Scan) can be made to succeed or fail.
type probeEngine struct {
	bootFakeEngine
	scanErr error
}

func (e probeEngine) Querier() core.Querier { return probeQuerier{scanErr: e.scanErr} }

func TestReadinessCheck_Draining(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGTERM analogue: the shutdown context is done
	r := &readiness{shutdown: ctx}
	err := r.check(context.Background())
	if err == nil || err.Error() != "draining" {
		t.Fatalf("draining check = %v, want \"draining\"", err)
	}
}

func TestReadinessCheck_RelationalDown(t *testing.T) {
	r := &readiness{shutdown: context.Background(), db: probeEngine{scanErr: errors.New("conn refused")}}
	err := r.check(context.Background())
	if err == nil {
		t.Fatal("expected a relational failure, got nil")
	}
	if got := err.Error(); got[:len("relational:")] != "relational:" {
		t.Fatalf("error = %q, want it to name the relational leg", got)
	}
}

func TestReadinessCheck_Ready(t *testing.T) {
	// A live relational leg (Scan succeeds) and no Mongo configured → ready.
	r := &readiness{shutdown: context.Background(), db: probeEngine{scanErr: nil}}
	if err := r.check(context.Background()); err != nil {
		t.Fatalf("ready check = %v, want nil", err)
	}
}

func TestReadinessCheck_NilShutdownNeverDrains(t *testing.T) {
	// A nil shutdown context (the buildApp test path) must not panic and must
	// not report draining.
	r := &readiness{db: probeEngine{scanErr: nil}}
	if err := r.check(context.Background()); err != nil {
		t.Fatalf("nil-shutdown check = %v, want nil", err)
	}
}

// --- HTTP surface via buildApp ---

func TestBuildApp_ReadyzReadyWhenNotDraining(t *testing.T) {
	app, err := buildApp(context.Background(), silentDeps(), Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/readyz", nil))
	if err != nil {
		t.Fatalf("Test /readyz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/readyz = %d, want 200", resp.StatusCode)
	}
	var body healthResponse
	if raw, _ := io.ReadAll(resp.Body); json.Unmarshal(raw, &body) != nil || body.Status != "ready" {
		t.Fatalf("/readyz body status = %q, want \"ready\"", body.Status)
	}
}

func TestBuildApp_ReadyzUnavailableWhenDraining(t *testing.T) {
	// A cancelled shutdown context is the SIGTERM analogue: /readyz must flip to
	// 503 so Kubernetes pulls the pod from the load balancer during the drain.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app, err := buildApp(ctx, silentDeps(), Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/readyz", nil))
	if err != nil {
		t.Fatalf("Test /readyz: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("draining /readyz = %d, want 503", resp.StatusCode)
	}
	var body healthResponse
	if raw, _ := io.ReadAll(resp.Body); json.Unmarshal(raw, &body) != nil || body.Reason != "draining" {
		t.Fatalf("/readyz draining reason = %q, want \"draining\"", body.Reason)
	}
}
