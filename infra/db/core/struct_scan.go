package core

import (
	"fmt"
	"reflect"
)

// Auto-scan: the AggregateLoader generates an explicit SELECT from the
// TableSchema (mapped columns) and scans the row directly into the struct
// fields by the schema's resolved field indices. The column → field mapping is
// the schema's bidirectional plan (ScanPlan); there is no convention inference.

// scanRowIntoStruct fills dst (must be pointer to struct) with the values of
// the indicated columns, in the order they appear in the SELECT. row.Scan
// receives the addresses of the matched fields. byCol is the per-source
// mappedColumn → reflect-field-index map (TableSchema.ScanPlan), so a renamed
// column resolves to the right field.
//
// A column without a corresponding entry in byCol is an error — the caller
// builds both the SELECT list and byCol from the same schema, so a mismatch
// indicates a construction bug.
func scanRowIntoStruct(row keyedRow, dst any, columns []string, byCol map[string]int) error {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return fmt.Errorf("scanRowIntoStruct: dst must be a non-nil pointer, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("scanRowIntoStruct: dst must point to a struct, got %s", v.Kind())
	}
	targets := make([]any, len(columns))
	for i, col := range columns {
		fieldIndex, ok := byCol[col]
		if !ok {
			return fmt.Errorf("scanRowIntoStruct: column %q has no corresponding field in %s", col, v.Type().Name())
		}
		targets[i] = v.Field(fieldIndex).Addr().Interface()
	}
	return row.Scan(targets...)
}

// keyedRow is satisfied by both pgx.Row and pgx.Rows (Scan(dest ...any) error).
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
func ScanLeadingKey(row keyedRow, dst any, columns []string, byCol map[string]int) (string, error) {
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return "", fmt.Errorf("ScanLeadingKey: dst must be a non-nil pointer, got %T", dst)
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return "", fmt.Errorf("ScanLeadingKey: dst must point to a struct, got %s", v.Kind())
	}
	var key string
	targets := make([]any, 0, len(columns)+1)
	targets = append(targets, &key)
	for _, col := range columns {
		fieldIndex, ok := byCol[col]
		if !ok {
			return "", fmt.Errorf("ScanLeadingKey: column %q has no corresponding field in %s", col, v.Type().Name())
		}
		targets = append(targets, v.Field(fieldIndex).Addr().Interface())
	}
	return key, row.Scan(targets...)
}
