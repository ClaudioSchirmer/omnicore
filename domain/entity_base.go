package domain

import (
	"reflect"

	"github.com/google/uuid"
)

type Entity interface {
	Modes() []EntityMode
	RequiresService() bool
	BuildRules(actionName string, service Service, r *Rules)

	NotificationContext() *NotificationContext
	Events() []DomainEvent
	GetID() *ID
	SetID(ID)

	// Old returns the state the entity was BORN with — the snapshot taken when
	// the framework hydrated it from the system of record (see CaptureOld).
	// Nil for Insert (no prior state by definition) and for an entity built by
	// hand that no framework path ever snapshotted. Inside BuildRules prefer
	// the typed helper domain.Old[T].
	Old() Entity

	initWithName(string, reflect.Type)
	resetEntity()
	setMode(EntityMode)
	getMode() EntityMode
	setService(Service)
	getService() Service
	getSignature() uuid.UUID
	setOldEntity(Entity)
	aggregateValueObjectsToValidate() []avoEntry
	contextCollection() []*NotificationContext
	fieldAliases() map[string]string
}

type voEntry struct {
	name string
	vo   ValueObjectValidator
}

type avoEntry struct {
	name string
	avo  AggregateValueObject
}

type BaseEntity struct {
	Managed   // id + revision + managed timestamps; GetID() ID / SetID / ClearID promoted
	signature uuid.UUID
	mode      EntityMode
	service   Service
	notifCtx  *NotificationContext
	contexts  []*NotificationContext
	events    []DomainEvent
	avos      []avoEntry
	old       Entity
	aliases   map[string]string
	className string
}

// RequiresService is the default for the Entity interface contract. Promoted
// via embed to any entity that embeds BaseEntity (directly or through
// AggregateRoot). Entities that need a domain.Service injected for BuildRules
// override by declaring their own RequiresService returning true.
func (b *BaseEntity) RequiresService() bool { return false }

// NotificationContext returns the entity's context, creating it on first use.
// It is never nil: an entity carries a place to put notifications from the
// moment anyone asks for one, with no initialization step for the developer to
// remember.
func (b *BaseEntity) NotificationContext() *NotificationContext { return b.ensureContext() }
func (b *BaseEntity) Events() []DomainEvent                     { return b.events }

// ensureContext returns the notification context, allocating it on first use.
// Every emit helper goes through it, so a notification raised before the
// framework has seen the entity — inside a constructor, or inside a root domain
// method called from a command's ToEntity, ahead of GetInsertable — is stored
// rather than dropped.
//
// The context is born ANONYMOUS. A method whose receiver is *BaseEntity cannot
// see the type that embeds it (reflect.TypeOf(b) is *BaseEntity, never *User),
// and the two things that need that type are precisely the context name and the
// entityType that resolves `labelKey:"…"` tags. initWithName stamps both the
// first time the framework holds the concrete Entity and backfills the labels of
// whatever was emitted in the meantime. Naming is therefore a late concern;
// emitting never is.
func (b *BaseEntity) ensureContext() *NotificationContext {
	if b.notifCtx == nil {
		b.notifCtx = NewNotificationContext("")
		b.mode = ModeDisplay
		b.signature = uuid.New()
	}
	// Never reassigned: AddFieldNameAlias may legitimately run before the first
	// emission, and its map must survive the context's birth.
	if b.aliases == nil {
		b.aliases = map[string]string{}
	}
	return b.notifCtx
}

// GetID shadows the promoted Managed.GetID() ID with the Entity contract's
// nullable *ID form (nil = not persisted). SetID / ClearID come promoted from
// Managed.
func (b *BaseEntity) GetID() *ID { return b.idPtr() }

func (b *BaseEntity) AddNotificationMessage(msg NotificationMessage) {
	b.ensureContext().AddNotificationMessage(msg)
}

// AddNotification is the convenience emit helper shared with
// NotificationContext.AddNotification and Rules.AddNotification — same form,
// same semantics. Inside BuildRules, prefer r.AddNotification (the *Rules
// already carries the right ctx and is identical between root and AVO).
// Outside BuildRules (custom helpers, kernel notifications), use this when you
// already hold the entity reference. The optional value variadic populates
// FieldValue (use to echo rejected input).
func (b *BaseEntity) AddNotification(name string, n Notification, value ...any) {
	b.ensureContext().AddNotification(name, n, value...)
}

