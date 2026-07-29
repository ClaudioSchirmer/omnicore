//go:build integration && sqlserver

package sqlserver

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The generated MERGE upsert must actually EXECUTE on a real SQL Server — the
// unit test only asserts the rendered string; the live run is what proves the
// HOLDLOCK MERGE parses, the WHEN MATCHED update sees target-scoped bare
// columns ("attempt + 1"), and the do-nothing form (no WHEN MATCHED clause)
// neither errors on conflict nor overwrites the row. Runs both forms through
// the engine's Querier + Dialect against the live container.
//
//	go test -tags=integration,sqlserver ./infra/db/engine/sqlserver/ -run BuildUpsertExecutes -count=1
func TestSQLServerEngine_BuildUpsertExecutes(t *testing.T) {
	eng, raw := setup(t)
	ctx := context.Background()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE upsert_probe (
		k       VARCHAR(50) NOT NULL,
		payload VARCHAR(100),
		attempt INT NOT NULL DEFAULT 0,
		CONSTRAINT uq_k UNIQUE (k)
	)`); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	q := eng.Querier()
	d := eng.Dialect()

	// WHEN MATCHED form (mirrors Record*Failure): insert, then conflict → bump
	// attempt + overwrite payload via source.payload.
	upd := d.BuildUpsert("upsert_probe",
		[]string{"k", "payload"}, []string{"k"},
		[]core.UpsertSet{
			{Col: "payload", Mode: core.UpsertSetNew},
			{Col: "attempt", Mode: core.UpsertSetBump},
		})
	if err := q.Exec(ctx, upd, "key1", "first"); err != nil {
		t.Fatalf("upsert insert: %v\nSQL: %s", err, upd)
	}
	if err := q.Exec(ctx, upd, "key1", "second"); err != nil {
		t.Fatalf("upsert conflict: %v\nSQL: %s", err, upd)
	}
	var payload string
	var attempt int
	if err := raw.QueryRowContext(ctx, `SELECT payload, attempt FROM upsert_probe WHERE k='key1'`).Scan(&payload, &attempt); err != nil {
		t.Fatalf("read after upsert: %v", err)
	}
	if payload != "second" || attempt != 1 {
		t.Fatalf("MERGE update wrong state: payload=%q attempt=%d (want second/1)", payload, attempt)
	}

	// Do-nothing form (mirrors MarkProcessed): the second insert on the same
	// key must be a no-op, not an error, and must NOT overwrite the payload.
	noop := d.BuildUpsert("upsert_probe",
		[]string{"k", "payload"}, []string{"k"}, nil)
	if err := q.Exec(ctx, noop, "key2", "keep"); err != nil {
		t.Fatalf("do-nothing insert: %v\nSQL: %s", err, noop)
	}
	if err := q.Exec(ctx, noop, "key2", "SHOULD-NOT-WIN"); err != nil {
		t.Fatalf("do-nothing conflict must not error: %v\nSQL: %s", err, noop)
	}
	if err := raw.QueryRowContext(ctx, `SELECT payload FROM upsert_probe WHERE k='key2'`).Scan(&payload); err != nil {
		t.Fatalf("read after do-nothing: %v", err)
	}
	if payload != "keep" {
		t.Fatalf("do-nothing MERGE overwrote the row: payload=%q (want keep)", payload)
	}
}
