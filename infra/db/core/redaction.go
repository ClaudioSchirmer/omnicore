package core

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

// Field-level redaction — how a field declared with RedactedField appears in
// each DERIVED COPY of the row, while the relational column and the hydrated
// entity keep the real value.
//
// The line the feature draws is WHO MAKES THE COPY. The framework copies a row
// by itself — into the outbox payload (and from there the topic, the failure
// ledgers and the Mongo document), and into the audit event. Those copies are
// the framework's responsibility, so they are what a Redactor governs. Exposure
// on a read surface is the DEVELOPER's: the Response DTO is the wire authority,
// `filter:`/`sort:` are opt-in tags, and per-identity field authorization is
// ReadCriteria.Restrict. Nothing here refuses a read.
//
// Two independent axes, declared per field and BOTH mandatory (InSync,
// InAudit): a missing axis is a construction panic rather than a framework
// guess at a security policy.
//
//   - InSync governs the payload assembled at commit — which is why it also
//     reaches the topic, every consuming service, the two failure ledgers, and
//     the projected document (for most views the document IS the payload; the
//     composer applies the SAME redactor so a rebuild cannot reintroduce what
//     the sync excluded).
//   - InAudit governs the audit event, applied AFTER the delta is computed, so
//     the trail still records THAT a field changed without recording what to.
//
// The family is closed and deliberately small: Plain, RedactWith,
// RedactKeepLast, RedactUsing. There is no Omit — an absent key already means
// "the 1:1 sibling row was removed" in the payload contract (see
// buildProjectionStages), so dropping a key would delete a live sibling from
// the document; and in the audit an absent entry is exactly the information the
// delta exists to carry.

// redactorKind discriminates the closed family. The zero value is UNSET, which
// is what makes a missing axis detectable at declaration time.
type redactorKind uint8

const (
	redactUnset redactorKind = iota
	redactPlain
	redactFixed
	redactKeepLast
	redactFunc
)

// Redactor is how one field's value appears in one derived copy. Values are
// produced only by the four constructors below; the zero Redactor means "not
// declared" and is refused by RedactedField.
type Redactor struct {
	kind  redactorKind
	fixed any
	keep  int
	fn    func(string) string
}

// Plain keeps the real value on this axis. It is not a no-op declaration: with
// both axes mandatory it is the only way to express "masked in the derived
// copies, intact in the audit trail" (or the reverse).
func Plain() Redactor { return Redactor{kind: redactPlain} }

// RedactWith replaces the value with a fixed one. v must match the field's
// effective scalar type — the type behind a value object, and behind a pointer
// for a nullable field — so the payload keeps the shape PayloadColumnTypes
// declares and the view's $jsonSchema still validates. A numeric literal is
// converted to the column's numeric type (RedactWith(0) on an int64 column is
// stored as int64(0)); every other mismatch is a construction panic.
func RedactWith(v any) Redactor { return Redactor{kind: redactFixed, fixed: v} }

// RedactKeepLast masks every rune but the last n — the partial mask for a
// document number, a card or a phone. String columns only. A value with n runes
// or fewer is masked ENTIRELY: keeping it verbatim would disclose the whole
// value precisely for the shortest inputs.
func RedactKeepLast(n int) Redactor { return Redactor{kind: redactKeepLast, keep: n} }

// RedactUsing masks through a caller-supplied function — the escape hatch for
// what the family does not cover (the local half of an e-mail, a formatted
// document number). String columns only.
//
// f MUST BE PURE. It runs at TWO independent points: assembling the payload
// inside the write transaction, and composing the document in the composer —
// which is also the rebuild path, possibly months later in a different binary.
// A f that reads the clock, randomness or mutable config makes a rebuilt
// document diverge from the one the sync wrote. The framework cannot verify
// purity; what it has is a late backstop (the verify shape check and the
// payload/composer equivalence gate).
//
// Two runtime rules keep a faulty f from failing OPEN: returning empty for a
// non-empty input panics (an empty string in a sibling group would read as a
// removed row), and a panic inside f is NOT recovered here — it unwinds into
// the persister's deferred rollback, so the write is abandoned rather than
// completed with an unredacted value.
func RedactUsing(f func(string) string) Redactor { return Redactor{kind: redactFunc, fn: f} }

// declared reports whether this axis carries a declaration.
func (r Redactor) declared() bool { return r.kind != redactUnset }

// active reports whether this axis actually transforms the value. Plain and
// unset do not, which lets the hot paths skip the walk entirely.
func (r Redactor) active() bool {
	return r.kind == redactFixed || r.kind == redactKeepLast || r.kind == redactFunc
}

