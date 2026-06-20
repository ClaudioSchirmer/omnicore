package infra

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The Composer reads through c.pg.pool.Query (the pgxPool interface), so it CAN
// be driven from the in-process seam — unlike the AggregateLoader which goes via
// Pool() (concrete type). pgxRowsToMaps consumes FieldDescriptions()+Values(),
// so these tests supply a small composerRows fake (the package-wide fakeRows is
// Scan-shaped and does not model those two methods).

type composerRows struct {
	pgx.Rows

	cols      []string
	data      [][]any
	pos       int
	valuesErr error
}

func (r *composerRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *composerRows) Values() ([]any, error) {
	if r.valuesErr != nil {
		return nil, r.valuesErr
	}
	return r.data[r.pos-1], nil
}

func (r *composerRows) FieldDescriptions() []pgconn.FieldDescription {
	fd := make([]pgconn.FieldDescription, len(r.cols))
	for i, c := range r.cols {
		fd[i].Name = c
	}
	return fd
}

func (r *composerRows) Close()     {}
func (r *composerRows) Err() error { return nil }

func composerRootSchema() *TableSchema {
	return NewTableSchema[*builderTestEntity]("orders").
		PK("ID", "id").
		Field("Name", "name").
		SoftDelete("deleted_at")
}

func composerLineSchema() *TableSchema {
	return NewTableSchema[fakeVO]("lines").
		PK("ID", "id").
		FK("order_id").
		Field("Label", "label").
		SoftDelete("deleted_at")
}

func composerBuyerSchema() *TableSchema {
	return NewTableSchema[fakeVO]("buyers").
		PK("ID", "id").
		Field("Label", "label")
}

func TestCompose_RootMissing(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &composerRows{}, nil // zero rows
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc != nil {
		t.Errorf("missing root must yield nil doc, got %v", doc)
	}
}

func TestCompose_QueryError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return nil, errFake
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected query error from Compose")
	}
}

func TestCompose_RootOnly(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &composerRows{
			cols: []string{"id", "name"},
			data: [][]any{{"o1", "first"}},
		}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc == nil || doc["id"] != "o1" || doc["name"] != "first" {
		t.Errorf("root doc drifted: %v", doc)
	}
}

func TestCompose_EmbedMany(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM orders"):
			return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "first"}}}, nil
		case strings.Contains(sql, "FROM lines"):
			return &composerRows{
				cols: []string{"id", "order_id", "label"},
				data: [][]any{{"l1", "o1", "a"}, {"l2", "o1", "b"}},
			}, nil
		}
		return &composerRows{}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		EmbedMany("lines", FromSchema(composerLineSchema()))

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	lines, ok := doc["lines"].([]bson.M)
	if !ok {
		t.Fatalf("lines embed shape = %T", doc["lines"])
	}
	if len(lines) != 2 {
		t.Errorf("embedded lines = %d, want 2 (doc=%v)", len(lines), doc)
	}
}

func TestCompose_EmbedOneToOne(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM orders"):
			return &composerRows{
				cols: []string{"id", "name", "buyer_id"},
				data: [][]any{{"o1", "first", "b1"}},
			}, nil
		case strings.Contains(sql, "FROM buyers"):
			return &composerRows{cols: []string{"id", "label"}, data: [][]any{{"b1", "acme"}}}, nil
		}
		return &composerRows{}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema()).
		Embed("buyer", FromSchema(composerBuyerSchema()).On("buyer_id").As("Buyer"))

	doc, err := c.Compose(context.Background(), view, "o1")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	buyer, ok := doc["buyer"].(bson.M)
	if !ok {
		t.Fatalf("buyer embed shape = %T", doc["buyer"])
	}
	if buyer["label"] != "acme" {
		t.Errorf("buyer embed drifted: %v", buyer)
	}
}

func TestComposeAll(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		if strings.Contains(sql, "FROM orders") {
			return &composerRows{cols: []string{"id", "name"}, data: [][]any{{"o1", "a"}, {"o2", "b"}}}, nil
		}
		return &composerRows{}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())

	docs, err := c.ComposeAll(context.Background(), view)
	if err != nil {
		t.Fatalf("ComposeAll: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("ComposeAll = %d docs, want 2", len(docs))
	}
}

func TestComposeAll_QueryError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) { return nil, errFake }
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())
	if _, err := c.ComposeAll(context.Background(), view); err == nil {
		t.Fatal("expected query error from ComposeAll")
	}
}

func TestPgxRowsToMaps_ValuesError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(sql string, args []any) (pgx.Rows, error) {
		return &composerRows{cols: []string{"id"}, data: [][]any{{"x"}}, valuesErr: errFake}, nil
	}
	c := NewComposer(newFakePostgres(pool))
	view := View("orders").Version(1).Root("orders").Schema(composerRootSchema())
	if _, err := c.Compose(context.Background(), view, "o1"); err == nil {
		t.Fatal("expected Values() error to surface")
	}
}

func TestNormalizeSQLValue_UUIDBytes(t *testing.T) {
	id := uuid.New()
	var raw [16]byte = id
	if got := NormalizeSQLValue(raw); got != id.String() {
		t.Errorf("NormalizeSQLValue([16]byte) = %v, want %v", got, id.String())
	}
	if got := NormalizeSQLValue("plain"); got != "plain" {
		t.Errorf("NormalizeSQLValue passthrough = %v", got)
	}
}
