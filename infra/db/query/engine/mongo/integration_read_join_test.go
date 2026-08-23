//go:build integration && postgres

package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db/engine/postgres"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/relational"
)

// A read join is the one part of the read path whose whole product is SQL a
// backend must accept: an alias per traversal, the joined columns riding the root
// SELECT, the anchor id qualified once any join is in the FROM, a NULL scanned
// into a pointer field. The unit suite asserts the STRING that comes out; only a
// live run proves a database agrees with it.
//
// The fixture deliberately gives customers and carriers a column of the SAME name
// (credito). An expression that reaches one of them unqualified is then ambiguous
// rather than merely lucky, so the qualification is proven, not assumed.

type joinCustomer struct {
	domain.BaseEntity
	Nome    string
	Credito int64
}

func (e *joinCustomer) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinCustomer) BuildRules(string, domain.Service, *domain.Rules) {}

type joinCarrier struct {
	domain.BaseEntity
	Codigo  string
	Credito int64
}

func (e *joinCarrier) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinCarrier) BuildRules(string, domain.Service, *domain.Rules) {}

type joinCity struct {
	domain.BaseEntity
	Nome string
}

func (e *joinCity) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinCity) BuildRules(string, domain.Service, *domain.Rules) {}

// joinOrderRoot declares a mandatory FK (inner join) and an optional one (left
// join), plus the Go fields each traversal lands on.
type joinOrderRoot struct {
	domain.AggregateRoot
	Code       string
	CustomerID domain.ID
	CarrierID  *domain.ID

	CustomerName    string // from the inner join — always matches
	CustomerCredito int64
	CarrierCode     *string // from the left join — nil when there is no carrier
}

func (e *joinOrderRoot) Modes() []domain.EntityMode { return []domain.EntityMode{domain.ModeInsert} }
func (e *joinOrderRoot) BuildRules(string, domain.Service, *domain.Rules) {
}
func (e *joinOrderRoot) GetAggregateRoot() *domain.AggregateRoot { return &e.AggregateRoot }
func (e *joinOrderRoot) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{joinOrderLine{}}
}

type joinOrderLine struct {
	domain.Managed
	Label  string
	CityID domain.ID
	// CityName arrives from a join declared on the CHILD — load-only.
	CityName string
}

func (l joinOrderLine) BuildRules(string, domain.Service, *domain.Rules) {}
func (l joinOrderLine) CollectionName() string                           { return "Lines" }
func (l joinOrderLine) IsSameBusinessIdentity(o domain.AggregateValueObject) bool {
	x, ok := o.(joinOrderLine)
	return ok && x.Label == l.Label
}

func joinLineSchema() *core.TableSchema {
	return core.NewTableSchema[joinOrderLine]("join_order_lines").
		ID("id").ParentID("join_order_id").
		Field("Label", "label").
		Field("CityID", "city_id").
		CreatedAt("created_at").UpdatedAt("updated_at")
}

func joinOrderSchema() *core.TableSchema {
	return core.NewTableSchema[*joinOrderRoot]("join_orders").
		ID("id").Revision("revision").
		Field("Code", "code").
		Field("CustomerID", "customer_id").
		Field("CarrierID", "carrier_id").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(joinLineSchema())
}

func joinCustomerSchema() *core.TableSchema {
	return core.NewTableSchema[*joinCustomer]("join_customers").
		ID("id").Field("Nome", "nome").Field("Credito", "credito")
}

func joinCarrierSchema() *core.TableSchema {
	return core.NewTableSchema[*joinCarrier]("join_carriers").
		ID("id").Field("Codigo", "codigo").Field("Credito", "credito")
}

func joinCitySchema() *core.TableSchema {
	return core.NewTableSchema[*joinCity]("join_cities").ID("id").Field("Nome", "nome")
}

