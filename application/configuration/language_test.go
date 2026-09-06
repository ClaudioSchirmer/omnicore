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
		if got := int(tc.lang); got != tc.want {
			t.Errorf("int(%v) = %d, want %d", tc.lang, got, tc.want)
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

func TestLanguageValidateEnum(t *testing.T) {
	t.Run("a declared language passes", func(t *testing.T) {
		ctx := domain.NewNotificationContext("Lang")
		fx := &struct{ Language Language }{Language: LangPTBR}
		r := domain.NewRulesFor(domain.ModeInsert, ctx, fx)
		if !domain.ValidateEnum(&fx.Language, r) {
			t.Error("expected LangPTBR to be valid")
		}
		if ctx.HasErrors() {
			t.Errorf("expected no notifications recorded, got %+v", ctx.Messages())
		}
	})

	// The zero value (LangUnknown) is the sentinel — never a member — so
	// membership validation rejects it and emits the UnknownNotification.
	t.Run("LangUnknown fails and records notification", func(t *testing.T) {
		ctx := domain.NewNotificationContext("Lang")
		fx := &struct{ Language Language }{Language: LangUnknown}
		r := domain.NewRulesFor(domain.ModeInsert, ctx, fx)
		if domain.ValidateEnum(&fx.Language, r) {
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

	// ValidateEnum now enforces the CLOSED SET in the domain (membership against
	// Values()), so an out-of-range cast like Language(99) — not a declared
	// member — fails. (This deliberately reverses the previous zero-value-guard
	// contract; the design mirrors the ddd-kernel EnumValueObject.)
	t.Run("out-of-range value fails — membership is enforced", func(t *testing.T) {
		ctx := domain.NewNotificationContext("Lang")
		fx := &struct{ Language Language }{Language: Language(99)}
		r := domain.NewRulesFor(domain.ModeInsert, ctx, fx)
		if domain.ValidateEnum(&fx.Language, r) {
			t.Error("expected Language(99) to be invalid (outside the declared set)")
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
