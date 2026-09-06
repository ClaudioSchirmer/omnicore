package domain

import (
	"strings"
	"testing"
)

// --- fixtures ---------------------------------------------------------------

type refInner struct {
	Part string `notifyAs:"pt" labelKey:"inner.part"`
}

type refEntity struct {
	BaseEntity
	Name     string `labelKey:"ref.name"`
	ZipCode  string `notifyAs:"cep"`
	Nickname *string
	Inner    refInner
	Zero1    struct{}
	Zero2    struct{}
	hidden   string //nolint:unused — proves unexported fields stay out of the atlas
}

func (e *refEntity) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (e *refEntity) BuildRules(string, Service, *Rules) {}

func boundRules(t *testing.T, e *refEntity) (*Rules, *NotificationContext) {
	t.Helper()
	ctx := NewNotificationContext("Ref")
	return NewRulesFor(ModeInsert, ctx, e), ctx
}

func mustPanicRef(t *testing.T, contains string, fn func()) {
	t.Helper()
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatalf("expected a panic containing %q, got none", contains)
		}
		if msg, ok := rec.(string); !ok || !strings.Contains(msg, contains) {
			t.Fatalf("panic = %v, want it to contain %q", rec, contains)
		}
	}()
	fn()
}

// --- happy paths ------------------------------------------------------------

func TestFieldRef_ResolvesNameLabelAndValue(t *testing.T) {
	e := &refEntity{Name: "Ada"}
	r, ctx := boundRules(t, e)

	r.AddNotification(&e.Name, RequiredFieldNotification{}, true)

	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if got := msgs[0].ResolveFieldName(); got != "name" {
		t.Errorf("field = %q, want %q", got, "name")
	}
	if msgs[0].LabelKey != "ref.name" {
		t.Errorf("LabelKey = %q, want ref.name (read from the field's own tag)", msgs[0].LabelKey)
	}
	if msgs[0].FieldValue != "Ada" {
		t.Errorf("exposeValue must echo the field's value, got %q", msgs[0].FieldValue)
	}
}

func TestFieldRef_ExposeFalseOmitsValue(t *testing.T) {
	e := &refEntity{Name: "Ada"}
	r, ctx := boundRules(t, e)
	r.AddNotification(&e.Name, RequiredFieldNotification{}, false)
	if v := ctx.Messages()[0].FieldValue; v != "" {
		t.Errorf("exposeValue=false must not echo, got %q", v)
	}
}

func TestFieldRef_NotifyTagOverridesWireName(t *testing.T) {
	e := &refEntity{}
	r, ctx := boundRules(t, e)
	r.AddNotification(&e.ZipCode, RequiredFieldNotification{}, false)
	if got := ctx.Messages()[0].ResolveFieldName(); got != "cep" {
		t.Errorf("notifyAs tag must win over lowerCamel: got %q, want cep", got)
	}
}

func TestFieldRef_NestedFieldRendersDottedPath(t *testing.T) {
	e := &refEntity{}
	r, ctx := boundRules(t, e)
	r.AddNotification(&e.Inner.Part, RequiredFieldNotification{}, false)
	msg := ctx.Messages()[0]
	// Each segment carries its own tag: Inner has none (camel), Part has "pt".
	if got := msg.ResolveFieldName(); got != "inner.pt" {
		t.Errorf("nested path = %q, want inner.pt", got)
	}
	if msg.LabelKey != "inner.part" {
		t.Errorf("leaf labelKey = %q, want inner.part", msg.LabelKey)
	}
}

func TestFieldRef_PointerFieldByReference(t *testing.T) {
	nick := "Lin"
	e := &refEntity{Nickname: &nick}
	r, ctx := boundRules(t, e)
	r.AddNotification(&e.Nickname, RequiredFieldNotification{}, true)
	msg := ctx.Messages()[0]
	if got := msg.ResolveFieldName(); got != "nickname" {
		t.Errorf("field = %q, want nickname", got)
	}
	if msg.FieldValue != "Lin" {
		t.Errorf("pointer field must unwrap on echo, got %q", msg.FieldValue)
	}
}

