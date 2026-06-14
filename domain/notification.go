package domain

import (
	"fmt"
	"reflect"
)

type Notification interface {
	isNotification()
	Semantic() NotificationSemantic
}

type NotificationBase struct{}

func (NotificationBase) isNotification() {}

// Semantic returns the default classification — SemanticValidation (422).
// Concrete notification types may override via method promotion (e.g.
// RecordNotFoundNotification → SemanticNotFound).
func (NotificationBase) Semantic() NotificationSemantic { return SemanticValidation }

type DomainNotificationBase struct{ NotificationBase }
type ApplicationNotificationBase struct{ NotificationBase }
type InfrastructureNotificationBase struct{ NotificationBase }

func NotificationKey(n Notification) string {
	t := reflect.TypeOf(n)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

type NotificationCarrier interface {
	error
	NotificationContexts() []*NotificationContext
}

// PathSegment is one piece of a structured field path. Either Name or Index is
// meaningful — never both at once. Index segments append "[N]" to the previous
// Name segment when rendered. Name segments are rendered via lowerCamel
// (PascalCase Go identifiers become camelCase JSON) when they start uppercase,
// or used verbatim when already lowercase.
type PathSegment struct {
	Name  string
	Index *int
}

// IndexSegment is a convenience constructor for index segments.
func IndexSegment(i int) PathSegment { return PathSegment{Index: &i} }

// NameSegment is a convenience constructor for name segments.
func NameSegment(name string) PathSegment { return PathSegment{Name: name} }

type NotificationMessage struct {
	// Path is the structured field path. When non-empty, it is the source of
	// truth for the wire-format field name (rendered via lowerCamel with acronym
	// awareness; index segments become "[N]"). Path-based emissions are how
	// framework-scoped contexts (AggregateValueObject children) compose
	// "addresses[0].zipCode" automatically.
	Path []PathSegment

	// Override, when set, takes precedence over both Path and FieldName when
	// resolving the wire field. ChangeFieldName populates this so manual
	// handlers can rewrite a field's wire name without losing the structured
	// Path or the original FieldName for diagnostics.
	Override string

	// FieldName is the legacy literal field name. When Path is empty and
	// Override is empty, FieldName is used as-is (no case conversion).
	// Framework internals (mode validators, validator.go, helpers) still set
	// FieldName directly with already-lowercase names ("id", "name", "email")
	// and that continues to work unchanged.
	FieldName string

	FieldValue   string
	FuncName     string
	Err          error
	Notification Notification

	// Vars carries per-emit translation variables. Merged on top of any
	// tag-derived vars the Notification type provides (per-message values
	// win on key collision). nil and empty map behave identically. The
	// translation layer reads this via domain.MessageVars(msg) so call sites
	// that do not need overrides pay nothing.
	Vars map[string]string
}

// ResolveFieldName returns the effective wire-format field name following the
// precedence Override > rendered Path > FieldName. Wire/DTO layers must call
// this instead of reading m.FieldName directly so Path-based and overridden
// messages render correctly.
func (m NotificationMessage) ResolveFieldName() string {
	if m.Override != "" {
		return m.Override
	}
	if len(m.Path) > 0 {
		return renderPath(m.Path)
	}
	return m.FieldName
}

type NotificationContext struct {
	context  string
	messages []NotificationMessage

	// contextVars carries the translation variables applied to the context
	// label (not to individual messages). Written via SetVars on the root of
	// the chain; read via ContextVars from any node (scoped views forward to
	// the root). nil and empty map behave identically.
	contextVars map[string]string

	// parent and prefix are set only on scoped views produced by Scoped().
	// A scoped context does not own messages — Add forwards to parent after
	// prepending the prefix to each message's Path. This is how the framework
	// gives AggregateValueObject implementations a context that auto-composes
	// "addresses[0]." without the AVO knowing it is a child.
	parent *NotificationContext
	prefix []PathSegment
}

func NewNotificationContext(name string) *NotificationContext {
	return &NotificationContext{context: name}
}

func (c *NotificationContext) Context() string {
	return c.context
}

// Messages returns a copy of the messages stored at the root of the context
// chain. Scoped contexts have no messages of their own — their forwards land in
// the root.
func (c *NotificationContext) Messages() []NotificationMessage {
	root := c.root()
	out := make([]NotificationMessage, len(root.messages))
	copy(out, root.messages)
	return out
}

// AddNotificationMessage stores msg in the context. When the context is scoped,
// the scope prefix is prepended to msg.Path and the message is forwarded to the
// parent. If the caller passed only msg.FieldName (legacy form) on a scoped
// context, FieldName is wrapped as a literal leaf so the prefix can still
// compose correctly.
func (c *NotificationContext) AddNotificationMessage(msg NotificationMessage) {
	if len(c.prefix) > 0 {
		if len(msg.Path) == 0 && msg.FieldName != "" {
			msg.Path = []PathSegment{{Name: msg.FieldName}}
			msg.FieldName = ""
		}
		if len(msg.Path) > 0 {
			combined := make([]PathSegment, 0, len(c.prefix)+len(msg.Path))
			combined = append(combined, c.prefix...)
			combined = append(combined, msg.Path...)
			msg.Path = combined
		}
	}
	if c.parent != nil {
		c.parent.AddNotificationMessage(msg)
		return
	}
	c.messages = append(c.messages, msg)
}

// AddNotification is the common emit helper. It writes a single-segment Path
// using the Go identifier name; an optional value variadic populates FieldValue
// (use it to echo back the rejected input on Invalid* notifications).
//
// In an AggregateValueObject called via a scoped context, the framework's prefix
// (collection name + index) composes with the supplied name to produce paths
// like "addresses[0].zipCode" automatically. In a root entity context, no
// prefix is prepended and "Name" renders as "name".
//
// For messages that need Err, FuncName, or Override, use AddNotificationMessage.
func (c *NotificationContext) AddNotification(name string, n Notification, value ...any) {
	msg := NotificationMessage{
		Path:         []PathSegment{{Name: name}},
		Notification: n,
	}
	if len(value) > 0 {
		msg.FieldValue = formatFieldValue(value[0])
	}
	c.AddNotificationMessage(msg)
}

// formatFieldValue converts a variadic value into the string form expected by
// NotificationMessage.FieldValue. Strings pass through; everything else is
// rendered with fmt.Sprint so callers can pass *string, ints, etc. without
// boilerplate at the call site.
func formatFieldValue(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case *string:
		if s == nil {
			return ""
		}
		return *s
	default:
		return fmt.Sprint(v)
	}
}

