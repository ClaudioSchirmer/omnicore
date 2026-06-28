package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/db/read/mongo"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"github.com/gofiber/fiber/v3"
)

// --- fixtures ---

type writeOnlyFeature struct{ mounted bool }

func (f *writeOnlyFeature) Mount(app *fiber.App, d Deps) {
	f.mounted = true
	// Routes a test fixture mounts go through openapi.MountRaw so the
	// Phase-5 scan does not flag them. Public:true short-circuits the
	// authz scan when it is also active.
	openapi.MountRaw(d.OpenAPIRegistry, app, fiber.MethodGet, "/write-only",
		func(c fiber.Ctx) error { return c.SendString("wo") },
		openapi.RawSpec{Summary: "test fixture", Public: true})
}

type readableFeature struct {
	viewNames []string
	mounted   bool
}

func (f *readableFeature) Mount(app *fiber.App, d Deps) {
	f.mounted = true
	openapi.MountRaw(d.OpenAPIRegistry, app, fiber.MethodGet, "/readable",
		func(c fiber.Ctx) error { return c.SendString("rd") },
		openapi.RawSpec{Summary: "test fixture", Public: true})
}

func (f *readableFeature) Views() []*mongo.ViewDefinition {
	out := make([]*mongo.ViewDefinition, 0, len(f.viewNames))
	for _, n := range f.viewNames {
		out = append(out, mongo.View(n).Root(n))
	}
	return out
}

type orderRecorder struct {
	id    string
	order *[]string
}

func (o *orderRecorder) Mount(_ *fiber.App, _ Deps) {
	*o.order = append(*o.order, o.id)
}

// silentDeps returns Deps with Config + Logger discarded — sufficient
// for buildApp (does not touch Postgres/Mongo). Pipeline is wired with the
// default Translator so the ErrorHandler can translate notifications when
// tests hit the 404 / panic paths.
func silentDeps() Deps {
	return Deps{
		Config:   &Config{Service: "test-service"},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Pipeline: pipeline.New(translation.Default()),
	}
}

// --- collectViews ---

func TestCollectViews_WriteOnlySkipped(t *testing.T) {
	views, err := collectViews([]Feature{&writeOnlyFeature{}})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("write-only feature should contribute zero views, got %d", len(views))
	}
}

func TestCollectViews_AggregatesAcrossFeatures(t *testing.T) {
	views, err := collectViews([]Feature{
		&readableFeature{viewNames: []string{"users"}},
		&writeOnlyFeature{},
		&readableFeature{viewNames: []string{"orders", "orders_summary"}},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(views) != 3 {
		t.Fatalf("want 3 views (users, orders, orders_summary), got %d", len(views))
	}
	got := []string{views[0].Name(), views[1].Name(), views[2].Name()}
	want := []string{"users", "orders", "orders_summary"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("views[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCollectViews_CollisionRejected(t *testing.T) {
	_, err := collectViews([]Feature{
		&readableFeature{viewNames: []string{"users"}},
		&readableFeature{viewNames: []string{"users"}},
	})
	if err == nil {
		t.Fatalf("expected collision error")
	}
	if !strings.Contains(err.Error(), "view name collision") || !strings.Contains(err.Error(), `"users"`) {
		t.Fatalf("error message should identify the collision; got: %v", err)
	}
}

// --- validateWiring ---

func TestValidateWiring_NothingToServeRejected(t *testing.T) {
	err := validateWiring(Wiring{})
	if err == nil {
		t.Fatalf("expected error for empty wiring")
	}
	if !strings.Contains(err.Error(), "nothing to serve") {
		t.Fatalf("error should mention 'nothing to serve'; got: %v", err)
	}
}

func TestValidateWiring_NoTranslationsRejected(t *testing.T) {
	// Has a Feature (clears the "nothing to serve" guard) but no
	// translation module — the second guard rejects it because the
	// whole stack consumes the Translator.
	w := Wiring{Features: []Feature{&writeOnlyFeature{}}}
	err := validateWiring(w)
	if err == nil {
		t.Fatalf("expected error for wiring with no Translations")
	}
	if !strings.Contains(err.Error(), "no Translations") {
		t.Fatalf("error should mention 'no Translations'; got: %v", err)
	}
}

func TestValidateWiring_FeaturePresent(t *testing.T) {
	w := Wiring{
		Features:     []Feature{&writeOnlyFeature{}},
		Translations: []translation.Module{translation.CoreENG()},
	}
	if err := validateWiring(w); err != nil {
		t.Fatalf("wiring with feature + translation should pass; got: %v", err)
	}
}

func TestValidateWiring_BeforeServePresent(t *testing.T) {
	w := Wiring{
		BeforeServe:  func(*fiber.App, Deps) error { return nil },
		Translations: []translation.Module{translation.CoreENG()},
	}
	if err := validateWiring(w); err != nil {
		t.Fatalf("wiring with BeforeServe + translation should pass; got: %v", err)
	}
}

// --- buildApp ---

func TestBuildApp_HealthAlwaysRegistered(t *testing.T) {
	app, err := buildApp(silentDeps(), Wiring{Features: []Feature{&writeOnlyFeature{}}})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("Test /health: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/health = %d, want 200", resp.StatusCode)
	}
}

func TestBuildApp_HealthEvenWithoutFeatures(t *testing.T) {
	// buildApp itself does not require Features (validateWiring is the upstream guard);
	// /health should respond anyway, so the k8s probe works before
	// the wire completes.
	app, err := buildApp(silentDeps(), Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/health", nil))
	if err != nil {
		t.Fatalf("Test /health: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/health without features = %d, want 200", resp.StatusCode)
	}
}

func TestBuildApp_MountsFeaturesInDeclarationOrder(t *testing.T) {
	var order []string
	wiring := Wiring{Features: []Feature{
		&orderRecorder{id: "first", order: &order},
		&orderRecorder{id: "second", order: &order},
		&orderRecorder{id: "third", order: &order},
	}}
	if _, err := buildApp(silentDeps(), wiring); err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("mount order = %v, want [first second third]", order)
	}
}

func TestBuildApp_FeatureRoutesReachable(t *testing.T) {
	wiring := Wiring{Features: []Feature{&writeOnlyFeature{}, &readableFeature{viewNames: []string{"u"}}}}
	app, err := buildApp(silentDeps(), wiring)
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	for path, want := range map[string]int{
		"/write-only": 200,
		"/readable":   200,
		"/missing":    404,
	} {
		resp, err := app.Test(httptest.NewRequest("GET", path, nil))
		if err != nil {
			t.Fatalf("Test %s: %v", path, err)
		}
		if resp.StatusCode != want {
			t.Fatalf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestBuildApp_BeforeServeError(t *testing.T) {
	wantErr := errors.New("boom")
	wiring := Wiring{
		Features:    []Feature{&writeOnlyFeature{}},
		BeforeServe: func(*fiber.App, Deps) error { return wantErr },
	}
	if _, err := buildApp(silentDeps(), wiring); err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("buildApp should wrap BeforeServe error; got: %v", err)
	}
}

// --- Serve (integration: drains on ctx cancel) ---

func TestServe_DrainsOnContextCancel(t *testing.T) {
	// Listen on :0 lets the OS pick a free port; we only need to verify
	// that serve honors ctx cancel and terminates without error.
	cfg := &Config{Service: "test"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	deps := silentDeps()
	deps.Config = cfg
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: serve should drain immediately.
	err := serve(ctx, deps, Wiring{Features: []Feature{&writeOnlyFeature{}}})
	if err != nil {
		t.Fatalf("serve should return nil after ctx cancel; got: %v", err)
	}
}
