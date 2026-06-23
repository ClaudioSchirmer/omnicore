//go:build integration

package infra

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// aggCustomer is an aggregate root used by aggregate_persister.go tests.
// Holds a collection of aggChannel children. Table inference: agg_customers.
type aggCustomer struct {
	domain.AggregateRoot
	Name  string
	Email string
}

func (e *aggCustomer) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (*aggCustomer) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *aggCustomer) GetAggregateRoot() *domain.AggregateRoot        { return &e.AggregateRoot }
func (*aggCustomer) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{aggChannel{}}
}

// aggChannel is the AggregateValueObject child. Has its own ID + Label.
// Inferred child table: agg_channels; FK column: agg_customer_id.
type aggChannel struct {
	ID    string
	Label string
}

func (c aggChannel) GetID() string                                    { return c.ID }
func (c aggChannel) BuildRules(string, domain.Service, *domain.Rules) {}

func createAggregateTables(t *testing.T, pg *Postgres) {
	t.Helper()
	createTable(t, pg, `CREATE TABLE agg_customers (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE agg_channels (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		agg_customer_id UUID NOT NULL REFERENCES agg_customers (id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
}

// aggCustomerSchema declares the aggCustomer aggregate (root + aggChannel child).
func aggCustomerSchema() *TableSchema {
	return NewTableSchema[*aggCustomer]("agg_customers").
		PK("id").
		Field("Name", "name").
		Field("Email", "email").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(NewTableSchema[aggChannel]("agg_channels").
			PK("id").
			FK("agg_customer_id").
			Field("Label", "label").
			SoftDelete("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at"))
}

// --- insertAggregate -----------------------------------------------------

func TestPostgres_InsertAggregate_PersistsRootAndChildren(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	root := &aggCustomer{Name: "Acme", Email: "ops@acme"}
	domain.AddAggregateChild(root, aggChannel{Label: "email"})
	domain.AddAggregateChild(root, aggChannel{Label: "sms"})

	ins, err := domain.GetInsertable(root, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := pg.Insert(testCtx(), ins, aggCustomerSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert aggregate: %v", err)
	}

	if rowCount(t, pg, "agg_customers") != 1 {
		t.Errorf("expected 1 root, got %d", rowCount(t, pg, "agg_customers"))
	}
	if rowCount(t, pg, "agg_channels") != 2 {
		t.Errorf("expected 2 children, got %d", rowCount(t, pg, "agg_channels"))
	}

	// One outbox row (granularity B) carrying root + children snapshot.
	rows := outboxRows(t, pg)
	if len(rows) != 1 {
		t.Fatalf("expected 1 outbox row (granularity B), got %d", len(rows))
	}
	if rows[0].AggregateID != res.ID {
		t.Errorf("outbox aggregate_id = %q, want %q", rows[0].AggregateID, res.ID)
	}
	var payload map[string]any
	_ = json.Unmarshal(rows[0].Payload, &payload)
	if _, ok := payload["children"].(map[string]any); !ok {
		t.Errorf("expected outbox payload to carry children block, got %v", payload)
	}
}

// --- updateAggregate: added / changed / removed ---------------------------

func TestPostgres_UpdateAggregate_AppliesChildChanges(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	// Seed root with two children via Insert.
	root := &aggCustomer{Name: "Beta", Email: "b@x"}
	domain.AddAggregateChild(root, aggChannel{Label: "email"})
	domain.AddAggregateChild(root, aggChannel{Label: "sms"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, aggCustomerSchema(), noHook)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Reload children with the assigned IDs to drive Change/Remove.
	loaded := &aggCustomer{Name: "Beta", Email: "b@x"}
	loaded.SetID(domain.NewID(res.ID))
	type childRow struct {
		ID, Label string
	}
	var existing []childRow
	rows, err := pg.Pool().Query(context.Background(),
		`SELECT id, label FROM agg_channels WHERE agg_customer_id = $1 ORDER BY label`, res.ID)
	if err != nil {
		t.Fatalf("query children: %v", err)
	}
	for rows.Next() {
		var c childRow
		if err := rows.Scan(&c.ID, &c.Label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		existing = append(existing, c)
	}
	rows.Close()
	if len(existing) != 2 {
		t.Fatalf("expected 2 children loaded, got %d", len(existing))
	}

	asChildren := []domain.AggregateValueObject{
		aggChannel{ID: existing[0].ID, Label: existing[0].Label},
		aggChannel{ID: existing[1].ID, Label: existing[1].Label},
	}
	loaded.AggregateConstructor(asChildren)

	upd, err := domain.GetUpdatable(loaded, func(c *aggCustomer) error {
		// Change first; remove second; add a new third.
		current := domain.GetCurrentItemsOf[aggChannel](&c.AggregateRoot)
		domain.ChangeAggregateChild(c, current[0], aggChannel{ID: current[0].ID, Label: current[0].Label + "-upd"})
		domain.RemoveAggregateChild(c, current[1])
		domain.AddAggregateChild(c, aggChannel{Label: "webhook"})
		return nil
	}, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	if _, err := pg.Update(testCtx(), upd, aggCustomerSchema(), noHook); err != nil {
		t.Fatalf("Update aggregate: %v", err)
	}

	// Expected: 1 active row updated, 1 archived, 1 newly inserted → 2 active + 1 archived.
	if got := activeCount(t, pg, "agg_channels"); got != 2 {
		t.Errorf("expected 2 active children, got %d", got)
	}
	if got := rowCount(t, pg, "agg_channels"); got != 3 {
		t.Errorf("expected 3 total child rows (one archived), got %d", got)
	}
	// One INSERT root outbox + one UPDATE root outbox = 2 outbox events total.
	if got := outboxCount(t, pg); got != 2 {
		t.Errorf("expected 2 outbox rows, got %d", got)
	}
}

// --- archiveAggregate cascades to children -----------------------------------

func TestPostgres_ArchiveAggregate_CascadesActiveChildren(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	root := &aggCustomer{Name: "G", Email: "g@x"}
	domain.AddAggregateChild(root, aggChannel{Label: "a"})
	domain.AddAggregateChild(root, aggChannel{Label: "b"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, aggCustomerSchema(), noHook)

	loaded := &aggCustomer{Name: "G", Email: "g@x"}
	loaded.SetID(domain.NewID(res.ID))
	// Cascade derives the typeName from AllAggregateItems() — we need to load
	// children into the aggregate root so the SQL knows the child table to
	// archive. Construct an empty AVO of the right type via AggregateConstructor.
	loaded.AggregateConstructor([]domain.AggregateValueObject{aggChannel{}})

	arch, _ := domain.GetArchivable(loaded, nil, "GetArchivable")
	if err := pg.Archive(testCtx(), arch, aggCustomerSchema(), noHook); err != nil {
		t.Fatalf("Archive aggregate: %v", err)
	}
	if activeCount(t, pg, "agg_customers") != 0 {
		t.Error("expected root to be archived")
	}
	if activeCount(t, pg, "agg_channels") != 0 {
		t.Error("expected ALL children to be archived by cascade")
	}
}

// --- unarchiveAggregate restores children ---------------------------------

func TestPostgres_UnarchiveAggregate_RestoresArchivedChildren(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	root := &aggCustomer{Name: "U", Email: "u@x"}
	domain.AddAggregateChild(root, aggChannel{Label: "x"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, aggCustomerSchema(), noHook)

	id := domain.NewID(res.ID)

	// Archive first.
	loaded := &aggCustomer{Name: "U", Email: "u@x"}
	loaded.SetID(id)
	loaded.AggregateConstructor([]domain.AggregateValueObject{aggChannel{}})
	arch, _ := domain.GetArchivable(loaded, nil, "GetArchivable")
	if err := pg.Archive(testCtx(), arch, aggCustomerSchema(), noHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Unarchive (separate entity instance, mirroring real flow after a reload).
	loaded2 := &aggCustomer{Name: "U", Email: "u@x"}
	loaded2.SetID(id)
	loaded2.AggregateConstructor([]domain.AggregateValueObject{aggChannel{}})
	una, _ := domain.GetUnarchivable(loaded2, nil, "GetUnarchivable")
	if err := pg.Unarchive(testCtx(), una, aggCustomerSchema(), noHook); err != nil {
		t.Fatalf("Unarchive aggregate: %v", err)
	}

	if activeCount(t, pg, "agg_customers") != 1 {
		t.Error("expected root to be unarchived")
	}
	if activeCount(t, pg, "agg_channels") != 1 {
		t.Error("expected child to be unarchived by cascade")
	}
}

// --- deleteAggregate relies on FK CASCADE ----------------------------------

func TestPostgres_DeleteAggregate_FKDeleteCascade(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	root := &aggCustomer{Name: "D", Email: "d@x"}
	domain.AddAggregateChild(root, aggChannel{Label: "x"})
	domain.AddAggregateChild(root, aggChannel{Label: "y"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, aggCustomerSchema(), noHook)

	loaded := &aggCustomer{Name: "D", Email: "d@x"}
	loaded.SetID(domain.NewID(res.ID))
	del, _ := domain.GetDeletable(loaded, nil, "GetDeletable")
	if err := pg.Delete(testCtx(), del, aggCustomerSchema(), noHook); err != nil {
		t.Fatalf("Delete aggregate: %v", err)
	}
	if rowCount(t, pg, "agg_customers") != 0 {
		t.Error("root not removed")
	}
	if rowCount(t, pg, "agg_channels") != 0 {
		t.Error("expected FK ON DELETE CASCADE to remove children too")
	}
}

// --- updateChild without ID is an error -----------------------------------

func TestUpdateChild_WithoutIDIsError(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	// Seed.
	root := &aggCustomer{Name: "U", Email: "u@x"}
	domain.AddAggregateChild(root, aggChannel{Label: "first"})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, aggCustomerSchema(), noHook)

	loaded := &aggCustomer{Name: "U", Email: "u@x"}
	loaded.SetID(domain.NewID(res.ID))
	// Load one child but give it NO id — Change request → updateChild requires id.
	loaded.AggregateConstructor([]domain.AggregateValueObject{aggChannel{ID: "", Label: "first"}})
	upd, _ := domain.GetUpdatable(loaded, func(c *aggCustomer) error {
		current := domain.GetCurrentItemsOf[aggChannel](&c.AggregateRoot)
		domain.ChangeAggregateChild(c, current[0], aggChannel{Label: "renamed"}) // empty ID still
		return nil
	}, nil, "GetUpdatable")

	_, err := pg.Update(testCtx(), upd, aggCustomerSchema(), noHook)
	if err == nil {
		t.Error("expected Update to fail when changed child carries no ID")
	}
}

// --- ChildTableOverride + ChildFKOverride --------------------------------

type aggInvoice struct {
	domain.AggregateRoot
	Reference string
}

func (e *aggInvoice) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert}
}
func (*aggInvoice) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *aggInvoice) GetAggregateRoot() *domain.AggregateRoot        { return &e.AggregateRoot }
func (*aggInvoice) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{lineItem{}}
}

type lineItem struct {
	ID     string
	Amount int
}

func (l lineItem) GetID() string                                    { return l.ID }
func (l lineItem) BuildRules(string, domain.Service, *domain.Rules) {}

func TestPostgres_InsertAggregate_RespectsChildTableAndFKOverride(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()

	createTable(t, pg, `CREATE TABLE agg_invoices (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		reference TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE tb_lines (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		invoice_id UUID NOT NULL REFERENCES agg_invoices(id) ON DELETE CASCADE,
		amount INT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)

	root := &aggInvoice{Reference: "INV-1"}
	domain.AddAggregateChild(root, lineItem{Amount: 100})
	ins, _ := domain.GetInsertable(root, nil, "GetInsertable")

	schema := NewTableSchema[*aggInvoice]("agg_invoices").
		PK("id").
		Field("Reference", "reference").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		Child(NewTableSchema[lineItem]("tb_lines").
			PK("id").
			FK("invoice_id").
			Field("Amount", "amount").
			SoftDelete("deleted_at").
			CreatedAt("created_at").
			UpdatedAt("updated_at"))
	if _, err := pg.Insert(testCtx(), ins, schema, noHook); err != nil {
		t.Fatalf("Insert with overrides: %v", err)
	}
	if rowCount(t, pg, "tb_lines") != 1 {
		t.Errorf("expected child to land in tb_lines, got %d rows", rowCount(t, pg, "tb_lines"))
	}
}

// --- BaseRepository wrappers --------------------------------------------

func TestBaseRepository_InsertUpdateArchiveUnarchiveDelete(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	repo := &BaseRepository[*flatPerson]{
		Postgres:  pg,
		NewEntity: func() *flatPerson { return &flatPerson{} },
		Schema:    flatPersonSchema(),
	}

	// Insert via repo.
	e := &flatPerson{Name: "R", Email: "r@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	id, err := repo.Scope(testCtx()).Insert(ins)
	if err != nil {
		t.Fatalf("repo Insert: %v", err)
	}
	if id.IsEmpty() {
		t.Error("expected non-empty ID from repo.Insert")
	}

	// Update via repo.
	e2 := &flatPerson{Name: "R", Email: "r@x"}
	e2.SetID(id)
	upd, _ := domain.GetUpdatable(e2, func(p *flatPerson) error { p.Name = "R2"; return nil }, nil, "GetUpdatable")
	if err := repo.Scope(testCtx()).Update(upd); err != nil {
		t.Fatalf("repo Update: %v", err)
	}

	// Archive then Unarchive.
	a := &flatPerson{Name: "R2", Email: "r@x"}
	a.SetID(id)
	arch, _ := domain.GetArchivable(a, nil, "GetArchivable")
	if err := repo.Scope(testCtx()).Archive(arch); err != nil {
		t.Fatalf("repo Archive: %v", err)
	}
	if activeCount(t, pg, "flat_persons") != 0 {
		t.Error("Archive did not flip deleted_at via repo")
	}

	u := &flatPerson{Name: "R2", Email: "r@x"}
	u.SetID(id)
	una, _ := domain.GetUnarchivable(u, nil, "GetUnarchivable")
	if err := repo.Scope(testCtx()).Unarchive(una); err != nil {
		t.Fatalf("repo Unarchive: %v", err)
	}

	// Delete.
	d := &flatPerson{Name: "R2", Email: "r@x"}
	d.SetID(id)
	del, _ := domain.GetDeletable(d, nil, "GetDeletable")
	if err := repo.Scope(testCtx()).Delete(del); err != nil {
		t.Fatalf("repo Delete: %v", err)
	}
	if rowCount(t, pg, "flat_persons") != 0 {
		t.Error("Delete via repo did not remove the row")
	}
}

func TestBaseRepository_ConstraintBindingMapsTo23505Notification(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	// Add a named unique constraint that we'll bind.
	if _, err := pg.Pool().Exec(context.Background(),
		`CREATE UNIQUE INDEX persons_email_uq ON flat_persons (email) WHERE deleted_at IS NULL`); err != nil {
		t.Fatalf("create unique index: %v", err)
	}

	repo := &BaseRepository[*flatPerson]{
		Postgres:    pg,
		NewEntity:   func() *flatPerson { return &flatPerson{} },
		ContextName: "Person",
		Schema:      flatPersonSchema(),
		Constraints: map[string]ConstraintBinding{
			"persons_email_uq": {
				Notification: domain.EntityAlreadyAddedNotification{},
				Field:        "email",
			},
		},
	}

	e1 := &flatPerson{Name: "A", Email: "same@x"}
	ins1, _ := domain.GetInsertable(e1, nil, "GetInsertable")
	if _, err := repo.Scope(testCtx()).Insert(ins1); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	e2 := &flatPerson{Name: "B", Email: "same@x"}
	ins2, _ := domain.GetInsertable(e2, nil, "GetInsertable")
	_, err := repo.Scope(testCtx()).Insert(ins2)
	if err == nil {
		t.Fatal("expected duplicate to fail")
	}
	var carrier domain.NotificationCarrier
	if !errorsAs(err, &carrier) {
		t.Fatalf("expected NotificationCarrier, got %T", err)
	}
	ctxs := carrier.NotificationContexts()
	if len(ctxs) != 1 {
		t.Fatalf("expected 1 context, got %d", len(ctxs))
	}
	if ctxs[0].Context() != "Person" {
		t.Errorf("context name = %q, want Person", ctxs[0].Context())
	}
	msgs := ctxs[0].Messages()
	if domain.NotificationKey(msgs[0].Notification) != "EntityAlreadyAddedNotification" {
		t.Errorf("notification = %T", msgs[0].Notification)
	}
}

func TestBaseRepository_ConstraintCodeOtherThan23505ReturnsRaw(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	repo := &BaseRepository[*flatPerson]{
		Postgres:  pg,
		NewEntity: func() *flatPerson { return &flatPerson{} },
		Schema:    flatPersonSchema(),
		Constraints: map[string]ConstraintBinding{
			"any_name": {Notification: domain.RequiredFieldNotification{}, Field: "x"},
		},
	}

	// Force an error that is NOT 23505 — e.g. inserting NULL into NOT NULL via direct SQL.
	loaded := &flatPerson{Name: "X", Email: "x@x"}
	loaded.SetID(domain.NewID(uuid.NewString()))
	upd, _ := domain.GetUpdatable(loaded, func(*flatPerson) error { return nil }, nil, "GetUpdatable")

	err := repo.Scope(testCtx()).Update(upd)
	if err == nil {
		t.Fatal("expected error when updating a non-existent row")
	}
	// Error here is NOT a constraint violation (no row to update), so mapErr
	// should return it raw.
	var carrier domain.NotificationCarrier
	if errorsAs(err, &carrier) {
		t.Errorf("did not expect a NotificationCarrier for non-23505 error, got %T", err)
	}
}

func TestBaseRepository_NewPanicsWhenFactoryMissing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when NewEntity is nil")
		}
	}()
	repo := &BaseRepository[*flatPerson]{Postgres: nil} // NewEntity intentionally nil
	_ = repo.New()
}

