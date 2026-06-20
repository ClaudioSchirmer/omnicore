package infra

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

type expUser struct {
	domain.BaseEntity
	Name  string `labelKey:"UserNameField"`
	Email string // intentionally unlabeled
}

func (e *expUser) Modes() []domain.EntityMode { return []domain.EntityMode{domain.ModeInsert} }
func (e *expUser) BuildRules(string, domain.Service, *domain.Rules) {}

type expAddr struct {
	ID      string
	ZipCode string `labelKey:"AddressZipCodeField"`
}

func (a expAddr) GetID() string                            { return a.ID }
func (a expAddr) BuildRules(string, domain.Service, *domain.Rules) {}

func buildExportTestView() *ViewDefinition {
	userSchema := NewTableSchema[*expUser]("users").
		PK("ID", "id").
		Field("Name", "name").
		Field("Email", "email")
	addrSchema := NewTableSchema[expAddr]("addresses").
		PK("ID", "id").
		FK("user_id").
		Field("ZipCode", "zip_code")
	// External (type-less) source carries its labelKey inline — the "mini-domain".
	partnerSchema := NewExternalSchema("partners").
		PK("ID", "id").
		Field("PartnerName", "name", "PartnerNameField")

	return View("users").Version(1).Root("users").
		Schema(userSchema).
		EmbedMany("addresses", FromSchema(addrSchema)).
		Embed("partner", FromSchema(partnerSchema).On("partner_id").As("Partner"))
}

func TestExportPlan_BuildsColumnsLabelsAndSegments(t *testing.T) {
	plan := buildExportTestView().ExportPlan()
	root := plan.Root

	if len(root.Columns) != 2 {
		t.Fatalf("root columns=%d want 2 (PK + managed excluded)", len(root.Columns))
	}
	if root.Columns[0].GoField != "Name" || root.Columns[0].WireLeaf != "name" || root.Columns[0].LabelKey != "UserNameField" {
		t.Fatalf("root col0 = %+v", root.Columns[0])
	}
	if root.Columns[1].GoField != "Email" || root.Columns[1].LabelKey != "" {
		t.Fatalf("unlabeled Email should carry empty LabelKey, got %+v", root.Columns[1])
	}

	if len(root.Children) != 2 {
		t.Fatalf("expected addresses + partner children, got %d", len(root.Children))
	}
	addr := root.Children[0]
	// GoSegment is derived from the source type name pluralized (expAddr →
	// expAddrs); WireSegment is the explicit EmbedMany doc-field name.
	if addr.GoSegment != "expAddrs" || addr.WireSegment != "addresses" {
		t.Fatalf("addresses segments: go=%q wire=%q", addr.GoSegment, addr.WireSegment)
	}
	if len(addr.Columns) != 1 || addr.Columns[0].WireLeaf != "zipCode" || addr.Columns[0].LabelKey != "AddressZipCodeField" {
		t.Fatalf("addresses col = %+v", addr.Columns)
	}

	// External embed: label comes from the schema-level labelKey, not a struct tag.
	partner := root.Children[1]
	if partner.GoSegment != "Partner" || partner.WireSegment != "partner" {
		t.Fatalf("partner segments: go=%q wire=%q", partner.GoSegment, partner.WireSegment)
	}
	if len(partner.Columns) != 1 || partner.Columns[0].WireLeaf != "partnerName" || partner.Columns[0].LabelKey != "PartnerNameField" {
		t.Fatalf("external partner column should carry inline label, got %+v", partner.Columns)
	}
}

func TestResolveMaxExportRows_Cascade(t *testing.T) {
	v := View("users")
	if got := v.ResolveMaxExportRows(0); got != DefaultMaxExportRows {
		t.Fatalf("framework fallback: got %d want %d", got, DefaultMaxExportRows)
	}
	if got := v.ResolveMaxExportRows(500); got != 500 {
		t.Fatalf("yaml default: got %d want 500", got)
	}
	v.MaxExportRows(50)
	if got := v.ResolveMaxExportRows(500); got != 50 {
		t.Fatalf("per-view override: got %d want 50", got)
	}
}

func TestField_SchemaLabelExternalOnly(t *testing.T) {
	// External schema accepts an inline label.
	NewExternalSchema("p").Field("Name", "name", "K")

	// Type-anchored schema with a schema-level label panics (struct tag is the
	// single source there).
	assertPanics(t, "type-anchored schema-level label", func() {
		NewTableSchema[*expUser]("users").Field("Name", "name", "K")
	})

	// More than one label panics.
	assertPanics(t, "two labels", func() {
		NewExternalSchema("p").Field("Name", "name", "A", "B")
	})
}