func TestFieldRef_AliasOverridesNotifyTag(t *testing.T) {
	e := &refEntity{}
	ensureInit(e)
	e.AddFieldNameAlias("ZipCode", "postalCode")
	r := NewRulesFor(ModeInsert, e.NotificationContext(), e)
	r.AddNotification(&e.ZipCode, RequiredFieldNotification{}, false)
	applyFieldAliases(e)
	if got := e.NotificationContext().Messages()[0].ResolveFieldName(); got != "postalCode" {
		t.Errorf("instance alias must override the notifyAs tag: got %q", got)
	}
}

func TestFieldRef_ChangeFieldNameOverridesEverything(t *testing.T) {
	e := &refEntity{}
	r, ctx := boundRules(t, e)
	r.AddNotification(&e.ZipCode, RequiredFieldNotification{}, false)
	ctx.ChangeFieldName("cep", "codigoPostal")
	if got := ctx.Messages()[0].ResolveFieldName(); got != "codigoPostal" {
		t.Errorf("ChangeFieldName (Override) is top precedence: got %q", got)
	}
}

// --- pedagogic panics -------------------------------------------------------

func TestFieldRef_UnboundRulesPanics(t *testing.T) {
	r := NewRules(ModeInsert, NewNotificationContext("X"), nil)
	e := &refEntity{}
	mustPanicRef(t, "not bound", func() {
		r.AddNotification(&e.Name, RequiredFieldNotification{}, false)
	})
}

func TestFieldRef_NonPointerPanics(t *testing.T) {
	e := &refEntity{}
	r, _ := boundRules(t, e)
	mustPanicRef(t, "pointer to the entity's own field", func() {
		r.AddNotification(e.Name, RequiredFieldNotification{}, false)
	})
}

func TestFieldRef_PointerFieldWithoutAmpersandPanics(t *testing.T) {
	nick := "Lin"
	e := &refEntity{Nickname: &nick}
	r, _ := boundRules(t, e)
	// The classic footgun: the field is already a pointer, so e.Nickname
	// compiles — but it points at the heap, not into the entity.
	mustPanicRef(t, "pass &e.Field, not e.Field", func() {
		r.AddNotification(e.Nickname, RequiredFieldNotification{}, false)
	})
}

func TestFieldRef_CopyThroughHelperPanics(t *testing.T) {
	e := &refEntity{}
	r, _ := boundRules(t, e)
	copy := *e
	mustPanicRef(t, "points outside", func() {
		r.AddNotification(&copy.Name, RequiredFieldNotification{}, false)
	})
}

func TestFieldRef_ZeroSizeAmbiguityPanics(t *testing.T) {
	e := &refEntity{}
	r, _ := boundRules(t, e)
	// Zero1 and Zero2 are zero-size, same type, same offset — unresolvable by
	// construction, refused instead of guessed.
	mustPanicRef(t, "ambiguous", func() {
		r.AddNotification(&e.Zero1, RequiredFieldNotification{}, false)
	})
}

// --- the pointer-receiver AVO path ------------------------------------------

type refAVO struct {
	Managed
	Street string `labelKey:"avo.street"`
}

func (refAVO) CollectionName() string { return "RefAVOs" }
func (a refAVO) IsSameBusinessIdentity(other AggregateValueObject) bool {
	o, ok := other.(refAVO)
	return ok && a.Street == o.Street
}
func (a *refAVO) BuildRules(_ string, _ Service, r *Rules) {
	if a.Street == "" {
		r.AddNotification(&a.Street, RequiredFieldNotification{}, false)
	}
}

type refProvider struct {
	AggregateRoot
}

