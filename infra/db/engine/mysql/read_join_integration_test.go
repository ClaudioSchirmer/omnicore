//go:build integration && mysql

package mysql

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/relational"
)

// A read join's whole product is SQL a backend must accept, and every part of it
// is dialect-shaped: the identifier quoting around each `j_<fk>` alias, the
// position of the row window, and — on MySQL above all — the BINARY(16) id codec,
// which has to encode the join's ON both ways or the traversal silently matches
// nothing. The behavior of the feature is proven once on Postgres; this file
// proves THIS dialect agrees.
//
// The fixture gives rj_customers and rj_carriers a column of the SAME name
// (credito) on purpose: an expression that reaches one of them unqualified is
// then rejected by the server rather than merely lucky.

type rjOrder struct {
	domain.AggregateRoot
	Code       string
	CustomerID domain.ID
	CarrierID  *domain.ID

	CustomerName    string // inner join — always matches
	CustomerCredito int64
	CarrierCode     *string // left join — nil when there is no carrier
}

func (e *rjOrder) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *rjOrder) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *rjOrder) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }
func (e *rjOrder) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{rjLine{}}
}

type rjLine struct {
	domain.Managed
	Label  string
	CityID domain.ID
	// CityName arrives from a join declared on the CHILD — load-only.
	CityName string
}

func (l rjLine) BuildRules(string, domain.Service, *domain.Rules) {}
func (l rjLine) CollectionName() string                           { return "Lines" }
func (l rjLine) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	x, ok := o.(rjLine)
	return ok && x.Label == l.Label
}

type rjTarget struct {
	domain.BaseEntity
	Nome    string
	Credito int64
}

func (e *rjTarget) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *rjTarget) BuildRules(string, domain.Service, *domain.Rules) {}

func rjLineSchema() *core.TableSchema {
	return core.NewTableSchema[rjLine]("rj_order_lines").
		ID("id").ParentID("rj_order_id").
		Field("Label", "label").
		Field("CityID", "city_id")
}

func rjOrderSchema() *core.TableSchema {
	return core.NewTableSchema[*rjOrder]("rj_orders").
		ID("id").Revision("revision").
		Field("Code", "code").
		Field("CustomerID", "customer_id").
		Field("CarrierID", "carrier_id").
		DeletedAt("deleted_at").
		Child(rjLineSchema())
}

func rjCustomerSchema() *core.TableSchema {
	return core.NewTableSchema[*rjTarget]("rj_customers").ID("id").
		Field("Nome", "nome").Field("Credito", "credito")
}

func rjCarrierSchema() *core.TableSchema {
	return core.NewTableSchema[*rjTarget]("rj_carriers").ID("id").
		Field("Nome", "codigo").Field("Credito", "credito")
}

func rjCitySchema() *core.TableSchema {
	return core.NewTableSchema[*rjTarget]("rj_cities").ID("id").Field("Nome", "nome")
}

