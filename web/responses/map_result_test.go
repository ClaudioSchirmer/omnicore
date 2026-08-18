package responses

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ─── Test fixtures ──────────────────────────────────────────────────────────
//
// Results are application-pure: exported fields, NO json tags, names equal to
// the Response's Go field names. Responses carry the json wire tags.

type addrResult struct {
	ID           string
	Label        *string
	Street       string
	Number       string
	Complement   *string
	Neighborhood string
	City         string
	State        string
	ZipCode      string
	Country      string
}

type userResult struct {
	ID        string
	Name      string
	Email     string
	Phone     *string
	Addresses []addrResult
}

type addrFixture struct {
	ID           string  `json:"id"`
	Label        *string `json:"label,omitempty"`
	Street       string  `json:"street"`
	Number       string  `json:"number"`
	Complement   *string `json:"complement,omitempty"`
	Neighborhood string  `json:"neighborhood"`
	City         string  `json:"city"`
	State        string  `json:"state"`
	ZipCode      string  `json:"zipCode"`
	Country      string  `json:"country"`
}

type userFixture struct {
	Auto
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Email     string        `json:"email"`
	Phone     *string       `json:"phone,omitempty"`
	Addresses []addrFixture `json:"addresses"`
}

// ─── End-to-end: every canonical field covered ──────────────────────────────

func TestMap_HappyPath_AllFieldsPopulated(t *testing.T) {
	phone := "14155552671"
	label := "home"
	complement := "Apt 12"
	r := userResult{
		ID:    "user-1",
		Name:  "Jane",
		Email: "jane@example.com",
		Phone: &phone,
		Addresses: []addrResult{{
			ID:           "addr-1",
			Label:        &label,
			Street:       "1 Infinite Loop",
			Number:       "1",
			Complement:   &complement,
			Neighborhood: "Mariani",
			City:         "Cupertino",
			State:        "CA",
			ZipCode:      "95014",
			Country:      "US",
		}},
	}
	got := AutoFromResult[userFixture](r)

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
		t.Errorf("Addr.ZipCode: want %q, got %q", "95014", a.ZipCode)
	}
	if a.Country != "US" {
		t.Errorf("Addr.Country: want %q, got %q", "US", a.Country)
	}
}

// ─── json tag — wire renaming ───────────────────────────────────────────────

func TestMap_JSONTag_RenamesToWireName(t *testing.T) {
	type Result struct {
		Nickname string
	}
	type R struct {
		Auto
		Nickname string `json:"apelido"`
	}
	got := AutoFromResult[R](Result{Nickname: "Janete"})
	if got.Nickname != "Janete" {
		t.Errorf("json rename failed — want Janete, got %q", got.Nickname)
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"apelido":"Janete"`) {
		t.Errorf("wire shape: want apelido:Janete, got %s", raw)
	}
}

func TestMap_NoJSONTag_FallsBackToGoFieldName(t *testing.T) {
	type Result struct {
		Name    string
		ZipCode string
	}
	type R struct {
		Auto
		Name    string
		ZipCode string
	}
	got := AutoFromResult[R](Result{Name: "Alice", ZipCode: "12345"})
	if got.Name != "Alice" {
		t.Errorf("Go field name fallback (Name): want Alice, got %q", got.Name)
	}
	if got.ZipCode != "12345" {
		t.Errorf("Go field name fallback (ZipCode): want 12345, got %q", got.ZipCode)
	}
}

func TestMap_JSONTagWithOmitempty_NameOnlyTakesFirstSegment(t *testing.T) {
	type Result struct {
		Phone string
	}
	type R struct {
		Auto
		Phone string `json:"phone,omitempty"`
	}
	got := AutoFromResult[R](Result{Phone: "555"})
	if got.Phone != "555" {
		t.Errorf(",omitempty stripping failed — want 555, got %q", got.Phone)
	}
}

func TestMap_JSONTagSkipsField(t *testing.T) {
	type Result struct {
		ID     string
		Secret string
	}
	type R struct {
		Auto
		ID     string `json:"id"`
		Secret string `json:"-"`
	}
	got := AutoFromResult[R](Result{ID: "x", Secret: "leak"})
	if got.ID != "x" {
		t.Errorf("ID: want x, got %q", got.ID)
	}
	if got.Secret != "" {
		t.Errorf(`json:"-" not honored — want empty, got %q`, got.Secret)
	}
}

// ─── name-based mapping — undeclared Result fields dropped ──────────────────

func TestMap_ResultFieldsAbsentFromResponse_Dropped(t *testing.T) {
	// The Response is the single wire authority: a Result field the Response
	// does not declare never reaches the wire.
	type Result struct {
		ID        string
		DeletedAt *string
		Internal  string
	}
	type R struct {
		Auto
		ID string `json:"id"`
	}
	got := AutoFromResult[R](Result{ID: "u1", Internal: "ignored"})
	if got.ID != "u1" {
		t.Errorf("ID: want u1, got %q", got.ID)
	}
	raw, _ := json.Marshal(got)
	if contains(string(raw), "Internal") || contains(string(raw), "ignored") {
		t.Errorf("undeclared Result field leaked to the wire: %s", raw)
	}
}

// ─── nil slice → empty typed slice ──────────────────────────────────────────

func TestMap_NilSlice_NormalizedToEmpty(t *testing.T) {
	type Result struct {
		ID        string
		Addresses []addrResult
	}
	type R struct {
		Auto
		ID        string        `json:"id"`
		Addresses []addrFixture `json:"addresses"`
	}
	got := AutoFromResult[R](Result{ID: "x"})
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

func TestMap_PopulatedSlice_ContentPreserved(t *testing.T) {
	type Result struct {
		Addresses []addrResult
	}
	type R struct {
		Auto
		Addresses []addrFixture `json:"addresses"`
	}
	got := AutoFromResult[R](Result{Addresses: []addrResult{{ID: "a1", Street: "S", ZipCode: "00000"}}})
	if len(got.Addresses) != 1 {
		t.Fatalf("expected 1 item, got %d", len(got.Addresses))
	}
	if got.Addresses[0].ZipCode != "00000" {
		t.Errorf("populated slice content wrong: %+v", got.Addresses[0])
	}
}

func TestMap_NilScalarSlice_NormalizedToEmpty(t *testing.T) {
	type Result struct {
		Tags []string
	}
	type R struct {
		Auto
		Tags []string `json:"tags"`
	}
	got := AutoFromResult[R](Result{})
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("scalar slice nil not normalized — got %v", got.Tags)
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"tags":[]`) {
		t.Errorf("wire shape: want tags:[], got %s", raw)
	}
}