func (p *refProvider) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (p *refProvider) BuildRules(string, Service, *Rules) {}
func (p *refProvider) GetAggregateRoot() *AggregateRoot   { return &p.AggregateRoot }
func (p *refProvider) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{refAVO{}}
}

func TestFieldRef_AVOPointerReceiverComposesScopedPath(t *testing.T) {
	p := &refProvider{}
	ensureInit(p)

	if ok := ValidateAggregateChild(p, refAVO{Street: ""}, ModeInsert, "GetInsertable", nil); ok {
		t.Fatal("expected the AVO's BuildRules to reject the empty Street")
	}
	msgs := p.NotificationContext().Messages()
	found := false
	for _, m := range msgs {
		if m.ResolveFieldName() == "refAVOs[0].street" {
			found = true
			if m.LabelKey != "avo.street" {
				t.Errorf("AVO leaf label = %q, want avo.street", m.LabelKey)
			}
		}
	}
	if !found {
		t.Errorf("expected a message at refAVOs[0].street, got %+v", msgs)
	}
}

type noRulesAVO struct {
	Managed
	X string
}

func (noRulesAVO) CollectionName() string                           { return "NoRules" }
func (noRulesAVO) IsSameBusinessIdentity(AggregateValueObject) bool { return false }

func TestFieldRef_AVOWithoutBuildRulesPanicsWithContract(t *testing.T) {
	p := &refProvider{}
	ensureInit(p)
	mustPanicRef(t, "POINTER receiver", func() {
		ValidateAggregateChild(p, noRulesAVO{}, ModeInsert, "GetInsertable", nil)
	})
}

type badChildProvider struct {
	AggregateRoot
}

func (p *badChildProvider) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (p *badChildProvider) BuildRules(string, Service, *Rules) {}
func (p *badChildProvider) GetAggregateRoot() *AggregateRoot   { return &p.AggregateRoot }
func (p *badChildProvider) AggregateChildren() []AggregateValueObject {
	return []AggregateValueObject{noRulesAVO{}}
}

// A DECLARED child without the pointer-receiver BuildRules explodes at the
// first primitive that consults AggregateChildren() — aggregate construction
// time — not at the first validation pass (i.e. the first write attempt).
func TestFieldRef_DeclaredChildWithoutBuildRulesPanicsAtDeclaration(t *testing.T) {
	p := &badChildProvider{}
	ensureInit(p)
	mustPanicRef(t, "POINTER receiver", func() {
		AddAggregateChild(p, noRulesAVO{X: "x"})
	})
}

// --- embeds and shared offsets ----------------------------------------------

type refEmbedBase struct {
	Code string `notifyAs:"codigo" labelKey:"embed.code"`
}

type refEmbedded struct {
	refEmbedBase        // anonymous at offset 0 — Code is promoted AND shares the embed's offset
	Title        string `labelKey:"embed.title"`
}

// A promoted field of an embed sitting at offset 0: the reference lands on the
// same address as the embed itself, and the atlas resolves it because the
// embed (anonymous) contributes no node of its own — only its promoted fields,
// with no extra path segment and with their declared vocabulary intact.
func TestFieldRef_PromotedFieldAtOffsetZeroResolves(t *testing.T) {
	e := &refEmbedded{}
	ctx := NewNotificationContext("Embed")
	r := NewRulesFor(ModeInsert, ctx, e)
	r.AddNotification(&e.Code, RequiredFieldNotification{}, false)
	msgs := ctx.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %+v", msgs)
	}
	if got := msgs[0].ResolveFieldName(); got != "codigo" {
		t.Errorf("promoted field = %q, want the notifyAs token %q", got, "codigo")
	}
	if msgs[0].LabelKey != "embed.code" {
		t.Errorf("labelKey = %q, want embed.code", msgs[0].LabelKey)
	}
}

type refHead struct {
	Inner refInner // struct field FIRST: Inner and Inner.Part share offset 0
	Tail  string
}

