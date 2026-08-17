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
	domain.Managed
	ZipCode string `labelKey:"AddressZipCodeField"`
}

func (a expAddr) BuildRules(string, domain.Service, *domain.Rules) {}

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
