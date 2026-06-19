package responses

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ─── Test fixtures ──────────────────────────────────────────────────────────

type addrFixture struct {
	ID           string  `json:"id"`
	Label        *string `json:"label,omitempty"`
	Street       string  `json:"street"`
	Number       string  `json:"number"`
	Complement   *string `json:"complement,omitempty"`
	Neighborhood string  `json:"neighborhood"`
	City         string  `json:"city"`
	State        string  `json:"state"`
	ZipCode      string  `json:"zipCode" view:"zip_code"`
	Country      string  `json:"country"`
}

type userFixture struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Email     string        `json:"email"`
	Phone     *string       `json:"phone,omitempty"`
	Addresses []addrFixture `json:"addresses"`
}

// ─── End-to-end: every canonical field covered ──────────────────────────────

func TestAutoFromDoc_HappyPath_AllFieldsPopulated(t *testing.T) {
	phone := "14155552671"
	label := "home"
	complement := "Apt 12"
	doc := map[string]any{
		"ID":    "user-1",
		"Name":  "Jane",
		"Email": "jane@example.com",
		"Phone": phone,
		"Addresses": []any{
			map[string]any{
				"ID":           "addr-1",
				"Label":        label,
				"Street":       "1 Infinite Loop",
				"Number":       "1",
				"Complement":   complement,
				"Neighborhood": "Mariani",
				"City":         "Cupertino",
				"State":        "CA",
				"ZipCode":     "95014", // ← matches the composer's real output
				"Country":      "US",
			},
		},
	}
	got := AutoFromDoc[userFixture](doc)

	if got.ID != "user-1" {
		t.Errorf("ID: want user-1, got %q", got.ID)
	}
	if got.Name != "Jane" {
		t.Errorf("Name: want Jane, got %q", got.Name)
	}
	if got.Email != "jane@example.com" {
		t.Errorf("Email: want jane@example.com, got %q", got.Email)
	}
	if got.Phone == nil || *got.Phone != phone {
		t.Errorf("Phone: want %q, got %v", phone, got.Phone)
	}
	if len(got.Addresses) != 1 {
		t.Fatalf("Addresses: want 1, got %d", len(got.Addresses))
	}
	a := got.Addresses[0]
	if a.ID != "addr-1" {
		t.Errorf("Addr.ID: want addr-1, got %q", a.ID)
	}
	if a.Label == nil || *a.Label != label {
		t.Errorf("Addr.Label: want %q, got %v", label, a.Label)
	}
	if a.Street != "1 Infinite Loop" {
		t.Errorf("Addr.Street: want %q, got %q", "1 Infinite Loop", a.Street)
	}
	if a.Number != "1" {
		t.Errorf("Addr.Number: want %q, got %q", "1", a.Number)
	}
	if a.Complement == nil || *a.Complement != complement {
		t.Errorf("Addr.Complement: want %q, got %v", complement, a.Complement)
	}
	if a.Neighborhood != "Mariani" {
		t.Errorf("Addr.Neighborhood: want %q, got %q", "Mariani", a.Neighborhood)
	}
	if a.City != "Cupertino" {
		t.Errorf("Addr.City: want %q, got %q", "Cupertino", a.City)
	}
	if a.State != "CA" {
		t.Errorf("Addr.State: want %q, got %q", "CA", a.State)
	}
	if a.ZipCode != "95014" {
		t.Errorf("Addr.ZipCode (view:zip_code rename): want %q, got %q", "95014", a.ZipCode)
	}
	if a.Country != "US" {
		t.Errorf("Addr.Country: want %q, got %q", "US", a.Country)
	}
}

// ─── view: tag — source key override ────────────────────────────────────────

func TestAutoFromDoc_ViewTag_RenamesSourceKey(t *testing.T) {
	type R struct {
		Nickname string `json:"apelido" view:"nome"`
	}
	doc := map[string]any{"Nickname": "Janete"}
	got := AutoFromDoc[R](doc)
	if got.Nickname != "Janete" {
		t.Errorf("view: rename failed — want Janete, got %q", got.Nickname)
	}
}

func TestAutoFromDoc_ViewTag_AbsentFallsBackToJSONTag(t *testing.T) {
	type R struct {
		Name string `json:"name"`
	}
	doc := map[string]any{"Name": "Bob"}
	got := AutoFromDoc[R](doc)
	if got.Name != "Bob" {
		t.Errorf("json: fallback failed — want Bob, got %q", got.Name)
	}
}

