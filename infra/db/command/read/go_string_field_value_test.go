package read

import "testing"

// goStringFieldValue reads the natural-key value off the role entity by Go
// name — every reflection edge is a distinct branch.
func TestGoStringFieldValue(t *testing.T) {
	type sample struct {
		Name    string
		PtrName *string
		Num     int
	}
	name := "Ana"
	var nilPtr *string

	cases := []struct {
		label  string
		entity any
		field  string
		want   string
	}{
		{"plainString", &sample{Name: "Ana"}, "Name", "Ana"},
		{"pointerString", &sample{PtrName: &name}, "PtrName", "Ana"},
		{"nilPointer", &sample{PtrName: nilPtr}, "PtrName", ""},
		{"nonString", &sample{Num: 7}, "Num", "7"},
		{"absentField", &sample{}, "Ghost", ""},
		{"nonStruct", "not-a-struct", "Name", ""},
	}
	for _, c := range cases {
		if got := goStringFieldValue(c.entity, c.field); got != c.want {
			t.Errorf("%s: goStringFieldValue = %q, want %q", c.label, got, c.want)
		}
	}
}
