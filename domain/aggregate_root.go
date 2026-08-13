package domain

import (
	"reflect"
	"sync"
)

type aggregateItemEntry struct {
	item           AggregateValueObject
	originalStatus AggregateItemStatus
	currentStatus  AggregateItemStatus
}

type AggregateRoot struct {
	BaseEntity
	aggregates map[string][]aggregateItemEntry
}

func (ar *AggregateRoot) ensureAggregates() {
	if ar.aggregates == nil {
		ar.aggregates = map[string][]aggregateItemEntry{}
	}
}

// AggregateConstructor is the load-from-DB path. typeNames here come from the
// infra loader (which declares children on the TableSchema) and are trusted —
// no type-guard runs. End-users should not call this directly; use the
// AddAggregateChild / ChangeAggregateChild / RemoveAggregateChild top-level
// functions instead, which enforce the type set declared by the root.
func (ar *AggregateRoot) AggregateConstructor(items []AggregateValueObject) {
	ar.ensureAggregates()
	for _, item := range items {
		if !ar.isAggregateItemValid(item) {
			continue
		}
		key := classNameOf(item)
		ar.aggregates[key] = append(ar.aggregates[key], aggregateItemEntry{
			item:           item,
			originalStatus: StatusConstructor,
			currentStatus:  StatusConstructor,
		})
	}
}

// addAggregateItem appends/re-activates an item in the internal map. Unchecked
// against the root's declared AggregateChildren() set — callers must use the
// top-level AddAggregateChild for the type-guarded path.
func (ar *AggregateRoot) addAggregateItem(item AggregateValueObject) {
	if !ar.isAggregateItemValid(item) {
		return
	}
	ar.ensureAggregates()
	key := classNameOf(item)
	list := ar.aggregates[key]

	for i, entry := range list {
		if !entry.item.IsSameBusinessIdentity(item) {
			continue
		}
		switch entry.currentStatus {
		case StatusAdded, StatusConstructor:
			ar.AddNotificationMessage(NotificationMessage{
				FieldName:    key,
				Notification: EntityAlreadyAddedNotification{},
			})
			return
		case StatusRemoved, StatusChanged:
			// Reactivate carrying the RE-SENT values, preserving the tracked id
			// so a re-added DB child resolves to an in-place UPDATE
			// (OperationOf(CONSTRUCTOR, ADDED)) that writes the INCOMING field
			// values — not the stale tracked ones. Leaving list[i].item as the
			// old value would silently drop edits to non-identity fields on a
			// full-replace PUT (the identity matched, but the payload changed a
			// field outside the identity). A never-persisted item has an empty
			// id, so it reactivates with the re-sent values and no id (a fresh
			// INSERT). Same-identity, id-agnostic re-send therefore updates in
			// place instead of churning the id, without losing any change.
			next := item
			if id := entry.item.GetID(); !id.IsEmpty() {
				if stamped, ok := withItemID(item, id.Value()); ok {
					next = stamped
				}
			}
			list[i].item = next
			list[i].currentStatus = StatusAdded
			ar.aggregates[key] = list
			return
		}
	}

	ar.aggregates[key] = append(list, aggregateItemEntry{
		item:           item,
		originalStatus: StatusAdded,
		currentStatus:  StatusAdded,
	})
}

// changeAggregateItem replaces an existing item with a new one (status
// CHANGED). Unchecked against the declared set.
func (ar *AggregateRoot) changeAggregateItem(original, replacement AggregateValueObject) {
	if !ar.isAggregateItemValid(original) || !ar.isAggregateItemValid(replacement) {
		return
	}
	ar.ensureAggregates()
	key := classNameOf(original)
	list := ar.aggregates[key]

	for i, entry := range list {
		if entry.item.IsSameBusinessIdentity(original) {
			list[i].item = replacement
			list[i].currentStatus = StatusChanged
			ar.aggregates[key] = list
			return
		}
	}

	ar.AddNotificationMessage(NotificationMessage{
		FieldName:    key,
		Notification: EntityDoesNotExistNotification{},
	})
}