func TestMap_NestedNilSlice_NormalizedInsideEachElement(t *testing.T) {
	type InnerResult struct {
		Tags []string
	}
	type Result struct {
		Items []InnerResult
	}
	type Inner struct {
		Tags []string `json:"tags"`
	}
	type R struct {
		Auto
		Items []Inner `json:"items"`
	}
	got := AutoFromResult[R](Result{Items: []InnerResult{{}}}) // element with nil Tags
	if len(got.Items) != 1 {
		t.Fatalf("expected 1 item")
	}
	if got.Items[0].Tags == nil {
		t.Errorf("nested slice nil not normalized")
	}
}

// ─── Optional fields (pointer types) ────────────────────────────────────────

func TestMap_PointerField_NilStaysNil(t *testing.T) {
	type Result struct {
		Phone *string
	}
	type R struct {
		Auto
		Phone *string `json:"phone,omitempty"`
	}
	got := AutoFromResult[R](Result{})
	if got.Phone != nil {
		t.Errorf("nil optional should stay nil — got %v", got.Phone)
	}
	// Sparse wire shape: ,omitempty elides the absent field entirely.
	raw, _ := json.Marshal(got)
	if contains(string(raw), "phone") {
		t.Errorf("nil pointer must not render on the sparse wire shape: %s", raw)
	}
}

func TestMap_PointerField_PopulatedWhenPresent(t *testing.T) {
	phone := "555"
	type Result struct {
		Phone *string
	}
	type R struct {
		Auto
		Phone *string `json:"phone,omitempty"`
	}
	got := AutoFromResult[R](Result{Phone: &phone})
	if got.Phone == nil || *got.Phone != "555" {
		t.Errorf("present optional should populate — got %v", got.Phone)
	}
}

func TestMap_SparseResult_MixedPresence(t *testing.T) {
	// Sparse (?fields=) endpoints carry pointer fields on BOTH shapes: the
	// requested field populates, the projected-out ones stay nil.
	name := "Jane"
	type Result struct {
		ID    *string
		Name  *string
		Email *string
	}
	type R struct {
		Auto
		ID    *string `json:"id,omitempty"`
		Name  *string `json:"name,omitempty"`
		Email *string `json:"email,omitempty"`
	}
	got := AutoFromResult[R](Result{Name: &name})
	if got.Name == nil || *got.Name != "Jane" {
		t.Errorf("requested field must populate — got %v", got.Name)
	}
	if got.ID != nil || got.Email != nil {
		t.Errorf("projected-out fields must stay nil — got ID=%v Email=%v", got.ID, got.Email)
	}
}

// ─── 64-bit integer precision ───────────────────────────────────────────────

func TestMap_Int64Precision_SurvivesRoundTrip(t *testing.T) {
	// 9007199254740993 = 2^53+1 — corrupts to 9007199254740992 through a
	// float64 hop. The json.Number decode path must keep it exact.
	const big = int64(9007199254740993)
	type Result struct {
		Count int64
	}
	type R struct {
		Auto
		Count int64 `json:"count"`
	}
	got := AutoFromResult[R](Result{Count: big})
	if got.Count != big {
		t.Errorf("int64 precision lost: want %d, got %d", big, got.Count)
	}
}