func (b *BaseEntity) AddNotificationContext(ctx *NotificationContext) {
	b.contexts = append(b.contexts, ctx)
}

func (b *BaseEntity) ValidateAggregateValueObject(name string, avo AggregateValueObject) {
	b.avos = append(b.avos, avoEntry{name: name, avo: avo})
}

func (b *BaseEntity) ValidateAggregateValueObjects(name string, avos []AggregateValueObject) {
	for _, avo := range avos {
		b.ValidateAggregateValueObject(name, avo)
	}
}

func (b *BaseEntity) RegisterEvent(event DomainEvent) {
	b.events = append(b.events, event)
}

// Old returns the birth-time snapshot captured by the framework (CaptureOld
// at hydration; the Get* family as the fallback floor). Inside BuildRules,
// prefer the typed helper domain.Old[T] over a manual type assertion on this
// return value.
func (b *BaseEntity) Old() Entity { return b.old }

func (b *BaseEntity) AddFieldNameAlias(orig, new string) {
	if b.aliases == nil {
		b.aliases = map[string]string{}
	}
	b.aliases[orig] = new
}

// initWithName stamps the identity the context could not know at birth: the
// class name it reports and the type whose `labelKey:"…"` tags its emissions
// resolve against. The framework calls it (through ensureInit) at every entry
// point that holds the concrete Entity, so it must be idempotent — the guard is
// the class name, not the context, which may already exist because the entity
// emitted before the framework ever saw it.
func (b *BaseEntity) initWithName(name string, entityType reflect.Type) {
	ctx := b.ensureContext()
	if b.className != "" {
		return
	}
	b.className = name
	ctx.context = name
	ctx.entityType = entityType
	ctx.resolvePendingLabels()
}

func (b *BaseEntity) resetEntity() {
	// Phase 20: notifCtx is intentionally NOT cleared here. Notifications added
	// during construction (e.g., by root domain methods like User.AddAddress
	// detecting duplicates) must survive until checkAllNotifications runs at
	// the end of validateForX — otherwise the framework would silently drop
	// construction-time invariant violations. IsValid clears explicitly when
	// it really needs a fresh check.
	b.events = nil
	b.avos = nil
	b.contexts = nil
	b.signature = uuid.New()
	b.mode = ModeDisplay
	b.service = nil
}

func (b *BaseEntity) setMode(m EntityMode)                        { b.mode = m }
func (b *BaseEntity) getMode() EntityMode                         { return b.mode }
func (b *BaseEntity) setService(s Service)                        { b.service = s }
func (b *BaseEntity) getService() Service                         { return b.service }
func (b *BaseEntity) getSignature() uuid.UUID                     { return b.signature }
func (b *BaseEntity) setOldEntity(p Entity)                       { b.old = p }
func (b *BaseEntity) aggregateValueObjectsToValidate() []avoEntry { return b.avos }
func (b *BaseEntity) contextCollection() []*NotificationContext   { return b.contexts }
func (b *BaseEntity) fieldAliases() map[string]string             { return b.aliases }

// CaptureOld snapshots e as its old state, unless it already carries one. It
// is the framework's birth-time hook: the relational loader's single-entity
// path calls it the moment an aggregate finishes hydrating, and the write-side
// load helpers (persistence.LoadForWrite / LoadArchivedForWrite) call it for
// repositories that bypass that loader. From then on domain.Old[T] answers the
// PERSISTED state for all five state-changing verbs — Update, PartialUpdate,
// Archive, Unarchive and Delete alike — no matter where the mutation happens
// (a Command's ApplyTo, a BuildRules closure, a domain method).
//
// A MANUAL handler gets this for free as long as it loads through the
// framework's loader. It must call CaptureOld itself only when it hydrates an
// entity some other way (a hand-rolled repository that does not use
// read.AggregateLoader, an entity assembled in memory) AND intends to mutate
// it before a Get* call — call it immediately after hydration, before the
// first mutation. Calling it late is not an error but records the wrong
// state, so the framework never calls it late: a second call is a NO-OP, which
// makes the earliest snapshot the winning one.
//
// Insert is deliberately outside this contract — a freshly constructed entity
// has no prior state, so Old() stays nil there.
func CaptureOld(e Entity) {
	if e == nil {
		return
	}
	ensureInit(e)
	captureOldIfAbsent(e)
}

