package domain

import (
	"fmt"
	"reflect"
	"strings"
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
	if t.Kind() == reflect.Pointer {
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
// or used verbatim when already lowercase. Wire, when set, is the segment's
// declared wire token (the `notifyAs:"..."` struct tag) and is used verbatim in
// place of the rendered Name.
type PathSegment struct {
	Name  string
	Wire  string
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

	// Override, when set, is the LITERAL wire field — it takes precedence over
	// Path and is never rendered. Three producers write it: ChangeFieldName /
	// the instance alias (rewriting a field's wire name without losing the
	// structured Path for diagnostics), the error helpers that carry a
	// dev-supplied name verbatim (SingleNotificationError and family), and the
	// deliberate non-field entries a renderer would mangle (the router
	// fallbacks' "GET /path", the migration directory). There is no third
	// slot: a message names its field through the rendered Path or through
	// this literal — nothing else.
	Override string

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

	// LabelKey is the catalog key the translation layer renders into the
	// MessageDTO.FieldLabel wire field. Populated by Rules.AddNotification at
	// emit time (resolved via the `labelKey:"..."` struct tag on the field that
	// triggered the notification — see domain/field_label.go::resolveLabelKey).
	// Empty when the source field has no `label` tag; the wire elides the
	// fieldLabel via json:"fieldLabel,omitempty" in that case. Consumers that
	// build NotificationMessage directly via AddNotificationMessage may set
	// LabelKey themselves to surface a label on a manual emission.
	LabelKey string
}

// ResolveFieldName returns the effective wire-format field name following the
// precedence Override (literal) > rendered Path. Wire/DTO layers must call
// this so overridden and Path-based messages render correctly.
func (m NotificationMessage) ResolveFieldName() string {
	if m.Override != "" {
		return m.Override
	}
	return renderPath(m.Path)
}

// renderPath turns a structured PathSegment slice into the wire-format field
// string. Name segments are rendered via lowerCamel (PascalCase → camelCase,
// acronym-aware: "URL" → "url", "ZipCode" → "zipCode"); names that already
// start with a lowercase character are passed through verbatim so legacy
// already-lowercase identifiers ("id", "name") stay unchanged. Index segments
// are appended in the form "[N]" with no preceding separator.
//
// Examples:
//
//	[{Name:"Name"}]                                       → "name"
//	[{Name:"URL"}]                                        → "url"
//	[{Name:"ZipCode"}]                                    → "zipCode"
//	[{Name:"Addresses"}, {Index:0}, {Name:"ZipCode"}]     → "addresses[0].zipCode"
//	[{Name:"id"}]                                         → "id"
func renderPath(path []PathSegment) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	wroteAny := false
	for _, seg := range path {
		if seg.Index != nil {
			b.WriteByte('[')
			b.WriteString(itoa(*seg.Index))
			b.WriteByte(']')
			wroteAny = true
			continue
		}
		if seg.Name == "" && seg.Wire == "" {
			continue
		}
		if wroteAny {
			b.WriteByte('.')
		}
		if seg.Wire != "" {
			b.WriteString(seg.Wire)
		} else {
			b.WriteString(toLowerCamel(seg.Name))
		}
		wroteAny = true
	}
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
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

	// entityType is the entity whose `labelKey:"…"` struct tags apply to
	// notifications emitted on THIS context, so a value object that emits via
	// AddNotification resolves the field label the same way Rules does. Set at
	// birth — initWithName for the root (simple OR aggregate-carrying, both
	// embed BaseEntity), scopedForType for a child AVO. nil for contexts that
	// describe no entity (infra/application, repoNotImpl): those carry no label.
	entityType reflect.Type
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
// parent.
func (c *NotificationContext) AddNotificationMessage(msg NotificationMessage) {
	if c == nil {
		return
	}
	if len(c.prefix) > 0 && len(msg.Path) > 0 {
		combined := make([]PathSegment, 0, len(c.prefix)+len(msg.Path))
		combined = append(combined, c.prefix...)
		combined = append(combined, msg.Path...)
		msg.Path = combined
	}
	if c.parent != nil {
		c.parent.AddNotificationMessage(msg)
		return
	}
	c.messages = append(c.messages, msg)
}

// AddNotificationNamed is the string-named emit seat — for notifications that
// are not about ONE addressable field (cross-field rules, state rejections) or
// that name a synthetic token. It writes a single-segment Path using the given
// name; the renderer converts a Go-cased name to camelCase and passes an
// already-lowercase token through verbatim. An optional value variadic
// populates FieldValue (echo the rejected input on Invalid* notifications). A
// pointer of any type is dereferenced; nil renders as the empty string.
//
// Field-addressed emissions go through Rules.AddNotification with the field
// reference (&e.Field) instead — this seat is the documented exception, not
// the default.
//
// In an AggregateValueObject called via a scoped context, the framework's prefix
// (collection name + index) composes with the supplied name to produce paths
// like "addresses[0].zipCode" automatically. In a root entity context, no
// prefix is prepended and "Name" renders as "name".
//
// For messages that need Err, FuncName, or Override, use AddNotificationMessage.
func (c *NotificationContext) AddNotificationNamed(name string, n Notification, value ...any) {
	msg := NotificationMessage{
		Path:         []PathSegment{{Name: name}},
		Notification: n,
	}
	if c != nil && c.entityType != nil {
		// The name-keyed twin of the field-reference read: when the name is a
		// field of the owning entity, its declared vocabulary applies here too
		// — same labelKey, same notifyAs wire token — so a field renders
		// IDENTICALLY whichever seat emitted about it.
		msg.LabelKey = resolveLabelKey(c.entityType, name)
		msg.Path[0].Wire = resolveNotifyAs(c.entityType, name)
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
//
// A POINTER is unwrapped first, and that is the whole point of the reflect
// step. Every optional field is a pointer, and fmt.Sprint prints the ADDRESS of
// one whose type does not render itself — *int, *float64, a *vos.Email (a value
// object is a named type over a base type and declares no String()). A caller
// who asked "which value did you refuse?" was answered 0xc000180dda: a rule that
// fired correctly, reported through a message nobody can act on. A nil pointer
// is an absent value, so it renders "" for the same reason nil does.
//
// *time.Time was never affected, and the loop below says why in code: time.Time
// has String(), so its pointer satisfies fmt.Stringer and keeps that rendering.
// Worth knowing, because it is why the gap survived — the one optional field
// this framework's own example ever echoed was a *time.Time.
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
	}

	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		// A type that renders ITSELF keeps that method. Unwrapping a value whose
		// pointer carries String()/Error() would discard the only code that
		// knows how to print it, and render the bare struct instead.
		if _, ok := rv.Interface().(fmt.Stringer); ok {
			break
		}
		if _, ok := rv.Interface().(error); ok {
			break
		}
		rv = rv.Elem()
	}
	return fmt.Sprint(rv.Interface())
}

func (c *NotificationContext) HasErrors() bool {
	return len(c.root().messages) > 0
}

func (c *NotificationContext) Clear() {
	root := c.root()
	root.messages = root.messages[:0]
}

// ChangeFieldName rewrites the wire-format field name for every message whose
// resolved field matches oldName — the imperative, per-request seat (a manual
// handler reshaping one response). It sets Override on the matching messages,
// so the underlying Path remains intact for diagnostics. oldName is the
// RESOLVED wire name (post-render); the declarative entity-wide rename is
// BaseEntity.AddFieldNameAlias, which keys on the GO field name instead.
func (c *NotificationContext) ChangeFieldName(oldName, newName string) {
	root := c.root()
	for i := range root.messages {
		if root.messages[i].ResolveFieldName() == oldName {
			root.messages[i].Override = newName
		}
	}
}

// applyGoFieldAlias rewrites the LEAF segment's wire token of every message
// whose leaf path segment carries the given Go field name — the mechanism
// behind BaseEntity.AddFieldNameAlias. Writing the segment's Wire (the same
// slot the `notifyAs` tag fills, and overriding it) keeps a scoped child's
// prefix intact: aliasing "ZipCode" → "cep" renders "addresses[0].cep", not a
// flattened literal. Messages without a Path (Override-literal emissions) are
// untouched — those names were written deliberately.
func (c *NotificationContext) applyGoFieldAlias(goName, wireName string) {
	root := c.root()
	for i := range root.messages {
		segs := root.messages[i].Path
		for j := len(segs) - 1; j >= 0; j-- {
			if segs[j].Index != nil {
				continue
			}
			if segs[j].Name == goName {
				segs[j].Wire = wireName
			}
			break
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

// resolvePendingLabels fills in the LabelKey of messages that were emitted
// before the context learned which entity it describes — the window between an
// entity's first AddNotification and the framework's first look at it. Only
// single-segment emissions on this context qualify: a message forwarded from a
// scoped child view arrives with its prefix already applied and its label
// already resolved against the child's own type, and one that carries a label
// was resolved at emit time.
func (c *NotificationContext) resolvePendingLabels() {
	if c.entityType == nil {
		return
	}
	root := c.root()
	for i := range root.messages {
		msg := &root.messages[i]
		if len(msg.Path) != 1 || msg.Path[0].Name == "" {
			continue
		}
		if msg.LabelKey == "" {
			msg.LabelKey = resolveLabelKey(c.entityType, msg.Path[0].Name)
		}
		if msg.Path[0].Wire == "" {
			msg.Path[0].Wire = resolveNotifyAs(c.entityType, msg.Path[0].Name)
		}
	}
}

// scopedForType is Scoped plus the entity type whose `labelKey:"…"` tags apply
// to emissions on the returned view — so a value object validated against a
// child AVO's context resolves its field label against that child's type.
func (c *NotificationContext) scopedForType(t reflect.Type, segments ...PathSegment) *NotificationContext {
	sc := c.Scoped(segments...)
	sc.entityType = t
	return sc
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
