package domain

import "reflect"

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
//	        r.AddNotification("Field", domain.RequiredFieldNotification{})
//	    } else if !valid(x.Field) {
//	        r.AddNotification("Field", InvalidFieldNotification{}, x.Field)
//	    }
//	})
//
// The framework constructs Rules with the appropriate ctx + entityType: a
// root entity gets its own NotificationContext and reflect.TypeOf(self); an
// aggregate value object gets a scoped child context that auto-prefixes
// collection name + index, and reflect.TypeOf(self) of the AVO so label tags
// on AVO fields resolve against the AVO struct.
type Rules struct {
	mode       EntityMode
	ctx        *NotificationContext
	entityType reflect.Type
}

// NewRules constructs a Rules dispatcher for the given mode wired to the given
// NotificationContext and entityType. ctx may be nil during construction in
// legacy paths; in that case AddNotification/AddNotificationMessage are no-ops.
// entityType may be nil for code paths that do not need label resolution
// (legacy callers, tests); in that case emitted messages carry LabelKey == ""
// and the wire elides the fieldLabel via omitempty.
func NewRules(mode EntityMode, ctx *NotificationContext, entityType reflect.Type) *Rules {
	return &Rules{mode: mode, ctx: ctx, entityType: entityType}
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

// AddNotification is the common emit helper. It writes a single-segment Path
// using the Go identifier name; the framework's renderer converts to camelCase
// (acronym-aware) and, when r.ctx is scoped, prepends the collection + index
// prefix. The optional value variadic populates FieldValue (echo the rejected
// input on Invalid* notifications); pass *string and it is dereferenced safely.
//
// The emitted NotificationMessage carries LabelKey resolved from the field's
// `label:"..."` struct tag on r.entityType (or "" when no tag is declared, or
// when r.entityType is nil). The translation layer (application/notifications/
// convert.go::ToContextDTOs) renders the key into MessageDTO.FieldLabel at
// the same call site that already renders Message — one round-trip per
// emitted notification.
func (r *Rules) AddNotification(name string, n Notification, value ...any) {
	if r.ctx == nil {
		return
	}
	msg := NotificationMessage{
		Path:         []PathSegment{{Name: name}},
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
// collision). The optional value variadic populates FieldValue with the same
// semantics as AddNotification.
//
// For full-control emits (Err, FuncName, multi-segment Path), use
// AddNotificationMessage directly.
func (r *Rules) AddNotificationWithVars(name string, n Notification, vars map[string]string, value ...any) {
	if r.ctx == nil {
		return
	}
	msg := NotificationMessage{
		Path:         []PathSegment{{Name: name}},
		Notification: n,
		Vars:         vars,
		LabelKey:     resolveLabelKey(r.entityType, name),
	}
	if len(value) > 0 {
		msg.FieldValue = formatFieldValue(value[0])
	}
	r.ctx.AddNotificationMessage(msg)
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