func TestAutoFromDoc_NoJSONTag_FallsBackToGoFieldName(t *testing.T) {
	// When no json tag is declared, the wire name falls back to the Go
	// field name, then snake-cased to the doc key (framework convention).
	// Tested side-by-side: Pascal/camel Go names map to snake doc keys.
	type R struct {
		Name    string
		ZipCode string
	}
	doc := map[string]any{"Name": "Alice", "ZipCode": "12345"}
	got := AutoFromDoc[R](doc)
	if got.Name != "Alice" {
		t.Errorf("Go field name fallback (Name → name): want Alice, got %q", got.Name)
	}
	if got.ZipCode != "12345" {
		t.Errorf("Go field name fallback (ZipCode → zip_code): want 12345, got %q", got.ZipCode)
	}
}

func TestAutoFromDoc_JSONTagWithOmitempty_NameOnlyTakesFirstSegment(t *testing.T) {
	type R struct {
		Phone string `json:"phone,omitempty"`
	}
	doc := map[string]any{"Phone": "555"}
	got := AutoFromDoc[R](doc)
	if got.Phone != "555" {
		t.Errorf(",omitempty stripping failed — want 555, got %q", got.Phone)
	}
}

func TestAutoFromDoc_JSONTagSkipsField(t *testing.T) {
	type R struct {
		ID     string `json:"id"`
		Secret string `json:"-"`
	}
	doc := map[string]any{"ID": "x", "Secret": "leak", "secret": "leak"}
	got := AutoFromDoc[R](doc)
	if got.ID != "x" {
		t.Errorf("ID: want x, got %q", got.ID)
	}
	if got.Secret != "" {
		t.Errorf(`json:"-" not honored — want empty, got %q`, got.Secret)
	}
}

func TestAutoFromDoc_ViewTagDash_TreatedAsAbsent(t *testing.T) {
	// view:"-" is documented as equivalent to omitting the tag (no
	// "skip" semantic — json:"-" already handles that). Verify the field
	// falls back to its json wire name for lookup.
	type R struct {
		Name string `json:"name" view:"-"`
	}
	doc := map[string]any{"Name": "Carol"}
	got := AutoFromDoc[R](doc)
	if got.Name != "Carol" {
		t.Errorf(`view:"-" should fall back to json: tag — want Carol, got %q`, got.Name)
	}
}

// ─── _id fallback ───────────────────────────────────────────────────────────

func TestAutoFromDoc_IDFallback_UnderscoreIDWhenIDAbsent(t *testing.T) {
	type R struct {
		ID string `json:"id"`
	}
	got := AutoFromDoc[R](map[string]any{"_id": "u2"})
	if got.ID != "u2" {
		t.Errorf("_id fallback failed — want u2, got %q", got.ID)
	}
}

func TestAutoFromDoc_IDFallback_IDWinsWhenBothPresent(t *testing.T) {
	type R struct {
		ID string `json:"id"`
	}
	got := AutoFromDoc[R](map[string]any{"ID": "primary", "_id": "shadow"})
	if got.ID != "primary" {
		t.Errorf("id should win over _id — want primary, got %q", got.ID)
	}
}

func TestAutoFromDoc_IDFallback_IgnoresNonStringUnderscoreID(t *testing.T) {
	type R struct {
		ID string `json:"id"`
	}
	got := AutoFromDoc[R](map[string]any{"_id": 42})
	if got.ID != "" {
		t.Errorf("non-string _id must not populate ID — got %q", got.ID)
	}
}

func TestAutoFromDoc_IDFallback_TopLevelOnly(t *testing.T) {
	// Nested sub-docs never carry their own _id; verify the fallback
	// does not bleed into recursed structs.
	type Child struct {
		ID string `json:"id"`
	}
	type R struct {
		Child Child `json:"child"`
	}
	doc := map[string]any{
		"ID":    "root",
		"Child": map[string]any{"_id": "should-be-ignored"},
	}
	got := AutoFromDoc[R](doc)
	if got.Child.ID != "" {
		t.Errorf("_id fallback must not apply to nested struct — got %q", got.Child.ID)
	}
}

// ─── nil slice → empty typed slice ──────────────────────────────────────────