// GetInsertable runs the framework's Insert validation pipeline on e and
// returns a ValidEntity ready for orchestration. actionName identifies the
// verb to BuildRules; pass the canonical "GetInsertable" for default rigor or
// a custom string (e.g. "AdminCreate") when two endpoints fire the same
// ModeInsert but demand different invariants — the value reaches both the
// root's BuildRules and every aggregate child's BuildRules verbatim through
// runAggregateValidations.
func GetInsertable(e Entity, service Service, actionName string) (Insertable, error) {
	return getInsertable(e, service, actionName)
}

// GetUpdatable is the closure-form constructor for Updatable. The apply
// callback receives the entity AFTER the framework has captured the
// pre-mutation snapshot (domain.Old / Old), so any field comparison
// inside BuildRules sees the prior state via domain.Old[T](e). Pass
// cmd.ApplyTo or cmd.ApplyPartiallyTo directly — same shape.
//
//	updatable, err := domain.GetUpdatable(loaded, cmd.ApplyTo, svc, "GetUpdatable")
//
// actionName identifies the verb (canonical "GetUpdatable" for default PUT
// rigor, custom string when a handler wants branch-specific rules). A nil
// apply is allowed (degenerate no-op mutation) for callers that want to
// produce an Updatable from an already-mutated entity; in that case Old()
// captures the post-mutation state and the transition-aware invariants do
// not work. The recommended path is to always pass a real apply.
func GetUpdatable[T Entity](e T, apply func(T) error, service Service, actionName string) (Updatable, error) {
	return getUpdatable(e, apply, service, actionName, false)
}

// GetPartialUpdatable is the PATCH twin of GetUpdatable — same shape, same
// pipeline, only the canonical actionName differs ("GetPartialUpdatable").
// The Auto handler PartialUpdateCommandHandler calls this with
// cmd.ApplyPartiallyTo. Pass a custom actionName to differentiate strictness
// per endpoint. The resulting Updatable carries IsPartial() == true so the
// write path scopes sibling updates to the provided set; the PUT vs PATCH
// distinction reaches the audit event through actionName, not the verb (both
// emit verb=update).
func GetPartialUpdatable[T Entity](e T, apply func(T) error, service Service, actionName string) (Updatable, error) {
	return getUpdatable(e, apply, service, actionName, true)
}

// GetDeletable validates e for hard delete. Pass canonical "GetDeletable" or
// a custom actionName for branching in BuildRules.
func GetDeletable(e Entity, service Service, actionName string) (Deletable, error) {
	return getDeletable(e, service, actionName)
}

// GetArchivable validates e for archive. BuildRules fires in ModeArchive, so
// IfArchive closures dispatch on it. The supplied actionName still reaches
// BuildRules and the audit event as a label — pass canonical "GetArchivable"
// or a custom string.
func GetArchivable(e Entity, service Service, actionName string) (Archivable, error) {
	return getArchivable(e, service, actionName)
}

// GetUnarchivable validates e for archive restore. Symmetric to GetArchivable —
// BuildRules runs in ModeUnarchive, so IfUnarchive closures dispatch on it.
// Pass canonical "GetUnarchivable" or a custom string.
func GetUnarchivable(e Entity, service Service, actionName string) (Unarchivable, error) {
	return getUnarchivable(e, service, actionName)
}

// extractAggregateMeta returns a populated *aggregateMeta when e implements
// AggregateRootProvider, or nil for non-aggregate entities. Used by the
// Get* functions to attach aggregate context to the ValidEntity, which the
// infra.Postgres methods consume to decide between simple and aggregate paths.
//
// Phase 19: aggregateMeta only carries the root pointer. Children types are
// discovered via reflection on root.AllAggregateItems(); table/ParentID inferred by
// infra (with optional per-Repository overrides).
func extractAggregateMeta(e Entity) *aggregateMeta {
	provider, ok := e.(AggregateRootProvider)
	if !ok {
		return nil
	}
	return &aggregateMeta{
		root: provider.GetAggregateRoot(),
	}
}

