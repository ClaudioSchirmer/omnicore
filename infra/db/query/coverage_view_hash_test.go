package query

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// compositeBSON exercises every writeBSONValue type-switch branch in one walk,
// including the default fallback (time.Duration is a named int64 type, so its
// dynamic type does not match `case int64`).
func compositeBSON() bson.M {
	return bson.M{
		"nilv":     nil,
		"boolv":    true,
		"strv":     "x",
		"intv":     1,
		"int32v":   int32(2),
		"int64v":   int64(3),
		"f32":      float32(1.5),
		"f64":      float64(2.5),
		"nested":   map[string]any{"a": 1},
		"dorder":   bson.D{{Key: "k", Value: "v"}},
		"arr":      bson.A{1, "two"},
		"anyslice": []any{true, false},
		"strslice": []string{"a", "b"},
		"custom":   time.Duration(5),
	}
}

func TestWriteBSONValue_AllBranchesAndDeterminism(t *testing.T) {
	v := compositeBSON()

	w1 := newCanonicalWriter()
	writeBSONValue(w1, v)
	d1 := w1.hexDigest()

	w2 := newCanonicalWriter()
	writeBSONValue(w2, v)
	d2 := w2.hexDigest()

	if d1 != d2 {
		t.Fatalf("writeBSONValue not deterministic: %s != %s", d1, d2)
	}
}

func TestWriteBSONValue_SensitiveToChange(t *testing.T) {
	a := newCanonicalWriter()
	writeBSONValue(a, bson.M{"k": 1})
	b := newCanonicalWriter()
	writeBSONValue(b, bson.M{"k": 2})
	if a.hexDigest() == b.hexDigest() {
		t.Fatal("distinct values must hash differently")
	}
}

func TestWriteBSONValue_MapKeyOrderInvariant(t *testing.T) {
	// bson.M iteration order is random; writeSortedMap must normalize it so
	// two logically equal maps hash equal regardless of insertion order.
	m1 := bson.M{"a": 1, "b": 2, "c": 3}
	m2 := bson.M{"c": 3, "b": 2, "a": 1}
	w1 := newCanonicalWriter()
	writeBSONValue(w1, m1)
	w2 := newCanonicalWriter()
	writeBSONValue(w2, m2)
	if w1.hexDigest() != w2.hexDigest() {
		t.Fatal("map key order must not affect the hash")
	}
}

func TestWriteFloatPtr_NilAndValue(t *testing.T) {
	wNil := newCanonicalWriter()
	writeFloatPtr(wNil, nil)
	f := 3.14
	wVal := newCanonicalWriter()
	writeFloatPtr(wVal, &f)
	if wNil.hexDigest() == wVal.hexDigest() {
		t.Fatal("nil vs value float pointer must hash differently")
	}
}

func TestWriteInt32Ptr_NilAndValue(t *testing.T) {
	wNil := newCanonicalWriter()
	writeInt32Ptr(wNil, nil)
	n := int32(7)
	wVal := newCanonicalWriter()
	writeInt32Ptr(wVal, &n)
	if wNil.hexDigest() == wVal.hexDigest() {
		t.Fatal("nil vs value int32 pointer must hash differently")
	}
}