func (c *NotificationContext) HasErrors() bool {
	return len(c.root().messages) > 0
}

func (c *NotificationContext) Clear() {
	root := c.root()
	root.messages = root.messages[:0]
}

// ChangeFieldName rewrites the wire-format field name for every message whose
// resolved field matches oldName. It sets Override on the matching messages, so
// the underlying Path/FieldName remain intact for diagnostics.
func (c *NotificationContext) ChangeFieldName(oldName, newName string) {
	root := c.root()
	for i := range root.messages {
		if root.messages[i].ResolveFieldName() == oldName {
			root.messages[i].Override = newName
		}
	}
}

// Scoped returns a child context that prepends the given segments to the Path
// of every message added through it. Forwarding lands in the same root, so
// Messages/HasErrors/Clear/ChangeFieldName all reflect the combined state.
func (c *NotificationContext) Scoped(segments ...PathSegment) *NotificationContext {
	if len(segments) == 0 {
		return c
	}
	merged := make([]PathSegment, 0, len(c.prefix)+len(segments))
	merged = append(merged, c.prefix...)
	merged = append(merged, segments...)
	root := c.root()
	return &NotificationContext{
		context: root.context,
		parent:  root,
		prefix:  merged,
	}
}

// SetVars assigns the translation variables used when the framework renders
// this context's label. Empty map and nil both clear the slot. Always writes
// to the root of the chain so scoped views read the same values via
// ContextVars.
func (c *NotificationContext) SetVars(vars map[string]string) {
	root := c.root()
	if len(vars) == 0 {
		root.contextVars = nil
		return
	}
	copied := make(map[string]string, len(vars))
	for k, v := range vars {
		copied[k] = v
	}
	root.contextVars = copied
}

// ContextVars returns the translation variables registered for this context's
// label. Scoped views forward to the root — same map every observer reads.
// Returns nil when no vars were set; callers that want to merge should
// treat nil and empty identically.
func (c *NotificationContext) ContextVars() map[string]string {
	return c.root().contextVars
}

func (c *NotificationContext) Copy(newContext ...string) *NotificationContext {
	root := c.root()
	name := root.context
	if len(newContext) > 0 && newContext[0] != "" {
		name = newContext[0]
	}
	clone := &NotificationContext{
		context:  name,
		messages: make([]NotificationMessage, len(root.messages)),
	}
	copy(clone.messages, root.messages)
	return clone
}

func (c *NotificationContext) root() *NotificationContext {
	for c.parent != nil {
		c = c.parent
	}
	return c
}

type DomainResult struct {
	Entity   ValidEntity
	Contexts []*NotificationContext
	Modified Fields
}

func NewDomainResult() *DomainResult {
	return &DomainResult{
		Contexts: []*NotificationContext{},
		Modified: Fields{},
	}
}

func (r *DomainResult) AddContext(ctx *NotificationContext) {
	if ctx != nil && ctx.HasErrors() {
		r.Contexts = append(r.Contexts, ctx)
	}
}

func (r *DomainResult) HasErrors() bool {
	for _, c := range r.Contexts {
		if c.HasErrors() {
			return true
		}
	}
	return false
}

func (r *DomainResult) WithEntity(e ValidEntity) *DomainResult {
	r.Entity = e
	return r
}