func IsValid(e Entity, mode EntityMode, service Service) (bool, []*NotificationContext) {
	ensureInit(e)
	// IsValid is the "fresh check" path — clear prior accumulated notifications
	// explicitly. getInsertable/getUpdatable/etc. don't clear because they're
	// one-shot validations on a freshly constructed entity, and root domain
	// methods may have emitted construction-time notifications that must reach
	// checkAllNotifications.
	e.NotificationContext().Clear()
	e.resetEntity()
	e.setService(service)

	switch mode {
	case ModeInsert:
		_ = validateForInsert(e, "isValid")
	case ModeUpdate:
		_ = validateForUpdate(e, "isValid")
	case ModeDelete:
		_ = validateForDelete(e, "isValid")
	case ModeArchive:
		_ = validateForArchive(e, "isValid")
	case ModeUnarchive:
		_ = validateForUnarchive(e, "isValid")
	}

	contexts := collectContexts(e)
	return len(contexts) == 0, contexts
}

func getInsertable(e Entity, service Service, actionName string) (Insertable, error) {
	ensureInit(e)
	e.resetEntity()
	e.setService(service)

	if err := validateForInsert(e, actionName); err != nil {
		return Insertable{}, err
	}

	name := classNameOf(e)
	builder := newBuilder(name, actionName, e.getSignature(), e.Events()).
		withAggregate(extractAggregateMeta(e))
	return builder.insertable(e, e.GetID()), nil
}

func getUpdatable[T Entity](e T, apply func(T) error, service Service, actionName string, partial bool) (Updatable, error) {
	ensureInit(e)
	// The birth-time snapshot (CaptureOld at hydration) is preserved; this only
	// fires for an entity no framework path ever snapshotted, keeping the apply
	// below outside Old() either way. The clone is a "ghost": exported fields
	// only, no events / notifCtx / aggregate state machinery. For aggregates it
	// also receives a deep copy of the current items so domain.Old(e) exposes
	// the prior children too.
	captureOldIfAbsent(e)
	if apply != nil {
		if err := apply(e); err != nil {
			return Updatable{}, err
		}
	}
	e.resetEntity()
	e.setService(service)

	if err := validateForUpdate(e, actionName); err != nil {
		return Updatable{}, err
	}

	name := classNameOf(e)
	builder := newBuilder(name, actionName, e.getSignature(), e.Events()).
		withAggregate(extractAggregateMeta(e))
	return builder.updatable(e, *e.GetID(), partial), nil
}

func getDeletable(e Entity, service Service, actionName string) (Deletable, error) {
	ensureInit(e)
	// Same capture rule as every other state-changing verb: the birth-time
	// snapshot wins, so domain.Old(e) — and the audit event built from it —
	// returns the entity as the system of record held it, never a state a
	// Command's ApplyTo or an IfDelete closure produced on the way here.
	captureOldIfAbsent(e)
	e.resetEntity()
	e.setService(service)

	if err := validateForDelete(e, actionName); err != nil {
		return Deletable{}, err
	}

	name := classNameOf(e)
	builder := newBuilder(name, actionName, e.getSignature(), e.Events()).
		withAggregate(extractAggregateMeta(e))
	return builder.deletable(e, *e.GetID()), nil
}

// Archive/Unarchive run BuildRules in ModeArchive / ModeUnarchive — their own
// verbs, dispatched by the IfArchive / IfUnarchive DSL closures (not IfUpdate,
// which is PUT/PATCH exclusively). The supplied actionName still flows to
// BuildRules and the audit event as a label, but the verb is the mode. The
// state-transition checks (Modes() declaring Archive/Unarchive + ID
// validity) still run after the BuildRules pass and feed into the same
// checkAllNotifications gate.

