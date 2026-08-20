package core

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- fixtures ---------------------------------------------------------------

// cvCurrency is an ENUM value object part (string-backed).
type cvCurrency string

const (
	cvBRL cvCurrency = "BRL"
	cvUSD cvCurrency = "USD"
)

func (c cvCurrency) Value() string                            { return string(c) }
func (c cvCurrency) Values() []cvCurrency                     { return []cvCurrency{cvBRL, cvUSD} }
func (c cvCurrency) UnknownNotification() domain.Notification { return cvUnknownNote{} }

type cvUnknownNote struct{ domain.DomainNotificationBase }

// cvCode is a SCALAR (raw) value object — the contrast case: it declares
// Value(), so it occupies exactly one column and is declared with Field.
type cvCode string

func (c cvCode) Value() string                                    { return string(c) }
func (c cvCode) IsValid(string, *domain.NotificationContext) bool { return c != "" }

// cvMoney is a MANDATORY composite: no Value(), owns its rule, one plain part
// and one enum-value-object part.
type cvMoney struct {
	Amount   int64      `labelKey:"MoneyAmountField"`
	Currency cvCurrency `labelKey:"MoneyCurrencyField"`
}

func (m cvMoney) IsValid(_ string, ctx *domain.NotificationContext) bool {
	if m.Amount < 0 {
		ctx.AddNotification("Amount", cvUnknownNote{})
		return false
	}
	return true
}

// cvPeriod is an OPTIONAL composite (held as *cvPeriod): one mandatory part and
// one nullable part, plus a CROSS-FIELD rule — the reason composites exist.
type cvPeriod struct {
	From time.Time  `labelKey:"PeriodFromField"`
	To   *time.Time `labelKey:"PeriodToField"`
}

func (p cvPeriod) IsValid(_ string, ctx *domain.NotificationContext) bool {
	if p.To != nil && p.To.Before(p.From) {
		ctx.AddNotification("To", cvUnknownNote{})
		return false
	}
	return true
}

type cvContract struct {
	ID     string
	Code   cvCode `labelKey:"ContractCodeField"`
	Salary cvMoney
	Trial  *cvPeriod
}

func cvSchema() *TableSchema {
	return NewTableSchema[cvContract]("contracts").
		ID("id").
		Field("Code", "code").
		Composite(NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").As("SalaryAmount").
			Field("Currency", "salary_currency").As("SalaryCurrency")).
		Composite(NewCompositeValueObject[cvPeriod]().
			Field("From", "trial_from").As("TrialFrom").
			Field("To", "trial_to").As("TrialTo")).
		CreatedAt("created_at")
}

// --- declaration ------------------------------------------------------------

func TestComposite_ResolvesPathsAndExposedNames(t *testing.T) {
	s := cvSchema()

	cols, byCol := s.ScanPlan()
	want := []string{"id", "code", "salary_amount", "salary_currency", "trial_from", "trial_to"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("ScanPlan cols = %v, want %v", cols, want)
	}
	// Contract{ID:0, Code:1, Salary:2, Trial:3} — a part is addressed INSIDE the
	// entity, which is the whole reason the plan carries a path.
	for col, wantPath := range map[string]FieldPath{
		"code":            {1},
		"salary_amount":   {2, 0},
		"salary_currency": {2, 1},
		"trial_from":      {3, 0},
		"trial_to":        {3, 1},
	} {
		if got := byCol[col]; !got.equal(wantPath) {
			t.Errorf("path of %q = %v, want %v", col, got, wantPath)
		}
	}

	// The exposed names are the aliases; the in-VO names are gone from byGo.
	for _, exposed := range []string{"SalaryAmount", "SalaryCurrency", "TrialFrom", "TrialTo"} {
		if _, ok := s.byGo[exposed]; !ok {
			t.Errorf("byGo is missing the exposed name %q", exposed)
		}
	}
	for _, inVO := range []string{"Amount", "Currency", "From", "To"} {
		if _, ok := s.byGo[inVO]; ok {
			t.Errorf("byGo still carries the un-aliased name %q", inVO)
		}
	}
	if col, ok := resolvedColumn(s, "SalaryAmount"); !ok || col != "salary_amount" {
		t.Errorf("Resolve(SalaryAmount) = %q,%v", col, ok)
	}
	if go_, ok := s.GoNameForRead("trial_to"); !ok || go_ != "TrialTo" {
		t.Errorf("GoNameForRead(trial_to) = %q,%v", go_, ok)
	}
}

