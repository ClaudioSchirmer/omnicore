package relational

import (
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// A declared read join adds Go fields to the loaded entity that the TableSchema
// does not know about. BuildDocument is schema-driven and ViewNode.ToGoDoc
// translates by column — both DROP a field with no column behind it — so the join
// values are placed here, after the translation, straight from the entity.
//
// They are read by GO NAME because that is what they already are: the join
// declaration mapped a column of the other side onto a field of THIS entity, and
// the served document is Go-keyed. There is no column to translate and no
// vocabulary to invent.

// applyJoinFields fills the declared join fields into an already-translated
// document: the root's onto the document itself, each child's onto the elements
// of that child's segment.
//
// The child pass matches by POSITION, and the two sides cannot slip. Both iterate
// the same AllAggregateItems() slice — appendChildren emitted the segment from it
// and ToGoDoc preserved the order translating it — and nothing removes an element
// in between: StripArchivedChildren only runs when the read is ACTIVE-scoped, and
// under that scope the loader never fetched an archived child in the first place,
// so it has nothing to strip. Under ?includeArchived it does not run at all.
func applyJoinFields(goDoc map[string]any, e domain.Entity, schema *core.TableSchema, joinFields map[string][]string) {
	if len(joinFields) == 0 || goDoc == nil {
		return
	}
	if names := joinFields[schema.Table()]; len(names) > 0 {
		copyGoFields(goDoc, e, names)
	}
	applyChildJoinFields(goDoc, e, schema, joinFields)
}

// applyChildJoinFields fills each child segment's join fields from the child
// items the aggregate carries. It walks the shared base's native children
// alongside the root's own only so the two passes read the same set appendChildren
// built — a join on a base child is refused at declaration (it belongs to the
// base's own repository), so those entries are never populated.
func applyChildJoinFields(goDoc map[string]any, e domain.Entity, schema *core.TableSchema, joinFields map[string][]string) {
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
	for _, child := range children {
		names := joinFields[child.Table()]
		if len(names) == 0 {
			continue
		}
		list := items[child.TypeName()]
		seg, _ := goDoc[child.CollectionSegment()].([]any)
		for i := range seg {
			if i >= len(list) {
				break
			}
			el, ok := seg[i].(map[string]any)
			if !ok {
				continue
			}
			copyGoFields(el, list[i].Item, names)
		}
	}
}

// copyGoFields reads each named Go field off src and writes it into doc under the
// same name. A field that is not there is skipped rather than zero-filled: the
// declaration was validated at boot, so an absence here means the caller passed
// something other than the entity the join was declared over.
//
// A nil pointer field lands as a plain nil, never as a typed nil inside the any.
// That is the document's own convention — every other NULL in it (an absent
// composite part, a live row's deleted_at) is present-with-nil, the shape a
// fetched NULL takes on the projection side — and a left join with no
// counterpart is a NULL like any other. Leaking the typed nil instead would make
// "there is no counterpart" indistinguishable from the zero value one layer up.
func copyGoFields(doc map[string]any, src any, names []string) {
	rv := reflect.ValueOf(src)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	for _, name := range names {
		fv := rv.FieldByName(name)
		if !fv.IsValid() || !fv.CanInterface() {
			continue
		}
		if fv.Kind() == reflect.Pointer && fv.IsNil() {
			doc[name] = nil
			continue
		}
		doc[name] = fv.Interface()
	}
}
