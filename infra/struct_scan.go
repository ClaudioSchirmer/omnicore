package infra

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Auto-scan: the AggregateLoader discovers entity columns via reflection and
// generates an explicit SELECT (covering only the exported domain fields) +
// row.Scan directly on the addresses of those fields. Replaces manual
// scanners with no performance cost — the column→index mapping is cached
// per reflect.Type.
//
// Name mapping (field name → column):
//   - tag `transient:"-"` → skips the field (does not persist in any adapter)
//   - no tag → snake_case of the field name (pascalToSnake)
//
// The `transient` tag is the domain's own declaration that a field is NOT
// part of the persisted state — same intent as Java's `transient` keyword:
// "skip on serialization, regardless of why". The four cases it covers:
//   1. Request-scoped inputs read off the AppContext/Identity and pushed
//      onto the entity by the Command mapper to feed BuildRules
//      (e.g., RequestingPrincipalEmail, RequestingPrincipalIsAdmin).
//   2. Computed values derived from other fields at runtime — cached on
//      the struct for invariant reuse without touching the DB.
//   3. In-memory caches the aggregate maintains while it is alive in a
//      request (last lookup result, count of mutations attempted, etc.).
//   4. Runtime bookkeeping flags (saw-this-once, attempt counter, ad-hoc
//      diagnostic markers) that exist only for the lifetime of the call.
//
// Domain expresses the property; infra observes it. There is no inverse —
// infra never tells the domain "skip this for me". The dependency direction
// stays domain → infra, never the other way around.
//
// Column name override (legacy schema, non-Go naming) does NOT live here — it
// is a Repository decision via RepoConfig.FieldOverrides. Reason: DDD-pure
// domain does not pronounce column names; override is per-service/per-schema,
// not per-type.
//
// pgx normalizes names by removing "_" and comparing case-insensitively
// (pgx/v5/rows.go fieldPosByName), so the snake_case convention matches
// transparently when the SQL column follows the same form.
//
// What is IGNORED by reflection (intentional):
//   - Anonymous fields (embeds: AggregateRoot, BaseEntity, etc.) — do not
//     contribute their own columns; managed cols live outside the struct.
//   - Private fields (PkgPath != "") — there is no way to populate them anyway.
//   - Field named exactly "ID" — DB-gen + separate WHERE clause.

// domainColumns returns the ordered list of columns that make up the
// auto-generated SELECT for t. Order follows the field declaration in the struct.
func domainColumns(t reflect.Type) []string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	idx := loadStructIndex(t)
	cols := make([]string, len(idx.order))
	for i, fi := range idx.order {
		cols[i] = fi.col
	}
	return cols
}

// pascalToSnake converts a PascalCase or camelCase name to snake_case.
// Handles leading and trailing acronyms: "CPF" → "cpf", "ZipCode" → "zip_code",
// "PostalCodeV2" → "postal_code_v2", "HTTPStatus" → "http_status".
func pascalToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && isUpper(r) {
			prev := runes[i-1]
			// Insert _ if there is a word transition: either the previous is
			// lowercase, or we are at the end of an acronym (previous upper,
			// next lower).
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			if isLower(prev) || (isUpper(prev) && isLower(next)) {
				b.WriteByte('_')
			}
		}
		if isUpper(r) {
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return r >= 'a' && r <= 'z' }

// structIndex is the result of inspecting a struct type: ordered list of
// fields that map to columns + a col→field index for O(1) lookup at scan
// time. Cached per reflect.Type in structIndexCache.
type structIndex struct {
	order []fieldInfo
	byCol map[string]int // col → position in order
}

type fieldInfo struct {
	col        string
	fieldIndex int // index in reflect.Type.NumField
}

var structIndexCache sync.Map // reflect.Type → *structIndex

func loadStructIndex(t reflect.Type) *structIndex {
	if cached, ok := structIndexCache.Load(t); ok {
		return cached.(*structIndex)
	}
	idx := buildStructIndex(t)
	cached, _ := structIndexCache.LoadOrStore(t, idx)
	return cached.(*structIndex)
}

// buildStructIndex returns ALL persistable fields (not anonymous, not
// unexported, without tag `transient:"-"`) — INCLUDES "id". Read side
// (AggregateLoader) needs "id" to populate the struct. Write side
// (InferColumns in infer.go) explicitly filters "id" because it is DB-gen +
// separate WHERE clause.
func buildStructIndex(t reflect.Type) *structIndex {
	si := &structIndex{
		byCol: map[string]int{},
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous || f.PkgPath != "" {
			continue
		}
		if tag, ok := f.Tag.Lookup("transient"); ok && tag == "-" {
			continue
		}
		col := pascalToSnake(f.Name)
		si.byCol[col] = len(si.order)
		si.order = append(si.order, fieldInfo{col: col, fieldIndex: i})
	}
	return si
}

// scanRowIntoStruct fills dst (must be pointer to struct) with the values of
// the indicated columns, in the order they appear in the SELECT. row.Scan
// receives the addresses of the matched fields.
//
// Columns must match known struct fields (via tag db: or snake_case). A
// column without a corresponding field is an error — the caller controls
// which columns to request from SQL, so this indicates a construction bug.
func scanRowIntoStruct(row pgx.Row, dst any, columns []string) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return fmt.Errorf("scanRowIntoStruct: dst must be a non-nil pointer, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("scanRowIntoStruct: dst must point to a struct, got %s", v.Kind())
	}
	idx := loadStructIndex(v.Type())
	targets := make([]any, len(columns))
	for i, col := range columns {
		pos, ok := idx.byCol[col]
		if !ok {
			return fmt.Errorf("scanRowIntoStruct: column %q has no corresponding field in %s", col, v.Type().Name())
		}
		targets[i] = v.Field(idx.order[pos].fieldIndex).Addr().Interface()
	}
	return row.Scan(targets...)
}