func getArchivable(e Entity, service Service, actionName string) (Archivable, error) {
	ensureInit(e)
	// Archive is a state transition (deleted_at flip + cascade). The birth-time
	// snapshot represents the entity in its pre-archive (active) state as the
	// system of record held it — useful in timelines to see "entity was
	// archived from state X", and the baseline an IfArchive mutation is
	// measured against.
	captureOldIfAbsent(e)
	e.resetEntity()
	e.setService(service)

	if err := validateForArchive(e, actionName); err != nil {
		return Archivable{}, err
	}

	name := classNameOf(e)
	builder := newBuilder(name, actionName, e.getSignature(), e.Events()).
		withAggregate(extractAggregateMeta(e))
	return builder.archivable(e, *e.GetID()), nil
}

func getUnarchivable(e Entity, service Service, actionName string) (Unarchivable, error) {
	ensureInit(e)
	// Unarchive symmetric to Archive — the snapshot is the archived state right
	// before the transition. The entity arrives hydrated (root + children) and
	// already snapshotted from the archived-scope load; a caller that assembles
	// one by hand gets the fallback capture below, with whatever state it holds.
	captureOldIfAbsent(e)
	e.resetEntity()
	e.setService(service)

	if err := validateForUnarchive(e, actionName); err != nil {
		return Unarchivable{}, err
	}

	name := classNameOf(e)
	builder := newBuilder(name, actionName, e.getSignature(), e.Events()).
		withAggregate(extractAggregateMeta(e))
	return builder.unarchivable(e, *e.GetID()), nil
}

func validateForInsert(e Entity, actionName string) error {
	e.setMode(ModeInsert)
	if err := checkService(e, actionName); err != nil {
		return err
	}
	rules := NewRules(ModeInsert, e.NotificationContext(), reflect.TypeOf(e))
	e.BuildRules(actionName, e.getService(), rules)

	if !modeAllowed(e, ModeInsert) {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "insertable",
			FuncName:     "Insert." + actionName + "()",
			Notification: InsertNotAllowedNotification{},
		})
	}
	if e.GetID() != nil {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "id",
			FieldValue:   e.GetID().Value(),
			FuncName:     "Insert." + actionName + "()",
			Notification: UnableToInsertWithIDNotification{},
		})
	}
	validateValueObjectFields(e, e.NotificationContext(), rules.ignoredValueObjects(), rules.forcedValueObjects())
	runAggregateValidations(e, ModeInsert, actionName)
	return checkAllNotifications(e)
}

func validateForUpdate(e Entity, actionName string) error {
	e.setMode(ModeUpdate)
	if err := checkService(e, actionName); err != nil {
		return err
	}
	rules := NewRules(ModeUpdate, e.NotificationContext(), reflect.TypeOf(e))
	e.BuildRules(actionName, e.getService(), rules)

	if !modeAllowed(e, ModeUpdate) {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "updatable",
			FuncName:     "Update." + actionName + "()",
			Notification: UpdateNotAllowedNotification{},
		})
	}
	if e.GetID() == nil {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "id",
			FuncName:     "Update." + actionName + "()",
			Notification: UnableToUpdateWithoutIDNotification{},
		})
	} else {
		e.GetID().IsValid("id", e.NotificationContext())
	}
	validateValueObjectFields(e, e.NotificationContext(), rules.ignoredValueObjects(), rules.forcedValueObjects())
	runAggregateValidations(e, ModeUpdate, actionName)
	return checkAllNotifications(e)
}

func validateForDelete(e Entity, actionName string) error {
	e.setMode(ModeDelete)
	if err := checkService(e, actionName); err != nil {
		return err
	}
	rules := NewRules(ModeDelete, e.NotificationContext(), reflect.TypeOf(e))
	e.BuildRules(actionName, e.getService(), rules)

	if !modeAllowed(e, ModeDelete) {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "deletable",
			FuncName:     "Delete." + actionName + "()",
			Notification: DeleteNotAllowedNotification{},
		})
	}
	if e.GetID() == nil {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "id",
			FuncName:     "Delete." + actionName + "()",
			Notification: UnableToDeleteWithoutIDNotification{},
		})
	} else {
		e.GetID().IsValid("id", e.NotificationContext())
	}
	validateValueObjectFields(e, e.NotificationContext(), rules.ignoredValueObjects(), rules.forcedValueObjects())
	runAggregateValidations(e, ModeDelete, actionName)
	return checkAllNotifications(e)
}

