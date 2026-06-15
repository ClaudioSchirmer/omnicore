package bootstrap

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"github.com/gofiber/fiber/v3"
)

// silentDepsWithRegistry is silentDeps + a pre-allocated OpenAPIRegistry,
// mirroring what runWithConfig sets up after wire() returns a Wiring with
// OpenAPI != nil. Used by buildApp tests below.
func silentDepsWithRegistry() Deps {
	d := silentDeps()
	d.OpenAPIRegistry = openapi.NewRegistry()
	return d
}

func TestBuildApp_OpenAPIDisabled_NoSpecRoutes(t *testing.T) {
	app, err := buildApp(silentDeps(), Wiring{
		Features: []Feature{&writeOnlyFeature{}},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	for _, path := range []string{"/openapi.json", "/docs"} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatalf("Test %s: %v", path, err)
		}
		if resp.StatusCode != 404 {
			t.Fatalf("OpenAPI disabled: GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestBuildApp_OpenAPIEnabled_SpecRouteReturnsJSON(t *testing.T) {
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
	}
	app, err := buildApp(silentDepsWithRegistry(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/openapi.json", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/openapi.json: got %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get(fiber.HeaderContentType)
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	var spec map[string]any
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, string(body))
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("openapi version: got %v, want 3.1.0", spec["openapi"])
	}
}

func TestBuildApp_OpenAPIEnabled_DocsRouteReturnsHTML(t *testing.T) {
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "TestAPI", Version: "1"},
	}
	app, err := buildApp(silentDepsWithRegistry(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/docs", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/docs: got %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, "<title>TestAPI</title>") {
		t.Fatalf("title missing from /docs body")
	}
	if !strings.Contains(html, "/openapi.json") {
		t.Fatalf("/docs HTML must reference /openapi.json")
	}
}

func TestBuildApp_OpenAPIEnabled_HealthAppearsInSpec(t *testing.T) {
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(silentDepsWithRegistry(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/openapi.json", nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	var spec map[string]any
	if err := json.Unmarshal(body, &spec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	paths := spec["paths"].(map[string]any)
	if _, ok := paths["/health"]; !ok {
		keys := make([]string, 0, len(paths))
		for k := range paths {
			keys = append(keys, k)
		}
		t.Fatalf("/health must appear in the rendered spec; paths=%v", keys)
	}
}

// recordingFeature captures whether d.OpenAPIRegistry was non-nil at Mount
// time. The buildApp test downstream can assert the registry plumbed
// through correctly.
type recordingFeature struct {
	gotRegistry *openapi.Registry
}

func (f *recordingFeature) Mount(_ *fiber.App, d Deps) {
	f.gotRegistry = d.OpenAPIRegistry
}

func TestBuildApp_OpenAPIEnabled_RegistryReachesFeatures(t *testing.T) {
	rec := &recordingFeature{}
	deps := silentDepsWithRegistry()
	_, err := buildApp(deps, Wiring{
		Features: []Feature{rec},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if rec.gotRegistry == nil {
		t.Fatal("feature.Mount must see a non-nil d.OpenAPIRegistry when Wiring.OpenAPI != nil")
	}
	if rec.gotRegistry != deps.OpenAPIRegistry {
		t.Fatal("feature should receive the exact registry buildApp was given on deps")
	}
}

func TestBuildApp_OpenAPIDisabled_FeatureSeesNilRegistry(t *testing.T) {
	rec := &recordingFeature{}
	_, err := buildApp(silentDeps(), Wiring{Features: []Feature{rec}})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if rec.gotRegistry != nil {
		t.Fatal("feature.Mount must see nil d.OpenAPIRegistry when Wiring.OpenAPI is unset")
	}
}

func TestBuildApp_OpenAPI_CustomUIPathServesDocs(t *testing.T) {
	deps := silentDepsWithRegistry()
	deps.Config.OpenAPI.UIPath = "/swagger"
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(deps, wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/swagger", nil))
	if err != nil {
		t.Fatalf("Test /swagger: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/swagger: got %d, want 200", resp.StatusCode)
	}
	resp2, err := app.Test(httptest.NewRequest("GET", "/docs", nil))
	if err != nil {
		t.Fatalf("Test /docs: %v", err)
	}
	if resp2.StatusCode != 404 {
		t.Fatalf("/docs should not exist when uiPath is /swagger; got %d", resp2.StatusCode)
	}
}

func TestBuildApp_OpenAPI_RootRedirectDisabledByDefault(t *testing.T) {
	deps := silentDepsWithRegistry()
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(deps, wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Test /: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("GET / should be 404 by default; got %d", resp.StatusCode)
	}
}

func TestBuildApp_OpenAPI_RootRedirectEnabled(t *testing.T) {
	deps := silentDepsWithRegistry()
	deps.Config.OpenAPI.RootRedirect = true
	deps.Config.OpenAPI.UIPath = "/docs"
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(deps, wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Test /: %v", err)
	}
	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("GET / status: got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/docs" {
		t.Fatalf("Location: got %q, want %q", loc, "/docs")
	}
}

func TestBuildApp_OpenAPI_RootRedirectCustomUIPath(t *testing.T) {
	deps := silentDepsWithRegistry()
	deps.Config.OpenAPI.RootRedirect = true
	deps.Config.OpenAPI.UIPath = "/swagger"
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(deps, wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Test /: %v", err)
	}
	if resp.StatusCode != fiber.StatusFound {
		t.Fatalf("GET / status: got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/swagger" {
		t.Fatalf("Location: got %q, want %q", loc, "/swagger")
	}
}

// rootMountingFeature owns GET / — used to verify the root redirect
// does not stomp on it. Goes through openapi.MountRaw so the Phase-5
// route-registration scan does not flag the fixture.
type rootMountingFeature struct{}

func (rootMountingFeature) Mount(app *fiber.App, d Deps) {
	openapi.MountRaw(d.OpenAPIRegistry, app, fiber.MethodGet, "/",
		func(c fiber.Ctx) error { return c.SendString("custom root") },
		openapi.RawSpec{Summary: "test root fixture", Public: true})
}

func TestBuildApp_OpenAPI_RootRedirectSkipsWhenFeatureOwnsRoot(t *testing.T) {
	deps := silentDepsWithRegistry()
	deps.Config.OpenAPI.RootRedirect = true
	deps.Config.OpenAPI.UIPath = "/docs"
	wiring := Wiring{
		Features: []Feature{rootMountingFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(deps, wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Test /: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("feature should win on /; got status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "custom root" {
		t.Fatalf("body: got %q, want %q", string(body), "custom root")
	}
}

func TestBuildApp_OpenAPI_RootRedirectIgnoredWhenOpenAPIDisabled(t *testing.T) {
	deps := silentDeps()
	deps.Config.OpenAPI.RootRedirect = true
	deps.Config.OpenAPI.UIPath = "/docs"
	// No OpenAPI on wiring — redirect target would not exist, so the
	// framework skips registering it.
	app, err := buildApp(deps, Wiring{Features: []Feature{&writeOnlyFeature{}}})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	if err != nil {
		t.Fatalf("Test /: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("rootRedirect must be ignored without OpenAPI; got status %d", resp.StatusCode)
	}
}

func TestBuildApp_OpenAPI_LanguageSelectorDefaultOff(t *testing.T) {
	wiring := Wiring{
		Features:     []Feature{&writeOnlyFeature{}},
		Translations: []translation.Module{stubModule{lang: configuration.LangENG}},
		OpenAPI:      &openapi.Config{Title: "T", Version: "1"},
	}
	app, err := buildApp(silentDepsWithRegistry(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/docs", nil))
	if err != nil {
		t.Fatalf("Test /docs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if strings.Contains(html, "omnicore-lang-selector") {
		t.Fatalf("LanguageSelector default off → no selector; html has it: %s", html)
	}
}

func TestBuildApp_OpenAPI_LanguageSelectorAutoPopulatesFromTranslations(t *testing.T) {
	wiring := Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		Translations: []translation.Module{
			stubModule{lang: configuration.LangPTBR},
			stubModule{lang: configuration.LangENG},
			stubModule{lang: configuration.LangPTBR}, // dup — must dedup
			stubModule{lang: configuration.LangFR},
		},
		OpenAPI: &openapi.Config{Title: "T", Version: "1", LanguageSelector: true},
	}
	app, err := buildApp(silentDepsWithRegistry(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/docs", nil))
	if err != nil {
		t.Fatalf("Test /docs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	if !strings.Contains(html, "omnicore-lang-selector") {
		t.Fatalf("expected selector in /docs HTML; html=%s", html)
	}
	for _, want := range []string{
		`<option value="pt">PT_BR</option>`,
		`<option value="en">ENG</option>`,
		`<option value="fr">FR</option>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("expected %q in /docs HTML; html=%s", want, html)
		}
	}
	if strings.Count(html, `value="pt"`) != 1 {
		t.Fatalf("PT_BR must appear exactly once (dedup); html=%s", html)
	}

	// ENG must appear FIRST in the dropdown (framework default) so the
	// natural HTML <select> behavior makes English the selected option
	// even though PT_BR was declared first in Translations.
	idxEN := strings.Index(html, `value="en"`)
	idxPT := strings.Index(html, `value="pt"`)
	idxFR := strings.Index(html, `value="fr"`)
	if idxEN < 0 || idxPT < 0 || idxFR < 0 {
		t.Fatalf("not all options rendered; en=%d pt=%d fr=%d", idxEN, idxPT, idxFR)
	}
	if !(idxEN < idxPT && idxEN < idxFR) {
		t.Fatalf("ENG must come first: en=%d pt=%d fr=%d; html=%s", idxEN, idxPT, idxFR, html)
	}
}

func TestBuildApp_OpenAPI_LanguageSelectorExplicitOverrideWins(t *testing.T) {
	// Consumer-provided Languages override auto-discovery so a manual
	// Wire flow (no Translations slice, custom dropdown labels, …) keeps
	// full control.
	wiring := Wiring{
		Features:     []Feature{&writeOnlyFeature{}},
		Translations: []translation.Module{stubModule{lang: configuration.LangENG}},
		OpenAPI: &openapi.Config{
			Title:            "T",
			Version:          "1",
			LanguageSelector: true,
			Languages: []openapi.LanguageOption{
				{Label: "Custom-A", Value: "ca"},
				{Label: "Custom-B", Value: "cb"},
			},
		},
	}
	app, err := buildApp(silentDepsWithRegistry(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/docs", nil))
	if err != nil {
		t.Fatalf("Test /docs: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	if !strings.Contains(html, `<option value="ca">Custom-A</option>`) {
		t.Fatalf("explicit option Custom-A missing; html=%s", html)
	}
	if strings.Contains(html, `value="en"`) {
		t.Fatalf("explicit Languages must replace auto-discovery; html=%s", html)
	}
}