func TestComposite_DefaultExposedNameIsThePartName(t *testing.T) {
	// No .As at all — the motivating case, where the part's own name reads right.
	s := NewTableSchema[cvContract]("contracts").
		ID("id").
		Composite(NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").
			Field("Currency", "salary_currency"))
	if _, ok := s.byGo["Amount"]; !ok {
		t.Error("the default exposed name must be the part's own name")
	}
}

func TestComposite_AsIsIdempotentOnTheSameName(t *testing.T) {
	s := NewTableSchema[cvContract]("contracts").
		ID("id").
		Composite(NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").As("Amount"))
	if _, ok := s.byGo["Amount"]; !ok {
		t.Error("As with the current name must be a no-op, not a rename")
	}
}

func TestComposite_PartTypingMirrorsField(t *testing.T) {
	s := cvSchema()
	// The enum part is unwrapped to its underlying, exactly as a root enum VO is.
	f := s.byGo["SalaryCurrency"]
	if !f.isVO || !f.isEnum || f.underlyingType.Kind() != reflect.String {
		t.Errorf("enum part typing = isVO:%v isEnum:%v underlying:%v", f.isVO, f.isEnum, f.underlyingType)
	}
	if plain := s.byGo["SalaryAmount"]; plain.isVO {
		t.Error("a plain part must not be marked as a value object")
	}
	// PayloadColumnTypes reports the UNDERLYING of a value-object part.
	types := s.PayloadColumnTypes()
	if got := types["salary_currency"]; got == nil || got.Kind() != reflect.String {
		t.Errorf("payload type of salary_currency = %v, want string", got)
	}
	if got := types["salary_amount"]; got != reflect.TypeOf(int64(0)) {
		t.Errorf("payload type of salary_amount = %v, want int64", got)
	}
	if got := types["trial_from"]; got != reflect.TypeOf(time.Time{}) {
		t.Errorf("payload type of trial_from = %v, want time.Time", got)
	}
}

func TestComposite_LabelComesFromTheValueObjectNotTheAlias(t *testing.T) {
	labels := cvSchema().LabelKeysByGoField()
	if got := labels["SalaryAmount"]; got != "MoneyAmountField" {
		t.Errorf("label of SalaryAmount = %q, want MoneyAmountField (the tag inside the value object)", got)
	}
	if got := labels["TrialTo"]; got != "PeriodToField" {
		t.Errorf("label of TrialTo = %q, want PeriodToField", got)
	}
	if got := labels["Code"]; got != "ContractCodeField" {
		t.Errorf("label of Code = %q, want ContractCodeField", got)
	}
}

// --- write side -------------------------------------------------------------

func TestComposite_WriteFieldsDecomposes(t *testing.T) {
	end := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	c := cvContract{
		Code:   "CT-9911",
		Salary: cvMoney{Amount: 850000, Currency: cvBRL},
		Trial:  &cvPeriod{From: time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC), To: &end},
	}
	got := cvSchema().WriteFields(c)
	if got["salary_amount"] != int64(850000) {
		t.Errorf("salary_amount = %v, want 850000", got["salary_amount"])
	}
	// The enum part binds as its UNDERLYING string, never the named type.
	if got["salary_currency"] != "BRL" {
		t.Errorf("salary_currency = %#v, want \"BRL\"", got["salary_currency"])
	}
	// A nullable part binds as its pointer, exactly like any nullable field.
	if p, ok := got["trial_to"].(*time.Time); !ok || !p.Equal(end) {
		t.Errorf("trial_to = %#v, want *time.Time(%v)", got["trial_to"], end)
	}
}

func TestComposite_AbsentOptionalWritesNullToEveryPart(t *testing.T) {
	got := cvSchema().WriteFields(cvContract{Code: "CT-1", Salary: cvMoney{Amount: 1, Currency: cvUSD}})
	for _, col := range []string{"trial_from", "trial_to"} {
		v, present := got[col]
		if !present {
			t.Fatalf("%s must be written (as NULL), not omitted", col)
		}
		if v != nil {
			t.Errorf("%s = %v, want nil — an absent composite is NULL in every one of its columns", col, v)
		}
	}
}

