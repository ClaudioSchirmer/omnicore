package core

import "testing"

// WireFieldOf turns a physical column into the wire-format field name a
// notification carries: the declared Go name through the lower-camel renderer.
// An undeclared column renders as itself.
func TestTableSchema_WireFieldOf(t *testing.T) {
	s := NewTableSchema[schemaSample]("t").ID("user_id").Field("Name", "full_name")
	if got := s.WireFieldOf("user_id"); got != "id" {
		t.Errorf("id column must render the fixed Go name: got %q, want %q", got, "id")
	}
	if got := s.WireFieldOf("full_name"); got != "name" {
		t.Errorf("declared column: got %q, want %q", got, "name")
	}
	if got := s.WireFieldOf("no_such_col"); got != "no_such_col" {
		t.Errorf("undeclared column renders as itself: got %q", got)
	}
}
