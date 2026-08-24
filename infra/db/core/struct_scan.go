package core

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Auto-scan: the AggregateLoader generates an explicit SELECT from the
// TableSchema (mapped columns) and scans the row directly into the struct
// fields by the schema's resolved field indices. The column → field mapping is
// the schema's bidirectional plan (ScanPlan); there is no convention inference.

// idType / idPtrType anchor the reflect-based detection of identity fields:
// the field TYPE is the declaration (mirroring how BoolColumns infers bool
// columns), so the scan plan knows which targets need the id proxies below and
// the criteria translator knows which probes to lift into domain.ID.
var (
	idType    = reflect.TypeOf(domain.ID{})
	idPtrType = reflect.TypeOf((*domain.ID)(nil))
)

// scanTargetFor returns the scan destination for one struct field: the plain
// field address for every ordinary field, or an id proxy for a domain.ID /
// *domain.ID field. Both drivers hand a proxy the raw column value (they honor
// sql.Scanner — database/sql natively, pgx via its Scanner fallback), so the
// proxy owns the decode — BINARY(16) bytes and uuid/text forms all restore to
// the canonical string — and SQL NULL is resolved explicitly: nil for the
// pointer field, a loud error for the value field (the nullable contract
// database/sql cannot express for Scanner types on its own).
func scanTargetFor(f reflect.Value) any {
	switch f.Type() {
	case idType:
		return &idScanTarget{dst: f.Addr().Interface().(*domain.ID)}
	case idPtrType:
		return &nullableIDScanTarget{dst: f.Addr().Interface().(**domain.ID)}
	}
	// A value-object field is scanned through its UNDERLYING scalar and
	// reconstructed (raw Convert, or enum membership converge to Unknown), so the
	// driver never binds the named type. Mirrors the id proxies above.
	if _, underlying, ok := valueObjectField(f.Type()); ok {
		if f.Kind() == reflect.Pointer {
			return &nullableVOScanTarget{dst: f, voType: f.Type().Elem(), underlying: underlying}
		}
		return &voScanTarget{dst: f, voType: f.Type(), underlying: underlying}
	}
	return f.Addr().Interface()
}

// coerceScalar normalizes the raw driver value into the shape the value-object
// reconstruction expects: a string-backed VO must receive a string even when the
// driver hands []byte (how some drivers deliver text/char columns), otherwise the
// enum membership walk (which asserts raw.(string)) would miss and converge every
// value to Unknown. Numeric/bool/time forms pass through — domain.NewValueObjectValue
// (raw Convert) and sameUnderlying (asInt64) already tolerate driver widths.
func coerceScalar(src any, underlying reflect.Type) any {
	switch underlying.Kind() {
	case reflect.String:
		if b, ok := src.([]byte); ok {
			return string(b)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// Some drivers (notably go-ora) deliver EVERY numeric column as text, so
		// an int-backed VO would receive a string the enum membership walk can
		// never match — converging every value to Unknown. Parse it back to an
		// integer the reconstruction tolerates (asInt64 handles the width).
		if s, ok := asText(src); ok {
			if n, err := strconv.ParseInt(numericPrefix(s), 10, 64); err == nil {
				return n
			}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if s, ok := asText(src); ok {
			if n, err := strconv.ParseUint(numericPrefix(s), 10, 64); err == nil {
				return n
			}
		}
	case reflect.Float32, reflect.Float64:
		if s, ok := asText(src); ok {
			if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return n
			}
		}
	}
	return src
}

// asText reports the string form of a driver value delivered as text (string or
// []byte), the two shapes a driver hands a character/number column.
func asText(src any) (string, bool) {
	switch v := src.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	}
	return "", false
}

// numericPrefix trims surrounding space and drops a trailing ".0"-style fraction
// a NUMBER column may carry when the driver renders an integer as text (go-ora
// hands "1", but a NUMBER(10,0) round-trip elsewhere can render "1.0"). Only the
// integer head is kept; a genuine fractional value is left for ParseInt to reject.
func numericPrefix(s string) string {
	s = strings.TrimSpace(s)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		frac := s[dot+1:]
		if strings.Trim(frac, "0") == "" { // only trailing zeros after the point
			return s[:dot]
		}
	}
	return s
}