// A struct field and its own first field share an offset; the POINTER TYPE is
// what disambiguates them — &e.Inner (*refInner) and &e.Inner.Part (*string)
// resolve to different nodes at the same address.
func TestFieldRef_StructAndItsFirstFieldShareOffset(t *testing.T) {
	e := &refHead{}
	ctx := NewNotificationContext("Head")
	r := NewRulesFor(ModeInsert, ctx, e)

	r.AddNotification(&e.Inner.Part, RequiredFieldNotification{}, false)
	r.AddNotification(&e.Inner, RequiredFieldNotification{}, false)

	msgs := ctx.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %+v", msgs)
	}
	if got := msgs[0].ResolveFieldName(); got != "inner.pt" {
		t.Errorf("nested part = %q, want %q", got, "inner.pt")
	}
	if got := msgs[1].ResolveFieldName(); got != "inner" {
		t.Errorf("struct field = %q, want %q", got, "inner")
	}
}

// Equalization: a field's declared vocabulary (notifyAs + labelKey on the
// ENTITY's field) renders identically whichever seat emits about it — the
// field-reference rule, the automatic value-object pass, an enum membership
// refusal, a named emission, and a pre-init emission backfilled later. Raw and
// enum VOs are generic and shared: their names always come from the OWNING
// field; only an AggregateValueObject names its own fields.
type eqEmail string

func (e eqEmail) Value() string { return string(e) }
func (e eqEmail) IsValid(fieldName string, ctx *NotificationContext) bool {
	if e == "" {
		ctx.AddNotificationNamed(fieldName, RequiredFieldNotification{})
		return false
	}
	return true
}

type eqEntity struct {
	BaseEntity
	Contact eqEmail `notifyAs:"contato" labelKey:"eq.contact"`
}

func (e *eqEntity) Modes() []EntityMode                { return []EntityMode{ModeInsert} }
func (e *eqEntity) BuildRules(string, Service, *Rules) {}

func TestEqualization_EverySeatRendersTheSameWireName(t *testing.T) {
	assertMsg := func(t *testing.T, msgs []NotificationMessage, seat string) {
		t.Helper()
		if len(msgs) == 0 {
			t.Fatalf("%s emitted nothing", seat)
		}
		m := msgs[len(msgs)-1]
		if got := m.ResolveFieldName(); got != "contato" {
			t.Errorf("%s: field = %q, want the notifyAs token %q", seat, got, "contato")
		}
		if m.LabelKey != "eq.contact" {
			t.Errorf("%s: labelKey = %q, want eq.contact", seat, m.LabelKey)
		}
	}

	t.Run("field reference", func(t *testing.T) {
		e := &eqEntity{}
		ctx := NewNotificationContext("Eq")
		r := NewRulesFor(ModeInsert, ctx, e)
		r.AddNotification(&e.Contact, RequiredFieldNotification{}, false)
		assertMsg(t, ctx.Messages(), "field-ref")
	})

	t.Run("automatic VO pass", func(t *testing.T) {
		e := &eqEntity{}
		ensureInit(e)
		validateValueObjectFields(e, e.NotificationContext(), nil, nil)
		assertMsg(t, e.NotificationContext().Messages(), "auto-VO")
	})

	t.Run("named emission on Rules", func(t *testing.T) {
		e := &eqEntity{}
		ctx := NewNotificationContext("Eq")
		r := NewRulesFor(ModeInsert, ctx, e)
		r.AddNotificationNamed("Contact", RequiredFieldNotification{})
		assertMsg(t, ctx.Messages(), "rules-named")
	})

	t.Run("pre-init emission backfilled", func(t *testing.T) {
		e := &eqEntity{}
		// Emitted before the framework ever saw the entity — no entityType yet.
		e.AddNotificationNamed("Contact", RequiredFieldNotification{})
		ensureInit(e) // resolvePendingLabels backfills label AND wire token
		assertMsg(t, e.NotificationContext().Messages(), "pre-init backfill")
	})
}