func validateForArchive(e Entity, actionName string) error {
	e.setMode(ModeArchive)
	if err := checkService(e, actionName); err != nil {
		return err
	}
	rules := NewRules(ModeArchive, e.NotificationContext(), reflect.TypeOf(e))
	e.BuildRules(actionName, e.getService(), rules)

	if !modeAllowed(e, ModeArchive) {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "archivable",
			FuncName:     "Archive." + actionName + "()",
			Notification: ArchiveNotAllowedNotification{},
		})
	}
	if e.GetID() == nil {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "id",
			FuncName:     "Archive." + actionName + "()",
			Notification: UnableToDeleteWithoutIDNotification{},
		})
	} else {
		e.GetID().IsValid("id", e.NotificationContext())
	}
	validateValueObjectFields(e, e.NotificationContext(), rules.ignoredValueObjects(), rules.forcedValueObjects())
	runAggregateValidations(e, ModeArchive, actionName)
	return checkAllNotifications(e)
}

func validateForUnarchive(e Entity, actionName string) error {
	e.setMode(ModeUnarchive)
	if err := checkService(e, actionName); err != nil {
		return err
	}
	rules := NewRules(ModeUnarchive, e.NotificationContext(), reflect.TypeOf(e))
	e.BuildRules(actionName, e.getService(), rules)

	if !modeAllowed(e, ModeUnarchive) {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "unarchivable",
			FuncName:     "Unarchive." + actionName + "()",
			Notification: UnarchiveNotAllowedNotification{},
		})
	}
	if e.GetID() == nil {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "id",
			FuncName:     "Unarchive." + actionName + "()",
			Notification: UnableToDeleteWithoutIDNotification{},
		})
	} else {
		e.GetID().IsValid("id", e.NotificationContext())
	}
	validateValueObjectFields(e, e.NotificationContext(), rules.ignoredValueObjects(), rules.forcedValueObjects())
	runAggregateValidations(e, ModeUnarchive, actionName)
	return checkAllNotifications(e)
}

func checkService(e Entity, actionName string) error {
	if e.getService() == nil && e.RequiresService() {
		e.NotificationContext().AddNotificationMessage(NotificationMessage{
			FieldName:    "service",
			FuncName:     actionName,
			Notification: ServiceIsRequiredNotification{},
		})
		return checkAllNotifications(e)
	}
	return nil
}

func modeAllowed(e Entity, m EntityMode) bool {
	for _, allowed := range e.Modes() {
		if allowed == m {
			return true
		}
	}
	return false
}

// validateValueObjectFields validates EVERY value object carried by value — a
// root entity OR an aggregate value object — in every mode. It discovers them
// automatically: each exported, non-embedded field whose value is a value object
// (raw or enum) validates by its Go field name on ctx; a nil pointer field is
// skipped (absent). Fields in ignored are skipped, and forced VOs (those that are
// not plain fields) run too. Dedup is by name so a forced field is not validated
// twice. The ignore/forced sets come from the owner's Rules (r.IgnoreValueObject /
// r.ValidateValueObject) — root and AVO alike.
func validateValueObjectFields(value any, ctx *NotificationContext, ignored []string, forced []voEntry) {
	ignoredSet := map[string]bool{}
	for _, name := range ignored {
		ignoredSet[name] = true
	}
	seen := map[string]bool{}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Struct {
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Anonymous || !f.IsExported() {
				continue
			}
			fv := rv.Field(i)
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					continue
				}
				fv = fv.Elem()
			}
			if ignoredSet[f.Name] || seen[f.Name] {
				continue
			}
			v := validatorFor(fv.Interface())
			if v == nil {
				continue
			}
			seen[f.Name] = true
			v.IsValid(f.Name, ctx)
		}
	}

	// Explicitly forced value objects (r.ValidateValueObject) — VOs that are not
	// plain fields — honoring the same ignore set and dedup.
	for _, entry := range forced {
		if ignoredSet[entry.name] || seen[entry.name] {
			continue
		}
		seen[entry.name] = true
		entry.vo.IsValid(entry.name, ctx)
	}
}

