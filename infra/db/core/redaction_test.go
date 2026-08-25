package core

import (
	"reflect"
	"strings"
	"testing"
)

// redactFixture is the entity the redaction declarations in this file map
// against: one string field, one nullable string, one integer and one bool, so
// every type rule of the family has a real Go field behind it.
type redactFixture struct {
	ID       string
	Document string
	Nickname *string
	Salary   int64
	Active   bool
}

func (redactFixture) CollectionName() string { return "RedactFixtures" }

func redactSchema(t *testing.T, opts ...RedactedFieldOption) *TableSchema {
	t.Helper()
	return NewTableSchema[redactFixture]("redact_fixtures").
		ID("id").
		RedactedField("Document", "document", opts...)
}

func TestRedactedField_BothAxesAreMandatory(t *testing.T) {
	mustPanicWith(t, "does not declare core.InAudit(...)", func() {
		redactSchema(t, InSync(RedactWith("***")))
	})
	mustPanicWith(t, "does not declare core.InSync(...)", func() {
		redactSchema(t, InAudit(RedactWith("***")))
	})
	mustPanicWith(t, "does not declare core.InSync(...)", func() {
		redactSchema(t)
	})
}

// A redacted field is a field first: it joins the bijection exactly like Field,
// so the column map, the read columns and the write bind are unchanged.
func TestRedactedField_StaysAnOrdinaryMapping(t *testing.T) {
	s := redactSchema(t, InSync(RedactWith("***")), InAudit(Plain()))
	if col, ok := s.ColumnOf("Document"); !ok || col != "document" {
		t.Fatalf("ColumnOf(Document) = %q, %v", col, ok)
	}
	if goName, ok := s.GoOf("document"); !ok || goName != "Document" {
		t.Fatalf("GoOf(document) = %q, %v", goName, ok)
	}
	// The WRITE bind carries the real value — the column exists to hold it.
	fields := s.WriteFields(redactFixture{Document: "12345678901"})
	if got := fields["document"]; got != "12345678901" {
		t.Fatalf("WriteFields must keep the real value, got %v", got)
	}
}

func TestRedactSyncColumns(t *testing.T) {
	s := redactSchema(t, InSync(RedactKeepLast(4)), InAudit(RedactWith("***")))
	m := map[string]any{"document": "12345678901", "id": "abc"}
	s.RedactSyncColumns(m)
	if m["document"] != "*******8901" {
		t.Fatalf("InSync mask = %v, want *******8901", m["document"])
	}
	if m["id"] != "abc" {
		t.Fatalf("a column with no declaration must be untouched, got %v", m["id"])
	}
}

func TestRedactAuditValues_AndAuditRedactorFor(t *testing.T) {
	s := redactSchema(t, InSync(RedactKeepLast(4)), InAudit(RedactWith("***")))
	m := map[string]any{"Document": "12345678901"}
	s.RedactAuditValues(m)
	if m["Document"] != "***" {
		t.Fatalf("InAudit mask = %v, want ***", m["Document"])
	}
	if _, ok := s.AuditRedactorFor("Document"); !ok {
		t.Fatal("AuditRedactorFor(Document) must resolve")
	}
	if _, ok := s.AuditRedactorFor("Nickname"); ok {
		t.Fatal("AuditRedactorFor must not resolve an undeclared field")
	}
}

// Plain declares an axis without transforming it — the whole point of having it
// in the family once both axes are mandatory.
func TestPlain_KeepsTheRealValueOnItsAxis(t *testing.T) {
	s := redactSchema(t, InSync(RedactWith("***")), InAudit(Plain()))
	sync := map[string]any{"document": "12345678901"}
	s.RedactSyncColumns(sync)
	if sync["document"] != "***" {
		t.Fatalf("InSync = %v, want ***", sync["document"])
	}
	auditValues := map[string]any{"Document": "12345678901"}
	s.RedactAuditValues(auditValues)
	if auditValues["Document"] != "12345678901" {
		t.Fatalf("InAudit(Plain()) must keep the real value, got %v", auditValues["Document"])
	}
}

