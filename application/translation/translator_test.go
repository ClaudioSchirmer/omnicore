package translation

import (
	"errors"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

func TestNew_EmptyTranslator(t *testing.T) {
	tr := New()
	if tr == nil {
		t.Fatal("expected non-nil Translator")
	}
	if v, err := tr.Get(configuration.LangENG, "any"); err == nil {
		t.Errorf("expected NotFound on empty translator, got %q (nil err)", v)
	}
}

func TestTranslator_ImportThenGet(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	v, err := tr.Get(configuration.LangENG, "RecordNotFoundNotification")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if v != "Record not found." {
		t.Errorf("Get() = %q, want %q", v, "Record not found.")
	}
}

func TestTranslator_Get_NotFoundLanguage(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	// Language never imported.
	_, err := tr.Get(configuration.LangPTBR, "RecordNotFoundNotification")
	if err == nil {
		t.Fatal("expected NotFoundError when the language was never imported")
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected *NotFoundError, got %T", err)
	}
	if nf.Language != configuration.LangPTBR || nf.Key != "RecordNotFoundNotification" {
		t.Errorf("NotFoundError fields = {%v,%q}, want {%v,%q}",
			nf.Language, nf.Key, configuration.LangPTBR, "RecordNotFoundNotification")
	}
}

func TestTranslator_Get_NotFoundKey(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	_, err := tr.Get(configuration.LangENG, "DefinitelyNotInTheCatalog")
	if err == nil {
		t.Fatal("expected NotFoundError when the key is absent")
	}
	if !strings.Contains(err.Error(), "DefinitelyNotInTheCatalog") {
		t.Errorf("Error() should mention the missing key, got %q", err.Error())
	}
}

func TestTranslator_GetOr_Fallback(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	// Existing key — fallback ignored.
	if got := tr.GetOr(configuration.LangENG, "RecordNotFoundNotification", "FB"); got != "Record not found." {
		t.Errorf("GetOr existing = %q, want %q", got, "Record not found.")
	}

	// Missing key — fallback returned.
	if got := tr.GetOr(configuration.LangENG, "Missing", "FB"); got != "FB" {
		t.Errorf("GetOr missing = %q, want %q", got, "FB")
	}

	// Missing language — fallback returned.
	if got := tr.GetOr(configuration.LangPTBR, "RecordNotFoundNotification", "FB"); got != "FB" {
		t.Errorf("GetOr missing-lang = %q, want %q", got, "FB")
	}
}

func TestTranslator_Has(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	if !tr.Has(configuration.LangENG, "RecordNotFoundNotification") {
		t.Error("Has() should be true for an imported key")
	}
	if tr.Has(configuration.LangENG, "Missing") {
		t.Error("Has() should be false for a missing key")
	}
	if tr.Has(configuration.LangPTBR, "RecordNotFoundNotification") {
		t.Error("Has() should be false for a language never imported")
	}
}

func TestTranslator_Import_MultipleModules(t *testing.T) {
	tr := New()
	tr.Import(CoreENG(), CorePTBR())

	if !tr.Has(configuration.LangENG, "RecordNotFoundNotification") {
		t.Error("ENG catalog missing after multi-import")
	}
	if !tr.Has(configuration.LangPTBR, "RecordNotFoundNotification") {
		t.Error("PTBR catalog missing after multi-import")
	}
}

func TestTranslator_Import_OverridesExistingKey(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	// Custom module with a clashing key — last write wins.
	override := stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"RecordNotFoundNotification": "Custom override.",
		},
	}
	tr.Import(override)

	v, err := tr.Get(configuration.LangENG, "RecordNotFoundNotification")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if v != "Custom override." {
		t.Errorf("expected override to win, got %q", v)
	}
}

func TestNotFoundError_Message(t *testing.T) {
	e := &NotFoundError{Language: configuration.LangENG, Key: "MissingKey"}
	got := e.Error()
	if !strings.Contains(got, "MissingKey") {
		t.Errorf("Error() should mention the key, got %q", got)
	}
	if !strings.Contains(got, "ENG") {
		t.Errorf("Error() should mention the language String(), got %q", got)
	}
}

func TestDefault_LoadsAllSevenCatalogs(t *testing.T) {
	tr := Default()
	if tr == nil {
		t.Fatal("Default() returned nil")
	}
	// Same key chosen because it exists in every built-in catalog (asserted by
	// TestCatalogs_KeySetsConsistent).
	for lang, name := range map[configuration.Language]string{
		configuration.LangPTBR: "PTBR",
		configuration.LangENG:  "ENG",
		configuration.LangES:   "ES",
		configuration.LangFR:   "FR",
		configuration.LangDE:   "DE",
		configuration.LangIT:   "IT",
		configuration.LangNL:   "NL",
	} {
		if !tr.Has(lang, "RecordNotFoundNotification") {
			t.Errorf("Default() missing the canonical key for %s", name)
		}
	}
}

func TestDefault_IsIdempotent(t *testing.T) {
	a := Default()
	b := Default()
	if a != b {
		t.Error("Default() should return the same instance on every call (sync.Once)")
	}
}

func TestCatalogs_LanguageMethodReturnsExpected(t *testing.T) {
	cases := []struct {
		mod  Module
		want configuration.Language
	}{
		{CorePTBR(), configuration.LangPTBR},
		{CoreENG(), configuration.LangENG},
		{CoreES(), configuration.LangES},
		{CoreFR(), configuration.LangFR},
		{CoreDE(), configuration.LangDE},
		{CoreIT(), configuration.LangIT},
		{CoreNL(), configuration.LangNL},
	}
	for _, tc := range cases {
		if got := tc.mod.Language(); got != tc.want {
			t.Errorf("module.Language() = %v, want %v", got, tc.want)
		}
	}
}

// stubModule covers the Module interface path that isn't a built-in catalog —
// the public Import contract has to accept arbitrary implementations.
type stubModule struct {
	lang    configuration.Language
	entries map[string]string
}

func (m stubModule) Language() configuration.Language { return m.lang }
func (m stubModule) Translations() map[string]string  { return m.entries }

// descSize is a stand-in enum value object: EnumDescription only needs its
// EnumDescriptionKey ("<Type>.<value>"), which is pure reflection over the value.
type descSize int

const descSmall descSize = 1

func TestTranslator_EnumDescription(t *testing.T) {
	tr := New()
	tr.Import(stubModule{lang: configuration.LangENG, entries: map[string]string{"descSize.1": "Small"}})

	if got := tr.EnumDescription(configuration.LangENG, descSmall); got != "Small" {
		t.Errorf("EnumDescription(descSmall) = %q, want Small", got)
	}
	// No catalog entry → falls back to the key itself.
	if got := tr.EnumDescription(configuration.LangENG, descSize(9)); got != "descSize.9" {
		t.Errorf("EnumDescription fallback = %q, want descSize.9", got)
	}
}
