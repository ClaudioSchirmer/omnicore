//go:build integration && oracle

package oracle

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The generated MERGE upsert must actually EXECUTE on a real Oracle — the unit
// test only asserts the rendered string; the live run is what proves the MERGE
// INTO … USING (… FROM dual) parses, the NULL-safe ON matches, the WHEN
// MATCHED update sees target-scoped bare columns ("attempt + 1"), and the
// do-nothing form (no WHEN MATCHED clause) neither errors on conflict nor
// overwrites the row. Runs both forms through the engine's Querier + Dialect
// against the live container.
//
//	go test -tags=integration,oracle ./infra/db/engine/oracle/ -run BuildUpsertExecutes -count=1
func TestOracleEngine_BuildUpsertExecutes(t *testing.T) {
	eng, raw := setup(t)
	ctx := context.Background()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE upsert_probe (
		k       VARCHAR2(50) NOT NULL,
		payload VARCHAR2(100),
		attempt NUMBER(10) DEFAULT 0 NOT NULL,
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
			{Col: "attempt", Mode: core.UpsertSetExpr, Expr: "attempt + 1"},
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

	// The Oracle-only leg: a NULL conflict-column value ('' binds as NULL — the
	// omnicore_upstream_failures.local_id discover-stage shape). The NULL-safe
	// ON must MATCH the existing NULL-keyed row on the retry, taking the UPDATE
	// arm (attempt increments) instead of dying on ORA-00001.
	if _, err := raw.ExecContext(ctx, `CREATE TABLE upsert_nullkey (
		a       VARCHAR2(50) NOT NULL,
		b       VARCHAR2(50),
		attempt NUMBER(10) DEFAULT 0 NOT NULL,
		CONSTRAINT uq_ab UNIQUE (a, b)
	)`); err != nil {
		t.Fatalf("create nullkey probe: %v", err)
	}
	nullSafe := d.BuildUpsert("upsert_nullkey",
		[]string{"a", "b"}, []string{"a", "b"},
		[]core.UpsertSet{{Col: "attempt", Mode: core.UpsertSetExpr, Expr: "attempt + 1"}})
	if err := q.Exec(ctx, nullSafe, "topic", ""); err != nil {
		t.Fatalf("null-key insert: %v\nSQL: %s", err, nullSafe)
	}
	if err := q.Exec(ctx, nullSafe, "topic", ""); err != nil {
		t.Fatalf("null-key retry must MATCH via the NULL-safe ON, got: %v\nSQL: %s", err, nullSafe)
	}
	var rows, att int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*), MAX(attempt) FROM upsert_nullkey`).Scan(&rows, &att); err != nil {
		t.Fatalf("read nullkey state: %v", err)
	}
	if rows != 1 || att != 1 {
		t.Fatalf("null-key upsert state: rows=%d attempt=%d (want 1/1 — one row, one increment)", rows, att)
	}
}
