package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
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

func (f *readableFeature) Views() []*query.ViewDefinition {
	out := make([]*query.ViewDefinition, 0, len(f.viewNames))
	for _, n := range f.viewNames {
		out = append(out, query.View(n))
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
	err := validateWiring(Wiring{}, false)
	if err == nil {
		t.Fatalf("expected error for empty wiring")
	}
	if !strings.Contains(err.Error(), "nothing to serve") {
		t.Fatalf("error should mention 'nothing to serve'; got: %v", err)
	}
}

func TestValidateWiring_DevAcceptsEmptyShell(t *testing.T) {
	// The dev-only empty shell: no Features, no BeforeServe, not even
	// Translations — the state of a freshly scaffolded service. Dev boots
	// it (framework surfaces only); every other profile rejects it.
	if err := validateWiring(Wiring{}, true); err != nil {
		t.Fatalf("dev must accept the empty shell; got: %v", err)
	}
}

func TestValidateWiring_DevStillRequiresTranslationsWithFeatures(t *testing.T) {
	// The empty-shell waiver covers ONLY the fully empty wiring — a dev
	// wiring WITH a feature and no translations keeps the guard-2 rejection.
	w := Wiring{Features: []Feature{&writeOnlyFeature{}}}
	err := validateWiring(w, true)
	if err == nil {
		t.Fatalf("expected error for dev wiring with a feature and no Translations")
	}
	if !strings.Contains(err.Error(), "no Translations") {
		t.Fatalf("error should mention 'no Translations'; got: %v", err)
	}
}

func TestValidateWiring_NoTranslationsRejected(t *testing.T) {
	// Has a Feature (clears the "nothing to serve" guard) but no
	// translation module — the second guard rejects it because the
	// whole stack consumes the Translator.
	w := Wiring{Features: []Feature{&writeOnlyFeature{}}}
	err := validateWiring(w, false)
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
	if err := validateWiring(w, false); err != nil {
		t.Fatalf("wiring with feature + translation should pass; got: %v", err)
	}
}

func TestValidateWiring_BeforeServePresent(t *testing.T) {
	w := Wiring{
		BeforeServe:  func(*fiber.App, Deps) error { return nil },
		Translations: []translation.Module{translation.CoreENG()},
	}
	if err := validateWiring(w, false); err != nil {
		t.Fatalf("wiring with BeforeServe + translation should pass; got: %v", err)
	}
}

// --- buildApp ---

func TestBuildApp_LivezAlwaysRegistered(t *testing.T) {
	app, err := buildApp(context.Background(), silentDeps(), Wiring{Features: []Feature{&writeOnlyFeature{}}})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/livez", nil))
	if err != nil {
		t.Fatalf("Test /livez: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/livez = %d, want 200", resp.StatusCode)
	}
}

func TestBuildApp_LivezEvenWithoutFeatures(t *testing.T) {
	// buildApp itself does not require Features (validateWiring is the upstream guard);
	// /livez should respond anyway, so the k8s liveness probe works before
	// the wire completes.
	app, err := buildApp(context.Background(), silentDeps(), Wiring{})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	resp, err := app.Test(httptest.NewRequest("GET", "/livez", nil))
	if err != nil {
		t.Fatalf("Test /livez: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/livez without features = %d, want 200", resp.StatusCode)
	}
}

func TestBuildApp_MountsFeaturesInDeclarationOrder(t *testing.T) {
	var order []string
	wiring := Wiring{Features: []Feature{
		&orderRecorder{id: "first", order: &order},
		&orderRecorder{id: "second", order: &order},
		&orderRecorder{id: "third", order: &order},
	}}
	if _, err := buildApp(context.Background(), silentDeps(), wiring); err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	if len(order) != 3 || order[0] != "first" || order[1] != "second" || order[2] != "third" {
		t.Fatalf("mount order = %v, want [first second third]", order)
	}
}

func TestBuildApp_FeatureRoutesReachable(t *testing.T) {
	wiring := Wiring{Features: []Feature{&writeOnlyFeature{}, &readableFeature{viewNames: []string{"u"}}}}
	app, err := buildApp(context.Background(), silentDeps(), wiring)
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
	if _, err := buildApp(context.Background(), silentDeps(), wiring); err == nil || !errors.Is(err, wantErr) {
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

// The drain must NARRATE itself: operators watching a slow shutdown need to
// see which component is being stopped and when it finished, not just the two
// bookend lines. Each stage that runs through the coordinated drain (and the
// sequential tracing / OnShutdown steps) emits a "draining" line on entry and
// a "drained" line with its elapsed time on success. All drain goroutines are
// joined before serve returns, so reading the buffer afterwards is race-free.
func TestServe_LogsDrainStages(t *testing.T) {
	var buf bytes.Buffer
	cfg := &Config{Service: "test"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	deps := silentDeps()
	deps.Config = cfg
	deps.Logger = slog.New(slog.NewJSONHandler(&buf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	onShutdownCalled := false
	wiring := Wiring{
		Features:   []Feature{&writeOnlyFeature{}},
		OnShutdown: func(context.Context) error { onShutdownCalled = true; return nil },
	}
	if err := serve(ctx, deps, wiring); err != nil {
		t.Fatalf("serve should return nil after ctx cancel; got: %v", err)
	}
	if !onShutdownCalled {
		t.Fatal("OnShutdown hook was not invoked")
	}

	out := buf.String()
	// The http + sync stages always run; tracing + onShutdown run sequentially
	// after the parallel drain. Each must show both a "draining" and a
	// "drained" line, bookended by the shutdown-received / -complete summaries.
	want := []string{
		`"msg":"shutdown signal received, draining..."`,
		`"msg":"draining","stage":"http"`,
		`"msg":"drained","stage":"http"`,
		`"msg":"draining","stage":"sync"`,
		`"msg":"drained","stage":"sync"`,
		`"msg":"draining","stage":"tracing"`,
		`"msg":"drained","stage":"tracing"`,
		`"msg":"draining","stage":"onShutdown"`,
		`"msg":"drained","stage":"onShutdown"`,
		`"msg":"shutdown complete"`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("drain log missing %s\n--- full output ---\n%s", w, out)
		}
	}
}
