//go:build integration && mysql

package mysql

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Phase 4 item 4 integration test: opening the engine with tracing ON (the
// otelsql path — the database/sql counterpart of otelpgx) yields a working
// engine. Spans go to the global tracer provider (the OTel no-op when none is
// installed, as here), so a write round-trips without error. Proves the traced
// open path is wired and functional, not merely compiled.
//
//	go test -tags=integration,mysql ./infra/db/mysql/ -run TracingOpen -count=1
func TestMySQLEngine_TracingOpenWorks(t *testing.T) {
	// setup() provisions flat_persons + outbox and yields a (tracing-off) engine
	// it owns; here we open a SECOND engine with tracing ON over the same DB.
	_, _ = setup(t)

	tracedEng, err := New(context.Background(), core.EngineConfig{DSN: dsn(), Tracing: true})
	if err != nil {
		t.Fatalf("New with tracing=true (otelsql) failed: %v", err)
	}
	defer tracedEng.Close()

	ctx := ctxFor()
	person := &flatPerson{Name: "Trace", Email: "trace@otelsql"}
	ins, err := domain.GetInsertable(person, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := tracedEng.Insert(ctx, ins, flatSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert through the traced engine: %v", err)
	}
	if _, err := uuid.Parse(res.ID.Value()); err != nil {
		t.Fatalf("traced Insert returned non-uuid id %q: %v", res.ID, err)
	}
}