func TestBaseRepository_NewReturnsFactoryResult(t *testing.T) {
	repo := &BaseRepository[*flatPerson]{
		NewEntity: func() *flatPerson { return &flatPerson{Name: "factory"} },
	}
	got := repo.New()
	if got == nil || got.Name != "factory" {
		t.Errorf("New() = %+v, want factory instance", got)
	}
}

func TestBaseRepository_EffectiveContextName_FromTypeName(t *testing.T) {
	repo := &BaseRepository[*flatPerson]{
		NewEntity: func() *flatPerson { return &flatPerson{} },
	}
	if got := repo.effectiveContextName(); got != "flatPerson" {
		t.Errorf("effectiveContextName() = %q, want flatPerson", got)
	}
}

func TestBaseRepository_EffectiveContextName_Override(t *testing.T) {
	repo := &BaseRepository[*flatPerson]{
		NewEntity:   func() *flatPerson { return &flatPerson{} },
		ContextName: "OverrideName",
	}
	if got := repo.effectiveContextName(); got != "OverrideName" {
		t.Errorf("effectiveContextName() = %q, want OverrideName", got)
	}
}

// --- helpers --------------------------------------------------------------

// errorsAs is the test-local wrapper around errors.As. Kept as a function so
// the assertion sites stay focused on the test intent.
func errorsAs(err error, target any) bool {
	switch tgt := target.(type) {
	case *domain.NotificationCarrier:
		return errors.As(err, tgt)
	default:
		return false
	}
}
