package infra

import "reflect"

// Phase 19 — Convention-based inference on the write side. DDD-pure domain
// declares only structure + rules; infra resolves table/column/FK by
// convention from the Go type. Per-Repository override via RepoConfig
// (not via struct tag — domain does not pronounce column names).
//
// Conventions:
//   - Table: singular PascalCase → smart plural snake_case
//     (ending in s/x/z/ch/sh → "es"; otherwise "s")
//     User → users; Address → addresses; Box → boxes
//   - Column: exported field name → snake_case
//     ZipCode → zip_code; CPF → cpf; URLPath → url_path
//   - FK on child: parent type name in singular snake_case + "_id"
//     User → user_id; OrderHeader → order_header_id
//
// Skip:
//   - Anonymous embeds (AggregateRoot, BaseEntity) — no column of their own
//   - Unexported fields (PkgPath != "") — impossible to populate
//   - Tag `transient:"-"` — domain declares the field exists only at runtime
//     (request-scoped input, computed/derived value, in-memory cache, runtime
//     bookkeeping flag); infra never materializes it
//   - Field named exactly "ID" — DB-gen + separate WHERE clause

// ColumnSpec describes an inferred column: SQL column name + index of the
// corresponding field in the struct (for reflect.Value.Field(i) and Set/Get).
type ColumnSpec struct {
	Column     string
	FieldIndex int
}

// TypeName returns the Go type name of T without pointer or package qualifier.
// Used by BaseRepository and AggregateLoader to derive the default ContextName
// when the service has not set one explicitly — convention: ContextName = name
// of the entity type ("User" for *appdomain.User).
//
//	TypeName[*User]()  → "User"
//	TypeName[User]()   → "User"
//
// Returns "" if T is an anonymous type or unnamed interface — the caller
// decides the fallback (BaseRepository/AggregateLoader also return "",
// preserving the pre-existing semantics of "no configured context name").
func TypeName[T any]() string {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

// InferTableName: singular PascalCase → smart plural snake_case.
//
//	User       → users
//	Address    → addresses     (rule: ends in "s" before pluralizing? no — just pluralizes)
//	Box        → boxes
//	Watch      → watches
//	Quiz       → quizzes (doubled z not handled — edge case; use override)
//	Datum      → datums  (irregular — use override)
//	CPFLookup  → cpf_lookups
func InferTableName(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return pluralize(pascalToSnake(t.Name()))
}

// InferForeignKey: parent type → FK column on the child.
//
//	User        → user_id
//	OrderHeader → order_header_id
func InferForeignKey(parentType reflect.Type) string {
	if parentType.Kind() == reflect.Ptr {
		parentType = parentType.Elem()
	}
	return pascalToSnake(parentType.Name()) + "_id"
}

// InferColumns returns the ordered list of columns that INSERT/UPDATE must
// produce for an entity of type t. Same order as the field declaration.
// Cached per reflect.Type via loadStructIndex (sync.Map).
//
// FieldOverrides applied if non-nil — key is the Go field name (PascalCase),
// value is the DB column name. Per-Repository override (RepoConfig).
func InferColumns(t reflect.Type, fieldOverrides map[string]string) []ColumnSpec {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	idx := loadStructIndex(t)
	out := make([]ColumnSpec, 0, len(idx.order))
	for _, fi := range idx.order {
		// Skip "id" on the write side — DB-gen + separate WHERE clause.
		// (Read side via loadStructIndex INCLUDES "id" to populate the struct.)
		if fi.col == "id" {
			continue
		}
		col := fi.col
		// fieldOverrides uses the Go field name (PascalCase) as key.
		if fieldOverrides != nil {
			fieldName := t.Field(fi.fieldIndex).Name
			if override, ok := fieldOverrides[fieldName]; ok {
				col = override
			}
		}
		out = append(out, ColumnSpec{Column: col, FieldIndex: fi.fieldIndex})
	}
	return out
}

// InferFieldValues extracts values via reflection in the column order.
// v can be a pointer or value — automatically unwrapped.
func InferFieldValues(v reflect.Value, cols []ColumnSpec) []any {
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	args := make([]any, len(cols))
	for i, c := range cols {
		args[i] = v.Field(c.FieldIndex).Interface()
	}
	return args
}

// ColumnsOnly extracts only the column names (useful for buildInsert/buildUpdate).
func ColumnsOnly(cols []ColumnSpec) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Column
	}
	return out
}

// FieldsFromEntity is the canonical Phase 19 helper that replaces the
// Entity.ToFields() call of the old design. Returns the map keyed by column
// name. Idempotent / lookup-cached via loadStructIndex.
func FieldsFromEntity(e any, fieldOverrides map[string]string) map[string]any {
	v := reflect.ValueOf(e)
	t := v.Type()
	cols := InferColumns(t, fieldOverrides)
	vals := InferFieldValues(v, cols)
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		out[c.Column] = vals[i]
	}
	return out
}

// pluralize implements basic English rules that cover common PT/EN cases:
//   - ends in "s", "x", "z", "ch", "sh" → "es"
//   - ends in consonant + "y" → "ies" (city → cities)
//   - ends in "f" → "ves" (calf → calves) — disabled (rare edge case; use override)
//   - rest → "s"
//
// Does not cover irregulars (person/people, datum/data, mouse/mice) — use override.
func pluralize(s string) string {
	if s == "" {
		return ""
	}
	n := len(s)
	last := s[n-1]
	// ends with sh or ch → +es
	if n >= 2 {
		tail := s[n-2:]
		if tail == "sh" || tail == "ch" {
			return s + "es"
		}
	}
	switch last {
	case 's', 'x', 'z':
		return s + "es"
	case 'y':
		if n >= 2 && !isVowel(s[n-2]) {
			return s[:n-1] + "ies"
		}
	}
	return s + "s"
}

func isVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

