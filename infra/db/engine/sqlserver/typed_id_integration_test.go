//go:build integration && sqlserver

package sqlserver

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The type-driven identity contract on SQL Server — the field's Go TYPE is the
// declaration, nothing is configured:
//
//   - domain.ID / *domain.ID field  → BINARY(16) column: binds 16 bytes on
//     write and in lifted criteria probes; the id scan proxy restores the
//     canonical value (nil for SQL NULL on the pointer field).
//   - string / *string field → CHAR(36)/VARCHAR(36) column: text in, text
//     out, text probes — never coerced, whatever the value's shape.
//
// TestSQLServerEngine_SecondaryUUIDColumn covers the required domain.ID
// quadrant; these two cover the nullable *domain.ID and the string/text
// quadrants. The Postgres/MySQL twins live in their engine packages.

// linkEntity: a NULLABLE identity reference (*domain.ID) over BINARY(16) NULL.
type linkEntity struct {
	domain.BaseEntity
	Name      string
	PartnerID *domain.ID
}

func (e *linkEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (*linkEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func linkSchema() *core.TableSchema {
	return core.NewTableSchema[*linkEntity]("links").
		PK("id").
		Field("Name", "name").
		Field("PartnerID", "partner_id")
}

func TestSQLServerEngine_NullableTypedIDColumn(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE links (
		id BINARY(16) NOT NULL PRIMARY KEY,
		name NVARCHAR(255) NOT NULL,
		partner_id BINARY(16) NULL
	)`); err != nil {
		t.Fatalf("create links: %v", err)
	}

	partner := domain.NewID(uuid.NewString())
	ins, err := domain.GetInsertable(&linkEntity{Name: "linked", PartnerID: &partner}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, linkSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert (non-nil *domain.ID into BINARY(16)): %v", err)
	}

	// Stored as 16 raw bytes, not 36-char text.
	var rawPartner []byte
	if err := raw.QueryRow(`SELECT partner_id FROM links WHERE name = 'linked'`).Scan(&rawPartner); err != nil {
		t.Fatalf("select partner_id: %v", err)
	}
	if len(rawPartner) != 16 {
		t.Fatalf("partner_id stored as %d bytes, want BINARY(16)", len(rawPartner))
	}

	loader := read.NewAggregateLoader[*linkEntity](eng, func() *linkEntity { return &linkEntity{} }).
		WithSchema(linkSchema())

	// The id scan proxy restores the canonical value into the pointer field.
	got, err := loader.FindOne(ctx, criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.PartnerID == nil || got.PartnerID.Value() != partner.Value() {
		t.Fatalf("PartnerID = %v, want &%q (BINARY(16) not restored into *domain.ID)", got.PartnerID, partner.Value())
	}

	// A bare-string probe on the *domain.ID field is lifted and matches.
	byRef, err := loader.FindOne(ctx, criteria.Where(criteria.Eq("PartnerID", partner.Value())))
	if err != nil {
		t.Fatalf("FindOne by lifted criteria: %v", err)
	}
	if byRef.GetID().Value() != res.ID.Value() {
		t.Fatalf("criteria matched id %q, want %q", byRef.GetID().Value(), res.ID)
	}

	// SQL NULL round-trips as nil.
	insNil, err := domain.GetInsertable(&linkEntity{Name: "unlinked"}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable (nil): %v", err)
	}
	resNil, err := eng.Insert(ctx, insNil, linkSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert (nil *domain.ID): %v", err)
	}
	gotNil, err := loader.FindOne(ctx, criteria.ByID(resNil.ID))
	if err != nil {
		t.Fatalf("FindOne (nil): %v", err)
	}
	if gotNil.PartnerID != nil {
		t.Fatalf("PartnerID = %v, want nil (SQL NULL)", gotNil.PartnerID.Value())
	}
}

// textRefEntity: uuid-VALUED references deliberately typed string/*string —
// the text choice. They pair with CHAR(36)/VARCHAR(36) columns and are never
// coerced, whatever the value's shape.
type textRefEntity struct {
	domain.BaseEntity
	Name    string
	OwnerID string
	BuddyID *string
}

func (e *textRefEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (*textRefEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func textRefSchema() *core.TableSchema {
	return core.NewTableSchema[*textRefEntity]("text_refs").
		PK("id").
		Field("Name", "name").
		Field("OwnerID", "owner_id").
		Field("BuddyID", "buddy_id")
}

func TestSQLServerEngine_StringFieldsStayText(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE text_refs (
		id BINARY(16) NOT NULL PRIMARY KEY,
		name NVARCHAR(255) NOT NULL,
		owner_id CHAR(36) NOT NULL,
		buddy_id VARCHAR(36) NULL
	)`); err != nil {
		t.Fatalf("create text_refs: %v", err)
	}

	owner := uuid.NewString()
	buddy := uuid.NewString()
	ins, err := domain.GetInsertable(&textRefEntity{Name: "textual", OwnerID: owner, BuddyID: &buddy}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, textRefSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert (uuid-shaped strings into text columns): %v", err)
	}

	// Stored as 36-char text, NOT coerced to 16 bytes.
	var rawOwner, rawBuddy string
	if err := raw.QueryRow(`SELECT owner_id, buddy_id FROM text_refs WHERE name = 'textual'`).Scan(&rawOwner, &rawBuddy); err != nil {
		t.Fatalf("select text refs: %v", err)
	}
	if rawOwner != owner || rawBuddy != buddy {
		t.Fatalf("stored (%q, %q), want the canonical text (%q, %q)", rawOwner, rawBuddy, owner, buddy)
	}

	loader := read.NewAggregateLoader[*textRefEntity](eng, func() *textRefEntity { return &textRefEntity{} }).
		WithSchema(textRefSchema())

	got, err := loader.FindOne(ctx, criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.OwnerID != owner || got.BuddyID == nil || *got.BuddyID != buddy {
		t.Fatalf("loaded (%q, %v), want (%q, &%q)", got.OwnerID, got.BuddyID, owner, buddy)
	}

	// A uuid-shaped probe on a string field binds as text and matches the text
	// column — the quadrant a value-shape codec used to break.
	byOwner, err := loader.FindOne(ctx, criteria.Where(criteria.Eq("OwnerID", owner)))
	if err != nil {
		t.Fatalf("FindOne by text criteria: %v", err)
	}
	if byOwner.GetID().Value() != res.ID.Value() {
		t.Fatalf("criteria matched id %q, want %q", byOwner.GetID().Value(), res.ID)
	}
}