// Phase 19: AggregateRootProvider only declares GetAggregateRoot. TypeNames that
// exist in the AggregateRoot are auto-validated; the manual step
// (ValidateAggregateValueObject) remains available for typeNames OUTSIDE the
// AggregateRoot (VOs without their own table, e.g. tags in a JSONB column).
//
// Collection name in the wire path is the name the AVO declares in
// CollectionName, cased for the wire (childCollectionSegment = toLowerCamel):
// "Addresses" → "addresses", "OrderLines" → "orderLines". It is a JSON wire
// segment, so the convention is camelCase — independent of the physical table
// name declared in the TableSchema.
//
// The framework passes to AVO.BuildRules a *Rules whose NotificationContext
// is scoped with the prefix:
//
//	[{Name: <collection>}, {Index: <i>}]
//
// AVO emits only the leaf field name (e.g. r.AddNotification("ZipCode", n))
// and the wire format produces "addresses[0].zipCode" without the AVO knowing the hierarchy.
func runAggregateValidations(e Entity, mode EntityMode, actionName string) {
	mappedTypeNames := map[string]struct{}{}
	rootCtx := e.NotificationContext()

	if provider, ok := e.(AggregateRootProvider); ok {
		if root := provider.GetAggregateRoot(); root != nil {
			all := root.AllAggregateItems()
			for typeName, items := range all {
				mappedTypeNames[typeName] = struct{}{}
				if len(items) == 0 {
					continue
				}
				// The collection name is declared by the AVO, so it is resolved
				// from an item's type — the map key is the Go type name, which
				// names nothing on the wire.
				collectionName := childCollectionSegment(reflect.TypeOf(items[0].Item))
				idx := 0
				for _, item := range items {
					if item.CurrentStatus == StatusRemoved {
						continue
					}
					scoped := rootCtx.scopedForType(
						reflect.TypeOf(item.Item),
						NameSegment(collectionName),
						IndexSegment(idx),
					)
					childRules := NewRules(mode, scoped, reflect.TypeOf(item.Item))
					item.Item.BuildRules(actionName, e.getService(), childRules)
					validateValueObjectFields(item.Item, scoped, childRules.ignoredValueObjects(), childRules.forcedValueObjects())
					idx++
				}
			}
		}
	}

	for _, entry := range e.aggregateValueObjectsToValidate() {
		if _, mapped := mappedTypeNames[entry.name]; mapped {
			continue
		}
		scoped := rootCtx.scopedForType(reflect.TypeOf(entry.avo), NameSegment(entry.name))
		childRules := NewRules(mode, scoped, reflect.TypeOf(entry.avo))
		entry.avo.BuildRules(actionName, e.getService(), childRules)
		validateValueObjectFields(entry.avo, scoped, childRules.ignoredValueObjects(), childRules.forcedValueObjects())
	}
}

func checkAllNotifications(e Entity) error {
	contexts := collectContexts(e)
	if len(contexts) > 0 {
		return NewDomainError(contexts)
	}
	return nil
}

func collectContexts(e Entity) []*NotificationContext {
	applyFieldAliases(e)
	out := []*NotificationContext{}
	if e.NotificationContext().HasErrors() {
		out = append(out, e.NotificationContext())
	}
	for _, ctx := range e.contextCollection() {
		if ctx.HasErrors() {
			out = append(out, ctx)
		}
	}
	return out
}

func applyFieldAliases(e Entity) {
	aliases := e.fieldAliases()
	if len(aliases) == 0 {
		return
	}
	ctx := e.NotificationContext()
	for orig, new := range aliases {
		ctx.ChangeFieldName(orig, new)
	}
}

// ensureInit gives the entity's context the identity only the concrete type can
// supply. It is not a precondition for emitting — BaseEntity allocates the
// context on demand — so domain code never has to call it; the framework runs
// it at every entry point that holds the Entity, and it is a no-op after the
// first.
func ensureInit(e Entity) {
	e.initWithName(classNameOf(e), reflect.TypeOf(e))
}

func classNameOf(e any) string {
	t := reflect.TypeOf(e)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Name()
}
