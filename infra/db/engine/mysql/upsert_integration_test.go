//go:build integration && mysql

package mysql

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Phase 4c integration test: the generated upsert SQL must actually EXECUTE on a
// real MySQL — the unit test only asserts the rendered string, and the do-nothing
// form is precisely where MySQL 8.4 rejects a bare `col = col` under the AS new
// row alias (errno 1052, "Column ... is ambiguous"). This runs both forms through
// the engine's Querier + Dialect against the live container.
//
//	go test -tags=integration,mysql ./infra/db/mysql/ -run BuildUpsertExecutes -count=1
func TestMySQLEngine_BuildUpsertExecutes(t *testing.T) {
	eng, raw := setup(t)
	ctx := context.Background()

	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS upsert_probe`); err != nil {
		t.Fatalf("drop probe: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE upsert_probe (
		k       VARCHAR(50) NOT NULL,
		payload VARCHAR(100),
		attempt INT NOT NULL DEFAULT 0,
		UNIQUE KEY uq_k (k)
	)`); err != nil {
		t.Fatalf("create probe: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS upsert_probe`) })

	q := eng.Querier()
	d := eng.Dialect()

	// DO UPDATE form (mirrors Record*Failure): insert, then conflict → bump
	// attempt + overwrite payload via new.payload.
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
		t.Fatalf("DO UPDATE wrong state: payload=%q attempt=%d (want second/1)", payload, attempt)
	}

	// DO NOTHING form (mirrors MarkProcessed): the second insert on the same key
	// must be a no-op, not an error, and must NOT overwrite the payload.
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
		t.Fatalf("DO NOTHING overwrote the row: payload=%q (want keep)", payload)
	}
}
