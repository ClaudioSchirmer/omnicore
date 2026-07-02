package core

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// scanRowIntoStruct / ScanLeadingKey branches. Relocated from the former
// infra-root coverage grab-bag once struct_scan.go moved to package db. The
// keyedRow seam is driven by a scriptable scanCovRow.

type scanCovRow struct {
	values  []any
	scanErr error
}

func (r *scanCovRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	for i, d := range dest {
		if i >= len(r.values) {
			break
		}
		if err := scanCovAssign(d, r.values[i]); err != nil {
			return err
		}
	}
	return nil
}

func scanCovAssign(dst any, src any) error {
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer, got %T", dst)
	}
	target := dv.Elem()
	sv := reflect.ValueOf(src)
	if !sv.IsValid() {
		return nil
	}
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

type scanTargetStruct struct {
	Name string
	Age  int
}

func TestScanRowIntoStruct_FillsFields(t *testing.T) {
	dst := &scanTargetStruct{}
	row := &scanCovRow{values: []any{"bob", 42}}
	err := scanRowIntoStruct(row, dst, []string{"name", "age"}, map[string]int{"name": 0, "age": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dst.Name != "bob" || dst.Age != 42 {
		t.Fatalf("fields drifted: %+v", dst)
	}
}

func TestScanRowIntoStruct_RejectsNonPointer(t *testing.T) {
	if err := scanRowIntoStruct(&scanCovRow{}, scanTargetStruct{}, nil, nil); err == nil {
		t.Fatal("expected error for non-pointer dst")
	}
	var nilPtr *scanTargetStruct
	if err := scanRowIntoStruct(&scanCovRow{}, nilPtr, nil, nil); err == nil {
		t.Fatal("expected error for nil pointer dst")
	}
}

func TestScanRowIntoStruct_RejectsNonStruct(t *testing.T) {
	x := 7
	if err := scanRowIntoStruct(&scanCovRow{}, &x, nil, nil); err == nil {
		t.Fatal("expected error for pointer-to-non-struct dst")
	}
}

func TestScanRowIntoStruct_UnknownColumn(t *testing.T) {
	dst := &scanTargetStruct{}
	err := scanRowIntoStruct(&scanCovRow{values: []any{"x"}}, dst, []string{"missing"}, map[string]int{"name": 0})
	if err == nil || !strings.Contains(err.Error(), "no corresponding field") {
		t.Fatalf("expected unknown-column error, got %v", err)
	}
}

func TestScanRowIntoStruct_ScanErrorPropagates(t *testing.T) {
	dst := &scanTargetStruct{}
	row := &scanCovRow{scanErr: errors.New("boom")}
	if err := scanRowIntoStruct(row, dst, []string{"name"}, map[string]int{"name": 0}); err == nil {
		t.Fatal("expected scan error to propagate")
	}
}

func TestScanLeadingKey_ReturnsKeyAndFills(t *testing.T) {
	dst := &scanTargetStruct{}
	row := &scanCovRow{values: []any{"id-1", "bob", 42}}
	key, err := ScanLeadingKey(row, dst, []string{"name", "age"}, map[string]int{"name": 0, "age": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "id-1" {
		t.Fatalf("key = %q, want id-1", key)
	}
	if dst.Name != "bob" || dst.Age != 42 {
		t.Fatalf("fields drifted: %+v", dst)
	}
}

func TestScanLeadingKey_RejectsNonPointer(t *testing.T) {
	if _, err := ScanLeadingKey(&scanCovRow{}, scanTargetStruct{}, nil, nil); err == nil {
		t.Fatal("expected error for non-pointer dst")
	}
	var nilPtr *scanTargetStruct
	if _, err := ScanLeadingKey(&scanCovRow{}, nilPtr, nil, nil); err == nil {
		t.Fatal("expected error for nil pointer dst")
	}
}

func TestScanLeadingKey_RejectsNonStruct(t *testing.T) {
	x := 0
	if _, err := ScanLeadingKey(&scanCovRow{}, &x, nil, nil); err == nil {
		t.Fatal("expected error for pointer-to-non-struct dst")
	}
}

func TestScanLeadingKey_UnknownColumn(t *testing.T) {
	dst := &scanTargetStruct{}
	_, err := ScanLeadingKey(&scanCovRow{values: []any{"id"}}, dst, []string{"missing"}, map[string]int{"name": 0})
	if err == nil || !strings.Contains(err.Error(), "no corresponding field") {
		t.Fatalf("expected unknown-column error, got %v", err)
	}
}
