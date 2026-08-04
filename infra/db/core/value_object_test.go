package core

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- test value objects -----------------------------------------------------

// voMoney is a raw value object over int.
type voMoney int

func (m voMoney) Value() int                                       { return int(m) }
func (m voMoney) IsValid(string, *domain.NotificationContext) bool { return true }

// voName is a raw value object over string.
type voName string

func (n voName) Value() string                                    { return string(n) }
func (n voName) IsValid(string, *domain.NotificationContext) bool { return n != "" }

// voTier is an enum value object over int (members 1,2; Unknown = 0).
type voTier int

const (
	tierUnknown voTier = 0
	tierGold    voTier = 1
	tierSilver  voTier = 2
)

func (t voTier) Value() int                               { return int(t) }
func (t voTier) Values() []voTier                         { return []voTier{tierGold, tierSilver} }
func (t voTier) UnknownNotification() domain.Notification { return voTierUnknownNote{} }

type voTierUnknownNote struct{ domain.DomainNotificationBase }

// voEntity is a schema-anchored entity carrying VO fields of each shape.
type voEntity struct {
	domain.BaseEntity
	Name  voName `labelKey:"-"`
	Money voMoney
	Tier  voTier
	Alt   *voName // nullable raw VO
	Rank  *voTier // nullable enum VO
}

func (e *voEntity) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *voEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func voSchema() *TableSchema {
	return NewTableSchema[*voEntity]("vos").
		ID("id").
		Field("Name", "name").
		Field("Money", "money").
		Field("Tier", "tier").
		Field("Alt", "alt").
		Field("Rank", "rank")
}

// --- Field() accepts VO + records metadata ---------------------------------

func TestField_AcceptsValueObject_RecordsUnderlying(t *testing.T) {
	s := voSchema() // must not panic
	cases := map[string]struct {
		isVO       bool
		isEnum     bool
		underlying reflect.Kind
	}{
		"Name":  {true, false, reflect.String},
		"Money": {true, false, reflect.Int},
		"Tier":  {true, true, reflect.Int},
		"Alt":   {true, false, reflect.String},
		"Rank":  {true, true, reflect.Int},
	}
	for _, f := range s.fields {
		want, ok := cases[f.goName]
		if !ok {
			continue
		}
		if f.isVO != want.isVO || f.isEnum != want.isEnum {
			t.Errorf("%s: isVO=%v isEnum=%v want %v/%v", f.goName, f.isVO, f.isEnum, want.isVO, want.isEnum)
		}
		if f.underlyingType == nil || f.underlyingType.Kind() != want.underlying {
			t.Errorf("%s: underlying=%v want kind %v", f.goName, f.underlyingType, want.underlying)
		}
	}
}

// --- writeFields unwraps VO → underlying scalar ----------------------------

func TestWriteFields_UnwrapsValueObjects(t *testing.T) {
	s := voSchema()
	alt := voName("nick")
	e := &voEntity{Name: "Ada", Money: 42, Tier: tierSilver, Alt: &alt, Rank: nil}
	got := s.WriteFields(e)
	if got["name"] != "Ada" {
		t.Errorf("name = %#v want string \"Ada\"", got["name"])
	}
	if got["money"] != 42 {
		t.Errorf("money = %#v want int 42", got["money"])
	}
	if got["tier"] != 2 {
		t.Errorf("tier = %#v want int 2", got["tier"])
	}
	if got["alt"] != "nick" {
		t.Errorf("alt = %#v want string \"nick\"", got["alt"])
	}
	if got["rank"] != nil {
		t.Errorf("rank = %#v want nil (nil nullable VO → NULL)", got["rank"])
	}
}

// --- PayloadColumnTypes reports the underlying, not the VO type -------------

func TestPayloadColumnTypes_ReturnsUnderlying(t *testing.T) {
	types := voSchema().PayloadColumnTypes()
	if got := types["name"]; got == nil || got.Kind() != reflect.String {
		t.Errorf("name payload type = %v, want string", got)
	}
	if got := types["tier"]; got == nil || got.Kind() != reflect.Int {
		t.Errorf("tier payload type = %v, want int (enum underlying)", got)
	}
}

// --- scanTargetFor reconstructs VO fields (raw Convert, enum converge) ------

func TestScanTargetFor_ReconstructsValueObjects(t *testing.T) {
	e := &voEntity{}
	v := reflect.ValueOf(e).Elem()

	// raw string VO
	if err := scanTargetFor(v.FieldByName("Name")).(sql.Scanner).Scan("Grace"); err != nil {
		t.Fatalf("scan Name: %v", err)
	}
	if e.Name != voName("Grace") {
		t.Errorf("Name = %q want Grace", e.Name)
	}

	// raw int VO (driver hands int64)
	if err := scanTargetFor(v.FieldByName("Money")).(sql.Scanner).Scan(int64(7)); err != nil {
		t.Fatalf("scan Money: %v", err)
	}
	if e.Money != voMoney(7) {
		t.Errorf("Money = %d want 7", e.Money)
	}

	// enum VO valid member
	if err := scanTargetFor(v.FieldByName("Tier")).(sql.Scanner).Scan(int64(1)); err != nil {
		t.Fatalf("scan Tier: %v", err)
	}
	if e.Tier != tierGold {
		t.Errorf("Tier = %d want tierGold", e.Tier)
	}

	// enum VO OUT-OF-SET → Unknown (converge, D3)
	if err := scanTargetFor(v.FieldByName("Tier")).(sql.Scanner).Scan(int64(99)); err != nil {
		t.Fatalf("scan Tier 99: %v", err)
	}
	if e.Tier != tierUnknown {
		t.Errorf("Tier(99) = %d want tierUnknown (converge)", e.Tier)
	}

	// string VO delivered as []byte (driver text form)
	if err := scanTargetFor(v.FieldByName("Name")).(sql.Scanner).Scan([]byte("Ada")); err != nil {
		t.Fatalf("scan Name []byte: %v", err)
	}
	if e.Name != voName("Ada") {
		t.Errorf("Name([]byte) = %q want Ada", e.Name)
	}
}