// rjSetup creates the fixture tables, seeds two orders — one WITH a carrier, one
// without — and returns the loader every case reads through.
func rjSetup(t *testing.T) *read.AggregateLoader[*rjOrder] {
	t.Helper()
	eng, raw := setup(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP TABLE IF EXISTS rj_order_lines`,
		`DROP TABLE IF EXISTS rj_orders`,
		`DROP TABLE IF EXISTS rj_customers`,
		`DROP TABLE IF EXISTS rj_carriers`,
		`DROP TABLE IF EXISTS rj_cities`,
		`CREATE TABLE rj_customers (
			id BINARY(16) PRIMARY KEY,
			nome VARCHAR(255) NOT NULL,
			credito BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE rj_carriers (
			id BINARY(16) PRIMARY KEY,
			codigo VARCHAR(255) NOT NULL,
			credito BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE rj_cities (
			id BINARY(16) PRIMARY KEY,
			nome VARCHAR(255) NOT NULL
		)`,
		`CREATE TABLE rj_orders (
			id BINARY(16) PRIMARY KEY,
			revision BIGINT NOT NULL DEFAULT 0,
			code VARCHAR(64) NOT NULL,
			customer_id BINARY(16) NOT NULL,
			carrier_id BINARY(16) NULL,
			deleted_at DATETIME NULL
		)`,
		`CREATE TABLE rj_order_lines (
			id BINARY(16) PRIMARY KEY,
			rj_order_id BINARY(16) NOT NULL,
			label VARCHAR(64) NOT NULL,
			city_id BINARY(16) NOT NULL
		)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("fixture DDL: %v\nstmt: %s", err, stmt)
		}
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"rj_order_lines", "rj_orders", "rj_customers", "rj_carriers", "rj_cities"} {
			_, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+tbl)
		}
	})

	newID := func() uuid.UUID { return uuid.New() }
	ana, bruno, carrier, city := newID(), newID(), newID(), newID()
	withCarrier, without := newID(), newID()

	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO rj_customers (id, nome, credito) VALUES (?, 'ana', 10)`, []any{ana[:]}},
		{`INSERT INTO rj_customers (id, nome, credito) VALUES (?, 'bruno', 32)`, []any{bruno[:]}},
		{`INSERT INTO rj_carriers (id, codigo, credito) VALUES (?, 'DHL', 99)`, []any{carrier[:]}},
		{`INSERT INTO rj_cities (id, nome) VALUES (?, 'Porto Alegre')`, []any{city[:]}},
		{`INSERT INTO rj_orders (id, code, customer_id, carrier_id) VALUES (?, 'A-1', ?, ?)`,
			[]any{withCarrier[:], ana[:], carrier[:]}},
		{`INSERT INTO rj_orders (id, code, customer_id) VALUES (?, 'B-2', ?)`,
			[]any{without[:], bruno[:]}},
		{`INSERT INTO rj_order_lines (id, rj_order_id, label, city_id) VALUES (?, ?, 'l1', ?)`,
			[]any{newID(), withCarrier[:], city[:]}},
		{`INSERT INTO rj_order_lines (id, rj_order_id, label, city_id) VALUES (?, ?, 'l1', ?)`,
			[]any{newID(), without[:], city[:]}},
	} {
		args := make([]any, len(s.args))
		for i, a := range s.args {
			if u, ok := a.(uuid.UUID); ok {
				args[i] = u[:]
				continue
			}
			args[i] = a
		}
		if _, err := raw.ExecContext(ctx, s.sql, args...); err != nil {
			t.Fatalf("seed: %v\nstmt: %s", err, s.sql)
		}
	}

	loader := read.NewAggregateLoader[*rjOrder](eng, func() *rjOrder { return &rjOrder{} }).
		WithSchema(rjOrderSchema()).
		WithJoins(
			read.InnerJoin(rjCustomerSchema()).On("customer_id").
				Field("CustomerName", "nome").
				Field("CustomerCredito", "credito"),
			read.LeftJoin(rjCarrierSchema()).On("carrier_id").
				Field("CarrierCode", "codigo"),
			read.InnerJoinInChild(rjLineSchema()).To(rjCitySchema()).On("city_id").
				Field("CityName", "nome"),
		)
	return loader
}

// The load: the id codec has to encode the join's ON in the dialect's stored
// form, an inner join must fill, a left join with no counterpart must scan NULL
// into the pointer without dropping the root, and a child join must fill every
// loaded element off the child's own batched SELECT.
func TestMySQLReadJoin_LoadsAcrossBothKinds(t *testing.T) {
	loader := rjSetup(t)

	orders, err := loader.FindAll(context.Background(), criteria.Where(nil).OrderBy("Code"))
	if err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("a left join must preserve the root with no counterpart, got %d orders", len(orders))
	}
	withCarrier, without := orders[0], orders[1]
	if withCarrier.CustomerName != "ana" || withCarrier.CustomerCredito != 10 {
		t.Errorf("inner join did not fill: %+v", withCarrier)
	}
	if withCarrier.CarrierCode == nil || *withCarrier.CarrierCode != "DHL" {
		t.Errorf("left join with a counterpart must fill, got %v", withCarrier.CarrierCode)
	}
	if without.CarrierCode != nil {
		t.Errorf("left join with NO counterpart must stay nil, got %q", *without.CarrierCode)
	}
	if without.CustomerName != "bruno" {
		t.Errorf("inner join on the carrier-less order: %+v", without)
	}
	for _, o := range orders {
		lines := o.AllAggregateItems()["rjLine"]
		if len(lines) != 1 {
			t.Fatalf("expected one line per order, got %d", len(lines))
		}
		if got := lines[0].Item.(rjLine).CityName; got != "Porto Alegre" {
			t.Errorf("the child join must fill every loaded element, got %q", got)
		}
	}
}

// Filter, sort, Exists and the aggregate DSL over a joined column — every one of
// them has to reach the server alias-qualified, since `nome` and `credito` live
// on more than one table in this FROM.
func TestMySQLReadJoin_IsAddressableInACriteria(t *testing.T) {
	loader := rjSetup(t)
	ctx := context.Background()

	filtered, err := loader.FindAll(ctx, criteria.Where(criteria.Eq("CustomerName", "ana")))
	if err != nil {
		t.Fatalf("filter by a joined column: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Code != "A-1" {
		t.Fatalf("filter by a joined column = %d rows, want the one order of 'ana'", len(filtered))
	}

	sorted, err := loader.FindAll(ctx, criteria.Where(nil).OrderByDesc("CustomerName"))
	if err != nil {
		t.Fatalf("sort by a joined column: %v", err)
	}
	if len(sorted) != 2 || sorted[0].CustomerName != "bruno" {
		t.Fatalf("sort by a joined column = %v", sorted)
	}

	ok, err := loader.Exists(ctx, criteria.Where(criteria.Eq("CustomerName", "bruno")))
	if err != nil || !ok {
		t.Fatalf("Exists over a joined column = (%v, %v), want (true, nil)", ok, err)
	}

	sum := read.SumInt("CustomerCredito")
	if err := loader.Aggregate(ctx, criteria.Where(nil), sum); err != nil {
		t.Fatalf("aggregate over a joined column: %v", err)
	}
	if sum.Value != 42 {
		t.Errorf("SumInt(CustomerCredito) = %d, want 42 (10 + 32)", sum.Value)
	}

	total, err := loader.CountEntities(ctx, criteria.Where(nil))
	if err != nil || total != 2 {
		t.Errorf("CountEntities = (%d, %v), want (2, nil) — the traversals must not inflate it", total, err)
	}
}

// The same reach, served through a relational view: the row window and the
// deterministic ORDER BY tiebreak are rendered in this dialect's native position,
// and a NULL left join must reach the consumer as an ABSENCE.
func TestMySQLReadJoin_ServedThroughARelationalView(t *testing.T) {
	loader := rjSetup(t)
	ctx := context.Background()

	var reader queries.ViewReader = relational.NewViewReader([]*query.RelationalViewDefinition{
		query.RelationalView("rj_orders", loader),
	})

	page, err := reader.ReadPage(ctx, "rj_orders", queries.ReadCriteria{
		Limit:   1,
		OrderBy: []queries.OrderByField{{Field: "CustomerName"}},
	})
	if err != nil {
		t.Fatalf("ReadPage sorted by a joined field: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0]["CustomerName"] != "ana" {
		t.Fatalf("the joined field must be served and sortable, got %#v", page.Items)
	}
	if page.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2 (the match set, not the window)", page.TotalCount)
	}
	lines, _ := page.Items[0]["Lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("the child collection must be served, got %#v", page.Items[0]["Lines"])
	}
	if el, _ := lines[0].(map[string]any); el["CityName"] != "Porto Alegre" {
		t.Errorf("a child join field must be served on the element, got %#v", lines[0])
	}

	// Page 2 through the offset cursor — the window is dialect-rendered.
	p2, err := reader.ReadPage(ctx, "rj_orders", queries.ReadCriteria{
		Limit:   1,
		OrderBy: []queries.OrderByField{{Field: "CustomerName"}},
		After:   page.EndCursor,
	})
	if err != nil {
		t.Fatalf("ReadPage page 2: %v", err)
	}
	if len(p2.Items) != 1 || p2.Items[0]["CustomerName"] != "bruno" {
		t.Fatalf("page 2 = %#v", p2.Items)
	}
	carrierless := p2.Items[0]
	if v, present := carrierless["CarrierCode"]; !present || v != nil {
		t.Errorf("a left join with no counterpart must be served as nil, got %#v (present=%v)", v, present)
	}
	type orderResult struct {
		Code        string
		CarrierCode *string
	}
	if got := queries.ResultFromDoc[orderResult](carrierless); got.CarrierCode != nil {
		t.Errorf("a NULL join field must leave the Result pointer nil, got %q", *got.CarrierCode)
	}

	filtered, err := reader.ReadPage(ctx, "rj_orders", queries.ReadCriteria{
		Filter: map[string]any{"CustomerName": "bruno"},
	})
	if err != nil {
		t.Fatalf("ReadPage filtered by a joined field: %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0]["Code"] != "B-2" {
		t.Fatalf("filter by a joined field = %v", filtered.Items)
	}
}
