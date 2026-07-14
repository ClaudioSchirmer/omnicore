package write

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// WriteOutbox lands one outbox row in the SAME TX as the data write — the
// framework's atomic-outbox invariant, written once for every engine. The table
// and column list are fixed framework identifiers (provisioned by embedded
// migration 0001, the exact shape Debezium's Outbox Event Router depends on), so
// only the positional placeholders vary by dialect, rendered via tx.Dialect()
// ($n on Postgres, ? on MySQL). The aggregate_id is the canonical UUID string
// (the CDC routing key) on every backend — outbox.aggregate_id is text, never
// BINARY(16). payload is the JSON snapshot (informational for the local
// SyncEngine, which re-reads from the source): the bound fields on
// INSERTED/UPDATED, the fields + the soft-delete column on
// ARCHIVED/UNARCHIVED, the structural keys (PK + shared-base FK) on DELETED —
// see lifecycle_payload.go. The producing request's W3C traceparent is stamped
// so the async projection links back to its trace; nil → NULL when tracing is
// off or no span is active (an empty string would store "" instead of NULL).
func WriteOutbox(ctx context.Context, tx Tx, table, eventType, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var traceparent any
	if tp := tracing.TraceparentFromContext(ctx); tp != "" {
		traceparent = tp
	}
	d := tx.Dialect()
	sql := fmt.Sprintf(
		"INSERT INTO outbox (aggregate_type, event_type, aggregate_id, payload, traceparent, created_at) VALUES (%s, %s, %s, %s, %s, %s)",
		d.Placeholder(1), d.Placeholder(2), d.Placeholder(3), d.Placeholder(4), d.Placeholder(5), d.NowExpr(),
	)
	return tx.Exec(ctx, sql, table, eventType, id, data, traceparent)
}
