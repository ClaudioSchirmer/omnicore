package infra

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

func writeOutbox(ctx context.Context, tx pgx.Tx, table, eventType, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Stamp the producing request's traceparent so the async projection can be
	// linked back to its trace. nil → NULL when tracing is off or no span is
	// active (an empty string would store "" instead of NULL).
	var traceparent any
	if tp := tracing.TraceparentFromContext(ctx); tp != "" {
		traceparent = tp
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO outbox (aggregate_type, event_type, aggregate_id, payload, traceparent, created_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())`,
		table, eventType, id, data, traceparent,
	)
	return err
}