// removeAggregateItem marks an item as REMOVED. Unchecked.
func (ar *AggregateRoot) removeAggregateItem(item AggregateValueObject) {
	if item == nil {
		ar.AddNotificationMessage(NotificationMessage{
			Notification: EntityDoesNotExistNotification{},
		})
		return
	}
	ar.ensureAggregates()
	key := classNameOf(item)
	list := ar.aggregates[key]

	for i, entry := range list {
		if entry.item.IsSameBusinessIdentity(item) && entry.currentStatus != StatusRemoved {
			list[i].currentStatus = StatusRemoved
			ar.aggregates[key] = list
			return
		}
	}

	ar.AddNotificationMessage(NotificationMessage{
		FieldName:    key,
		Notification: EntityDoesNotExistNotification{},
	})
}

func (ar *AggregateRoot) ClearAggregateItemsOfType(typeName string) {
	if ar.aggregates == nil {
		return
	}
	list := ar.aggregates[typeName]
	for i := range list {
		list[i].currentStatus = StatusRemoved
	}
	ar.aggregates[typeName] = list
}

// AssignAggregateItemID stamps a persistence-minted id onto a tracked item —
// the write-back half of child insertion. The relational persister mints each
// child's ID inside the INSERT (the domain never generates child ids); this
// method lets it reflect that id back into the aggregate map, so post-write
// readers (FromEntity result projections, the audit/outbox snapshots) see the
// child exactly as persisted instead of with an empty id. It matches the
// tracked entry by IsSameBusinessIdentity against the PRE-assignment value and
// sets the item's id through its promoted domain.Managed.SetID; statuses are
// left untouched. Returns false when the item isn't tracked — callers treat
// that as "no write-back possible", never an error. Every aggregate child
// embeds domain.Managed, so SetID is always available.
func (ar *AggregateRoot) AssignAggregateItemID(item AggregateValueObject, id string) bool {
	if item == nil || ar.aggregates == nil {
		return false
	}
	stamped, ok := withItemID(item, id)
	if !ok {
		return false
	}
	key := classNameOf(item)
	list := ar.aggregates[key]
	for i, entry := range list {
		if entry.item.IsSameBusinessIdentity(item) {
			list[i].item = stamped
			ar.aggregates[key] = list
			return true
		}
	}
	return false
}

// withItemID returns a copy of item with its id set through the promoted
// domain.Managed.SetID. Value-object items are plain structs (change tracking
// depends on it); a non-struct reports false. Every AVO embeds Managed, so the
// SetID assertion on the addressable copy always succeeds.
func withItemID(item AggregateValueObject, id string) (AggregateValueObject, bool) {
	t := reflect.TypeOf(item)
	if t == nil || t.Kind() != reflect.Struct {
		return nil, false
	}
	v := reflect.New(t).Elem()
	v.Set(reflect.ValueOf(item))
	s, ok := v.Addr().Interface().(interface{ SetID(ID) })
	if !ok {
		return nil, false
	}
	s.SetID(NewID(id))
	out, ok := v.Interface().(AggregateValueObject)
	return out, ok
}

func (ar *AggregateRoot) AllAggregateItems() map[string][]AggregateItem[AggregateValueObject] {
	out := map[string][]AggregateItem[AggregateValueObject]{}
	for k, list := range ar.aggregates {
		items := make([]AggregateItem[AggregateValueObject], len(list))
		for i, entry := range list {
			items[i] = AggregateItem[AggregateValueObject]{
				Item:           entry.item,
				OriginalStatus: entry.originalStatus,
				CurrentStatus:  entry.currentStatus,
			}
		}
		out[k] = items
	}
	return out
}

func (ar *AggregateRoot) isAggregateItemValid(item AggregateValueObject) bool {
	if item == nil {
		ar.AddNotificationMessage(NotificationMessage{
			Notification: EntityDoesNotExistNotification{},
		})
		return false
	}
	return true
}

func GetAggregateItemsOf[T AggregateValueObject](ar *AggregateRoot) []AggregateItem[T] {
	var zero T
	key := classNameOf(zero)
	if key == "" {
		key = classNameOf((*T)(nil))
	}
	list := ar.aggregates[key]
	out := make([]AggregateItem[T], 0, len(list))
	for _, entry := range list {
		if v, ok := entry.item.(T); ok {
			out = append(out, AggregateItem[T]{
				Item:           v,
				OriginalStatus: entry.originalStatus,
				CurrentStatus:  entry.currentStatus,
			})
		}
	}
	return out
}