// Apply returns the value as this axis exposes it.
//
// NULL STAYS NULL, on every kind. A nil value means the column is SQL NULL, and
// substituting anything for it would both lie about nullability and break the
// payload's all-null rule in the dangerous direction: a 1:1 sibling row deleted
// by the write would arrive with a non-null column and the projector would keep
// the stale sub-document alive forever.
func (r Redactor) Apply(v any) any {
	if !r.active() || isNilValue(v) {
		return v
	}
	switch r.kind {
	case redactFixed:
		return r.fixed
	case redactKeepLast:
		return maskKeepLast(redactableString(v), r.keep)
	case redactFunc:
		in := redactableString(v)
		out := r.fn(in)
		if in != "" && out == "" {
			panic(fmt.Sprintf(
				"core.RedactUsing: the redactor returned an empty string for a non-empty value — an empty "+
					"scalar reads as a removed 1:1 sibling row in the payload contract; return a mask "+
					"(e.g. %q) instead of nothing", "***"))
		}
		return out
	}
	return v
}

// isNilValue reports whether v carries no value — an untyped nil, or a typed
// nil pointer/interface/map/slice that reached the map as SQL NULL.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// redactableString renders a string-typed value, dereferencing the nullable
// form. Reached only after the declaration guard proved the column's effective
// scalar is string, so the type assertion cannot be surprised by a foreign kind.
func redactableString(v any) string {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String {
		return rv.String()
	}
	return fmt.Sprint(v)
}

// maskKeepLast replaces every rune but the trailing n with '*', counting RUNES
// so a multi-byte value keeps its visible length. n <= 0, or a value no longer
// than n, masks everything.
func maskKeepLast(s string, n int) string {
	total := utf8.RuneCountInString(s)
	keep := n
	if keep < 0 || keep >= total {
		keep = 0
	}
	return strings.Repeat("*", total-keep) + string([]rune(s)[total-keep:])
}

// RedactedFieldOption is one clause of a RedactedField declaration. The two
// axes are mandatory; Label is external-only (a type-anchored schema declares
// its header through the `labelKey:"…"` struct tag, exactly like Field).
type RedactedFieldOption func(*redactedFieldSpec)

type redactedFieldSpec struct {
	inSync   Redactor
	inAudit  Redactor
	labelKey string
}

// InSync declares how the field appears in the derived copies the sync pipeline
// carries: the outbox payload, and therefore the topic, the consuming services,
// the failure ledgers and the projected document (rebuild and ripple included).
func InSync(r Redactor) RedactedFieldOption {
	return func(s *redactedFieldSpec) { s.inSync = r }
}

// InAudit declares how the field appears in the audit event — the audit_events
// row, the slog echo and the /audit endpoint, which all read the same event.
// Applied after the delta is computed, so a change is still recorded as having
// happened.
func InAudit(r Redactor) RedactedFieldOption {
	return func(s *redactedFieldSpec) { s.inAudit = r }
}

// Label declares the header catalog key of a redacted field on a TYPE-LESS
// schema (NewExternalSchema) — the option form of the labelKey Field takes
// variadically. Declaring it on a type-anchored schema is a panic, same rule as
// Field's.
func Label(catalogKey string) RedactedFieldOption {
	return func(s *redactedFieldSpec) { s.labelKey = catalogKey }
}

// mustFit validates one axis against the field's effective scalar type at
// declaration time, and returns the axis with a fixed value normalized to that
// type. scalar is nil on a type-less schema, where there is no Go field to
// validate against — the declaration is accepted and the mismatch, if any,
// surfaces where every other type-less mismatch does.
func (r Redactor) mustFit(table, goName, axis string, scalar reflect.Type) Redactor {
	switch r.kind {
	case redactUnset, redactPlain:
		return r
	case redactKeepLast:
		if r.keep <= 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): %s on field %q declares RedactKeepLast(%d) — n must be positive; "+
					"to mask the whole value use RedactWith(\"***\")",
				table, axis, goName, r.keep))
		}
		mustStringScalar(table, goName, axis, "RedactKeepLast", scalar)
		return r
	case redactFunc:
		if r.fn == nil {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): %s on field %q declares RedactUsing(nil)", table, axis, goName))
		}
		mustStringScalar(table, goName, axis, "RedactUsing", scalar)
		return r
	case redactFixed:
		return r.mustFitFixed(table, goName, axis, scalar)
	}
	return r
}

