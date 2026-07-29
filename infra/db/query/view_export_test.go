package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

type expUser struct {
	domain.BaseEntity
	Name  string `labelKey:"UserNameField"`
	Email string // intentionally unlabeled
	Phone string `labelKey:"UserPhoneField"` // lives on a sibling table
}

func (e *expUser) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *expUser) BuildRules(string, domain.Service, *domain.Rules) {}

type expAddr struct {
	ID      string
	ZipCode string `labelKey:"AddressZipCodeField"`
}

func (a expAddr) GetID() domain.ID                                 { return domain.NewID(a.ID) }
func (a expAddr) BuildRules(string, domain.Service, *domain.Rules) {}

// buildExportTestView models the canonical shape: root + sibling (FLAT) + an own
// 1:N child (auto), plus one EXTERNAL embed (the only kind allowed). The export
// plan must walk the whole tree — sibling columns land FLAT on the root node, the
// own child nests under its derived segment, the external embed under its field.
func buildExportTestView() *ViewDefinition {
	userSchema := core.NewTableSchema[*expUser]("users").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		Sibling(core.NewSiblingSchema[*expUser]("users_ext").Field("Phone", "phone")).
		Child(core.NewTableSchema[expAddr]("addresses").
			ID("id").ParentID("user_id").Field("ZipCode", "zip_code"))
	// External (type-less) source carries its labelKey inline — the "mini-domain".
	partnerSchema := core.NewExternalSchema("partners").
		ID("id").
		Field("PartnerName", "name", "PartnerNameField")

	return View("users").Version(1).
		Schema(userSchema).
		Embed(JoinUpstream(partnerSchema, "Partner", "partner")).On("partner_id")
}

func TestExportPlan_BuildsColumnsLabelsAndSegments(t *testing.T) {
	plan := buildExportTestView().ExportPlan()
	root := plan.Root

	// Root columns are FLAT: the root's own fields, then the sibling's — ID and
	// managed columns excluded.
	if len(root.Columns) != 3 {
		t.Fatalf("root columns=%d want 3 (Name, Email, sibling Phone)", len(root.Columns))
	}
	if root.Columns[0].GoField != "Name" || root.Columns[0].WireLeaf != "name" || root.Columns[0].LabelKey != "UserNameField" {
		t.Fatalf("root col0 = %+v", root.Columns[0])
	}
	if root.Columns[1].GoField != "Email" || root.Columns[1].LabelKey != "" {
		t.Fatalf("unlabeled Email should carry empty LabelKey, got %+v", root.Columns[1])
	}
	if root.Columns[2].GoField != "Phone" || root.Columns[2].WireLeaf != "phone" || root.Columns[2].LabelKey != "UserPhoneField" {
		t.Fatalf("sibling column must fold FLAT into the root node, got %+v", root.Columns[2])
	}

	// Children: the external embed (declared first) then the auto own child.
	if len(root.Children) != 2 {
		t.Fatalf("expected partner + addresses children, got %d", len(root.Children))
	}
	partner := root.Children[0]
	if partner.GoSegment != "Partner" || partner.WireSegment != "partner" {
		t.Fatalf("partner segments: go=%q wire=%q", partner.GoSegment, partner.WireSegment)
	}
	if len(partner.Columns) != 1 || partner.Columns[0].WireLeaf != "partnerName" || partner.Columns[0].LabelKey != "PartnerNameField" {
		t.Fatalf("external partner column should carry inline label, got %+v", partner.Columns)
	}

	addr := root.Children[1]
	// Own-child segment is the pluralized child type (expAddr → expAddrs); the wire
	// token is its lower-camel form.
	if addr.GoSegment != "expAddrs" || addr.WireSegment != "expAddrs" {
		t.Fatalf("addresses segments: go=%q wire=%q", addr.GoSegment, addr.WireSegment)
	}
	if len(addr.Columns) != 1 || addr.Columns[0].WireLeaf != "zipCode" || addr.Columns[0].LabelKey != "AddressZipCodeField" {
		t.Fatalf("addresses col = %+v", addr.Columns)
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
	core.NewExternalSchema("p").Field("Name", "name", "K")

	// Type-anchored schema with a schema-level label panics (struct tag is the
	// single source there).
	assertPanics(t, "type-anchored schema-level label", func() {
		core.NewTableSchema[*expUser]("users").Field("Name", "name", "K")
	})

	// More than one label panics.
	assertPanics(t, "two labels", func() {
		core.NewExternalSchema("p").Field("Name", "name", "A", "B")
	})
}