// voScanTarget scans one column into a REQUIRED value-object field. It decodes the
// driver value into the VO's underlying scalar, reconstructs the VO through
// domain.NewValueObjectValue, and sets the field. SQL NULL is a loud error (the
// nullable contract database/sql cannot express for Scanner types on its own).
type voScanTarget struct {
	dst        reflect.Value // the addressable VO field (settable)
	voType     reflect.Type  // e.g. vos.Email
	underlying reflect.Type  // e.g. string
}

func (t *voScanTarget) Scan(src any) error {
	if src == nil {
		return fmt.Errorf(
			"NULL scanned into a non-nullable value-object field of type %s — declare the field *%s (and the column NULL-able)",
			t.voType, t.voType)
	}
	vo, err := domain.NewValueObjectValue(t.voType, coerceScalar(src, t.underlying))
	if err != nil {
		return err
	}
	t.dst.Set(reflect.ValueOf(vo))
	return nil
}

// nullableVOScanTarget scans one column into a NULLABLE value-object field
// (*vos.X): SQL NULL restores as nil, any value as &VO.
type nullableVOScanTarget struct {
	dst        reflect.Value // the addressable *VO field
	voType     reflect.Type  // the element type, e.g. vos.Phone
	underlying reflect.Type
}

func (t *nullableVOScanTarget) Scan(src any) error {
	if src == nil {
		t.dst.Set(reflect.Zero(t.dst.Type()))
		return nil
	}
	vo, err := domain.NewValueObjectValue(t.voType, coerceScalar(src, t.underlying))
	if err != nil {
		return err
	}
	p := reflect.New(t.voType)
	p.Elem().Set(reflect.ValueOf(vo))
	t.dst.Set(p)
	return nil
}

// decodeIDValue normalizes the raw forms the drivers hand a Scanner for an id
// column: 16 raw bytes (MySQL BINARY(16)) or a 16-byte array decode to the
// canonical uuid string; text — []byte or string, how pgx delivers a UUID
// column and either driver delivers a CHAR(36) — passes through as-is (like
// domain.NewID, no validation: the column's value is the identity).
func decodeIDValue(src any) (string, error) {
	switch v := src.(type) {
	case []byte:
		if len(v) == 16 {
			u, err := uuid.FromBytes(v)
			if err != nil {
				return "", fmt.Errorf("decoding BINARY(16) id: %w", err)
			}
			return u.String(), nil
		}
		return string(v), nil
	case [16]byte:
		u, err := uuid.FromBytes(v[:])
		if err != nil {
			return "", fmt.Errorf("decoding 16-byte id: %w", err)
		}
		return u.String(), nil
	case string:
		return v, nil
	default:
		return "", fmt.Errorf("unsupported driver value %T for a domain.ID field", src)
	}
}

// idScanTarget scans one column into a REQUIRED identity field (domain.ID).
type idScanTarget struct{ dst *domain.ID }

func (t *idScanTarget) Scan(src any) error {
	if src == nil {
		return fmt.Errorf("NULL scanned into a non-nullable domain.ID field — declare the field *domain.ID (and the column NULL-able)")
	}
	s, err := decodeIDValue(src)
	if err != nil {
		return err
	}
	*t.dst = domain.NewID(s)
	return nil
}

// nullableIDScanTarget scans one column into a NULLABLE identity field
// (*domain.ID): SQL NULL restores as nil, any value as &domain.ID.
type nullableIDScanTarget struct{ dst **domain.ID }

func (t *nullableIDScanTarget) Scan(src any) error {
	if src == nil {
		*t.dst = nil
		return nil
	}
	s, err := decodeIDValue(src)
	if err != nil {
		return err
	}
	id := domain.NewID(s)
	*t.dst = &id
	return nil
}

// Identity columns reached THROUGH A JOIN. A join field carries no domain type —
// no value object, and therefore not domain.ID either — so an identity column of
// the joined aggregate has to land in a plain string. It cannot land there raw:
// mysql, sqlserver and oracle store an identity as BINARY(16)/RAW(16), which a
// string field would receive as 16 bytes rather than a uuid, silently. These two
// proxies decode it into the canonical text, reusing the id decode domain.ID
// already owns and assigning what it yields.

// idTextScanTarget scans an identity column into a REQUIRED string field.
type idTextScanTarget struct{ dst *string }

func (t *idTextScanTarget) Scan(src any) error {
	var id domain.ID
	if err := (&idScanTarget{dst: &id}).Scan(src); err != nil {
		return err
	}
	*t.dst = id.Value()
	return nil
}