// mustFitFixed checks a RedactWith value against the column's effective scalar
// and normalizes a numeric literal to it, so RedactWith(0) is legal on an
// int64/float64 column and the payload still carries the column's own type.
func (r Redactor) mustFitFixed(table, goName, axis string, scalar reflect.Type) Redactor {
	if r.fixed == nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s on field %q declares RedactWith(nil) — a redacted copy never turns a "+
				"present value into NULL (an all-null group reads as a removed sibling row); declare a "+
				"non-nil replacement",
			table, axis, goName))
	}
	if scalar == nil {
		return r
	}
	ft := reflect.TypeOf(r.fixed)
	if ft == scalar {
		return r
	}
	if isNumericKind(ft.Kind()) && isNumericKind(scalar.Kind()) {
		r.fixed = reflect.ValueOf(r.fixed).Convert(scalar).Interface()
		return r
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): %s on field %q declares RedactWith(%s) but the field's persisted scalar is %s — "+
			"the replacement must carry the column's own type, or the payload breaks the type map the read-side "+
			"decoder coerces through (PayloadColumnTypes) and the view's $jsonSchema. Declare RedactWith(%s(…)).",
		table, axis, goName, ft, scalar, scalar))
}

func mustStringScalar(table, goName, axis, ctor string, scalar reflect.Type) {
	if scalar == nil || scalar.Kind() == reflect.String {
		return
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): %s on field %q declares %s, which is string-only, but the field's persisted "+
			"scalar is %s — a mask that changes the column's type breaks PayloadColumnTypes and the view's "+
			"$jsonSchema. Use RedactWith(%s(…)) instead.",
		table, axis, goName, ctor, scalar, scalar))
}

func isNumericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// naturalKeyRedactionPanic is the diagnostic for the one slot a redacted field
// cannot occupy. Every OTHER framework-owned slot (ID, ParentID, Revision,
// DeletedAt, CreatedAt, UpdatedAt) is already mutually exclusive with any field
// declaration, in both orders — mustClaimNames refuses a field on those columns
// and each setter refuses a column a field already claimed. The natural key is
// the exception, because it MUST also be a mapped field: it is a business column
// the base writes like any other.
//
// It still cannot be a REDACTED one. A shared base's id is
// UUIDv5(fixed public namespace, natural key) — an unsalted SHA-1 of the
// plaintext — and that id travels as _ids.base_id in every payload and is the _id
// of the projected document. Masking the column while the derived id discloses it
// is a declaration the framework cannot honor: for a low-entropy key (an
// 11-digit taxpayer id is ≈2³⁷) recovering the value from the id is an offline
// brute force. Refusing it at construction is the same class of guard as the
// type check on RedactWith — not a policy the framework is imposing, but a
// promise it will not pretend to keep.
func naturalKeyRedactionPanic(table, column, goName string) string {
	return fmt.Sprintf(
		"infra.TableSchema(%s): %q is the shared base's NaturalID column and cannot be a RedactedField "+
			"(declared here as %q). The base's id is derived from this value in the clear "+
			"(UUIDv5 over a fixed, public namespace), and that id travels in every payload and is the "+
			"projected document's _id — so masking the column would hide nothing. If the value is "+
			"sensitive it is not the identity: keep the natural key on a non-sensitive column (or a "+
			"synthetic one) and declare the sensitive value as an ordinary RedactedField beside it.",
		table, column, goName)
}

// mustNotRedactNaturalKey enforces that rule from the FIELD side — a
// RedactedField naming a column the schema already declared as its natural key.
func (s *TableSchema) mustNotRedactNaturalKey(column, goName string) {
	if s.naturalIDCol != "" && column == s.naturalIDCol {
		panic(naturalKeyRedactionPanic(s.table, column, goName))
	}
}

// mustNotRedactNaturalIDColumn enforces the same rule from the OTHER side — a
// NaturalID call naming a column already declared as a redacted field. Declaration
// order must not decide whether a guard fires, and NaturalID deliberately does
// not run ensureColumnFree (the natural key is REQUIRED to be a mapped field), so
// it needs this narrower check of its own.
func (s *TableSchema) mustNotRedactNaturalIDColumn(column string) {
	f, ok := s.byCol[column]
	if !ok {
		return
	}
	if f.inSync.declared() || f.inAudit.declared() {
		panic(naturalKeyRedactionPanic(s.table, column, f.goName))
	}
}

// HasRedactions reports whether any field of THIS schema declares a redaction
// that actually transforms a value. The copy paths gate their walk on it, so a
// service that declares no RedactedField pays a single boolean per write.
func (s *TableSchema) HasRedactions() bool {
	if s == nil {
		return false
	}
	for _, f := range s.fields {
		if f.inSync.active() || f.inAudit.active() {
			return true
		}
	}
	return false
}