// NULL STAYS NULL on every kind. Substituting for a nil would lie about
// nullability and — worse — make a 1:1 sibling row that this write DELETED
// arrive with a non-null column, so the projector would keep the stale
// sub-document alive instead of dropping it.
func TestApply_NilStaysNil(t *testing.T) {
	var nilPtr *string
	for name, r := range map[string]Redactor{
		"fixed":    RedactWith("***"),
		"keepLast": RedactKeepLast(4),
		"using":    RedactUsing(func(string) string { return "x" }),
		"plain":    Plain(),
	} {
		if got := r.Apply(nil); got != nil {
			t.Fatalf("%s: Apply(nil) = %v, want nil", name, got)
		}
		// A nullable field reaches the map as a typed nil pointer (writeFields
		// stores what the struct holds), and that is what binds as SQL NULL and
		// marshals as JSON null. Apply must hand it back untouched — asserting
		// `!= nil` would be wrong here, since an interface holding a typed nil is
		// not the nil interface.
		if got := r.Apply(nilPtr); !isNilValue(got) {
			t.Fatalf("%s: Apply((*string)(nil)) = %#v, want a NULL", name, got)
		}
	}
}

func TestRedactKeepLast_Masking(t *testing.T) {
	cases := []struct {
		in   string
		keep int
		want string
	}{
		{"12345678901", 4, "*******8901"},
		{"12345678901", 1, "**********1"},
		// A value no longer than n is masked ENTIRELY: keeping it verbatim would
		// disclose the whole value precisely for the shortest inputs.
		{"1234", 4, "****"},
		{"12", 4, "**"},
		{"", 4, ""},
		// Runes, not bytes — a multi-byte value keeps its visible length.
		{"joão123", 3, "****123"},
	}
	for _, c := range cases {
		if got := maskKeepLast(c.in, c.keep); got != c.want {
			t.Errorf("maskKeepLast(%q, %d) = %q, want %q", c.in, c.keep, got, c.want)
		}
	}
}

// A nullable field arrives as a pointer; the mask reads through it and returns
// the plain scalar, which is the form PayloadColumnTypes declares.
func TestRedactKeepLast_DereferencesNullable(t *testing.T) {
	v := "12345678901"
	if got := RedactKeepLast(4).Apply(&v); got != "*******8901" {
		t.Fatalf("Apply(*string) = %v", got)
	}
}

func TestRedactUsing_RunsTheHook(t *testing.T) {
	r := RedactUsing(func(s string) string { return "[" + s[:1] + "]" })
	if got := r.Apply("joão"); got != "[j]" {
		t.Fatalf("Apply = %v, want [j]", got)
	}
}

// A hook that empties a non-empty value fails LOUD: an empty scalar reads as a
// removed 1:1 sibling row in the payload contract, so silently accepting it
// would delete a live sibling from the projected document.
func TestRedactUsing_EmptyingANonEmptyValuePanics(t *testing.T) {
	mustPanicWith(t, "returned an empty string for a non-empty value", func() {
		_ = RedactUsing(func(string) string { return "" }).Apply("12345678901")
	})
}

func TestRedactWith_TypeIsValidatedAgainstTheColumn(t *testing.T) {
	// A numeric literal is converted to the column's own numeric type, so
	// RedactWith(0) is legal on an int64 field and the payload still carries int64.
	s := NewTableSchema[redactFixture]("redact_fixtures").
		ID("id").
		RedactedField("Salary", "salary", InSync(RedactWith(0)), InAudit(RedactWith(0)))
	m := map[string]any{"salary": int64(9900)}
	s.RedactSyncColumns(m)
	if got := m["salary"]; got != int64(0) {
		t.Fatalf("redacted salary = %#v, want int64(0)", got)
	}
	if got := reflect.TypeOf(m["salary"]).Kind(); got != reflect.Int64 {
		t.Fatalf("the replacement must carry the column's own type, got %s", got)
	}

	// Anything that is not a numeric-to-numeric normalization must match exactly.
	mustPanicWith(t, "the field's persisted scalar is int64", func() {
		NewTableSchema[redactFixture]("redact_fixtures").
			ID("id").
			RedactedField("Salary", "salary", InSync(RedactWith("***")), InAudit(Plain()))
	})
	mustPanicWith(t, "the field's persisted scalar is bool", func() {
		NewTableSchema[redactFixture]("redact_fixtures").
			ID("id").
			RedactedField("Active", "active", InSync(RedactWith("***")), InAudit(Plain()))
	})
}