// GetAddedItemsOf / GetChangedItemsOf / GetRemovedItemsOf categorize children by
// the persistence operation (OperationOf), which crosses original + current status
// — NOT currentStatus alone. A DB item re-added or changed is "changed" (UPDATE),
// not "added"; a brand-new item added then removed is in none of them (no-op).
func GetAddedItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	return filterAggregateByOp[T](ar, OpInsert)
}

func GetChangedItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	return filterAggregateByOp[T](ar, OpUpdate)
}

func GetRemovedItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	return filterAggregateByOp[T](ar, OpDelete)
}

func GetCurrentItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	items := GetAggregateItemsOf[T](ar)
	out := make([]T, 0, len(items))
	for _, it := range items {
		if it.CurrentStatus != StatusRemoved {
			out = append(out, it.Item)
		}
	}
	return out
}

func filterAggregateByOp[T AggregateValueObject](ar *AggregateRoot, op AggregateItemOp) []T {
	items := GetAggregateItemsOf[T](ar)
	out := make([]T, 0, len(items))
	for _, it := range items {
		if OperationOf(it.OriginalStatus, it.CurrentStatus) == op {
			out = append(out, it.Item)
		}
	}
	return out
}

// ─── Phase 20: typed primitives with root-declared boundary ─────────────────
//
// Public surface for mutating an aggregate root's collection. Each primitive
// consults root.AggregateChildren() and rejects items whose typeName is not
// declared — emitting InvalidAggregateChildNotification on the root's
// NotificationContext. End-users typically don't call these directly: the
// idiomatic DDD form is to expose domain methods on the root itself
// (u.AddAddress, u.RemoveAddress, …) that delegate here after enforcing
// business invariants.

var allowedChildrenCache sync.Map // map[reflect.Type]map[string]struct{}

// allowedChildTypeNames returns the set of typeNames declared by root.
// Cached per concrete root type (reflect.Type of the implementer).
func allowedChildTypeNames(root AggregateRootProvider) map[string]struct{} {
	t := reflect.TypeOf(root)
	if cached, ok := allowedChildrenCache.Load(t); ok {
		return cached.(map[string]struct{})
	}
	set := map[string]struct{}{}
	for _, sample := range root.AggregateChildren() {
		set[classNameOf(sample)] = struct{}{}
	}
	allowedChildrenCache.Store(t, set)
	return set
}

func isAllowedChild(root AggregateRootProvider, item AggregateValueObject) bool {
	if item == nil {
		return false
	}
	_, ok := allowedChildTypeNames(root)[classNameOf(item)]
	return ok
}

// ensureRootInit guarantees the root's NotificationContext exists before any
// primitive emits a notification. AggregateRootProvider implementers always
// also implement Entity (BaseEntity embedded), so the type assertion is safe in
// practice. Without this, type-guard rejections at command time (before
// GetInsertable) would be silently dropped because BaseEntity.AddNotification
// no-ops when notifCtx is nil.
func ensureRootInit(root AggregateRootProvider) {
	if e, ok := root.(Entity); ok {
		ensureInit(e)
	}
}

// rejectChild emits InvalidAggregateChildNotification on the root's context
// with the offending typeName echoed as FieldValue.
func rejectChild(root AggregateRootProvider, item AggregateValueObject) {
	ar := root.GetAggregateRoot()
	if ar == nil {
		return
	}
	typeName := ""
	if item != nil {
		typeName = classNameOf(item)
	}
	ar.AddNotificationMessage(NotificationMessage{
		FieldName:    typeName,
		FieldValue:   typeName,
		Notification: InvalidAggregateChildNotification{},
	})
}

// AddAggregateChild appends an item to the aggregate after checking it against
// root.AggregateChildren(). Items of undeclared types are rejected: an
// InvalidAggregateChildNotification is emitted on the root's NotificationContext
// and the internal collection is left untouched.
func AddAggregateChild(root AggregateRootProvider, item AggregateValueObject) {
	ensureRootInit(root)
	if !isAllowedChild(root, item) {
		rejectChild(root, item)
		return
	}
	root.GetAggregateRoot().addAggregateItem(item)
}