// RedactSyncColumns applies each field's InSync redactor in place over a
// COLUMN-KEYED map — the outbox payload as buildWritePayload assembles it, and
// the composed document on the composer/rebuild side. Keys the schema does not
// own are left untouched, so a caller may hand it a map that also carries
// managed columns or another schema's segment.
//
// It must run on a COPY of the write map, never on the domain.Fields the DML
// binds: the payload and the INSERT share one map by design (see
// aggregate_write.go), and redacting in place would write the mask to the
// column that is supposed to hold the real value.
func (s *TableSchema) RedactSyncColumns(m map[string]any) {
	if s == nil || m == nil {
		return
	}
	for _, f := range s.fields {
		if !f.inSync.active() {
			continue
		}
		if v, ok := m[f.column]; ok {
			m[f.column] = f.inSync.Apply(v)
		}
	}
}

// RedactAuditValues applies each field's InAudit redactor in place over a
// GO-FIELD-KEYED map — an audit snapshot as GoFieldValues produced it.
//
// On a delta this is NOT the whole job: the diff must be computed on the real
// values first (computeChanges drops keys whose sides compare equal, so
// redacting first would erase the very fact that the field changed), and the
// resulting entries redacted afterwards through AuditRedactorFor.
func (s *TableSchema) RedactAuditValues(m map[string]any) {
	if s == nil || m == nil {
		return
	}
	for _, f := range s.fields {
		if !f.inAudit.active() {
			continue
		}
		if v, ok := m[f.goName]; ok {
			m[f.goName] = f.inAudit.Apply(v)
		}
	}
}

// AuditRedactorFor returns the InAudit redactor declared for a Go field name,
// and whether it transforms anything. It is the post-diff hook: the audit
// builder walks the computed changes and rewrites From/To through it, keeping
// the entry itself so the trail still says the field changed.
func (s *TableSchema) AuditRedactorFor(goName string) (Redactor, bool) {
	if s == nil {
		return Redactor{}, false
	}
	f, ok := s.byGo[goName]
	if !ok || !f.inAudit.active() {
		return Redactor{}, false
	}
	return f.inAudit, true
}

// fingerprint renders this axis as a stable token for the view hash. It must be
// deterministic across processes and builds, which is why RedactUsing collapses
// to its kind alone: a closure has no portable identity, so the framework CANNOT
// see that the function's body changed. Changing what a hook returns is
// therefore a shape change the DEVELOPER must announce with a Version bump —
// documented, not silently detected.
func (r Redactor) fingerprint() string {
	switch r.kind {
	case redactPlain:
		return "plain"
	case redactFixed:
		return fmt.Sprintf("fixed(%v:%v)", reflect.TypeOf(r.fixed), r.fixed)
	case redactKeepLast:
		return fmt.Sprintf("keepLast(%d)", r.keep)
	case redactFunc:
		return "using"
	}
	return ""
}

// RedactionShape renders this schema's declared redaction as sorted, stable
// tokens — the material the view hash mixes in so that DECLARING a redaction,
// or CHANGING one, is a shape change the framework detects.
//
// That detection is the mechanism that cleans up: a changed hash is
// DriftForgotToBump, which forces a Version bump, which forces a rebuild, and
// the rebuild recomposes every document through the composer — replacing the
// values a previous policy had already projected. Without it, turning a field
// redacted would protect future writes while every document already in the read
// model kept its plaintext.
//
// EMPTY when no field of this schema declares a redaction, and the hash writer
// omits the block entirely in that case: a service that does not use the feature
// must hash exactly as it did before the feature existed, or every view in every
// existing service would report drift on its next boot.
func (s *TableSchema) RedactionShape() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, f := range s.fields {
		if !f.inSync.declared() && !f.inAudit.declared() {
			continue
		}
		out = append(out, fmt.Sprintf("%s=sync:%s;audit:%s",
			f.column, f.inSync.fingerprint(), f.inAudit.fingerprint()))
	}
	sort.Strings(out)
	return out
}

// effectiveScalar is the type a redactor actually sees: the scalar behind a
// value object (the write path unwraps it), and behind a pointer for a nullable
// field. Mirrors what writeFields and GoFieldValues put in their maps.
func effectiveScalar(ft reflect.Type) reflect.Type {
	if _, u, ok := valueObjectField(ft); ok {
		ft = u
	}
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	return ft
}
