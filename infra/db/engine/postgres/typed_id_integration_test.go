//go:build integration && postgres

package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The type-driven identity contract on Postgres — the MySQL twins live in the
// mysql package (typed_id_integration_test.go). The field's Go TYPE is the
// declaration: domain.ID / *domain.ID pairs with the native UUID column (bound
// as uuid text, the server casts; the id scan proxy restores the value, nil
// for SQL NULL), while string / *string pairs with text columns and is never
// coerced. These pin the full round-trip — write, scan, lifted criteria probe,
// SQL NULL — for both quadrants, required and nullable.

// pgLinkEntity: required (TenantID) + nullable (PartnerID) identity references
// over native UUID columns.
type pgLinkEntity struct {
	domain.BaseEntity
	Name      string
	TenantID  domain.ID
	PartnerID *domain.ID
}

func (e *pgLinkEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (*pgLinkEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func pgLinkSchema() *core.TableSchema {
	return core.NewTableSchema[*pgLinkEntity]("links").
		ID("id").
		Field("Name", "name").
		Field("TenantID", "tenant_id").
		Field("PartnerID", "partner_id")
}

func TestPGEngine_TypedIDColumns(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := pg.Pool().Exec(ctx, `CREATE TABLE links (
		id UUID PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		tenant_id UUID NOT NULL,
		partner_id UUID NULL
	)`); err != nil {
		t.Fatalf("create links: %v", err)
	}

	tenant := domain.NewID(uuid.NewString())
	partner := domain.NewID(uuid.NewString())
	ins, err := domain.GetInsertable(&pgLinkEntity{Name: "linked", TenantID: tenant, PartnerID: &partner}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := pg.Insert(testCtx(), ins, pgLinkSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert (domain.ID + *domain.ID into UUID columns): %v", err)
	}

	loader := read.NewAggregateLoader[*pgLinkEntity](pg, func() *pgLinkEntity { return &pgLinkEntity{} }).
		WithSchema(pgLinkSchema())

	got, err := loader.FindOne(testCtx(), criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.TenantID.Value() != tenant.Value() {
		t.Fatalf("TenantID = %q, want %q", got.TenantID.Value(), tenant.Value())
	}
	if got.PartnerID == nil || got.PartnerID.Value() != partner.Value() {
		t.Fatalf("PartnerID = %v, want &%q", got.PartnerID, partner.Value())
	}

	// Bare-string probes on the identity-typed fields are lifted and match the
	// UUID columns.
	byRef, err := loader.FindOne(testCtx(), criteria.Where(criteria.And(
		criteria.Eq("TenantID", tenant.Value()),
		criteria.Eq("PartnerID", partner.Value()),
	)))
	if err != nil {
		t.Fatalf("FindOne by lifted criteria: %v", err)
	}
	if byRef.GetID().Value() != res.ID.Value() {
		t.Fatalf("criteria matched id %q, want %q", byRef.GetID().Value(), res.ID)
	}

	// SQL NULL round-trips as nil.
	insNil, err := domain.GetInsertable(&pgLinkEntity{Name: "unlinked", TenantID: tenant}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable (nil): %v", err)
	}
	resNil, err := pg.Insert(testCtx(), insNil, pgLinkSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert (nil *domain.ID): %v", err)
	}
	gotNil, err := loader.FindOne(testCtx(), criteria.ByID(resNil.ID))
	if err != nil {
		t.Fatalf("FindOne (nil): %v", err)
	}
	if gotNil.PartnerID != nil {
		t.Fatalf("PartnerID = %v, want nil (SQL NULL)", gotNil.PartnerID.Value())
	}
}

// pgTextRefEntity: uuid-VALUED references deliberately typed string/*string —
// the text choice, over CHAR(36)/VARCHAR(36).
type pgTextRefEntity struct {
	domain.BaseEntity
	Name    string
	OwnerID string
	BuddyID *string
}

func (e *pgTextRefEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (*pgTextRefEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func pgTextRefSchema() *core.TableSchema {
	return core.NewTableSchema[*pgTextRefEntity]("text_refs").
		ID("id").
		Field("Name", "name").
		Field("OwnerID", "owner_id").
		Field("BuddyID", "buddy_id")
}

func TestPGEngine_StringFieldsStayText(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := pg.Pool().Exec(ctx, `CREATE TABLE text_refs (
		id UUID PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		owner_id CHAR(36) NOT NULL,
		buddy_id VARCHAR(36) NULL
	)`); err != nil {
		t.Fatalf("create text_refs: %v", err)
	}

	owner := uuid.NewString()
	buddy := uuid.NewString()
	ins, err := domain.GetInsertable(&pgTextRefEntity{Name: "textual", OwnerID: owner, BuddyID: &buddy}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := pg.Insert(testCtx(), ins, pgTextRefSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert (uuid-shaped strings into text columns): %v", err)
	}

	// Stored as text, byte-for-byte.
	var rawOwner, rawBuddy string
	if err := pg.Pool().QueryRow(ctx, `SELECT owner_id, buddy_id FROM text_refs WHERE name = 'textual'`).Scan(&rawOwner, &rawBuddy); err != nil {
		t.Fatalf("select text refs: %v", err)
	}
	if rawOwner != owner || rawBuddy != buddy {
		t.Fatalf("stored (%q, %q), want the canonical text (%q, %q)", rawOwner, rawBuddy, owner, buddy)
	}

	loader := read.NewAggregateLoader[*pgTextRefEntity](pg, func() *pgTextRefEntity { return &pgTextRefEntity{} }).
		WithSchema(pgTextRefSchema())

	got, err := loader.FindOne(testCtx(), criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.OwnerID != owner || got.BuddyID == nil || *got.BuddyID != buddy {
		t.Fatalf("loaded (%q, %v), want (%q, &%q)", got.OwnerID, got.BuddyID, owner, buddy)
	}

	byOwner, err := loader.FindOne(testCtx(), criteria.Where(criteria.Eq("OwnerID", owner)))
	if err != nil {
		t.Fatalf("FindOne by text criteria: %v", err)
	}
	if byOwner.GetID().Value() != res.ID.Value() {
		t.Fatalf("criteria matched id %q, want %q", byOwner.GetID().Value(), res.ID)
	}
}