func TestComposite_GoFieldValuesKeysByExposedName(t *testing.T) {
	c := cvContract{Code: "CT-9911", Salary: cvMoney{Amount: 850000, Currency: cvBRL}}
	got := cvSchema().GoFieldValues(c)
	if got["SalaryAmount"] != int64(850000) || got["SalaryCurrency"] != "BRL" {
		t.Errorf("audit values = %v, want SalaryAmount/SalaryCurrency entries", got)
	}
	if v, ok := got["TrialFrom"]; !ok || v != nil {
		t.Errorf("TrialFrom = %v,%v — an absent composite records as nil", v, ok)
	}
	if _, ok := got["Salary"]; ok {
		t.Error("the composite itself must not appear in the audit timeline — only its parts do")
	}
}

// --- scan side --------------------------------------------------------------

// cvRow is a driver-faithful fake: it honors sql.Scanner and resolves pointer
// depth the way database/sql's convertAssign and pgx's pointer-to-pointer scan
// plan both do, which is what the optional-composite targets rely on.
type cvRow struct{ values []any }

func (r *cvRow) Scan(dest ...any) error {
	for i, d := range dest {
		var src any
		if i < len(r.values) {
			src = r.values[i]
		}
		if sc, ok := d.(sql.Scanner); ok {
			if err := sc.Scan(src); err != nil {
				return err
			}
			continue
		}
		if err := cvAssign(d, src); err != nil {
			return err
		}
	}
	return nil
}

