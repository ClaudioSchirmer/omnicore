package query

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// RelationalViewDefinition declares a read model served straight from the
// relational backend (the source of record) instead of a materialized Mongo
// projection: a read hits the SoR, so it is strongly consistent with the write
// that just happened — no CDC hop, no projection lag.
//
// It is a DIFFERENT TYPE from ViewDefinition, not a flag on it, and that is the
// whole point. It carries no version, no registry row, no rebuild, no drift, no
// collection and no Mongo spec, and the projection machinery takes
// *ViewDefinition concretely — so a relational read model cannot be handed to
// ApplyMongoSpecs, the SyncEngine, the drift detector, the rebuild or the
// reconciler. Nothing is skipped at runtime because nothing can arrive.
//
// What a relational read can and cannot serve is decided by the relational read
// engine at its own entry point, before any IO — never here, and never by
// anything above the read seam.
type RelationalViewDefinition struct {
	name   string
	loader AggregateReader
	// maxLimit / maxExportRows are the per-view read ceilings, resolved in the
	// same cascade the Mongo reader honors: this override > the yaml default
	// (query.maxLimit / query.maxExportRows) > the framework constant. Zero =
	// unset. Operational state — and there is no hash here for them to stay out
	// of, because a relational read model has no shape to version.
	maxLimit      int64
	maxExportRows int64
}

// RelationalView declares a relational-backed read model addressed by name on
// every surface exactly like a Mongo-backed view.
//
// The loader is the ONLY structural input: it carries both the ability to read
// the aggregate and the core.TableSchema it reads through, so the view's schema
// and its loader cannot disagree — the mismatch a separate schema argument would
// make possible does not exist, and needs no boot guard. Pass the aggregate's
// existing repo.Loader; there is one per aggregate, shared with the repository.
//
// The name shares ONE namespace with every other read model of the service: a
// collision with a query.View, a SharedBaseView or a ComposedView aborts the boot.
func RelationalView(name string, loader AggregateReader) *RelationalViewDefinition {
	return &RelationalViewDefinition{name: name, loader: loader}
}

// MaxLimit caps the per-page row count this view returns (?first= / ?last=).
// Zero (unset) defers to the yaml default, then to the framework's own floor.
func (v *RelationalViewDefinition) MaxLimit(n int64) *RelationalViewDefinition {
	v.maxLimit = n
	return v
}

// MaxExportRows caps the row count a tabular export (CSV/XLSX) of this view
// streams. Zero (unset) defers to the yaml default, then to DefaultMaxExportRows.
func (v *RelationalViewDefinition) MaxExportRows(n int64) *RelationalViewDefinition {
	v.maxExportRows = n
	return v
}

// Name is the read-side identity the four surfaces address this view by.
func (v *RelationalViewDefinition) Name() string { return v.name }

// Loader is the aggregate reader this view is served through.
func (v *RelationalViewDefinition) Loader() AggregateReader { return v.loader }

// SchemaDef is the view's root schema, taken from the loader — the single source
// both the read membrane and the criteria resolution consult. Nil only when the
// declaration is invalid, which ValidateRelationalViews rejects at boot.
func (v *RelationalViewDefinition) SchemaDef() *core.TableSchema {
	if v.loader == nil {
		return nil
	}
	return v.loader.Schema()
}

// RootTable is the physical table the view reads from.
func (v *RelationalViewDefinition) RootTable() string {
	if s := v.SchemaDef(); s != nil {
		return s.Table()
	}
	return ""
}

// MaxLimitValue returns the declared per-view page ceiling, or 0 when unset.
func (v *RelationalViewDefinition) MaxLimitValue() int64 { return v.maxLimit }

// MaxExportRowsValue returns the declared per-view export cap, or 0 when unset.
func (v *RelationalViewDefinition) MaxExportRowsValue() int64 { return v.maxExportRows }

// ResolveMaxExportRows resolves the effective export-row ceiling: the per-view
// override when declared, else the supplied yaml default, else the framework
// fallback. Together with Name() this satisfies web.ExportView, so a relational
// view plugs into the tabular-export wrapper with no change on the web side.
func (v *RelationalViewDefinition) ResolveMaxExportRows(yamlDefault int64) int64 {
	switch {
	case v.maxExportRows > 0:
		return v.maxExportRows
	case yamlDefault > 0:
		return yamlDefault
	default:
		return DefaultMaxExportRows
	}
}

// BuildViewNode assembles the column↔Go translator for this view: the schema
// alone, with its own aggregate children and — when the schema is a shared-base
// role — the base's native children. No embed segments and no roles: a relational
// view declares neither, so there is nothing else to register.
func (v *RelationalViewDefinition) BuildViewNode() *ViewNode {
	return newViewNode(v.SchemaDef(), nil)
}

// ValidateRelationalViews enforces what a relational read model must carry, and
// that its name does not collide with any other read-side identity of the
// service. Every other rule a Mongo view needs a boot guard for — no embeds, no
// roles, no Mongo spec, no version — is enforced here by the type system: those
// methods do not exist on this type.
//
// mongoViews and composed are the OTHER two name spaces the service addresses on
// read; a view name is unique across all three because a name is how a surface
// asks for a read model, whatever backs it.
func ValidateRelationalViews(views []*RelationalViewDefinition, mongoViews []*ViewDefinition, composed []*ComposedViewDefinition) error {
	taken := make(map[string]string, len(mongoViews)+len(composed))
	for _, v := range mongoViews {
		taken[v.Name()] = "a view"
	}
	for _, c := range composed {
		taken[c.Name()] = "a composed view"
	}

	var problems []string
	for _, v := range views {
		if v == nil {
			continue
		}
		if v.name == "" {
			problems = append(problems, "relational view: declared with an empty name")
			continue
		}
		if why := ReservedNameSuffixProblem(v.name); why != "" {
			problems = append(problems, fmt.Sprintf("relational view %q: %s", v.name, why))
			continue
		}
		if v.loader == nil {
			problems = append(problems, fmt.Sprintf(
				"relational view %q: declared with a nil loader — pass the aggregate's repo.Loader, "+
					"which carries both the read and the schema", v.name))
			continue
		}
		if v.SchemaDef() == nil {
			problems = append(problems, fmt.Sprintf(
				"relational view %q: its loader has no schema bound — the repository must declare "+
					"WithSchema(...) so the view resolves Go field names to columns", v.name))
			continue
		}
		if kind, dup := taken[v.name]; dup {
			problems = append(problems, fmt.Sprintf(
				"relational view %q: name collides with %s — a read-model name is service-unique "+
					"across every backing, because the name is what a surface reads by", v.name, kind))
			continue
		}
		taken[v.name] = "a relational view"
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid relational view declaration(s):\n  - %s", strings.Join(problems, "\n  - "))
}