// ChangeAggregateChild replaces an item with a new one (status CHANGED) after
// checking both original and replacement against root.AggregateChildren().
func ChangeAggregateChild(root AggregateRootProvider, original, replacement AggregateValueObject) {
	ensureRootInit(root)
	if !isAllowedChild(root, original) {
		rejectChild(root, original)
		return
	}
	if !isAllowedChild(root, replacement) {
		rejectChild(root, replacement)
		return
	}
	root.GetAggregateRoot().changeAggregateItem(original, replacement)
}

// RemoveAggregateChild marks an item as REMOVED after checking it against
// root.AggregateChildren(). Items of undeclared types are rejected the same
// way as AddAggregateChild.
func RemoveAggregateChild(root AggregateRootProvider, item AggregateValueObject) {
	ensureRootInit(root)
	if !isAllowedChild(root, item) {
		rejectChild(root, item)
		return
	}
	root.GetAggregateRoot().removeAggregateItem(item)
}

// ReplaceAggregateChildrenOf substitutes the entire collection of items of the
// generic VO type. Equivalent to ClearAggregateItemsOfType(typeName) followed
// by a loop of AddAggregateChild. Each item is checked against
// root.AggregateChildren(); items of undeclared types are rejected and skipped
// (Clear still runs for the requested typeName, so the previous collection is
// always wiped before adding).
//
// Typical use in UpdateCommand.ApplyTo (when the root doesn't expose a
// dedicated domain method like ReplaceAddresses):
//
//	domain.ReplaceAggregateChildrenOf(u, addrs)
func ReplaceAggregateChildrenOf[VO AggregateValueObject](root AggregateRootProvider, items []VO) {
	ensureRootInit(root)
	typeName := classNameOfVO[VO]()
	root.GetAggregateRoot().ClearAggregateItemsOfType(typeName)
	for _, it := range items {
		AddAggregateChild(root, it)
	}
}

// ValidateAggregateChild is the optional inline validation primitive (Phase 20).
// Runs item.BuildRules with a NotificationContext scoped exactly the same way
// runAggregateValidations would scope it at the boundary (collection name as
// the item declares it in CollectionName, lower-camel for the wire; index =
// len(current items) of that type). Returns true when the scoped context accumulated no notifications,
// false otherwise. Notifications, when emitted, are attached to the root's
// NotificationContext via the scoped child — same shape as boundary validation.
//
// Use only when the root's domain method wants to reject a malformed item
// SYNCHRONOUSLY at add time (instead of accumulating notifications until
// GetInsertable/GetUpdatable). Pitfall: if you both call ValidateAggregateChild
// AND let the item enter the collection, the boundary validation will run
// BuildRules again and you'll get duplicate notifications. Pick one path per
// item — boundary validation is the default.
//
// The caller passes the EntityMode explicitly (the same mode the boundary
// runAggregateValidations would dispatch under) so the item's IfInsert/IfUpdate/
// IfArchive/… closures fire correctly; actionName is the free-form label the
// item's BuildRules receives verbatim (audit parity, upsert-flavor branch).
func ValidateAggregateChild(
	root AggregateRootProvider,
	item AggregateValueObject,
	mode EntityMode,
	actionName string,
	svc Service,
) bool {
	if item == nil {
		return false
	}
	ensureRootInit(root)
	ar := root.GetAggregateRoot()
	if ar == nil {
		return false
	}
	rootCtx := ar.NotificationContext()
	if rootCtx == nil {
		return false
	}
	typeName := classNameOf(item)
	collectionName := childCollectionSegment(reflect.TypeOf(item))
	existing := ar.aggregates[typeName]
	idx := 0
	for _, e := range existing {
		if e.currentStatus != StatusRemoved {
			idx++
		}
	}
	scoped := rootCtx.scopedForType(reflect.TypeOf(item), NameSegment(collectionName), IndexSegment(idx))
	before := len(scoped.Messages())
	childRules := NewRules(mode, scoped, reflect.TypeOf(item))
	item.BuildRules(actionName, svc, childRules)
	validateValueObjectFields(item, scoped, childRules.ignoredValueObjects(), childRules.forcedValueObjects())
	return len(scoped.Messages()) == before
}

// classNameOfVO returns the type name of the generic parameter VO without
// instantiating. Works for VO as a value type or pointer type.
func classNameOfVO[VO AggregateValueObject]() string {
	t := reflect.TypeOf((*VO)(nil)).Elem()
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}
