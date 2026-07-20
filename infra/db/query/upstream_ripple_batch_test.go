package query

import (
	"context"
	"strings"
	"testing"
)

// TestRipple_BatchComposeErrorFallsBackPerID proves the safety fallback of the
// batched ripple: when the set-based ComposeBatch fails, the pass drops to per-id
// compose so a healthy local id is STILL recomposed (no data lost) and the
// failure registry stays per-id. Here the set-based root fetch (WHERE id IN (...))
// errors while the per-id fetch (WHERE id = ?) succeeds, so the parent is upserted
// via the fallback and no failure is recorded.
func TestRipple_BatchComposeErrorFallsBackPerID(t *testing.T) {
	colls := happyColls()
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		if strings.Contains(sql, " IN (") {
			return nil, errFake // the batched root fetch fails...
		}
		data := make([][]any, 0, len(args)) // ...the per-id fetch echoes the id
		for _, a := range args {
			id, _ := a.(string)
			data = append(data, []any{id, "u1", "first"})
		}
		return mapsFromColsData([]string{"id", "buyer_id", "name"}, data), nil
	})
	s := newManyUpstream(t, upstreamFakeMongo(colls), eng)

	s.ripple(context.Background(), "u1", nil, Document{"_id": "u1", "order_id": "o1"})

	if len(colls["orders"].updates) == 0 {
		t.Error("a batch-compose error must fall back to per-id and still recompose the parent")
	}
}
