package domain

import (
	"fmt"
	"reflect"
)

// Rules is the mode-scoped validation DSL used by both root entities
// (Entity.BuildRules) and aggregate value objects (AggregateValueObject.BuildRules).
// It carries the active EntityMode (so IfInsert/IfUpdate/… can self-dispatch),
// the NotificationContext (so AddNotification/AddNotificationMessage can emit
// without the caller having to thread ctx through), AND the Go reflect.Type
// of the entity / value object that owns this Rules (so AddNotification can
// resolve the field's `label` struct tag at emit time and stamp LabelKey on
// the emitted NotificationMessage for downstream wire/audit rendering).
//
// This is the mechanism that makes the inner body of validation rules look
// IDENTICAL in root and AVO code:
//
//	r.IfInsertOrUpdate(func() {
//	    if x.Field == "" {
//	        r.AddNotification(&x.Field, domain.RequiredFieldNotification{}, false)
//	    } else if !valid(x.Field) {
//	        r.AddNotification(&x.Field, InvalidFieldNotification{}, true)
//	    }
//	})
//
// The framework constructs Rules with the appropriate ctx + entityType AND
// binds it to the instance whose BuildRules it invokes (bindFieldBase), which
// is what lets AddNotification resolve &x.Field back to the field it
// addresses: a root entity gets its own NotificationContext and its own
// pointer; an aggregate value object gets a scoped child context that
// auto-prefixes collection name + index, and the addressable copy the
// framework materialized for its pointer-receiver BuildRules.
type Rules struct {
	mode       EntityMode
	ctx        *NotificationContext
	entityType reflect.Type
	ignoredVOs []string
	forcedVOs  []voEntry
	pass       *rulesPass

	// Field-reference resolution state, set by bindFieldBase (field_ref.go):
	// the bound instance, its address, and the per-type field atlas.
	base    reflect.Value
	basePtr uintptr
	atlas   *fieldAtlas
}

// rulesPass is the halt state shared by every Rules built for ONE validation
// pass — the root's and each aggregate child's. It is the only mutable state
// that crosses Rules instances, and it exists so StopIfInvalid can stop work
// the OWNER of that Rules does not run itself: the automatic value-object
// validation and the aggregate children still queued behind it.
type rulesPass struct {
	halted bool
}

// stopRulesSignal is the private panic value StopIfInvalid raises to unwind a
// BuildRules body. It never escapes the framework: every seat that invokes
// BuildRules recovers it BY TYPE (buildRules) and re-panics anything else with
// its value intact, so a genuine bug in a rule still reaches the pipeline
// exactly as it does today. This is the standard library's own pattern for an
// internal unwind — see text/template's writeError/errRecover.
type stopRulesSignal struct{}

// NewRules constructs a Rules dispatcher for the given mode wired to the given
// NotificationContext and entityType. ctx may be nil during construction in
// legacy paths; in that case AddNotification/AddNotificationMessage are no-ops.
// entityType may be nil for code paths that do not need label resolution
// (legacy callers, tests); in that case emitted messages carry LabelKey == ""
// and the wire elides the fieldLabel via omitempty.
func NewRules(mode EntityMode, ctx *NotificationContext, entityType reflect.Type) *Rules {
	return &Rules{mode: mode, ctx: ctx, entityType: entityType}
}

// NewRulesFor constructs a Rules BOUND to base — the form manual code and
// tests use when they need field-reference emissions outside a BuildRules
// invocation (inside one, the framework binds the receiver automatically).
// base must be a non-nil pointer to the struct whose fields will be
// referenced; entityType is derived from it, so labelKey/notifyAs tags resolve
// against that struct.
func NewRulesFor(mode EntityMode, ctx *NotificationContext, base any) *Rules {
	r := NewRules(mode, ctx, reflect.TypeOf(base))
	r.bindFieldBase(base)
	return r
}

func (r *Rules) Mode() EntityMode {
	return r.mode
}

// Context returns the NotificationContext this Rules emits to. Useful for
// passing into nested helpers (e.g. ValueObject.IsValid) that still take a
// ctx parameter.
func (r *Rules) Context() *NotificationContext {
	return r.ctx
}

