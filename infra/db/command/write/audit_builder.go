package write

import (
	"reflect"
	"sort"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// The labelKey-tag map (Go-field-name → catalog key) is read from the schema's
// internals, so it lives on the schema foundation (core.TableSchema.
// LabelKeysByGoField); the auditor calls that exported accessor below.

// The persisted Go-field values of an entity (keyed by Go field name, the
// faithful domain vocabulary the audit timeline speaks) are read from the
// schema's internals, so they come from the schema foundation
// (core.TableSchema.GoFieldValues); the builders below call that method.

// tenantClaim is the JWT claim name from which audit pulls TenantID. The
// default Auth0/Keycloak convention; customizable tenant claims declared via
// auth.authorization.tenant.claim are NOT honored here today (the column
// stays empty when the operator diverges from the default). Documented
// limitation on audit.AuditEvent.TenantID.
const tenantClaim = "tenant_id"

// BuildInsertEvent assembles a kind=snapshot audit.AuditEvent describing the
// successful insertion of insertable, carrying its post-write state.
// auditClaims filters which JWT claims appear in ActorClaims; pass nil or
// empty to omit the block entirely.
func BuildInsertEvent(ctx persistence.RequestContext, i domain.Insertable, id domain.ID, schema *TableSchema, auditClaims []string) audit.AuditEvent {
	ev := audit.AuditEvent{
		EntityType: i.EntityName(),
		EntityID:   id.Value(),
		Verb:       "insert",
		ActionName: i.ActionName(),
		Kind:       "snapshot",
		DateTime:   i.DateTime(),
		Snapshot:   composedFieldValues(schema, i.Source()),
		Children:   childrenOf(schema, i.Source(), "insert"),
	}
	populateContext(&ev, ctx, auditClaims)
	return ev
}

// BuildUpdateEvent assembles a kind=delta audit.AuditEvent describing the
// successful update of updatable. The Changes slice carries only mutated
// columns — unchanged ones absent — and is computed against Old() captured
// before the apply closure ran. Verb is "update" for both PUT and PATCH
// (identical SQL fingerprint); the distinction lives in ActionName.
func BuildUpdateEvent(ctx persistence.RequestContext, u domain.Updatable, schema *TableSchema, auditClaims []string) audit.AuditEvent {
	prev := oldFieldsOf(schema, u.Source())
	cur := composedFieldValues(schema, u.Source())
	labels := composedLabelKeys(schema)
	ev := audit.AuditEvent{
		EntityType: u.EntityName(),
		EntityID:   u.ID(),
		Verb:       "update",
		ActionName: u.ActionName(),
		Kind:       "delta",
		DateTime:   u.DateTime(),
		Changes:    computeChanges(prev, cur, labels),
		Children:   childrenOf(schema, u.Source(), "update"),
	}
	populateContext(&ev, ctx, auditClaims)
	return ev
}

// BuildArchiveEvent assembles a kind=transition audit.AuditEvent describing the
// successful archive of archivable. The verb itself encodes the recovery
// path (the symmetric unarchive); no Snapshot or Changes block is emitted.
func BuildArchiveEvent(ctx persistence.RequestContext, a domain.Archivable, schema *TableSchema, auditClaims []string) audit.AuditEvent {
	ev := audit.AuditEvent{
		EntityType: a.EntityName(),
		EntityID:   a.ID(),
		Verb:       "archive",
		ActionName: a.ActionName(),
		Kind:       "transition",
		DateTime:   a.DateTime(),
		Children:   childrenOf(schema, a.Source(), "archive"),
	}
	populateContext(&ev, ctx, auditClaims)
	return ev
}

// BuildUnarchiveEvent is the symmetric inverse of BuildArchiveEvent.
func BuildUnarchiveEvent(ctx persistence.RequestContext, u domain.Unarchivable, schema *TableSchema, auditClaims []string) audit.AuditEvent {
	ev := audit.AuditEvent{
		EntityType: u.EntityName(),
		EntityID:   u.ID(),
		Verb:       "unarchive",
		ActionName: u.ActionName(),
		Kind:       "transition",
		DateTime:   u.DateTime(),
		Children:   childrenOf(schema, u.Source(), "unarchive"),
	}
	populateContext(&ev, ctx, auditClaims)
	return ev
}

// BuildDeleteEvent assembles a kind=snapshot audit.AuditEvent describing the
// successful hard-delete of deletable. The snapshot prefers Old() (state at
// function entry, captured before the delete SQL fired) and falls back to
// the live source only when Old() is nil (entity built outside the
// framework's GetDeletable path).
func BuildDeleteEvent(ctx persistence.RequestContext, d domain.Deletable, schema *TableSchema, auditClaims []string) audit.AuditEvent {
	snap := oldFieldsOf(schema, d.Source())
	if snap == nil {
		snap = composedFieldValues(schema, d.Source())
	}
	ev := audit.AuditEvent{
		EntityType: d.EntityName(),
		EntityID:   d.ID(),
		Verb:       "delete",
		ActionName: d.ActionName(),
		Kind:       "snapshot",
		DateTime:   d.DateTime(),
		Snapshot:   snap,
		Children:   childrenOf(schema, d.Source(), "delete"),
	}
	populateContext(&ev, ctx, auditClaims)
	return ev
}

// populateContext fills the request-scoped fields of ev (ThreadID, Actor,
// ActorIssuer, ActorClaims, TenantID) from ctx. ActorClaims is filtered
// through auditClaims; TenantID is read from the raw claim map at the
// canonical "tenant_id" key.
func populateContext(ev *audit.AuditEvent, ctx persistence.RequestContext, auditClaims []string) {
	if ctx == nil {
		return
	}
	ev.ThreadID = ctx.ID().String()
	// RequestContext embeds context.Context, so the active span (if any) rides
	// it; capture the trace id once here as the single source both audit
	// destinations mirror.
	ev.TraceID = tracing.TraceIDFromContext(ctx)
	ev.Actor = ctx.ActorSubject()
	ev.ActorIssuer = ctx.ActorIssuer()
	rawClaims := ctx.ActorClaims()
	ev.ActorClaims = filterClaims(rawClaims, auditClaims)
	ev.TenantID = extractTenantID(rawClaims)
}

// extractTenantID returns the value of the "tenant_id" claim coerced to
// string. Missing claim or non-string-coercible value returns "". Tolerates
// the three idiomatic shapes Identity.TenantID also accepts: string,
// []string{one}, []any{one}.
func extractTenantID(claims map[string]any) string {
	if claims == nil {
		return ""
	}
	raw, ok := claims[tenantClaim]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case []string:
		if len(v) == 1 {
			return v[0]
		}
	case []any:
		if len(v) == 1 {
			if s, ok := v[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// filterClaims returns a new map containing only the entries from `all`
// whose keys appear in `allowlist`. Returns nil when either input is empty
// or no allowed key is present — so the audit event's actorClaims block
// stays absent rather than serializing an empty object.
func filterClaims(all map[string]any, allowlist []string) map[string]any {
	if len(all) == 0 || len(allowlist) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, key := range allowlist {
		if v, ok := all[key]; ok {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// composedFieldValues returns the persisted Go-field values of src across the
// schema's OWN fields ∪ its shared-base fields ∪ its sibling fields — the
// complete audited surface of a (possibly SharedBase/sibling-partitioned)
// entity. For a flat schema it degenerates to schema.GoFieldValues(src). The
// entity is a single flat Go struct, so the base and sibling sub-schemas read
// their own fields off the very same value; the audit timeline stays faithful
// to the whole domain object rather than to just the role's own table.
func composedFieldValues(schema *TableSchema, src any) map[string]any {
	out := schema.GoFieldValues(src)
	if base, _, ok := schema.SharedBaseRef(); ok {
		// The shared base is type-less (built with NewSharedBase, no [T]), so its
		// field struct-indexes are unresolved; read the shared fields off the flat
		// role entity by Go field name — exactly how sharedBaseValues feeds the
		// base UPSERT.
		for k, v := range baseFieldValuesByName(base, src) {
			out[k] = v
		}
	}
	for _, sib := range schema.Siblings() {
		for k, v := range sib.GoFieldValues(src) {
			out[k] = v
		}
	}
	return out
}

// baseFieldValuesByName reads the shared-base fields off a flat role entity by
// Go field name (PascalCase), the read strategy a type-less base schema forces.
func baseFieldValuesByName(base *TableSchema, src any) map[string]any {
	rv := reflect.ValueOf(src)
	for rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	out := map[string]any{}
	if rv.Kind() != reflect.Struct {
		return out
	}
	for _, goName := range base.GoFields() {
		if f := rv.FieldByName(goName); f.IsValid() {
			out[goName] = f.Interface()
		}
	}
	return out
}

// composedLabelKeys is the label-map analogue of composedFieldValues: the
// Go-field → catalog-key map unioned across the role, its shared base, and its
// siblings, so a delta over a base/sibling field still carries its label.
func composedLabelKeys(schema *TableSchema) map[string]string {
	out := map[string]string{}
	for k, v := range schema.LabelKeysByGoField() {
		out[k] = v
	}
	if base, _, ok := schema.SharedBaseRef(); ok {
		// The type-less base cannot reflect the entity's struct tags, so its
		// LabelKeysByGoField only carries labels declared explicitly on the base
		// field. Recover the shared fields' `labelKey` tags off the role's Go type
		// by field name — the same by-name strategy composedFieldValues uses for
		// the values — then let any explicit base label win.
		for k, v := range baseLabelKeysByName(base, schema.GoType()) {
			out[k] = v
		}
		for k, v := range base.LabelKeysByGoField() {
			out[k] = v
		}
	}
	for _, sib := range schema.Siblings() {
		for k, v := range sib.LabelKeysByGoField() {
			out[k] = v
		}
	}
	return out
}

// baseLabelKeysByName reads the `labelKey` struct tag of each shared-base field
// off the flat role Go type by field name — the label counterpart of
// baseFieldValuesByName, needed because the base schema is type-less.
func baseLabelKeysByName(base *TableSchema, roleType reflect.Type) map[string]string {
	out := map[string]string{}
	if roleType == nil {
		return out
	}
	for roleType.Kind() == reflect.Ptr {
		roleType = roleType.Elem()
	}
	if roleType.Kind() != reflect.Struct {
		return out
	}
	for _, goName := range base.GoFields() {
		if sf, ok := roleType.FieldByName(goName); ok {
			if tag, ok := sf.Tag.Lookup("labelKey"); ok && tag != "" && tag != "-" {
				out[goName] = tag
			}
		}
	}
	return out
}

// oldFieldsOf returns the pre-mutation snapshot of e keyed by Go field name,
// or nil when e has no Old (Insert path, or entity hydrated outside the
// framework loader). Composed over role ∪ base ∪ siblings like the post-state.
func oldFieldsOf(schema *TableSchema, e domain.Entity) map[string]any {
	if e == nil {
		return nil
	}
	prev := e.Old()
	if prev == nil {
		return nil
	}
	return composedFieldValues(schema, prev)
}

// computeChanges returns a deterministic []audit.FieldChange (sorted by field name)
// listing keys whose pre/post values differ. Unchanged keys are omitted —
// kind=delta carries only the diff, no redundancy with snapshot. Returns nil
// when prev and cur are equal across every key (no observable change).
//
// labelsByField carries the Go-field-name → catalog key map produced by
// labelKeysByGoField(reflect.TypeOf(entity)). Pass nil when the caller does
// not have a type in hand or when no `label` tags are declared on the
// source struct — every emitted FieldChange then carries FieldLabelKey=""
// and the omitempty elides it from the wire.
//
// Keys present in one map but not the other are emitted with the missing
// side as nil. In practice this only happens when the entity's schema
// changed between snapshot capture and current state, which the framework
// avoids by capturing within the same transaction.
func computeChanges(prev, cur map[string]any, labelsByField map[string]string) []audit.FieldChange {
	seen := map[string]struct{}{}
	for k := range prev {
		seen[k] = struct{}{}
	}
	for k := range cur {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]audit.FieldChange, 0, len(keys))
	for _, k := range keys {
		a, b := prev[k], cur[k]
		if reflect.DeepEqual(a, b) {
			continue
		}
		out = append(out, audit.FieldChange{
			Field:         k,
			FieldLabelKey: labelsByField[k],
			From:          a,
			To:            b,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// childrenOf inspects src for AggregateRootProvider and returns the per-child
// audit block, or nil for flat entities and aggregates with no relevant
// entries. The verb selects the per-op interpretation per item — see
// childEventOf for the discrimination rules. The returned map is keyed by
// the Go type name of the AggregateValueObject (e.g. "Address"); iteration
// order over typeNames is sorted so the audit line is deterministic.
func childrenOf(schema *TableSchema, src domain.Entity, verb string) map[string][]audit.ChildEvent {
	if src == nil || schema == nil {
		return nil
	}
	provider, ok := src.(domain.AggregateRootProvider)
	if !ok {
		return nil
	}
	root := provider.GetAggregateRoot()
	if root == nil {
		return nil
	}
	prevByTypeID := oldChildrenIndex(schema, src)
	all := root.AllAggregateItems()
	typeNames := make([]string, 0, len(all))
	for typeName := range all {
		typeNames = append(typeNames, typeName)
	}
	sort.Strings(typeNames)

	out := map[string][]audit.ChildEvent{}
	for _, typeName := range typeNames {
		items := all[typeName]
		// ResolveAggregateChild finds the owning schema whether the collection
		// is a role-native child or a SharedBase base-child (shared by every
		// role); ChildSchema alone would miss base-children and emit op-only
		// events with an empty snapshot.
		child, _, _ := schema.ResolveAggregateChild(typeName)
		var entries []audit.ChildEvent
		for _, it := range items {
			entry, include := childEventOf(it, child, typeName, verb, prevByTypeID)
			if !include {
				continue
			}
			entries = append(entries, entry)
		}
		if len(entries) > 0 {
			out[typeName] = entries
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// oldChildrenIndex returns the pre-mutation children fields indexed by
// typeName → child ID, used by childEventOf to compute CHANGED diffs and
// to surface REMOVED snapshots. Returns nil when src has no Old (Insert
// path or entity hydrated outside the framework loader).
func oldChildrenIndex(schema *TableSchema, src domain.Entity) map[string]map[string]map[string]any {
	if src == nil || schema == nil {
		return nil
	}
	prev := src.Old()
	if prev == nil {
		return nil
	}
	prevProv, ok := prev.(domain.AggregateRootProvider)
	if !ok {
		return nil
	}
	prevRoot := prevProv.GetAggregateRoot()
	if prevRoot == nil {
		return nil
	}
	out := map[string]map[string]map[string]any{}
	for typeName, items := range prevRoot.AllAggregateItems() {
		child, _, _ := schema.ResolveAggregateChild(typeName)
		inner := map[string]map[string]any{}
		for _, it := range items {
			id := it.Item.GetID()
			if id == "" {
				continue
			}
			inner[id] = child.GoFieldValues(it.Item)
		}
		if len(inner) > 0 {
			out[typeName] = inner
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// childEventOf builds a single audit.ChildEvent from the aggregate item + verb +
// optional prior index. The second return is false when the item is not
// observable for the given verb (e.g. Removed-status items on an Archive
// cascade — those are already gone, the cascade doesn't apply to them).
//
// SQL-grounded vocabulary: every child op echoes the SQL fingerprint of the
// row-level change, identical to the root verb vocabulary. Distinct SQL →
// distinct op; identical SQL → identical op.
//
//	op         | SQL on the addresses (child) row
//	-----------+-----------------------------------------------------------
//	inserted   | INSERT INTO addresses (...)
//	updated    | UPDATE addresses SET col=val, updated_at=NOW() WHERE id=$1
//	archived   | UPDATE addresses SET deleted_at=NOW() WHERE id=$1
//	unarchived | UPDATE addresses SET deleted_at=NULL WHERE id=$1
//	deleted    | DELETE FROM addresses (via FK ON DELETE CASCADE on root delete)
//
// Per-verb dispatch:
//
//	insert     : every item → inserted + snapshot (Constructor/Added both)
//	update     : Added → inserted+snapshot; Changed → updated+changes;
//	             Removed → archived+snapshot (SQL is UPDATE deleted_at=NOW;
//	             the row stays in the DB, recoverable via unarchive);
//	             Constructor → skipped (untouched item, no SQL)
//	archive    : non-Removed items → archived + snapshot
//	unarchive  : non-Removed items → unarchived + snapshot
//	delete     : non-Removed items → deleted + snapshot
func childEventOf(
	it domain.AggregateItem[domain.AggregateValueObject],
	child *TableSchema,
	typeName, verb string,
	prevByTypeID map[string]map[string]map[string]any,
) (audit.ChildEvent, bool) {
	id := it.Item.GetID()
	prevFields := func() map[string]any {
		if prevByTypeID == nil {
			return nil
		}
		inner, ok := prevByTypeID[typeName]
		if !ok {
			return nil
		}
		return inner[id]
	}
	currentFields := func() map[string]any { return child.GoFieldValues(it.Item) }

	switch verb {
	case "insert":
		return audit.ChildEvent{ID: id, Op: "inserted", Snapshot: currentFields()}, true
	case "update":
		// Categorize by the persistence operation (OperationOf), not currentStatus
		// alone — so a re-added DB child audits as "updated", not "inserted", etc.,
		// matching what the persister actually does.
		switch domain.OperationOf(it.OriginalStatus, it.CurrentStatus) {
		case domain.OpInsert:
			return audit.ChildEvent{ID: id, Op: "inserted", Snapshot: currentFields()}, true
		case domain.OpUpdate:
			return audit.ChildEvent{ID: id, Op: "updated", Changes: computeChanges(prevFields(), currentFields(), child.LabelKeysByGoField())}, true
		case domain.OpDelete:
			return audit.ChildEvent{ID: id, Op: "archived", Snapshot: prevFields()}, true
		default: // OpNoop
			return audit.ChildEvent{}, false
		}
	case "archive":
		if it.CurrentStatus == domain.StatusRemoved {
			return audit.ChildEvent{}, false
		}
		return audit.ChildEvent{ID: id, Op: "archived", Snapshot: currentFields()}, true
	case "unarchive":
		if it.CurrentStatus == domain.StatusRemoved {
			return audit.ChildEvent{}, false
		}
		return audit.ChildEvent{ID: id, Op: "unarchived", Snapshot: currentFields()}, true
	case "delete":
		if it.CurrentStatus == domain.StatusRemoved {
			return audit.ChildEvent{}, false
		}
		return audit.ChildEvent{ID: id, Op: "deleted", Snapshot: currentFields()}, true
	}
	return audit.ChildEvent{}, false
}
