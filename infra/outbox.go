package infra

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

func writeOutbox(ctx context.Context, tx pgx.Tx, table, eventType, id string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO outbox (aggregate_type, event_type, aggregate_id, payload, created_at)
		 VALUES ($1, $2, $3, $4, NOW())`,
		table, eventType, id, data,
	)
	return err
}
