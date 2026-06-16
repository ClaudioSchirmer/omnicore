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
// infra loader (which registered children via WithChild[V]) and are trusted —
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
		if !reflect.DeepEqual(entry.item, item) {
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
		if reflect.DeepEqual(entry.item, original) {
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
		if reflect.DeepEqual(entry.item, item) && entry.currentStatus != StatusRemoved {
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

func GetAddedItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	return filterAggregateOf[T](ar, StatusAdded)
}

func GetChangedItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	return filterAggregateOf[T](ar, StatusChanged)
}

func GetRemovedItemsOf[T AggregateValueObject](ar *AggregateRoot) []T {
	return filterAggregateOf[T](ar, StatusRemoved)
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

func filterAggregateOf[T AggregateValueObject](ar *AggregateRoot, status AggregateItemStatus) []T {
	items := GetAggregateItemsOf[T](ar)
	out := make([]T, 0, len(items))
	for _, it := range items {
		if it.CurrentStatus == status {
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
// runAggregateValidations would scope it at the boundary (collection name from
// PluralizeSnake(PascalToSnake(typeName)), index = len(current items) of that
// type). Returns true when the scoped context accumulated no notifications,
// false otherwise. Notifications, when emitted, are attached to the root's
// NotificationContext via the scoped child — same shape as boundary validation.
//
// Use only when the root's domain method wants to reject a malformed item
// SYNCHRONOUSLY at add time (instead of accumulating notifications until
// GetInsertable/GetUpdatable). Pitfall: if you both call ValidateAggregateChild
// AND let the item enter the collection, the boundary validation will run
// BuildRules again and you'll get duplicate notifications. Pick one path per
// item — boundary validation is the default.
func ValidateAggregateChild(
	root AggregateRootProvider,
	item AggregateValueObject,
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
	collectionName := PluralizeSnake(PascalToSnake(typeName))
	existing := ar.aggregates[typeName]
	idx := 0
	for _, e := range existing {
		if e.currentStatus != StatusRemoved {
			idx++
		}
	}
	scoped := rootCtx.Scoped(NameSegment(collectionName), IndexSegment(idx))
	before := len(scoped.Messages())
	mode := modeFromActionName(actionName)
	item.BuildRules(actionName, svc, NewRules(mode, scoped, reflect.TypeOf(item)))
	return len(scoped.Messages()) == before
}

// modeFromActionName maps the standard framework action names back to their
// EntityMode. Custom action names (e.g. "AdminCreate") default to ModeInsert
// since they typically branch flavours within insert/update flows — callers
// that need a specific mode should pass the standard action names.
func modeFromActionName(actionName string) EntityMode {
	switch actionName {
	case "GetUpdatable", "Update":
		return ModeUpdate
	case "GetDeletable", "Delete":
		return ModeDelete
	case "GetArchivable", "Archive":
		return ModeArchive
	case "GetUnarchivable", "Unarchive":
		return ModeUnarchive
	default:
		return ModeInsert
	}
}

// classNameOfVO returns the type name of the generic parameter VO without
// instantiating. Works for VO as a value type or pointer type.
func classNameOfVO[VO AggregateValueObject]() string {
	t := reflect.TypeOf((*VO)(nil)).Elem()
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}