func TestScanTargetFor_NullableVO(t *testing.T) {
	e := &voEntity{}
	v := reflect.ValueOf(e).Elem()

	// NULL → nil pointer
	if err := scanTargetFor(v.FieldByName("Alt")).(sql.Scanner).Scan(nil); err != nil {
		t.Fatalf("scan Alt NULL: %v", err)
	}
	if e.Alt != nil {
		t.Errorf("Alt = %v want nil", e.Alt)
	}

	// value → &VO
	if err := scanTargetFor(v.FieldByName("Alt")).(sql.Scanner).Scan("nick"); err != nil {
		t.Fatalf("scan Alt value: %v", err)
	}
	if e.Alt == nil || *e.Alt != voName("nick") {
		t.Errorf("Alt = %v want &nick", e.Alt)
	}

	// nullable enum, out-of-set → &Unknown
	if err := scanTargetFor(v.FieldByName("Rank")).(sql.Scanner).Scan(int64(99)); err != nil {
		t.Fatalf("scan Rank 99: %v", err)
	}
	if e.Rank == nil || *e.Rank != tierUnknown {
		t.Errorf("Rank(99) = %v want &tierUnknown", e.Rank)
	}
}

// --- driver delivering numeric columns as text (go-ora) --------------------

// go-ora hands EVERY NUMBER column as a string; an int-backed VO must still
// reconstruct. Without coerceScalar parsing the text, the enum membership walk
// sees a string it can never match and converges every value to Unknown.
func TestScanTargetFor_NumericColumnDeliveredAsText(t *testing.T) {
	e := &voEntity{}
	v := reflect.ValueOf(e).Elem()

	// raw int VO delivered as string
	if err := scanTargetFor(v.FieldByName("Money")).(sql.Scanner).Scan("7"); err != nil {
		t.Fatalf("scan Money text: %v", err)
	}
	if e.Money != voMoney(7) {
		t.Errorf("Money(text) = %d want 7", e.Money)
	}

	// enum int VO valid member delivered as []byte
	if err := scanTargetFor(v.FieldByName("Tier")).(sql.Scanner).Scan([]byte("1")); err != nil {
		t.Fatalf("scan Tier text: %v", err)
	}
	if e.Tier != tierGold {
		t.Errorf("Tier(text) = %d want tierGold", e.Tier)
	}

	// a NUMBER(n,0) rendered "1.0" still lands on the integer member
	if err := scanTargetFor(v.FieldByName("Tier")).(sql.Scanner).Scan("1.0"); err != nil {
		t.Fatalf("scan Tier 1.0: %v", err)
	}
	if e.Tier != tierGold {
		t.Errorf("Tier(1.0) = %d want tierGold", e.Tier)
	}

	// out-of-set text still converges to Unknown
	if err := scanTargetFor(v.FieldByName("Tier")).(sql.Scanner).Scan("99"); err != nil {
		t.Fatalf("scan Tier 99 text: %v", err)
	}
	if e.Tier != tierUnknown {
		t.Errorf("Tier(99 text) = %d want tierUnknown", e.Tier)
	}

	// nullable enum int VO delivered as string
	if err := scanTargetFor(v.FieldByName("Rank")).(sql.Scanner).Scan("1"); err != nil {
		t.Fatalf("scan Rank text: %v", err)
	}
	if e.Rank == nil || *e.Rank != tierGold {
		t.Errorf("Rank(text) = %v want &tierGold", e.Rank)
	}
}

// --- required VO field rejects SQL NULL loudly -----------------------------

func TestScanTargetFor_RequiredVONull(t *testing.T) {
	e := &voEntity{}
	v := reflect.ValueOf(e).Elem()
	if err := scanTargetFor(v.FieldByName("Name")).(sql.Scanner).Scan(nil); err == nil {
		t.Error("expected an error scanning NULL into a required VO field")
	}
}

// --- domain.ID is NOT treated as a persisted VO ----------------------------

func TestValueObjectField_ExcludesDomainID(t *testing.T) {
	if _, _, ok := valueObjectField(reflect.TypeOf(domain.ID{})); ok {
		t.Error("domain.ID must NOT be classified as a persisted value object")
	}
	if _, _, ok := valueObjectField(reflect.TypeOf((*domain.ID)(nil))); ok {
		t.Error("*domain.ID must NOT be classified as a persisted value object")
	}
}
