package bootstrap

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// stubModule is a translation.Module shim — the framework's Module
// contract is small enough that boostrap tests can satisfy it inline
// without dragging the real CorePTBR/CoreENG/... helpers into the
// package's test surface (they live in application/translation and
// are already covered by their own tests).
type stubModule struct {
	lang configuration.Language
}

func (s stubModule) Language() configuration.Language { return s.lang }
func (s stubModule) Translations() map[string]string  { return nil }

func TestCollectLanguageOptions_NilReturnsNil(t *testing.T) {
	if got := collectLanguageOptions(nil); got != nil {
		t.Fatalf("nil input → got %v, want nil", got)
	}
}

func TestCollectLanguageOptions_EmptyReturnsNil(t *testing.T) {
	if got := collectLanguageOptions([]translation.Module{}); got != nil {
		t.Fatalf("empty input → got %v, want nil", got)
	}
}

func TestCollectLanguageOptions_DedupAndENGFirst(t *testing.T) {
	mods := []translation.Module{
		stubModule{lang: configuration.LangFR},
		stubModule{lang: configuration.LangPTBR},
		stubModule{lang: configuration.LangENG},
		stubModule{lang: configuration.LangPTBR}, // duplicate — must be skipped
		stubModule{lang: configuration.LangES},
		stubModule{lang: configuration.LangENG}, // duplicate — must be skipped
	}
	got := collectLanguageOptions(mods)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (deduped); got=%+v", len(got), got)
	}
	// ENG rotated to position 0 (English-first default); declaration
	// order otherwise preserved: FR, PT_BR, ES.
	wantOrder := []configuration.Language{
		configuration.LangENG,
		configuration.LangFR,
		configuration.LangPTBR,
		configuration.LangES,
	}
	for i, l := range wantOrder {
		if got[i].Label != l.String() {
			t.Fatalf("position %d label: got %q, want %q", i, got[i].Label, l.String())
		}
		if got[i].Value != l.HTTPPrefix() {
			t.Fatalf("position %d value: got %q, want %q", i, got[i].Value, l.HTTPPrefix())
		}
	}
}

func TestCollectLanguageOptions_NoENGPreservesDeclarationOrder(t *testing.T) {
	mods := []translation.Module{
		stubModule{lang: configuration.LangFR},
		stubModule{lang: configuration.LangPTBR},
		stubModule{lang: configuration.LangES},
	}
	got := collectLanguageOptions(mods)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3; got=%+v", len(got), got)
	}
	wantOrder := []configuration.Language{
		configuration.LangFR,
		configuration.LangPTBR,
		configuration.LangES,
	}
	for i, l := range wantOrder {
		if got[i].Value != l.HTTPPrefix() {
			t.Fatalf("position %d: got %q, want %q (ENG absent → declaration order preserved)",
				i, got[i].Value, l.HTTPPrefix())
		}
	}
}

func TestCollectLanguageOptions_ENGAlreadyFirstStaysFirst(t *testing.T) {
	mods := []translation.Module{
		stubModule{lang: configuration.LangENG},
		stubModule{lang: configuration.LangPTBR},
	}
	got := collectLanguageOptions(mods)
	if got[0].Value != "en" {
		t.Fatalf("position 0: got %q, want %q", got[0].Value, "en")
	}
	if got[1].Value != "pt" {
		t.Fatalf("position 1: got %q, want %q (no rotation needed)", got[1].Value, "pt")
	}
}

func TestCollectLanguageOptions_SkipsUnknownLanguage(t *testing.T) {
	mods := []translation.Module{
		stubModule{lang: configuration.LangUnknown}, // HTTPPrefix() == ""
		stubModule{lang: configuration.LangENG},
	}
	got := collectLanguageOptions(mods)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (unknown skipped); got=%+v", len(got), got)
	}
	if got[0].Value != "en" {
		t.Fatalf("survivor value: got %q, want %q", got[0].Value, "en")
	}
}

func TestCollectLanguageOptions_AllUnknownReturnsNil(t *testing.T) {
	mods := []translation.Module{
		stubModule{lang: configuration.LangUnknown},
	}
	if got := collectLanguageOptions(mods); got != nil {
		t.Fatalf("all-unknown → got %v, want nil", got)
	}
}