func TestMap_Int64Precision_NestedAndUint64(t *testing.T) {
	const big = int64(9223372036854775807) // max int64 — hopeless through float64
	type InnerResult struct {
		N int64
	}
	type Result struct {
		Items []InnerResult
		U     uint64
	}
	type Inner struct {
		N int64 `json:"n"`
	}
	type R struct {
		Auto
		Items []Inner `json:"items"`
		U     uint64  `json:"u"`
	}
	got := AutoFromResult[R](Result{Items: []InnerResult{{N: big}}, U: 18446744073709551615})
	if len(got.Items) != 1 || got.Items[0].N != big {
		t.Errorf("nested int64 precision lost: got %+v", got.Items)
	}
	if got.U != 18446744073709551615 {
		t.Errorf("uint64 precision lost: got %d", got.U)
	}
}

// ─── Embedded struct promotion ──────────────────────────────────────────────

func TestMap_EmbeddedStruct_FieldsPromoted(t *testing.T) {
	type timestampsResult struct {
		CreatedAt string
		UpdatedAt string
	}
	type Result struct {
		timestampsResult
		ID string
	}
	type withTimestamps struct {
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
	}
	type R struct {
		Auto
		withTimestamps        // anonymous embed
		ID             string `json:"id"`
	}
	got := AutoFromResult[R](Result{
		timestampsResult: timestampsResult{CreatedAt: "2026-06-01", UpdatedAt: "2026-06-02"},
		ID:               "u1",
	})
	if got.ID != "u1" {
		t.Errorf("ID: want u1, got %q", got.ID)
	}
	if got.CreatedAt != "2026-06-01" {
		t.Errorf("CreatedAt promoted: want 2026-06-01, got %q", got.CreatedAt)
	}
	if got.UpdatedAt != "2026-06-02" {
		t.Errorf("UpdatedAt promoted: want 2026-06-02, got %q", got.UpdatedAt)
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"createdAt":"2026-06-01"`) {
		t.Errorf("promoted field must render under its json wire name: %s", raw)
	}
}

// ─── Slice-of-struct: renaming inside nested elements ───────────────────────

func TestMap_JSONTagInsideSliceElement_RenamesPerElement(t *testing.T) {
	type ItemResult struct {
		Code string
	}
	type Result struct {
		Items []ItemResult
	}
	type Item struct {
		Code string `json:"codigo"`
	}
	type R struct {
		Auto
		Items []Item `json:"items"`
	}
	got := AutoFromResult[R](Result{Items: []ItemResult{{Code: "A"}, {Code: "B"}}})
	if len(got.Items) != 2 || got.Items[0].Code != "A" || got.Items[1].Code != "B" {
		t.Errorf("nested rename failed — got %+v", got.Items)
	}
	raw, _ := json.Marshal(got)
	if !contains(string(raw), `"codigo":"A"`) {
		t.Errorf("nested element must render the json wire name: %s", raw)
	}
}

// ─── Edge cases ─────────────────────────────────────────────────────────────

func TestMap_NilResult_ReturnsZeroWithNormalizedSlices(t *testing.T) {
	type R struct {
		Auto
		ID        string        `json:"id"`
		Addresses []addrFixture `json:"addresses"`
	}
	// A nil Result is now a TYPED nil (Map infers the Result type from its
	// argument), which is the shape a handler returning *Result can produce.
	got := AutoFromResult[R]((*addrFixture)(nil))
	if got.ID != "" {
		t.Errorf("ID should be empty — got %q", got.ID)
	}
	if got.Addresses == nil {
		t.Error("Addresses slice still nil after nil result")
	}
}

func TestMap_ZeroResult_ReturnsZeroWithNormalizedSlices(t *testing.T) {
	type Result struct {
		Addresses []addrResult
	}
	type R struct {
		Auto
		Addresses []addrFixture `json:"addresses"`
	}
	got := AutoFromResult[R](Result{})
	if got.Addresses == nil {
		t.Error("Addresses slice still nil after zero result")
	}
}

// ─── Plan cache ─────────────────────────────────────────────────────────────

func TestMap_PlanCache_SameTypeReturnsSameInstance(t *testing.T) {
	t1 := reflect.TypeOf(userFixture{})
	p1 := planFor(t1)
	p2 := planFor(t1)
	if p1 != p2 {
		t.Errorf("plan should be cached — got distinct instances")
	}
}

// ─── helpers shared across the package's test files ─────────────────────────

// namedMap / namedSlice mirror bson.M / bson.A — distinct named types whose
// underlying shape matches; asMap/asSliceOfMaps must reach through them.
type namedMap map[string]any
type namedSlice []any

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
