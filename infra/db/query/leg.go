package query

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The Leg is the shared "where the data comes from" piece of BOTH composition
// families: the materialized Embed family (View.Embed / EmbedMany /
// EmbedInChild) and the read-time Link family (ComposedView.Link / LinkMany /
// LinkInChild). A leg carries only the source (an internal view or a locally
// materialized external collection) plus the two segment names; everything
// else — the join column, multiplicity, 1:N knobs — lives on the verb's
// binding at the declaration site.

// Leg is one side of a composed-view link OR an embed source: an internal
// view (JoinView) or a locally materialized external collection (JoinUpstream).
// It is a pure piece — what to pull plus the two segment names (goSegment for
// criteria/Response, externalName for the document field). The join column and
// the LinkMany-only knobs (OrderBy/Desc/MaxLinkManyLimit) are declared on the
// verb's binding, not here. Both leg kinds serve both families.
type Leg struct {
	view         *ViewDefinition   // internal leg (nil for external)
	schema       *core.TableSchema // external leg schema (nil for internal)
	goSegment    string
	externalName string
}

// IsMongo reports whether the leg resolves to a local external Mongo collection
// (a JoinUpstream leg). A JoinView leg returns false.
func (l *Leg) IsMongo() bool { return l.schema != nil && l.schema.IsExternal() }

// Collection is the leg's local Mongo collection — the external schema's table or
// the internal leg view's name.
func (l *Leg) Collection() string { return legCollection(l) }

// Table is an alias of Collection, kept for the embed boot guards that inspect a
// leg as an embed source.
func (l *Leg) Table() string { return legCollection(l) }

// SchemaDef returns the external leg's core.TableSchema (nil for a JoinView leg).
func (l *Leg) SchemaDef() *core.TableSchema { return l.schema }

// schemaAndEmbeds returns the leg's schema tree: an internal leg exposes its
// view's root schema + declared embeds (the leg arrives exactly as a direct
// read of that view would); an external leg its external schema, no embeds.
func (l *Leg) schemaAndEmbeds() (*core.TableSchema, []embedDef) {
	if l.view != nil {
		return l.view.schema, l.view.embeds
	}
	return l.schema, nil
}

// JoinView produces an internal leg over a registered view. The leg reads the
// view's own Mongo collection with the view's schema tree (children, embeds,
// roles translate exactly as a direct read of that view would). goName is the Go
// segment (criteria/Response), externalName the document field the leg lands
// under — both mandatory (declared like TableSchema.Field(go, external)).
func JoinView(v *ViewDefinition, goName, externalName string) *Leg {
	if v == nil {
		panic("query.JoinView(nil)")
	}
	if goName == "" || externalName == "" {
		panic(fmt.Sprintf("query.JoinView(%q): goName and externalName are both mandatory", v.Name()))
	}
	return &Leg{view: v, goSegment: goName, externalName: externalName}
}

// JoinUpstream produces an external leg over a locally materialized upstream
// collection — a core.NewExternalSchema describing the collection an
// UpstreamSubscription declares. Boot validates the subscription exists (a
// leg never reads another service's live storage). goName / externalName are
// both mandatory (a type-less schema cannot derive them).
func JoinUpstream(ts *core.TableSchema, goName, externalName string) *Leg {
	if ts == nil {
		panic("query.JoinUpstream(nil)")
	}
	if !ts.IsExternal() {
		panic(fmt.Sprintf(
			"query.JoinUpstream(%q): the schema is write-anchored — an external leg takes a core.NewExternalSchema "+
				"describing a locally materialized upstream collection; an internal view joins via query.JoinView",
			ts.Table()))
	}
	if goName == "" || externalName == "" {
		panic(fmt.Sprintf("query.JoinUpstream(%q): goName and externalName are both mandatory", ts.Table()))
	}
	return &Leg{schema: ts, goSegment: goName, externalName: externalName}
}

func legCollection(l *Leg) string {
	if l.view != nil {
		return l.view.Name()
	}
	return l.schema.Table()
}
