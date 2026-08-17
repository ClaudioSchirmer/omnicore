// Package relational is the relational-backed read engine: it serves a view
// marked query.View(...).RelationalSource(...) by loading the aggregate from the
// SoR through the loader the view carries, then mapping the loaded entity into
// the same column-keyed document shape a Mongo-backed view is stored in — so the
// four web surfaces read a relational view exactly as they read a Mongo one.
//
// The mapping here is self-contained: it reads the loaded typed entity, sharing
// NO code with the Composer (whose shaping is fetch-coupled). The two small
// shaping rules the Composer also applies are reproduced from exported building
// blocks — the child-array key via core.TableSchema.CollectionSegment, the revision watermark
// via query.DocRevisionField. The parity integration test (this document vs the
// Composer's, compared as canonical JSON) guards that the two paths agree,
// including any managed/magic column added to the schema later.
package relational

import (
	"reflect"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// BuildDocument maps a loaded aggregate (root + own children, with the
// domain.Managed carrier populated) into the canonical column-keyed view
// Document. Scope: a query.View aggregate — root scalars + own children (depth 1)
// + siblings (root-level and child-level) + the shared base when the view is
// rooted at a shared-base ROLE (its fields flattened + its native children). The
// Embed family and a SharedBaseView (the view KIND) are refused at boot for a
// relational view, so they never reach here.
func BuildDocument(schema *core.TableSchema, e domain.Entity) query.Document {
	doc := query.Document{}
	mergeWriteFields(doc, schema, e)
	if idCol := schema.IDColumn(); idCol != "" {
		if idp := e.GetID(); idp != nil {
			doc[idCol] = idColumnValue(*idp)
		}
	}
	// Root: managed timestamps under their physical columns; the revision rides
	// the _revision watermark (never its physical column) — the shape the
	// Composer's remapRevision produces on a root row.
	applyManaged(doc, schema, e, query.DocRevisionField)
	mergeSharedBase(doc, schema, e)
	mergeSiblings(doc, schema, e)
	appendChildren(doc, schema, e)
	return doc
}

// mergeWriteFields copies the schema's business scalar columns into the doc,
// column-keyed. WriteFields excludes the id and the managed columns — those are
// added separately, so the read-side doc carries them the way the Composer does.
func mergeWriteFields(doc query.Document, schema *core.TableSchema, e any) {
	for col, val := range schema.WriteFields(e) {
		doc[col] = val
	}
}

// managedGetters is the read face of the domain.Managed carrier as promoted onto
// the root (via BaseEntity) and onto every aggregate child. GetID is excluded —
// the root returns *ID and a child returns ID, so id is read off the concrete
// value, not this interface.
type managedGetters interface {
	GetRevision() int64
	GetCreatedAt() *time.Time
	GetUpdatedAt() *time.Time
	GetDeletedAt() *time.Time
}

// applyManaged writes the managed timestamps under their physical columns — only
// the ones the schema declares (schema-driven, matching the Composer's
// ReadColumns), a live row's deleted_at emitted as a present nil like the
// Composer's fetched NULL — and the revision under revisionKey:
// query.DocRevisionField for a root (the watermark), the physical RevisionColumn
// for a child (the Composer leaves a child row's revision under its physical
// column).
func applyManaged(doc query.Document, schema *core.TableSchema, e any, revisionKey string) {
	m, ok := e.(managedGetters)
	if !ok {
		return
	}
	if col := schema.CreatedAtColumn(); col != "" {
		doc[col] = timeValue(m.GetCreatedAt())
	}
	if col := schema.UpdatedAtColumn(); col != "" {
		doc[col] = timeValue(m.GetUpdatedAt())
	}
	if col, has := schema.DeletedAtColumn(); has && col != "" {
		doc[col] = timeValue(m.GetDeletedAt())
	}
	if schema.RevisionColumn() != "" && revisionKey != "" {
		doc[revisionKey] = m.GetRevision()
	}
}

// mergeSharedBase flattens the shared base's business columns into the doc when
// the view is rooted at a shared-base ROLE. The base schema is type-less (its own
// field indices are unset), so its fields are read off the loaded ROLE entity via
// SharedBaseScanPlan — the base-column → role-field-index map the loader scans the
// base INTO (hydrateSharedBase). Reading back through the same indices is exactly
// symmetric. The plan carries the base's business columns only (not the shared id,
// not the base's managed columns — those would shadow the role's), so the doc gets
// the base scalars flat under their physical columns. No-op for a plain aggregate
// (no shared base), so the plain-view doc shape is unchanged.
func mergeSharedBase(doc query.Document, schema *core.TableSchema, e any) {
	_, byCol, ok := schema.SharedBaseScanPlan()
	if !ok || len(byCol) == 0 {
		return
	}
	v := reflect.ValueOf(e)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	for col, path := range byCol {
		// A part of an ABSENT optional composite value object has no value to
		// read: the document carries nil, which is what the column holds.
		fv, ok := path.ValueIn(v)
		if !ok {
			doc[col] = nil
			continue
		}
		doc[col] = fv.Interface()
	}
}

// mergeSiblings merges each sibling schema's columns FLAT into the doc (skipping
// the shared PK the sibling borrows), reading the sibling's Go fields off the
// same entity — the read-side twin of the Composer's mergeOwnerSiblings, sourced
// from the loaded entity instead of a fetched row.
func mergeSiblings(doc query.Document, schema *core.TableSchema, e any) {
	pkCol := schema.IDColumn()
	for _, sib := range schema.Siblings() {
		for col, val := range sib.WriteFields(e) {
			if col == pkCol {
				continue
			}
			doc[col] = val
		}
	}
}

// appendChildren nests each child collection under the name its type declares in
// CollectionName (the same key the Composer and the ViewNode use), one column-keyed Document per
// loaded child element. It covers the root's OWN children AND — when the view is
// rooted at a shared-base role — the shared base's NATIVE children (both are
// loaded onto the same aggregate root and land in AllAggregateItems keyed by type
// name). The loader honored the read scope (active / include-archived), so the
// read-time archived strip is left to the reader's ViewNode, exactly as on the
// Mongo path.
func appendChildren(doc query.Document, schema *core.TableSchema, e domain.Entity) {
	children := schema.ChildSchemas()
	if baseChildren := schema.BaseChildSchemas(); len(baseChildren) > 0 {
		children = append(append([]*core.TableSchema{}, children...), baseChildren...)
	}
	if len(children) == 0 {
		return
	}
	prov, ok := e.(domain.AggregateRootProvider)
	if !ok {
		return
	}
	items := prov.GetAggregateRoot().AllAggregateItems()
	rootID := ""
	if idp := e.GetID(); idp != nil {
		rootID = idp.String()
	}
	for _, child := range children {
		list := items[child.TypeName()]
		arr := make([]query.Document, 0, len(list))
		for _, it := range list {
			arr = append(arr, childDocument(child, it.Item, rootID))
		}
		doc[child.CollectionSegment()] = arr
	}
}

// childDocument maps one child element: business scalars + its own id + managed
// columns (physical, revision NOT remapped) + its ParentID (the root id it was
// joined on) + its flat siblings.
func childDocument(child *core.TableSchema, avo domain.AggregateValueObject, rootID string) query.Document {
	cd := query.Document{}
	mergeWriteFields(cd, child, avo)
	if idCol := child.IDColumn(); idCol != "" {
		cd[idCol] = idColumnValue(avo.GetID())
	}
	if fk := child.ParentIDColumn(); fk != "" && rootID != "" {
		cd[fk] = idColumnValue(domain.NewID(rootID))
	}
	applyManaged(cd, child, avo, child.RevisionColumn())
	mergeSiblings(cd, child, avo)
	return cd
}

// timeValue unwraps a managed *time.Time to a value the JSON parity compare
// normalizes against the Composer's scanned time.Time (nil stays nil == NULL).
func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// idColumnValue renders a domain.ID under an id/parentID column. The exact form
// the Composer stores (raw driver value vs canonical string, per-dialect id
// codecs like MySQL BINARY(16)) is pinned by the parity integration test across
// all four engines; this canonical-string form is the starting point the test
// confirms or corrects.
func idColumnValue(id domain.ID) any {
	return id.String()
}
