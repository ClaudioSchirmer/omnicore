package query

import (
	"fmt"
	"strings"

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
	// fields is the JoinView-only materialization allowlist declared via
	// Fields(...) — GO names of the source view (root fields by Go name, managed
	// slots by their fixed names, top-level segments by their Go segment name).
	// Nil = no cut (the segment materializes the whole source document, the
	// historical shape). Never valid on a JoinUpstream leg or on a ComposedView
	// link (both boot-rejected).
	fields []string
}

// Fields declares the materialization allowlist of a JoinView leg: only the
// listed source fields enter the embedded segment. Entries are GO NAMES — the
// vocabulary every layer above infra speaks: business fields by their declared
// Go name ("UserName"), managed slots by their FIXED Go names ("DeletedAt",
// "CreatedAt", "ParentID"), and a top-level segment of the source document (a
// child collection, an embed, a role) by its Go segment name, which admits or
// cuts that segment WHOLE. The framework always materializes the identity and
// its reserved watermarks (_id, _revision, _base_revision), an EmbedMany's
// leg-side join column and a declared OrderBy column — none of them is ever
// declared here.
//
// THE ARCHIVE SWITCH. Including or omitting "DeletedAt" is the per-consumer
// archive-behavior switch of the segment:
//
//   - "DeletedAt" listed → the segment follows the source's archive: hidden on
//     default reads (1:1 → null, 1:N → the element leaves the array), revealed
//     by ?includeArchived=true — the same rule every uncut segment applies (a
//     whole-document segment always carries the source's DeletedAt column),
//     here chosen explicitly;
//   - "DeletedAt" omitted → the segment has NO archived rule, by declaration:
//     the archived source keeps its data in the embedding document forever and
//     keeps receiving updates through the ripple.
//
// Fields governs SHAPE, never EXISTENCE: a hard DELETE of the source still
// nulls the segment (or drops the element), fields or no fields.
//
// JoinView-only: a JoinUpstream leg needs no narrowing device — its
// NewExternalSchema is already this consumer's own declaration of what it
// reads, and the subscription yaml `fields:` is the storage cut of the mirror.
// Declaring Fields on an external leg, or using a Fields-bearing leg on a
// ComposedView link, is a fatal boot error.
//
// Fields participates in the embedding view's RebuildHash only when declared
// (a view without it keeps its byte-identical hash stream); declaring or
// changing it is projection shape — bump Version(N) and let the rebuild trim
// the stored documents.
func (l *Leg) Fields(cols ...string) *Leg {
	if len(cols) == 0 {
		panic(fmt.Sprintf(
			"query.Fields on leg %q: at least one field is required — an empty allowlist would materialize "+
				"nothing; omit Fields(...) entirely to materialize the whole source document", l.externalName))
	}
	seen := make(map[string]bool, len(cols))
	for _, c := range cols {
		if c == "" {
			panic(fmt.Sprintf("query.Fields on leg %q: empty field name", l.externalName))
		}
		if strings.HasPrefix(c, "_") {
			panic(fmt.Sprintf(
				"query.Fields on leg %q: %q is a reserved framework field — the identity and watermarks "+
					"(_id, _revision, _base_revision) are always materialized and never declared", l.externalName, c))
		}
		if seen[c] {
			panic(fmt.Sprintf("query.Fields on leg %q: duplicate field %q", l.externalName, c))
		}
		seen[c] = true
	}
	l.fields = append([]string(nil), cols...)
	return l
}

// FieldsList returns the declared materialization allowlist (nil when the leg
// declares none). Consumed by the boot guards, the hash and the trim machinery.
func (l *Leg) FieldsList() []string { return l.fields }

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