// A mask that changes the column's type would break PayloadColumnTypes and the
// view's $jsonSchema, so the string-only members are refused elsewhere.
func TestStringOnlyMembers_RefuseANonStringColumn(t *testing.T) {
	mustPanicWith(t, "RedactKeepLast, which is string-only", func() {
		NewTableSchema[redactFixture]("redact_fixtures").
			ID("id").
			RedactedField("Salary", "salary", InSync(RedactKeepLast(4)), InAudit(Plain()))
	})
	mustPanicWith(t, "RedactUsing, which is string-only", func() {
		NewTableSchema[redactFixture]("redact_fixtures").
			ID("id").
			RedactedField("Salary", "salary", InSync(RedactUsing(strings.ToUpper)), InAudit(Plain()))
	})
}

// A nullable string is still a string field: the pointer is not what the
// redactor sees, so the string-only members are legal on it.
func TestStringOnlyMembers_AcceptANullableString(t *testing.T) {
	s := NewTableSchema[redactFixture]("redact_fixtures").
		ID("id").
		RedactedField("Nickname", "nickname", InSync(RedactKeepLast(2)), InAudit(Plain()))
	m := map[string]any{"nickname": nil}
	s.RedactSyncColumns(m)
	if m["nickname"] != nil {
		t.Fatalf("a NULL nullable field must stay NULL, got %v", m["nickname"])
	}
}

func TestRedactedField_RejectsDegenerateDeclarations(t *testing.T) {
	mustPanicWith(t, "n must be positive", func() {
		redactSchema(t, InSync(RedactKeepLast(0)), InAudit(Plain()))
	})
	mustPanicWith(t, "RedactUsing(nil)", func() {
		redactSchema(t, InSync(RedactUsing(nil)), InAudit(Plain()))
	})
	mustPanicWith(t, "RedactWith(nil)", func() {
		redactSchema(t, InSync(RedactWith(nil)), InAudit(Plain()))
	})
}

// ─── Composite value objects — a PART is a persisted field like any other ────
//
// A part lands in the payload, the projected document and the audit event under
// its EXPOSED name, so it carries the same two axes a root field does — and it
// carries them INDEPENDENTLY of its siblings inside the value object: the
// currency of a salary is not sensitive, the amount is.

func cvRedactSchema(amount Redactor) *TableSchema {
	return NewTableSchema[cvContract]("contracts").
		ID("id").
		Field("Code", "code").
		Composite(NewCompositeValueObject[cvMoney]().
			RedactedField("Amount", "salary_amount", InSync(amount), InAudit(RedactWith(int64(0)))).As("SalaryAmount").
			Field("Currency", "salary_currency").As("SalaryCurrency"))
}

func TestCompositePart_RedactionAppliesUnderTheExposedName(t *testing.T) {
	s := cvRedactSchema(RedactWith(int64(0)))
	sync := map[string]any{"salary_amount": int64(850000), "salary_currency": "BRL"}
	s.RedactSyncColumns(sync)
	if sync["salary_amount"] != int64(0) {
		t.Fatalf("the redacted part = %#v, want int64(0)", sync["salary_amount"])
	}
	if sync["salary_currency"] != "BRL" {
		t.Fatalf("a sibling part of the SAME value object must be untouched, got %v", sync["salary_currency"])
	}
	// The audit map is keyed by the EXPOSED name (the As alias), which is what the
	// timeline speaks.
	audit := map[string]any{"SalaryAmount": int64(850000), "SalaryCurrency": "BRL"}
	s.RedactAuditValues(audit)
	if audit["SalaryAmount"] != int64(0) {
		t.Fatalf("audit SalaryAmount = %#v, want int64(0)", audit["SalaryAmount"])
	}
	if _, ok := s.AuditRedactorFor("SalaryAmount"); !ok {
		t.Fatal("AuditRedactorFor must resolve the part under its exposed name")
	}
}

func TestCompositePart_BothAxesAreMandatory(t *testing.T) {
	mustPanicWith(t, "does not declare core.InAudit(...)", func() {
		NewCompositeValueObject[cvMoney]().
			RedactedField("Amount", "salary_amount", InSync(RedactWith(int64(0))))
	})
	mustPanicWith(t, "does not declare core.InSync(...)", func() {
		NewCompositeValueObject[cvMoney]().
			RedactedField("Amount", "salary_amount", InAudit(RedactWith(int64(0))))
	})
}

// A part's type is validated against the field INSIDE the value object, so the
// same rules a root field gets apply one level down.
func TestCompositePart_TypeIsValidatedAgainstThePart(t *testing.T) {
	mustPanicWith(t, "the field's persisted scalar is int64", func() {
		NewCompositeValueObject[cvMoney]().
			RedactedField("Amount", "salary_amount", InSync(RedactWith("***")), InAudit(Plain()))
	})
	mustPanicWith(t, "RedactKeepLast, which is string-only", func() {
		NewCompositeValueObject[cvMoney]().
			RedactedField("Amount", "salary_amount", InSync(RedactKeepLast(2)), InAudit(Plain()))
	})
}