func cvAssign(dst, src any) error {
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer, got %T", dst)
	}
	target := dv.Elem()
	if src == nil {
		if target.Kind() == reflect.Pointer {
			target.Set(reflect.Zero(target.Type()))
			return nil
		}
		return fmt.Errorf("converting NULL to %s is unsupported", target.Type())
	}
	if target.Kind() == reflect.Pointer {
		target.Set(reflect.New(target.Type().Elem()))
		return cvAssign(target.Interface(), src)
	}
	sv := reflect.ValueOf(src)
	if sv.Type().AssignableTo(target.Type()) {
		target.Set(sv)
		return nil
	}
	if sv.Type().ConvertibleTo(target.Type()) {
		target.Set(sv.Convert(target.Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %T to %s", src, target.Type())
}

func cvScan(t *testing.T, values ...any) (cvContract, error) {
	t.Helper()
	s := cvSchema()
	cols, byCol := s.ScanPlan()
	var dst cvContract
	err := scanRowIntoStruct(&cvRow{values: values}, &dst, cols, byCol)
	return dst, err
}

func TestComposite_ScanReconstructsBothComposites(t *testing.T) {
	from := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	got, err := cvScan(t, "id-1", "CT-9911", int64(920000), "BRL", from, to)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Salary.Amount != 920000 || got.Salary.Currency != cvBRL {
		t.Errorf("Salary = %+v, want {920000 BRL}", got.Salary)
	}
	if got.Trial == nil || !got.Trial.From.Equal(from) || got.Trial.To == nil || !got.Trial.To.Equal(to) {
		t.Fatalf("Trial = %+v, want both ends set", got.Trial)
	}
}

func TestComposite_ScanAbsentOptionalIsNil(t *testing.T) {
	// Every part column NULL ⇒ the value object was never there.
	got, err := cvScan(t, "id-1", "CT-9911", int64(1), "BRL", nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Trial != nil {
		t.Errorf("Trial = %+v, want nil when every part column is NULL", got.Trial)
	}
}

func TestComposite_ScanPresentOptionalWithNullablePart(t *testing.T) {
	from := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	got, err := cvScan(t, "id-1", "CT-9911", int64(1), "BRL", from, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Trial == nil || !got.Trial.From.Equal(from) {
		t.Fatalf("Trial = %+v, want present", got.Trial)
	}
	if got.Trial.To != nil {
		t.Errorf("Trial.To = %v, want nil — a nullable part stays absent", got.Trial.To)
	}
}

func TestComposite_ScanHalfWrittenOptionalIsALoudError(t *testing.T) {
	// One part carries a value, the mandatory one is NULL: the row is corrupt,
	// and reconstructing a zero From would be a silent lie.
	to := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	_, err := cvScan(t, "id-1", "CT-9911", int64(1), "BRL", nil, to)
	if err == nil {
		t.Fatal("a half-written composite must be a loud error")
	}
	if !strings.Contains(err.Error(), "half-written") {
		t.Errorf("error = %v, want it to name the half-written composite", err)
	}
}

func TestComposite_ScanEnumPartConvergesToUnknown(t *testing.T) {
	// A value outside the declared set reconstructs as the Unknown sentinel, the
	// same convergence a root enum value object gets.
	got, err := cvScan(t, "id-1", "CT-9911", int64(1), "XYZ", nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Salary.Currency != cvCurrency("") {
		t.Errorf("Currency = %q, want the Unknown sentinel", got.Salary.Currency)
	}
}

func TestComposite_ScanMandatoryCompositeNullPartErrors(t *testing.T) {
	// A composite held BY VALUE has no aggregate rule: each part follows its own
	// Go type, so a NULL into the non-pointer Amount is the driver's loud error.
	s := NewTableSchema[cvContract]("contracts").
		ID("id").
		Composite(NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").
			Field("Currency", "salary_currency"))
	cols, byCol := s.ScanPlan()
	var dst cvContract
	err := scanRowIntoStruct(&cvRow{values: []any{"id-1", nil, "BRL"}}, &dst, cols, byCol)
	if err == nil {
		t.Fatal("NULL into a non-nullable part of a mandatory composite must error")
	}
}

// --- boot guards ------------------------------------------------------------
//
// Several misuses that used to be boot panics are now COMPILE errors and have
// no test: Field/As of a part exist only on *CompositeValueObject, so calling
// them on a schema does not build, and a part cannot escape the declaration it
// belongs to. What remains here is everything the compiler cannot know.

func TestComposite_GuardFieldOnACompositeTeachesTheFix(t *testing.T) {
	mustPanicWith(t, "Composite(core.NewCompositeValueObject", func() {
		NewTableSchema[cvContract]("contracts").ID("id").Field("Salary", "salary")
	})
}

func TestComposite_GuardScalarAndEnumValueObjects(t *testing.T) {
	// The kind is verified by the CONSTRUCTOR, at the line that names the type.
	mustPanicWith(t, "SCALAR value object", func() { NewCompositeValueObject[cvCode]() })
	mustPanicWith(t, "ENUM value object", func() { NewCompositeValueObject[cvCurrency]() })
}

func TestComposite_GuardNotAValueObject(t *testing.T) {
	mustPanicWith(t, "not a value object", func() { NewCompositeValueObject[cvPlain]() })
}

type cvPlain struct{ A string }

func TestComposite_GuardPointerReceiverIsValidIsHinted(t *testing.T) {
	mustPanicWith(t, "value receiver", func() { NewCompositeValueObject[cvPtrRule]() })
}

// cvPtrRule declares IsValid on the POINTER, which the framework's value-based
// discovery never sees.
type cvPtrRule struct{ A string }

func (p *cvPtrRule) IsValid(string, *domain.NotificationContext) bool { return true }

func TestComposite_GuardNonStruct(t *testing.T) {
	mustPanicWith(t, "is a STRUCT", func() { NewCompositeValueObject[cvNotStruct]() })
}

// cvNotStruct owns a rule but is not a struct, so it has no parts to decompose.
type cvNotStruct string

func (c cvNotStruct) IsValid(string, *domain.NotificationContext) bool { return true }

func TestComposite_GuardNilDeclaration(t *testing.T) {
	mustPanicWith(t, "Composite(nil)", func() {
		NewTableSchema[cvContract]("contracts").ID("id").Composite(nil)
	})
}

func TestComposite_GuardEntityHasNoFieldOfThatType(t *testing.T) {
	mustPanicWith(t, "no exported field typed", func() {
		NewTableSchema[cvContract]("contracts").ID("id").
			Composite(NewCompositeValueObject[cvUnused]().Field("A", "a"))
	})
}

type cvUnused struct{ A string }

func (c cvUnused) IsValid(string, *domain.NotificationContext) bool { return true }

func TestComposite_GuardTwoFieldsOfTheSameCompositeType(t *testing.T) {
	mustPanicWith(t, "more than one field", func() {
		NewTableSchema[cvTwoMoney]("t").ID("id").
			Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount"))
	})
}

type cvTwoMoney struct {
	ID       string
	Billing  cvMoney
	Shipping cvMoney
}

func TestComposite_GuardAsWithoutAField(t *testing.T) {
	mustPanicWith(t, "does not follow a Field", func() {
		NewCompositeValueObject[cvMoney]().As("Nope")
	})
	mustPanicWith(t, "As(\"\")", func() {
		NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount").As("")
	})
}

func TestComposite_GuardEmptyDecomposition(t *testing.T) {
	mustPanicWith(t, "declares no Field", func() {
		NewTableSchema[cvContract]("contracts").ID("id").
			Composite(NewCompositeValueObject[cvMoney]())
	})
}

func TestComposite_GuardUnknownField(t *testing.T) {
	mustPanicWith(t, "not an exported single-depth field of the value object", func() {
		NewCompositeValueObject[cvMoney]().Field("Nope", "nope")
	})
}

func TestComposite_GuardDuplicateWithinTheValueObject(t *testing.T) {
	mustPanicWith(t, "declared twice", func() {
		NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").
			Field("Amount", "again")
	})
	mustPanicWith(t, "bijection", func() {
		NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").
			Field("Currency", "salary_amount")
	})
}

func TestComposite_GuardOnceRuleSameSchema(t *testing.T) {
	mustPanicWith(t, "decomposed twice on this schema", func() {
		NewTableSchema[cvContract]("contracts").
			ID("id").
			Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount")).
			Composite(NewCompositeValueObject[cvMoney]().Field("Currency", "salary_currency"))
	})
}

func TestComposite_GuardExternalSchema(t *testing.T) {
	mustPanicWith(t, "on an external schema", func() {
		NewExternalSchema("upstream").
			Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount"))
	})
}

func TestComposite_GuardAliasCollision(t *testing.T) {
	mustPanicWith(t, "declared twice", func() {
		NewTableSchema[cvContract]("contracts").
			ID("id").
			Field("Code", "code").
			Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount").As("Code"))
	})
}

// --- the once rule across schemas (boot checkpoint) -------------------------

func TestComposite_OnceRuleAcrossRootAndSibling(t *testing.T) {
	s := NewTableSchema[cvContract]("contracts").
		ID("id").
		Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount")).
		Sibling(NewSiblingSchema[cvContract]("contract_extra").
			Composite(NewCompositeValueObject[cvMoney]().Field("Currency", "salary_currency")))
	mustPanicWith(t, "is decomposed on BOTH", func() { s.ValidateOldCloneSafety() })
}

func TestComposite_OnceRuleIsOrderIndependent(t *testing.T) {
	// The sibling is attached BEFORE the root declares its composite; the guard
	// lives at the checkpoint precisely so this still fails.
	s := NewTableSchema[cvContract]("contracts").
		ID("id").
		Sibling(NewSiblingSchema[cvContract]("contract_extra").
			Composite(NewCompositeValueObject[cvMoney]().Field("Currency", "salary_currency"))).
		Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "salary_amount"))
	mustPanicWith(t, "is decomposed on BOTH", func() { s.ValidateOldCloneSafety() })
}

func TestComposite_InOneSchemaPassesTheCheckpoint(t *testing.T) {
	cvSchema().ValidateOldCloneSafety()
}

func TestComposite_GuardCustomJSONMarshaler(t *testing.T) {
	s := NewTableSchema[cvMarshalCarrier]("t").
		ID("id").
		Composite(NewCompositeValueObject[cvMarshaling]().Field("A", "a"))
	mustPanicWith(t, "json.Marshaler", func() { s.ValidateOldCloneSafety() })
}

type cvMarshaling struct{ A string }

func (c cvMarshaling) IsValid(string, *domain.NotificationContext) bool { return true }
func (c cvMarshaling) MarshalJSON() ([]byte, error)                     { return []byte(`""`), nil }

type cvMarshalCarrier struct {
	ID string
	M  cvMarshaling
}

func TestComposite_GuardPartTaggedJSONDash(t *testing.T) {
	s := NewTableSchema[cvDashCarrier]("t").
		ID("id").
		Composite(NewCompositeValueObject[cvDashed]().Field("A", "a"))
	mustPanicWith(t, "json:\"-\"", func() { s.ValidateOldCloneSafety() })
}

type cvDashed struct {
	A string `json:"-"`
}

func (c cvDashed) IsValid(string, *domain.NotificationContext) bool { return true }

type cvDashCarrier struct {
	ID string
	D  cvDashed
}

// --- unwrapVO -----------------------------------------------------------------

func TestUnwrapVO_CompositePassesThroughInsteadOfBecomingNull(t *testing.T) {
	m := cvMoney{Amount: 5, Currency: cvBRL}
	if got := UnwrapVO(m); got == nil {
		t.Fatal("a composite must never unwrap to nil — that would bind a silent NULL for a value that is there")
	}
	var absent *cvPeriod
	if got := UnwrapVO(absent); got != nil {
		t.Errorf("a nil optional composite = %v, want nil (SQL NULL)", got)
	}
}

// --- shared base ------------------------------------------------------------

// A shared base is TYPE-LESS: its columns are resolved against each role's Go
// type at .SharedBase(...) time. A composite's part cannot be resolved by its
// exposed name (that name lives on the value object), so it resolves by
// PROVENANCE — the value object's type, then the field inside it.

type cvRole struct {
	ID     string
	Salary cvMoney
	Code   cvCode
}

func cvBase() *TableSchema {
	return NewSharedBaseSchema("people").
		ID("id").
		NaturalID("code").
		Revision("revision").
		Field("Code", "code").
		Composite(NewCompositeValueObject[cvMoney]().
			Field("Amount", "salary_amount").As("SalaryAmount").
			Field("Currency", "salary_currency").As("SalaryCurrency"))
}

func TestComposite_SharedBaseResolvesPartsAgainstTheRole(t *testing.T) {
	role := NewTableSchema[cvRole]("role_rows").
		ID("id").
		Revision("revision").
		SharedBase(cvBase(), "person_id")

	cols, byCol, ok := role.SharedBaseScanPlan()
	if !ok {
		t.Fatal("the role must carry a shared-base scan plan")
	}
	if len(cols) != 3 {
		t.Fatalf("base scan cols = %v, want code + the two salary parts", cols)
	}
	// cvRole{ID:0, Salary:1, Code:2} — the parts address INSIDE Salary.
	if got := byCol["salary_amount"]; !got.equal(FieldPath{1, 0}) {
		t.Errorf("path of salary_amount = %v, want {1 0}", got)
	}
	if got := byCol["salary_currency"]; !got.equal(FieldPath{1, 1}) {
		t.Errorf("path of salary_currency = %v, want {1 1}", got)
	}
	if got := byCol["code"]; !got.equal(FieldPath{2}) {
		t.Errorf("path of code = %v, want {2}", got)
	}
}

func TestComposite_SharedBaseRoleMustCarryTheComposite(t *testing.T) {
	type roleWithoutMoney struct {
		ID   string
		Code cvCode
	}
	mustPanicWith(t, "has no exported field of that type", func() {
		NewTableSchema[roleWithoutMoney]("role_rows").
			ID("id").
			Revision("revision").
			SharedBase(cvBase(), "person_id")
	})
}

func TestComposite_OnceRuleAcrossRoleAndBase(t *testing.T) {
	role := NewTableSchema[cvRole]("role_rows").
		ID("id").
		Revision("revision").
		SharedBase(cvBase(), "person_id").
		Composite(NewCompositeValueObject[cvMoney]().Field("Amount", "role_amount"))
	mustPanicWith(t, "is decomposed on BOTH", func() { role.ValidateOldCloneSafety() })
}

func TestComposite_SharedBaseEquivalenceComparesTheValueObject(t *testing.T) {
	other := NewSharedBaseSchema("people").
		ID("id").
		NaturalID("code").
		Revision("revision").
		Field("Code", "code").
		Field("SalaryAmount", "salary_amount").
		Field("SalaryCurrency", "salary_currency")
	mustPanicWith(t, "the value object behind field", func() {
		AssertSharedBaseEquivalent(cvBase(), other)
	})
}

// --- an optional composite whose parts are themselves value objects ---------

type cvContact struct {
	Email cvCode  `labelKey:"ContactEmailField"`
	Phone *cvCode `labelKey:"ContactPhoneField"`
}

func (c cvContact) IsValid(string, *domain.NotificationContext) bool { return true }

type cvPerson struct {
	ID      string
	Contact *cvContact
}

func cvPersonSchema() *TableSchema {
	return NewTableSchema[cvPerson]("people").
		ID("id").
		Composite(NewCompositeValueObject[cvContact]().
			Field("Email", "contact_email").
			Field("Phone", "contact_phone"))
}

func cvScanPerson(t *testing.T, values ...any) (cvPerson, error) {
	t.Helper()
	cols, byCol := cvPersonSchema().ScanPlan()
	var dst cvPerson
	err := scanRowIntoStruct(&cvRow{values: values}, &dst, cols, byCol)
	return dst, err
}

func TestComposite_OptionalCompositeWithValueObjectParts(t *testing.T) {
	got, err := cvScanPerson(t, "id-1", "a@b.c", "+55")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Contact == nil || got.Contact.Email != cvCode("a@b.c") {
		t.Fatalf("Contact = %+v, want the reconstructed value objects", got.Contact)
	}
	if got.Contact.Phone == nil || *got.Contact.Phone != cvCode("+55") {
		t.Errorf("Contact.Phone = %v, want a reconstructed nullable value object", got.Contact.Phone)
	}
}

func TestComposite_OptionalCompositeWithValueObjectPartsAbsent(t *testing.T) {
	got, err := cvScanPerson(t, "id-1", nil, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Contact != nil {
		t.Errorf("Contact = %+v, want nil when every part column is NULL", got.Contact)
	}
}

func TestComposite_OptionalCompositeVOPartHalfWritten(t *testing.T) {
	// The mandatory value-object part is NULL while the nullable one carries a
	// value: present, and half-written.
	_, err := cvScanPerson(t, "id-1", nil, "+55")
	if err == nil {
		t.Fatal("a half-written composite with value-object parts must be a loud error")
	}
	if !strings.Contains(err.Error(), "half-written") {
		t.Errorf("error = %v, want it to name the half-written composite", err)
	}
}

// --- FieldPath ---------------------------------------------------------------

func TestFieldPath_Primitives(t *testing.T) {
	if (FieldPath{1}).equal(FieldPath{1, 0}) {
		t.Error("paths of different lengths are never equal")
	}
	if (FieldPath{1, 2}).equal(FieldPath{1, 3}) {
		t.Error("paths differing at a hop are never equal")
	}
	if got := (FieldPath{2, 0}).prefix(); !got.equal(FieldPath{2}) {
		t.Errorf("prefix of a part = %v, want the composite's own path", got)
	}
	if got := (FieldPath{2}).prefix(); got != nil {
		t.Errorf("prefix of a root field = %v, want nil", got)
	}
	if (FieldPath{}).resolved() {
		t.Error("an empty path addresses nothing")
	}
}

func TestFieldPath_ValueInStopsAtANilPointer(t *testing.T) {
	c := cvContract{Code: "x"} // Trial is nil
	v := reflect.ValueOf(&c).Elem()
	if _, ok := (FieldPath{3, 0}).ValueIn(v); ok {
		t.Error("reading through a nil optional composite must report absence, not a zero value")
	}
	if _, ok := (FieldPath{99}).ValueIn(v); ok {
		t.Error("an out-of-range hop must not resolve")
	}
	if _, ok := (FieldPath{}).ValueIn(v); ok {
		t.Error("an empty path resolves to nothing")
	}
}

func TestFieldPath_TargetInAllocatesAndGuards(t *testing.T) {
	c := cvContract{}
	v := reflect.ValueOf(&c).Elem()
	got := (FieldPath{3, 0}).TargetIn(v)
	if !got.IsValid() || c.Trial == nil {
		t.Fatal("TargetIn must materialize the optional composite so its part is addressable")
	}
	if (FieldPath{99}).TargetIn(v).IsValid() {
		t.Error("an out-of-range hop must not resolve")
	}
	if (FieldPath{}).TargetIn(v).IsValid() {
		t.Error("an empty path resolves to nothing")
	}
	// A non-addressable root cannot be allocated into.
	if (FieldPath{3, 0}).TargetIn(reflect.ValueOf(cvContract{})).IsValid() {
		t.Error("TargetIn must refuse a non-settable root rather than panic")
	}
}

func TestFieldPath_StructFieldIn(t *testing.T) {
	ct := reflect.TypeOf(cvContract{})
	sf, ok := (FieldPath{2, 1}).StructFieldIn(ct)
	if !ok || sf.Name != "Currency" {
		t.Errorf("StructFieldIn = %v,%v, want the Currency part", sf.Name, ok)
	}
	if _, ok := (FieldPath{99}).StructFieldIn(ct); ok {
		t.Error("an out-of-range hop must not resolve")
	}
	if _, ok := (FieldPath{0}).StructFieldIn(nil); ok {
		t.Error("a nil type resolves to nothing")
	}
	if _, ok := (FieldPath{1, 0}).StructFieldIn(ct); ok {
		t.Error("descending into a non-struct must not resolve")
	}
	if _, ok := (FieldPath{2}).TypeIn(nil); ok {
		t.Error("TypeIn on a nil type resolves to nothing")
	}
}