// IgnoreValueObject excludes an automatically-discovered value-object field from
// this entity's validation pass, by its Go field name. Every VO field validates
// in every mode by default; call this inside a mode gate to opt out — e.g.
// r.IfDelete(func() { r.IgnoreValueObject("Email") }). Works identically on a
// root and on an aggregate value object (each carries its own Rules).
func (r *Rules) IgnoreValueObject(name string) {
	r.ignoredVOs = append(r.ignoredVOs, name)
}

// ValidateValueObject FORCES a value object into the validation pass — the escape
// hatch for a VO that is not a plain exported field (computed, held in a
// slice/map), since those are picked up automatically. It takes both kinds (a
// raw ValueObject or an EnumValueObject); anything else panics.
func (r *Rules) ValidateValueObject(name string, vo any) {
	v := validatorFor(vo)
	if v == nil {
		panic(fmt.Sprintf("ValidateValueObject %q: %T is neither a ValueObject (IsValid) nor an EnumValueObject (Values + UnknownNotification)", name, vo))
	}
	r.forcedVOs = append(r.forcedVOs, voEntry{name: name, vo: v})
}

func (r *Rules) ignoredValueObjects() []string { return r.ignoredVOs }
func (r *Rules) forcedValueObjects() []voEntry { return r.forcedVOs }

// AddNotification is the common emit helper — addressed by FIELD REFERENCE,
// never by string. Pass a pointer to the entity's own field:
//
//	r.AddNotification(&e.ZipCode, domain.InvalidFormatNotification{}, true)
//
// The reference is resolved deterministically (offset + type — field_ref.go)
// to the field it addresses, and everything the message needs is read off that
// field itself: the Go name (rendered lowerCamel for the wire, acronym-aware),
// the `notifyAs:"..."` wire-name override, and the `labelKey:"..."` catalog key.
// When r.ctx is scoped, the collection + index prefix is prepended.
//
// exposeValue says whether the field's CURRENT value is echoed in FieldValue —
// the value is read from the reference, so there is nothing to pass. Pointer
// fields (optional) are unwrapped; nil renders as the empty string; a type
// that renders itself (fmt.Stringer/error, time.Time among them) keeps its own
// rendering.
//
// Misuse — a non-pointer, a reference into a copy, a pointer field passed
// without & — panics immediately with a message naming the fix. For
// notifications that are not about one addressable field, use
// AddNotificationNamed.
func (r *Rules) AddNotification(field any, n Notification, exposeValue bool) {
	if r.ctx == nil {
		return
	}
	node, rv := r.resolveFieldRef(field)
	msg := NotificationMessage{
		Path:         clonePath(node.segs),
		Notification: n,
		LabelKey:     labelKeyOf(node.leaf),
	}
	if exposeValue {
		msg.FieldValue = formatFieldValue(rv.Elem().Interface())
	}
	r.ctx.AddNotificationMessage(msg)
}

// AddNotificationNamed is the string-named seat — the documented exception for
// notifications that are not about ONE addressable field (cross-field rules,
// state rejections, synthetic tokens). It writes a single-segment Path with
// the given name (a Go-cased name renders lowerCamel; an already-lowercase
// token passes verbatim) and resolves the labelKey tag by that name on
// r.entityType when one matches. The optional value variadic populates
// FieldValue with the same semantics AddNotification's exposeValue provides.
func (r *Rules) AddNotificationNamed(name string, n Notification, value ...any) {
	if r.ctx == nil {
		return
	}
	msg := NotificationMessage{
		Path:         []PathSegment{{Name: name, Wire: resolveNotifyAs(r.entityType, name)}},
		Notification: n,
		LabelKey:     resolveLabelKey(r.entityType, name),
	}
	if len(value) > 0 {
		msg.FieldValue = formatFieldValue(value[0])
	}
	r.ctx.AddNotificationMessage(msg)
}

// AddNotificationMessage forwards a full NotificationMessage. Use this when the
// message needs Err, FuncName, Override, or a multi-segment Path; otherwise
// AddNotification is shorter and covers the common (Field, Notification, value)
// triple directly.
func (r *Rules) AddNotificationMessage(msg NotificationMessage) {
	if r.ctx == nil {
		return
	}
	r.ctx.AddNotificationMessage(msg)
}