// A part's header comes from the tag inside the value object — the value object
// owns its own vocabulary — so Label is refused here, exactly as a schema-level
// labelKey is refused on a type-anchored schema.
func TestCompositePart_LabelIsRefused(t *testing.T) {
	mustPanicWith(t, "core.Label(...) on part", func() {
		NewCompositeValueObject[cvMoney]().
			RedactedField("Amount", "salary_amount",
				InSync(Plain()), InAudit(Plain()), Label("Whatever"))
	})
}

// The declaration reaches the schema's shape, so it moves the view hash like any
// other field's does (asserted end to end in infra/db/query).
func TestCompositePart_ParticipatesInTheRedactionShape(t *testing.T) {
	shape := cvRedactSchema(RedactWith(int64(0))).RedactionShape()
	if len(shape) != 1 || !strings.Contains(shape[0], "salary_amount=") {
		t.Fatalf("RedactionShape = %v, want one entry for salary_amount", shape)
	}
	other := cvRedactSchema(RedactWith(int64(-1))).RedactionShape()
	if shape[0] == other[0] {
		t.Fatal("a different replacement value must render a different fingerprint")
	}
}

// ─── The type-less EXTERNAL schema — the seventh declarable site ─────────────
//
// NewExternalSchema is the one site with no Go struct behind it: its columns
// belong to an UPSTREAM service and are consumed through query.JoinUpstream, so
// a redaction there means "do not project this upstream column into MY
// document". It has no place in a domain fixture, which is why it is proven here
// rather than in the e2e suite.
//
// Two things follow from the missing struct, and both are asserted: the type
// check is SKIPPED (there is no field to read a type from), and the redaction
// still applies to the column normally.

func TestExternalSchema_RedactionApplies(t *testing.T) {
	s := NewExternalSchema("upstream_people").
		ID("id").
		Field("DisplayName", "display_name").
		RedactedField("Document", "document",
			InSync(RedactKeepLast(3)),
			InAudit(RedactWith("***")),
		)

	m := map[string]any{"document": "52998224725", "display_name": "Ana"}
	s.RedactSyncColumns(m)
	if m["document"] != "********725" {
		t.Fatalf("external InSync = %v, want ********725", m["document"])
	}
	if m["display_name"] != "Ana" {
		t.Fatalf("an undeclared external column must be untouched, got %v", m["display_name"])
	}
	if len(s.RedactionShape()) != 1 {
		t.Fatalf("the declaration must reach the hashed shape, got %v", s.RedactionShape())
	}
}

// With no struct to validate against, a strategy the framework would refuse on a
// typed schema is ACCEPTED here — there is nothing to contradict. Documenting
// that in a test keeps it a known consequence of type-lessness rather than a
// surprise: an upstream column's type is the upstream's business.
func TestExternalSchema_TypeCheckIsSkipped(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a type-less schema has no field to type-check; got panic: %v", r)
		}
	}()
	NewExternalSchema("upstream_people").
		ID("id").
		RedactedField("Amount", "amount", InSync(RedactKeepLast(2)), InAudit(Plain()))
}

// The axes stay mandatory even where the type check cannot run: the framework
// refuses to guess a security policy regardless of what it can see.
func TestExternalSchema_BothAxesStillMandatory(t *testing.T) {
	mustPanicWith(t, "does not declare core.InAudit(...)", func() {
		NewExternalSchema("upstream_people").
			ID("id").
			RedactedField("Document", "document", InSync(RedactWith("***")))
	})
}

// A plain Field declares nothing, so every redaction walk over it is a no-op and
// a service that uses no RedactedField pays one boolean per write.
func TestPlainField_HasNoRedaction(t *testing.T) {
	s := NewTableSchema[redactFixture]("redact_fixtures").
		ID("id").
		Field("Document", "document")
	if s.HasRedactions() {
		t.Fatal("a schema with no RedactedField must report no redactions")
	}
	m := map[string]any{"document": "12345678901"}
	s.RedactSyncColumns(m)
	if m["document"] != "12345678901" {
		t.Fatalf("a plain Field must be untouched, got %v", m["document"])
	}
}
