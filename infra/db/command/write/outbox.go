package write

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// WriteOutbox lands one outbox row in the SAME TX as the data write — the
// framework's atomic-outbox invariant, written once for every engine. The table
// and column list are fixed framework identifiers (provisioned by embedded
// migration 0001, the exact shape Debezium's Outbox Event Router depends on), so
// only the positional placeholders vary by dialect, rendered via tx.Dialect()
// ($n on Postgres, ? on MySQL). The aggregate_id is the canonical UUID string
// (the CDC routing key) on every backend — outbox.aggregate_id is text, never
// BINARY(16). payload is the event-carried-state snapshot (see
// outbox_payload.go): every scalar flat at the top, the _ids structural block
// (aggregate PK, base id + revision, purge flag), and the children groups with
// per-item ops; DELETED keeps the historical structural keys and adds _ids.
// The producing request's W3C traceparent is stamped
// so the async projection links back to its trace; nil → NULL when tracing is
// off or no span is active (an empty string would store "" instead of NULL).
func WriteOutbox(ctx context.Context, tx Tx, table, eventType, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// The row PK follows the framework id standard like every other table: a
	// UUID v7 minted in Go, bound in the dialect's native id form (uuid text on
	// PG, BINARY(16) elsewhere) — no AUTO_INCREMENT/IDENTITY/DB default.
	rowID, err := newWriteID()
	if err != nil {
		return err
	}
	var traceparent any
	if tp := tracing.TraceparentFromContext(ctx); tp != "" {
		traceparent = tp
	}
	d := tx.Dialect()
	sql := fmt.Sprintf(
		"INSERT INTO outbox (id, aggregate_type, event_type, aggregate_id, payload, traceparent, created_at) VALUES (%s, %s, %s, %s, %s, %s, %s)",
		d.Placeholder(1), d.Placeholder(2), d.Placeholder(3), d.Placeholder(4), d.Placeholder(5), d.Placeholder(6), d.NowExpr(),
	)
	// The payload binds as TEXT: the outbox payload column is text-shaped JSON
	// on every dialect (jsonb / JSON / NVARCHAR(MAX)), and binding the raw
	// []byte would reach SQL Server as a varbinary parameter, which it refuses
	// to implicitly convert to NVARCHAR. Text binds identically on PG and MySQL.
	return tx.Exec(ctx, sql, d.EncodeArg(domain.NewID(rowID)), table, eventType, id, string(data), traceparent)
}