func createReadJoinTables(t *testing.T, p *postgres.Postgres) {
	t.Helper()
	createTable(t, p, `CREATE TABLE join_customers (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		nome TEXT NOT NULL,
		credito BIGINT NOT NULL DEFAULT 0
	)`)
	createTable(t, p, `CREATE TABLE join_carriers (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		codigo TEXT NOT NULL,
		credito BIGINT NOT NULL DEFAULT 0
	)`)
	createTable(t, p, `CREATE TABLE join_cities (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		nome TEXT NOT NULL
	)`)
	createTable(t, p, `CREATE TABLE join_orders (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		revision BIGINT NOT NULL DEFAULT 0,
		code TEXT NOT NULL,
		customer_id UUID NOT NULL REFERENCES join_customers(id),
		carrier_id UUID REFERENCES join_carriers(id),
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, p, `CREATE TABLE join_order_lines (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		join_order_id UUID NOT NULL REFERENCES join_orders(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		city_id UUID NOT NULL REFERENCES join_cities(id),
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
}

// joinFixture seeds two orders: one WITH a carrier, one without, each carrying a
// line in a named city. It returns the loader every case below reads through.
func joinFixture(t *testing.T, pg *postgres.Postgres) (*read.AggregateLoader[*joinOrderRoot], map[string]string) {
	t.Helper()
	ctx := context.Background()
	ids := map[string]string{}
	exec := func(key, sql string, args ...any) {
		var id string
		if err := pg.Pool().QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		ids[key] = id
	}
	exec("cust:ana", `INSERT INTO join_customers (nome, credito) VALUES ('ana', 10) RETURNING id`)
	exec("cust:bruno", `INSERT INTO join_customers (nome, credito) VALUES ('bruno', 32) RETURNING id`)
	exec("carrier", `INSERT INTO join_carriers (codigo, credito) VALUES ('DHL', 99) RETURNING id`)
	exec("city", `INSERT INTO join_cities (nome) VALUES ('Porto Alegre') RETURNING id`)

	exec("order:with", `INSERT INTO join_orders (code, customer_id, carrier_id) VALUES ('A-1', $1, $2) RETURNING id`,
		ids["cust:ana"], ids["carrier"])
	exec("order:without", `INSERT INTO join_orders (code, customer_id) VALUES ('B-2', $1) RETURNING id`,
		ids["cust:bruno"])
	for _, k := range []string{"order:with", "order:without"} {
		if _, err := pg.Pool().Exec(ctx,
			`INSERT INTO join_order_lines (join_order_id, label, city_id) VALUES ($1, 'l1', $2)`,
			ids[k], ids["city"]); err != nil {
			t.Fatalf("seed line for %s: %v", k, err)
		}
	}

	loader := read.NewAggregateLoader[*joinOrderRoot](pg, func() *joinOrderRoot { return &joinOrderRoot{} }).
		WithSchema(joinOrderSchema()).
		WithJoins(
			read.InnerJoin(joinCustomerSchema()).On("customer_id").
				Field("CustomerName", "nome").
				Field("CustomerCredito", "credito"),
			read.LeftJoin(joinCarrierSchema()).On("carrier_id").
				Field("CarrierCode", "codigo"),
			read.InnerJoinInChild(joinLineSchema()).To(joinCitySchema()).On("city_id").
				Field("CityName", "nome"),
		)
	return loader, ids
}

// The load itself: an inner join always fills, a left join with no counterpart
// scans NULL into the pointer field (and does NOT drop the root), and a child
// join fills every loaded child element.
func TestReadJoin_LoadsAcrossBothKinds(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createReadJoinTables(t, pg)
	loader, _ := joinFixture(t, pg)

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
		lines := o.AllAggregateItems()["joinOrderLine"]
		if len(lines) != 1 {
			t.Fatalf("expected one line per order, got %d", len(lines))
		}
		if got := lines[0].Item.(joinOrderLine).CityName; got != "Porto Alegre" {
			t.Errorf("the child join must fill every loaded element, got %q", got)
		}
	}
}

// A joined column is addressable in a criteria — filter, order and the aggregate
// DSL — and every one of them has to reach the database qualified: `nome` and
// `credito` exist on more than one table in this FROM.
func TestReadJoin_IsAddressableInACriteria(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createReadJoinTables(t, pg)
	loader, _ := joinFixture(t, pg)
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

	// The aggregate DSL: SUM over the JOINED credito, not the anchor's and not the
	// carrier's. An unqualified expression is rejected outright by the backend.
	sum := read.SumInt("CustomerCredito")
	if err := loader.Aggregate(ctx, criteria.Where(nil), sum); err != nil {
		t.Fatalf("aggregate over a joined column: %v", err)
	}
	if sum.Value != 42 {
		t.Errorf("SumInt(CustomerCredito) = %d, want 42 (10 + 32)", sum.Value)
	}

	// COUNT is not inflated by the traversals: both joins land on the target's
	// primary key, so each root matches at most one row on each side.
	total, err := loader.CountEntities(ctx, criteria.Where(nil))
	if err != nil || total != 2 {
		t.Errorf("CountEntities = (%d, %v), want (2, nil)", total, err)
	}
}

// A relational view declares no join of its own: it carries the loader, and the
// loader carries the reach. This walks the whole read as a surface does — the
// joined fields must be servable, filterable and sortable, and a NULL left join
// must reach the consumer as an ABSENCE, never as the zero value.
func TestReadJoin_ServedThroughARelationalView(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createReadJoinTables(t, pg)
	loader, _ := joinFixture(t, pg)
	ctx := context.Background()

	var reader queries.ViewReader = relational.NewViewReader([]*query.RelationalViewDefinition{
		query.RelationalView("join_orders", loader),
	})

	page, err := reader.ReadPage(ctx, "join_orders", queries.ReadCriteria{
		OrderBy: []queries.OrderByField{{Field: "CustomerName"}},
	})
	if err != nil {
		t.Fatalf("ReadPage sorted by a joined field: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected both orders, got %d", len(page.Items))
	}
	if page.Items[0]["CustomerName"] != "ana" {
		t.Errorf("the joined field must be served and sortable, got %#v", page.Items[0])
	}
	// A child join field rides that child's elements.
	lines, _ := page.Items[0]["Lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("the child collection must be served, got %#v", page.Items[0]["Lines"])
	}
	if el, _ := lines[0].(map[string]any); el["CityName"] != "Porto Alegre" {
		t.Errorf("a child join field must be served on the element, got %#v", lines[0])
	}

	// 'bruno' has no carrier. The document must say so as a NULL — the fill one
	// layer up turns anything else into an empty string, which is exactly the
	// confusion the LeftJoin nullability rule exists to prevent.
	carrierless := page.Items[1]
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
	if got := queries.ResultFromDoc[orderResult](page.Items[0]); got.CarrierCode == nil || *got.CarrierCode != "DHL" {
		t.Errorf("a present join field must fill the Result, got %v", got.CarrierCode)
	}

	// Filtering by a joined field reaches the SoR under the Go name declared.
	filtered, err := reader.ReadPage(ctx, "join_orders", queries.ReadCriteria{
		Filter: map[string]any{"CustomerName": "bruno"},
	})
	if err != nil {
		t.Fatalf("ReadPage filtered by a joined field: %v", err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0]["Code"] != "B-2" {
		t.Fatalf("filter by a joined field = %v", filtered.Items)
	}
	if filtered.TotalCount != 1 {
		t.Errorf("the total must be counted under the same filter, got %d", filtered.TotalCount)
	}

	// A CHILD join field is load-only: it is served, never addressable at the root.
	if _, err := reader.ReadPage(ctx, "join_orders", queries.ReadCriteria{
		Filter: map[string]any{"CityName": "Porto Alegre"},
	}); err == nil {
		t.Error("a child join field must not be addressable in a criteria")
	}
}
