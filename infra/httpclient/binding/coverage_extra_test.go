package binding

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// --- toValues edge shapes ----------------------------------------------

func TestToValues_MapStringSlice(t *testing.T) {
	in := map[string][]string{"k": {"a", "b"}}
	out, err := toValues(in)
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	if got := out["k"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("map[string][]string not preserved: %v", out)
	}
}

func TestToValues_PointerToStruct(t *testing.T) {
	type body struct {
		Name string `form:"name"`
	}
	out, err := toValues(&body{Name: "Ada"})
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	if out.Get("name") != "Ada" {
		t.Errorf("pointer struct not encoded: %v", out)
	}
}

func TestToValues_NilPointer(t *testing.T) {
	type body struct{ Name string }
	var p *body
	out, err := toValues(p)
	if err != nil {
		t.Fatalf("toValues nil pointer: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("nil pointer should yield empty values; got %v", out)
	}
}

func TestToValues_UnsupportedInput(t *testing.T) {
	if _, err := toValues(42); err == nil {
		t.Error("expected error for non-struct scalar input")
	}
}

func TestToValues_UnsupportedFieldKind(t *testing.T) {
	type body struct {
		Ch chan int `form:"ch"`
	}
	if _, err := toValues(body{Ch: make(chan int)}); err == nil {
		t.Error("expected error for unsupported field kind")
	}
}

func TestToValues_OmitEmptySkips(t *testing.T) {
	type body struct {
		Name string `form:"name,omitempty"`
		Age  int    `form:"age,omitempty"`
	}
	out, err := toValues(body{Name: "Ada"})
	if err != nil {
		t.Fatalf("toValues: %v", err)
	}
	if out.Get("name") != "Ada" {
		t.Errorf("name missing: %v", out)
	}
	if _, ok := out["age"]; ok {
		t.Errorf("zero age should be omitted: %v", out)
	}
}

// --- fromValues edge shapes --------------------------------------------

func TestFromValues_IntoMapStringString(t *testing.T) {
	values := url.Values{"a": {"1"}, "b": {"2", "ignored"}}
	var out map[string]string
	if err := fromValues(values, &out); err != nil {
		t.Fatalf("fromValues: %v", err)
	}
	if out["a"] != "1" || out["b"] != "2" {
		t.Errorf("map decode wrong: %v", out)
	}
}

func TestFromValues_NilTarget(t *testing.T) {
	if err := fromValues(url.Values{}, nil); err == nil {
		t.Error("expected error for nil target")
	}
}

func TestFromValues_NonPointer(t *testing.T) {
	var s struct{ A string }
	if err := fromValues(url.Values{}, s); err == nil {
		t.Error("expected error for non-pointer target")
	}
}

func TestFromValues_PointerToNonStruct(t *testing.T) {
	n := 0
	if err := fromValues(url.Values{}, &n); err == nil {
		t.Error("expected error for pointer-to-non-struct target")
	}
}

func TestFromValues_SetFieldError(t *testing.T) {
	type body struct {
		Age int `form:"age"`
	}
	var got body
	if err := fromValues(url.Values{"age": {"not-a-number"}}, &got); err == nil {
		t.Error("expected parse error for non-numeric int field")
	}
}

func TestFromValues_DashTagSkipped(t *testing.T) {
	type body struct {
		Keep string `form:"keep"`
		Drop string `form:"-"`
	}
	var got body
	if err := fromValues(url.Values{"keep": {"v"}, "-": {"x"}}, &got); err != nil {
		t.Fatalf("fromValues: %v", err)
	}
	if got.Keep != "v" || got.Drop != "" {
		t.Errorf("dash tag not skipped: %+v", got)
	}
}

// --- scalarLikeKind -----------------------------------------------------

func TestScalarLikeKind(t *testing.T) {
	truthy := []reflect.Kind{
		reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint16, reflect.Uint64,
		reflect.Float32, reflect.Float64,
	}
	for _, k := range truthy {
		if !scalarLikeKind(k) {
			t.Errorf("scalarLikeKind(%s) = false, want true", k)
		}
	}
	falsy := []reflect.Kind{
		reflect.Slice, reflect.Map, reflect.Struct, reflect.Pointer,
		reflect.Chan, reflect.Interface,
	}
	for _, k := range falsy {
		if scalarLikeKind(k) {
			t.Errorf("scalarLikeKind(%s) = true, want false", k)
		}
	}
}

// --- sliceToStrings -----------------------------------------------------

func TestSliceToStrings(t *testing.T) {
	t.Run("slice of strings", func(t *testing.T) {
		got, err := sliceToStrings(reflect.ValueOf([]string{"a", "b"}))
		if err != nil {
			t.Fatalf("sliceToStrings: %v", err)
		}
		if strings.Join(got, ",") != "a,b" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("slice of ints", func(t *testing.T) {
		got, err := sliceToStrings(reflect.ValueOf([]int{1, 2, 3}))
		if err != nil {
			t.Fatalf("sliceToStrings: %v", err)
		}
		if strings.Join(got, ",") != "1,2,3" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("pointer to slice", func(t *testing.T) {
		s := []string{"x"}
		got, err := sliceToStrings(reflect.ValueOf(&s))
		if err != nil {
			t.Fatalf("sliceToStrings: %v", err)
		}
		if len(got) != 1 || got[0] != "x" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("nil pointer", func(t *testing.T) {
		var s *[]string
		got, err := sliceToStrings(reflect.ValueOf(s))
		if err != nil {
			t.Fatalf("sliceToStrings nil: %v", err)
		}
		if got != nil {
			t.Errorf("nil pointer should yield nil; got %v", got)
		}
	})

	t.Run("not a slice", func(t *testing.T) {
		if _, err := sliceToStrings(reflect.ValueOf(42)); err == nil {
			t.Error("expected error for non-slice input")
		}
	})

	t.Run("element error", func(t *testing.T) {
		// scalarToString rejects non-scalar elements (e.g. nested slices).
		if _, err := sliceToStrings(reflect.ValueOf([][]int{{1}})); err == nil {
			t.Error("expected error for non-scalar element")
		}
	})
}