// nullableIDTextScanTarget scans an identity column into a NULLABLE string field
// (*string): SQL NULL restores as nil — the left join with no counterpart, and
// the nullable identity column alike.
type nullableIDTextScanTarget struct{ dst **string }

func (t *nullableIDTextScanTarget) Scan(src any) error {
	var id *domain.ID
	if err := (&nullableIDScanTarget{dst: &id}).Scan(src); err != nil {
		return err
	}
	if id == nil {
		*t.dst = nil
		return nil
	}
	v := id.Value()
	*t.dst = &v
	return nil
}

// IDTextScanTarget returns the scan destination for a join field that maps an
// identity column of the JOINED aggregate. The field was proven to be string or
// *string at construction (validateJoinFieldType), so a false here would be a
// framework bug rather than a declaration mistake.
func IDTextScanTarget(f reflect.Value) (any, bool) {
	switch f.Type() {
	case reflect.TypeOf(""):
		return &idTextScanTarget{dst: f.Addr().Interface().(*string)}, true
	case reflect.TypeOf((*string)(nil)):
		return &nullableIDTextScanTarget{dst: f.Addr().Interface().(**string)}, true
	}
	return nil, false
}

// scanRowIntoStruct fills dst (must be pointer to struct) with the values of
// the indicated columns, in the order they appear in the SELECT. row.Scan
// receives the addresses of the matched fields. byCol is the per-source
// mappedColumn → reflect-field-index map (TableSchema.ScanPlan), so a renamed
// column resolves to the right field.
//
// A column without a corresponding entry in byCol is an error — the caller
// builds both the SELECT list and byCol from the same schema, so a mismatch
// indicates a construction bug.
func scanRowIntoStruct(row keyedRow, dst any, columns []string, byCol map[string]FieldPath) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("scanRowIntoStruct: dst must be a non-nil pointer, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("scanRowIntoStruct: dst must point to a struct, got %s", v.Kind())
	}
	targets, composites, err := buildScanTargets(v, columns, byCol, "scanRowIntoStruct")
	if err != nil {
		return err
	}
	if err := row.Scan(targets...); err != nil {
		return err
	}
	return composites.finalize()
}

// keyedRow is satisfied by both the Postgres (pgx.Row/pgx.Rows) and MySQL
// (*sql.Row/*sql.Rows) driver row types (Scan(dest ...any) error).
type keyedRow interface {
	Scan(dest ...any) error
}

// ScanLeadingKey scans a row shaped (key, col1, col2, …) into dst's fields for
// the given columns and returns the leading key column as a string. Used by the
// entity search engine where the row carries an identifier the struct does not
// expose as a field: the root id (FindOne/FindAll do not know it a priori) and
// the child foreign key (needed to group batched children by root). The key is
// scanned into a string — the same uuid→string scan the executor's
// `RETURNING <pk>` path uses.
func ScanLeadingKey(row keyedRow, dst any, columns []string, byCol map[string]FieldPath) (string, error) {
	return ScanLeadingKeyTrailing(row, dst, columns, byCol)
}

// ScanLeadingKeyTrailing is ScanLeadingKey plus a tail of caller-owned scan
// targets appended AFTER the struct columns — used to read the framework-managed
// columns (created_at/updated_at/deleted_at/revision) into external
// sql.Null* destinations rather than struct fields, since the entity's carrier
// slots are unexported. The SELECT must list: leading key, columns..., then the
// trailing columns in the same order as `trailing`.
func ScanLeadingKeyTrailing(row keyedRow, dst any, columns []string, byCol map[string]FieldPath, trailing ...any) (string, error) {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return "", fmt.Errorf("ScanLeadingKey: dst must be a non-nil pointer, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return "", fmt.Errorf("ScanLeadingKey: dst must point to a struct, got %s", v.Kind())
	}
	var key string
	fieldTargets, composites, err := buildScanTargets(v, columns, byCol, "ScanLeadingKey")
	if err != nil {
		return "", err
	}
	targets := make([]any, 0, len(columns)+1+len(trailing))
	targets = append(targets, &key)
	targets = append(targets, fieldTargets...)
	targets = append(targets, trailing...)
	if err := row.Scan(targets...); err != nil {
		return key, err
	}
	return key, composites.finalize()
}
