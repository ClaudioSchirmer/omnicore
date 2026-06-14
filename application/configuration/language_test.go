package configuration

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestAllLanguages(t *testing.T) {
	langs := AllLanguages()
	want := []Language{LangPTBR, LangENG, LangES, LangFR, LangDE, LangIT, LangNL}
	if len(langs) != len(want) {
		t.Fatalf("AllLanguages() len = %d, want %d", len(langs), len(want))
	}
	for i, l := range want {
		if langs[i] != l {
			t.Fatalf("AllLanguages()[%d] = %v, want %v", i, langs[i], l)
		}
	}
}

func TestHTTPPrefix(t *testing.T) {
	cases := []struct {
		lang Language
		want string
	}{
		{LangPTBR, "pt"},
		{LangENG, "en"},
		{LangES, "es"},
		{LangFR, "fr"},
		{LangDE, "de"},
		{LangIT, "it"},
		{LangNL, "nl"},
		{LangUnknown, ""},
	}
	for _, tc := range cases {
		if got := tc.lang.HTTPPrefix(); got != tc.want {
			t.Fatalf("%v.HTTPPrefix() = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

func TestString(t *testing.T) {
	cases := []struct {
		lang Language
		want string
	}{
		{LangPTBR, "PT_BR"},
		{LangENG, "ENG"},
		{LangES, "ES"},
		{LangFR, "FR"},
		{LangDE, "DE"},
		{LangIT, "IT"},
		{LangNL, "NL"},
		{LangUnknown, "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.lang.String(); got != tc.want {
			t.Fatalf("%v.String() = %q, want %q", tc.lang, got, tc.want)
		}
	}
}

func TestLanguageValue(t *testing.T) {
	cases := []struct {
		lang Language
		want int
	}{
		{LangUnknown, 0},
		{LangPTBR, 1},
		{LangENG, 2},
		{LangES, 3},
		{LangFR, 4},
		{LangDE, 5},
		{LangIT, 6},
		{LangNL, 7},
	}
	for _, tc := range cases {
		if got := tc.lang.Value(); got != tc.want {
			t.Errorf("%v.Value() = %d, want %d", tc.lang, got, tc.want)
		}
	}
}

func TestLanguageUnknownNotification(t *testing.T) {
	n := LangUnknown.UnknownNotification()
	if n == nil {
		t.Fatal("expected non-nil notification")
	}
	if key := domain.NotificationKey(n); key != "InvalidLanguageDomainNotification" {
		t.Errorf("NotificationKey = %q, want InvalidLanguageDomainNotification", key)
	}

	// Independent of receiver — same notification for any value.
	if got := LangPTBR.UnknownNotification(); got == nil {
		t.Error("expected non-nil notification from non-Unknown receiver")
	}
}

func TestLanguageIsValid(t *testing.T) {
	t.Run("non-zero language passes", func(t *testing.T) {
		ctx := domain.NewNotificationContext("Lang")
		if !LangPTBR.IsValid("Language", ctx) {
			t.Error("expected LangPTBR to be valid")
		}
		if ctx.HasErrors() {
			t.Errorf("expected no notifications recorded, got %+v", ctx.Messages())
		}
	})

	// Zero value (LangUnknown) is the sentinel ValidateEnum rejects — emits
	// the UnknownNotification on the supplied ctx.
	t.Run("LangUnknown fails and records notification", func(t *testing.T) {
		ctx := domain.NewNotificationContext("Lang")
		if LangUnknown.IsValid("Language", ctx) {
			t.Error("expected LangUnknown to be invalid")
		}
		if !ctx.HasErrors() {
			t.Error("expected at least one notification recorded for LangUnknown")
		}
		msgs := ctx.Messages()
		if len(msgs) == 0 || domain.NotificationKey(msgs[0].Notification) != "InvalidLanguageDomainNotification" {
			t.Errorf("expected InvalidLanguageDomainNotification, got %+v", msgs)
		}
	})

	// Pins the deliberate contract of ValidateEnum (see godoc in
	// omnicore/domain/value_object.go): the function is a zero-value guard,
	// not a range check. An out-of-range non-zero value like Language(99)
	// passes. Closed-set enforcement lives at the wire boundary
	// (translator / middleware / Request DTO), not inside this helper.
	t.Run("out-of-range value passes by design — ValidateEnum is a zero-value guard", func(t *testing.T) {
		ctx := domain.NewNotificationContext("Lang")
		if !Language(99).IsValid("Language", ctx) {
			t.Error("ValidateEnum semantic changed — update the godoc AND this pin together")
		}
	})
}

func TestHTTPPrefixesUnique(t *testing.T) {
	seen := map[string]Language{}
	for _, lang := range AllLanguages() {
		p := lang.HTTPPrefix()
		if p == "" {
			t.Fatalf("%v.HTTPPrefix() is empty — every known language must declare one", lang)
		}
		if other, dup := seen[p]; dup {
			t.Fatalf("HTTPPrefix %q collides between %v and %v", p, other, lang)
		}
		seen[p] = lang
	}
}
