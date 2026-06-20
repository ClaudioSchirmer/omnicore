package openapi

import (
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- schema generator defensive + anonymous-embed branches ------------------

func TestGenerate_UnsupportedKindEmitsEmptySchema(t *testing.T) {
	g := NewGenerator(nil)
	s := g.Generate(reflect.TypeOf(make(chan int)))
	if s == nil || s.Type != "" {
		t.Fatalf("unsupported kind must yield an empty schema, got %+v", s)
	}
}

type embedBody struct {
	Inner string `json:"inner"`
}

type namedScalar int

type structWithPointerAndUnexportedEmbed struct {
	*embedBody         // anonymous pointer embed → deref + promote
	namedScalar        // unexported non-struct anonymous field → skipped
	Name        string `json:"name"`
}

func TestGenerate_AnonymousPointerEmbedAndUnexportedScalar(t *testing.T) {
	g := NewGenerator(&Components{Schemas: map[string]*Schema{}})
	s := g.Generate(reflect.TypeOf(structWithPointerAndUnexportedEmbed{}))
	if s == nil {
		t.Fatal("expected a schema")
	}
	// The named struct registers a component; resolve the ref to inspect props.
	def := g.Components().Schemas[s.Ref[len("#/components/schemas/"):]]
	if def == nil {
		t.Fatalf("expected registered component for %q", s.Ref)
	}
	if _, ok := def.Properties["inner"]; !ok {
		t.Errorf("pointer embed must promote inner, props=%v", def.Properties)
	}
	if _, ok := def.Properties["name"]; !ok {
		t.Errorf("name must be present, props=%v", def.Properties)
	}
}

// --- example tag parsing / validation error branches ------------------------

func TestParseExampleTag_DomainIDInvalidUUID(t *testing.T) {
	if _, err := parseExampleTag(reflect.TypeOf(domain.ID{}), "not-a-uuid"); err == nil {
		t.Fatal("expected invalid-uuid error for domain.ID example")
	}
}

func TestValidateExample_NonMarshalableValue(t *testing.T) {
	if _, err := validateExample(make(chan int), nil, false); err == nil {
		t.Fatal("expected not-JSON-marshalable error")
	}
}

func TestValidateExample_NilValueRemovesDefault(t *testing.T) {
	raw, err := validateExample(nil, nil, false)
	if err != nil || raw != nil {
		t.Fatalf("nil example must yield (nil,nil), got raw=%v err=%v", raw, err)
	}
}
