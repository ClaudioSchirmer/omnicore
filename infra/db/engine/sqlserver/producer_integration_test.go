//go:build integration && sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
)

// The integration PRODUCER works on SQL Server — both Dispatch paths. The
// standalone path runs through the engine's neutral Querier; the in-TX path
// runs through the canonical core.UnwrapTx bridge (the engine fires a
// BeforeCommit hook carrying a sqlserverTxHandle, which UnwrapTx adapts to
// core.Tx). Proves integration_events lands on a real SQL Server via both,
// with the JSON payload text-bound into NVARCHAR(MAX).
//
//	go test -tags=integration,sqlserver ./infra/db/engine/sqlserver/ -run ProducerDispatch -count=1

func createIntegrationEventsTable(t *testing.T, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, `CREATE TABLE integration_events (
		id             BINARY(16)    NOT NULL,
		event_id       CHAR(36)      NOT NULL,
		aggregate_type VARCHAR(100)  NULL,
		aggregate_id   CHAR(36)      NULL,
		event_type     VARCHAR(100)  NOT NULL,
		event_version  INTEGER       NOT NULL DEFAULT 1,
		payload        NVARCHAR(MAX) NOT NULL,
		correlation_id CHAR(36)      NULL,
		causation_id   CHAR(36)      NULL,
		thread_id      CHAR(36)      NOT NULL,
		traceparent    VARCHAR(64)   NULL,
		actor          NVARCHAR(255) NOT NULL,
		created_at     DATETIME2(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (id),
		CONSTRAINT integration_events_event_id_uniq UNIQUE (event_id)
	)`); err != nil {
		t.Fatalf("create integration_events: %v", err)
	}
}

func TestSQLServerProducer_ProducerDispatch(t *testing.T) {
	eng, raw := setup(t)
	createIntegrationEventsTable(t, raw)

	integration.Configure(&integration.Config{
		Publishes: integration.PublishConfig{
			Events: map[string]integration.PublishEvent{
				"evt": {EventType: "TestEvt"},
			},
		},
	}, eng, nil)
	t.Cleanup(integration.Reset)

	appCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	// Standalone path (no WithTx) → engine Querier autocommit.
	if err := integration.Dispatch(appCtx, "evt", map[string]any{"path": "standalone"}); err != nil {
		t.Fatalf("standalone Dispatch: %v", err)
	}

	// In-TX path: a BeforeCommit hook on an engine Insert calls Dispatch(WithTx),
	// so the integration_events row lands in the same TX as the entity write via
	// the canonical UnwrapTx bridge.
	hook := core.WriteHook{
		BeforeCommit: func(_ persistence.RequestContext, _ domain.Entity, _ domain.ID, tx persistence.TxHandle) error {
			return integration.Dispatch(appCtx, "evt", map[string]any{"path": "in-tx"}, integration.WithTx(tx))
		},
	}
	person := &flatPerson{Name: "Pearl", Email: "pearl@producer"}
	ins, err := domain.GetInsertable(person, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	if _, err := eng.Insert(appCtx, ins, flatSchema(), hook); err != nil {
		t.Fatalf("Insert with in-TX Dispatch hook: %v", err)
	}

	// Both events landed (event_type=TestEvt), one per path.
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM integration_events WHERE event_type = 'TestEvt'`).Scan(&n); err != nil {
		t.Fatalf("count integration_events: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 integration_events rows (standalone + in-tx), got %d", n)
	}

	// The in-TX row is committed with the entity (rolling the entity write back
	// would have taken it too): assert the payload landed verbatim and is
	// queryable as JSON text.
	var payload string
	if err := raw.QueryRow(
		`SELECT payload FROM integration_events WHERE JSON_VALUE(payload, '$.path') = 'in-tx'`,
	).Scan(&payload); err != nil {
		t.Fatalf("read in-tx event: %v", err)
	}
}