// AddNotificationWithVars is the sibling of AddNotification that attaches
// per-emit translation variables on top of any tag-derived vars the
// Notification type already carries. Use this when the same notification type
// ships with default vars from its struct tags but a specific call site needs
// to inject additional or overriding values (per-emit vars win on key
// collision). Field reference and exposeValue behave exactly as in
// AddNotification.
//
// For full-control emits (Err, FuncName, multi-segment Path, a named token
// with vars), use AddNotificationMessage directly.
func (r *Rules) AddNotificationWithVars(field any, n Notification, vars map[string]string, exposeValue bool) {
	if r.ctx == nil {
		return
	}
	node, rv := r.resolveFieldRef(field)
	msg := NotificationMessage{
		Path:         clonePath(node.segs),
		Notification: n,
		Vars:         vars,
		LabelKey:     labelKeyOf(node.leaf),
	}
	if exposeValue {
		msg.FieldValue = formatFieldValue(rv.Elem().Interface())
	}
	r.ctx.AddNotificationMessage(msg)
}

// StopIfInvalid ends the whole validation pass when a notification has ALREADY
// been emitted — the guard barrier. Call it on its own line, between rules;
// there is nothing to write around it:
//
//	r.IfInsertOrUpdate(func() {
//	    if x.Owner == nil {
//	        r.AddNotification(&x.Owner, domain.RequiredFieldNotification{}, false)
//	    }
//	})
//	r.StopIfInvalid()
//	// nothing below this line runs once a rule above it has rejected the write:
//	// no closure, no hand-written if, no bare statement
//
// What it stops is everything the pass has not done yet: the rest of this
// BuildRules, the automatic value-object validation of this owner, and the
// BuildRules and value objects of every aggregate child still queued. Called
// from a child's BuildRules it cuts that child's own value objects and the
// remaining siblings — the pass is sequential, so what it stops is what has not
// run yet. The framework's structural gates (Modes() and ID validity) sit
// outside the barrier and always report.
//
// With no notification emitted it does nothing and the pass continues. That is
// what makes it safe by construction: a stop cannot turn an invalid write into
// a valid one, because it only ever happens when the entity is already
// rejected, and the notification that triggered it is what the caller gets
// back. Use it wherever a rule is a precondition for the rules below it — an
// owner that must exist before it is dereferenced, a snapshot that must have
// been captured, a parse that must have succeeded.
func (r *Rules) StopIfInvalid() {
	if r.ctx == nil || !r.ctx.HasErrors() {
		return
	}
	if r.pass != nil {
		r.pass.halted = true
	}
	panic(stopRulesSignal{})
}

func (r *Rules) IfInsert(fn func()) *Rules {
	if r.mode == ModeInsert {
		fn()
	}
	return r
}

func (r *Rules) IfUpdate(fn func()) *Rules {
	if r.mode == ModeUpdate {
		fn()
	}
	return r
}

func (r *Rules) IfDelete(fn func()) *Rules {
	if r.mode == ModeDelete {
		fn()
	}
	return r
}

// IfArchive fires only on the archive state-transition. Archive is its own
// verb — a distinct EntityMode with its own sealed Archivable and its own
// Get* entry point — so it dispatches here, NOT under IfUpdate. Put
// archive-only invariants (e.g. an owner check on the archive verb) in this
// closure; IfUpdate covers PUT/PATCH exclusively.
func (r *Rules) IfArchive(fn func()) *Rules {
	if r.mode == ModeArchive {
		fn()
	}
	return r
}

// IfUnarchive is the symmetric inverse of IfArchive, firing only on the
// unarchive state-transition.
func (r *Rules) IfUnarchive(fn func()) *Rules {
	if r.mode == ModeUnarchive {
		fn()
	}
	return r
}

func (r *Rules) IfInsertOrUpdate(fn func()) *Rules {
	if r.mode == ModeInsert || r.mode == ModeUpdate {
		fn()
	}
	return r
}

func (r *Rules) IfDisplay(fn func()) *Rules {
	if r.mode == ModeDisplay {
		fn()
	}
	return r
}