func TestAutoFromDoc_NilSlice_NormalizedToEmpty(t *testing.T) {
	type R struct {
		ID        string        `json:"id"`
		Addresses []addrFixture `json:"addresses"`
	}
	got := AutoFromDoc[R](map[string]any{"ID": "x"})
	if got.Addresses == nil {
		t.Fatal("nil slice should be normalized to empty")
	}
	if len(got.Addresses) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(got.Addresses))
	}
	// Marshal to verify wire shape: must be "[]" not "null".
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"addresses":[]`) {
		t.Errorf("wire shape: want addresses:[], got %s", raw)
	}
}

func TestAutoFromDoc_PrefilledSlice_NotOverwritten(t *testing.T) {
	type R struct {
		Addresses []addrFixture `json:"addresses"`
	}
	doc := map[string]any{
		"Addresses": []any{
			map[string]any{"ID": "a1", "Street": "S", "ZipCode": "00000"},
		},
	}
	got := AutoFromDoc[R](doc)
	if len(got.Addresses) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got.Addresses))
	}
	if got.Addresses[0].ZipCode != "00000" {
		t.Errorf("pre-filled slice content wrong: %+v", got.Addresses[0])
	}
}

func TestAutoFromDoc_NilScalarSlice_NormalizedToEmpty(t *testing.T) {
	type R struct {
		Tags []string `json:"tags"`
	}
	got := AutoFromDoc[R](map[string]any{})
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("scalar slice nil not normalized — got %v", got.Tags)
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"tags":[]`) {
		t.Errorf("wire shape: want tags:[], got %s", raw)
	}
}

func TestAutoFromDoc_NestedNilSlice_NormalizedInsideEachElement(t *testing.T) {
	type Inner struct {
		Tags []string `json:"tags"`
	}
	type R struct {
		Items []Inner `json:"items"`
	}
	doc := map[string]any{
		"Items": []any{
			map[string]any{}, // no "tags" key — should become [] inside
		},
	}
	got := AutoFromDoc[R](doc)
	if len(got.Items) != 1 {
		t.Fatalf("expected 1 item")
	}
	if got.Items[0].Tags == nil {
		t.Errorf("nested slice nil not normalized")
	}
}

// ─── Optional fields (pointer types) ────────────────────────────────────────

func TestAutoFromDoc_PointerField_AbsentStaysNil(t *testing.T) {
	type R struct {
		Phone *string `json:"phone,omitempty"`
	}
	got := AutoFromDoc[R](map[string]any{})
	if got.Phone != nil {
		t.Errorf("absent optional should be nil — got %v", got.Phone)
	}
}

func TestAutoFromDoc_PointerField_PopulatedWhenPresent(t *testing.T) {
	type R struct {
		Phone *string `json:"phone,omitempty"`
	}
	got := AutoFromDoc[R](map[string]any{"Phone": "555"})
	if got.Phone == nil || *got.Phone != "555" {
		t.Errorf("present optional should populate — got %v", got.Phone)
	}
}

// ─── Embedded struct promotion ──────────────────────────────────────────────

func TestAutoFromDoc_EmbeddedStruct_FieldsPromoted(t *testing.T) {
	type withTimestamps struct {
		CreatedAt string `json:"createdAt" view:"created_at"`
		UpdatedAt string `json:"updatedAt" view:"updated_at"`
	}
	type R struct {
		withTimestamps        // anonymous embed
		ID             string `json:"id"`
	}
	doc := map[string]any{
		"ID":         "u1",
		"CreatedAt": "2026-06-01",
		"UpdatedAt": "2026-06-02",
	}
	got := AutoFromDoc[R](doc)
	if got.ID != "u1" {
		t.Errorf("ID: want u1, got %q", got.ID)
	}
	if got.CreatedAt != "2026-06-01" {
		t.Errorf("CreatedAt promoted: want 2026-06-01, got %q", got.CreatedAt)
	}
	if got.UpdatedAt != "2026-06-02" {
		t.Errorf("UpdatedAt promoted: want 2026-06-02, got %q", got.UpdatedAt)
	}
}

// ─── Slice-of-struct: view: rename inside nested element ────────────────────

