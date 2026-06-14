package domain

// Rules is the mode-scoped validation DSL used by both root entities
// (Entity.BuildRules) and aggregate value objects (AggregateValueObject.BuildRules).
// It carries both the active EntityMode (so IfInsert/IfUpdate/… can self-dispatch)
// and the NotificationContext (so AddNotification/AddNotificationMessage can emit
// without the caller having to thread ctx through). This is the mechanism that
// makes the inner body of validation rules look IDENTICAL in root and AVO code:
//
//	r.IfInsertOrUpdate(func() {
//	    if x.Field == "" {
//	        r.AddNotification("Field", domain.RequiredFieldNotification{})
//	    } else if !valid(x.Field) {
//	        r.AddNotification("Field", InvalidFieldNotification{}, x.Field)
//	    }
//	})
//
// The framework constructs Rules with the appropriate ctx: a root entity gets
// its own NotificationContext; an aggregate value object gets a scoped child
// context that auto-prefixes collection name + index.
type Rules struct {
	mode EntityMode
	ctx  *NotificationContext
}

// NewRules constructs a Rules dispatcher for the given mode wired to the given
// NotificationContext. ctx may be nil during construction in legacy paths; in
// that case AddNotification/AddNotificationMessage are no-ops.
func NewRules(mode EntityMode, ctx *NotificationContext) *Rules {
	return &Rules{mode: mode, ctx: ctx}
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
func (r *Rules) AddNotification(name string, n Notification, value ...any) {
	if r.ctx == nil {
		return
	}
	r.ctx.AddNotification(name, n, value...)
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
