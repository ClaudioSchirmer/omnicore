package pipeline

import "testing"

func TestFullBody_SatisfiesEnforcer(t *testing.T) {
	var fb any = FullBody{}
	if _, ok := fb.(FullBodyEnforcer); !ok {
		t.Fatal("FullBody{} should satisfy FullBodyEnforcer")
	}
}

func TestFullBody_EmbeddingSatisfiesEnforcer(t *testing.T) {
	type withMarker struct {
		FullBody
		Field string
	}
	var v any = &withMarker{}
	if _, ok := v.(FullBodyEnforcer); !ok {
		t.Fatal("struct embedding FullBody should satisfy FullBodyEnforcer")
	}
}

func TestFullBody_NoEmbedDoesNotSatisfy(t *testing.T) {
	type withoutMarker struct {
		Field string
	}
	var v any = &withoutMarker{}
	if _, ok := v.(FullBodyEnforcer); ok {
		t.Fatal("struct without FullBody embed must NOT satisfy FullBodyEnforcer")
	}
}