func TestAutoFromDoc_ViewTagInsideSliceElement_RenamesPerElement(t *testing.T) {
	type Item struct {
		Code string `json:"code" view:"codigo"`
	}
	type R struct {
		Items []Item `json:"items"`
	}
	doc := map[string]any{
		"Items": []any{
			map[string]any{"Code": "A"},
			map[string]any{"Code": "B"},
		},
	}
	got := AutoFromDoc[R](doc)
	if len(got.Items) != 2 || got.Items[0].Code != "A" || got.Items[1].Code != "B" {
		t.Errorf("nested view: rename failed — got %+v", got.Items)
	}
}

func TestAutoFromDoc_ViewTagOnSliceField_RenamesSourceKey(t *testing.T) {
	type Item struct {
		ID string `json:"id"`
	}
	type R struct {
		Items []Item `json:"items" view:"itens"`
	}
	doc := map[string]any{
		"Items": []any{map[string]any{"ID": "1"}},
	}
	got := AutoFromDoc[R](doc)
	if len(got.Items) != 1 || got.Items[0].ID != "1" {
		t.Errorf("slice-level view: rename failed — got %+v", got.Items)
	}
}

// ─── Unmapped doc keys ignored ──────────────────────────────────────────────

func TestAutoFromDoc_ExtraDocKeys_Ignored(t *testing.T) {
	type R struct {
		ID string `json:"id"`
	}
	doc := map[string]any{
		"ID":         "u1",
		"deleted_at": nil,
		"_internal":  "ignored",
	}
	got := AutoFromDoc[R](doc)
	if got.ID != "u1" {
		t.Errorf("ID: want u1, got %q", got.ID)
	}
}

// ─── Edge cases ─────────────────────────────────────────────────────────────

func TestAutoFromDoc_NilDoc_ReturnsZeroWithNormalizedSlices(t *testing.T) {
	type R struct {
		ID        string        `json:"id"`
		Addresses []addrFixture `json:"addresses"`
	}
	got := AutoFromDoc[R](nil)
	if got.ID != "" {
		t.Errorf("ID should be empty — got %q", got.ID)
	}
	if got.Addresses == nil {
		t.Error("Addresses slice still nil after nil doc")
	}
}

func TestAutoFromDoc_EmptyDoc_ReturnsZeroWithNormalizedSlices(t *testing.T) {
	type R struct {
		Addresses []addrFixture `json:"addresses"`
	}
	got := AutoFromDoc[R](map[string]any{})
	if got.Addresses == nil {
		t.Error("Addresses slice still nil after empty doc")
	}
}

// ─── Named map/slice types (bson.M / bson.A shape) ─────────────────────────

// Mongo driver's bson.M is `type M map[string]any` and bson.A is
// `type A []any` — distinct types under Go type assertions even though
// their underlying shape matches. Test that the helper reaches through
// the named type so it works against the real driver output.

type namedMap map[string]any
type namedSlice []any

func TestAutoFromDoc_HandlesNamedMapType(t *testing.T) {
	type R struct {
		Name string `json:"name"`
	}
	// caller's value is map[string]any (the framework boundary), but nested
	// fields may be named types (mongo-driver returns bson.M for nested docs
	// when DefaultDocumentM is enabled).
	type Outer struct {
		Inner R `json:"inner"`
	}
	doc := map[string]any{
		"Inner": namedMap{"Name": "Carol"},
	}
	got := AutoFromDoc[Outer](doc)
	if got.Inner.Name != "Carol" {
		t.Errorf("named map type not handled — want Carol, got %q", got.Inner.Name)
	}
}

func TestAutoFromDoc_HandlesNamedSliceType(t *testing.T) {
	type Inner struct {
		Code string `json:"code" view:"codigo"`
	}
	type Outer struct {
		Items []Inner `json:"items"`
	}
	doc := map[string]any{
		"Items": namedSlice{
			namedMap{"Code": "A"},
			namedMap{"Code": "B"},
		},
	}
	got := AutoFromDoc[Outer](doc)
	if len(got.Items) != 2 || got.Items[0].Code != "A" || got.Items[1].Code != "B" {
		t.Errorf("named slice/map types not handled — got %+v", got.Items)
	}
}

// ─── Plan cache ─────────────────────────────────────────────────────────────

func TestAutoFromDoc_PlanCache_SameTypeReturnsSameInstance(t *testing.T) {
	t1 := reflect.TypeOf(userFixture{})
	p1 := planFor(t1)
	p2 := planFor(t1)
	if p1 != p2 {
		t.Errorf("plan should be cached — got distinct instances")
	}
}

// ─── helper ────────────────────────────────────────────────────────────────

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
