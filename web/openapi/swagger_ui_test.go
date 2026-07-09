package openapi

import (
	"strings"
	"testing"
)

func TestSwaggerUIHTML_DeclaresInlineIconLinks(t *testing.T) {
	html := SwaggerUIHTML("API", SpecPath, nil)

	if !strings.Contains(html, `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,`) {
		t.Fatalf("favicon link missing or malformed; html=%s", html)
	}
	if !strings.Contains(html, `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'>`) {
		t.Fatalf("favicon SVG payload missing; html=%s", html)
	}
	if !strings.Contains(html, `<link rel="apple-touch-icon" href="data:,">`) {
		t.Fatalf("apple-touch-icon link missing; html=%s", html)
	}
	if !strings.Contains(html, `<link rel="apple-touch-icon-precomposed" href="data:,">`) {
		t.Fatalf("apple-touch-icon-precomposed link missing; html=%s", html)
	}
}

func TestSwaggerUIHTML_NoLanguagesOmitsSelector(t *testing.T) {
	html := SwaggerUIHTML("API", SpecPath, nil)
	if strings.Contains(html, "omnicore-lang-selector") {
		t.Fatalf("nil languages must NOT render the selector; html=%s", html)
	}
	if strings.Contains(html, "requestInterceptor") {
		t.Fatalf("nil languages must NOT inject the requestInterceptor; html=%s", html)
	}
	if strings.Contains(html, "omnicore-lang-bar") {
		t.Fatalf("nil languages must NOT render the language bar; html=%s", html)
	}
}

func TestSwaggerUIHTML_EmptyLanguagesOmitsSelector(t *testing.T) {
	html := SwaggerUIHTML("API", SpecPath, []LanguageOption{})
	if strings.Contains(html, "omnicore-lang-selector") {
		t.Fatalf("empty languages must NOT render the selector; html=%s", html)
	}
}

func TestSwaggerUIHTML_LanguagesRenderSelector(t *testing.T) {
	langs := []LanguageOption{
		{Label: "PT_BR", Value: "pt"},
		{Label: "ENG", Value: "en"},
		{Label: "ES", Value: "es"},
		{Label: "FR", Value: "fr"},
	}
	html := SwaggerUIHTML("API", SpecPath, langs)

	if !strings.Contains(html, `<select id="omnicore-lang-selector">`) {
		t.Fatalf("selector element missing; html=%s", html)
	}
	if !strings.Contains(html, "requestInterceptor") {
		t.Fatalf("requestInterceptor missing; html=%s", html)
	}
	if !strings.Contains(html, `request.headers['Accept-Language']`) {
		t.Fatalf("Accept-Language injection missing; html=%s", html)
	}

	for _, l := range langs {
		opt := `<option value="` + l.Value + `">` + l.Label + `</option>`
		if !strings.Contains(html, opt) {
			t.Fatalf("expected option %q in html; html=%s", opt, html)
		}
	}
}

func TestSwaggerUIHTML_LanguageOrderPreserved(t *testing.T) {
	langs := []LanguageOption{
		{Label: "FR", Value: "fr"},
		{Label: "PT_BR", Value: "pt"},
		{Label: "ENG", Value: "en"},
	}
	html := SwaggerUIHTML("API", SpecPath, langs)

	idxFR := strings.Index(html, `value="fr"`)
	idxPT := strings.Index(html, `value="pt"`)
	idxEN := strings.Index(html, `value="en"`)
	if idxFR < 0 || idxPT < 0 || idxEN < 0 {
		t.Fatalf("not all options rendered; fr=%d pt=%d en=%d", idxFR, idxPT, idxEN)
	}
	if idxFR >= idxPT || idxPT >= idxEN {
		t.Fatalf("declaration order not preserved: fr=%d pt=%d en=%d", idxFR, idxPT, idxEN)
	}
}

func TestSwaggerUIHTML_LanguageOptionsEscaped(t *testing.T) {
	langs := []LanguageOption{
		{Label: `<script>alert('lbl')</script>`, Value: `"><script>alert('val')</script>`},
	}
	html := SwaggerUIHTML("API", SpecPath, langs)

	if strings.Contains(html, `<script>alert('lbl')</script>`) {
		t.Fatalf("label not escaped; html=%s", html)
	}
	if strings.Contains(html, `"><script>alert('val')</script>`) {
		t.Fatalf("value not escaped; html=%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;alert(") {
		t.Fatalf("expected escaped script tag in html=%s", html)
	}
}
