//go:build integration && postgres

package postgres

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The generated upsert SQL must actually EXECUTE on a real Postgres — the unit
// test only asserts the rendered string, and DO UPDATE is precisely where PG
// rejects a bare right-hand-side column as ambiguous against EXCLUDED
// (SQLSTATE 42702). That exact failure shipped in every failure ledger and
// went unnoticed for as long as no test ever executed the conflict branch —
// this test is the reason it cannot come back.
//
//	go test -tags='integration postgres kafka' ./infra/db/engine/postgres/ -run BuildUpsertExecutes -count=1
func TestPGEngine_BuildUpsertExecutes(t *testing.T) {
	eng, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	q := eng.Querier()
	d := eng.Dialect()

	if err := q.Exec(ctx, `CREATE TABLE upsert_probe (
		k       VARCHAR(50) NOT NULL,
		payload VARCHAR(100),
		attempt INT NOT NULL DEFAULT 0,
		CONSTRAINT uq_probe_k UNIQUE (k)
	)`); err != nil {
		t.Fatalf("create probe: %v", err)
	}

	// DO UPDATE form (mirrors Record*Failure): insert, then conflict → bump
	// attempt + overwrite payload via EXCLUDED.payload.
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
	rows, err := q.QueryMaps(ctx, `SELECT payload, attempt FROM upsert_probe WHERE k='key1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read after upsert: rows=%d err=%v", len(rows), err)
	}
	if rows[0]["payload"] != "second" || fmtInt(rows[0]["attempt"]) != 1 {
		t.Fatalf("DO UPDATE wrong state: %v (want second/1)", rows[0])
	}

	// DO NOTHING form (mirrors MarkProcessed): the second insert on the same
	// key must be a no-op, not an error, and must NOT overwrite the payload.
	noop := d.BuildUpsert("upsert_probe",
		[]string{"k", "payload"}, []string{"k"}, nil)
	if err := q.Exec(ctx, noop, "key2", "keep"); err != nil {
		t.Fatalf("do-nothing insert: %v\nSQL: %s", err, noop)
	}
	if err := q.Exec(ctx, noop, "key2", "SHOULD-NOT-WIN"); err != nil {
		t.Fatalf("do-nothing conflict must not error: %v\nSQL: %s", err, noop)
	}
	rows, err = q.QueryMaps(ctx, `SELECT payload FROM upsert_probe WHERE k='key2'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read after do-nothing: rows=%d err=%v", len(rows), err)
	}
	if rows[0]["payload"] != "keep" {
		t.Fatalf("DO NOTHING overwrote the row: %v (want keep)", rows[0])
	}
}

// fmtInt normalizes the driver's integer scan type (int32/int64) for the probe.
func fmtInt(v any) int {
	switch n := v.(type) {
	case int32:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	}
	return -1
}
