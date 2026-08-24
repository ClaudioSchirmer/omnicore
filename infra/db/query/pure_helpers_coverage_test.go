package query

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/hydrate"
)

// Direct coverage for the small seam helpers: the MySQL bool coercion and the
// source-of-record row probe.

func TestCoerceTypes(t *testing.T) {
	schema := composerBoolSchema()

	t.Run("int64AndIntBecomeBool", func(t *testing.T) {
		row := Document{"active": int64(1), "verified": 0, "name": "x"}
		hydrate.CoerceTypes(row, schema)
		if row["active"] != true || row["verified"] != false {
			t.Errorf("coerced row = %v", row)
		}
		if row["name"] != "x" {
			t.Errorf("non-bool column must pass through, got %v", row["name"])
		}
	})
	t.Run("nullAndRealBoolPassThrough", func(t *testing.T) {
		row := Document{"active": nil, "verified": true}
		hydrate.CoerceTypes(row, schema)
		if row["active"] != nil || row["verified"] != true {
			t.Errorf("row = %v", row)
		}
	})
	t.Run("nilRowAndNilSchemaAreNoops", func(t *testing.T) {
		hydrate.CoerceTypes(nil, schema)
		hydrate.CoerceTypes(Document{"active": int64(1)}, nil)
	})
}

func TestSorHasRows(t *testing.T) {
	d := fakeDialect{}

	t.Run("hasRow", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return &fakeRows{rows: 1}, nil }}
		got, err := sorHasRows(context.Background(), q, d, "orders")
		if err != nil || !got {
			t.Fatalf("hasRow: %v, %v", got, err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return &fakeRows{}, nil }}
		got, err := sorHasRows(context.Background(), q, d, "orders")
		if err != nil || got {
			t.Fatalf("empty: %v, %v", got, err)
		}
	})
	t.Run("queryError", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return nil, errFake }}
		if _, err := sorHasRows(context.Background(), q, d, "orders"); err == nil {
			t.Fatal("expected the probe error")
		}
	})
	t.Run("rowsErr", func(t *testing.T) {
		q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { return &fakeRows{nextErr: errFake}, nil }}
		if _, err := sorHasRows(context.Background(), q, d, "orders"); err == nil {
			t.Fatal("expected the cursor error")
		}
	})
}
