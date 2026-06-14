package web

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// ─── Test fixtures ──────────────────────────────────────────────────────────

type emptyReq struct{}

type stringPathReq struct {
	Email string `path:"email"`
}

type multiSegmentReq struct {
	TenantID string `path:"tenantId"`
	UserID   string `path:"userId"`
}

type uuidPathReq struct {
	ID uuid.UUID `path:"id"`
}

type domainIDPathReq struct {
	ID domain.ID `path:"id"`
}

type scalarsReq struct {
	Page    int64   `path:"page"`
	Version uint16  `path:"version"`
	Rate    float64 `path:"rate"`
	Flag    bool    `path:"flag"`
}

type ptrPathReq struct {
	Email *string `path:"email"`
}

type slicePathReq struct {
	Tags []string `path:"tags"`
}

type pathPlusJSONReq struct {
	Email string `path:"email" json:"email"`
}

type dupSegmentReq struct {
	A string `path:"x"`
	B string `path:"x"`
}

// ─── §9.1 path-binding via the public BindPath helper ───────────────────────

func TestBindPath_NoTags_IsNoop(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	var captured string
	app.Get("/x/:id", func(c *fiber.Ctx) error {
		var req emptyReq
		bad, ok := BindPath(c, &req)
		if !ok {
			t.Fatalf("BindPath returned !ok with empty Request (badField=%q)", bad)
		}
		captured = "ran"
		return c.SendString("ok")
	})
	_, body := doRequest(t, app, "GET", "/x/anything")
	if body != "ok" || captured != "ran" {
		t.Fatalf("unexpected result body=%q captured=%q", body, captured)
	}
}

func TestBindPath_SingleSegmentString(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	var got string
	app.Get("/users/:email", func(c *fiber.Ctx) error {
		var req stringPathReq
		bad, ok := BindPath(c, &req)
		if !ok {
			t.Fatalf("BindPath unexpected !ok badField=%q", bad)
		}
		got = req.Email
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/users/jane@example.com")
	if got != "jane@example.com" {
		t.Fatalf("expected jane@example.com, got %q", got)
	}
}

func TestBindPath_MultiSegment(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	var tenant, user string
	app.Get("/tenants/:tenantId/users/:userId", func(c *fiber.Ctx) error {
		var req multiSegmentReq
		bad, ok := BindPath(c, &req)
		if !ok {
			t.Fatalf("BindPath !ok badField=%q", bad)
		}
		tenant = req.TenantID
		user = req.UserID
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/tenants/acme/users/u42")
	if tenant != "acme" || user != "u42" {
		t.Fatalf("got tenant=%q user=%q", tenant, user)
	}
}

func TestBindPath_UUIDAndDomainID(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	const sample = "550e8400-e29b-41d4-a716-446655440000"
	var gotUUID uuid.UUID
	var gotDomain domain.ID
	app.Get("/u/:id", func(c *fiber.Ctx) error {
		var r1 uuidPathReq
		if bad, ok := BindPath(c, &r1); !ok {
			t.Fatalf("uuid BindPath !ok badField=%q", bad)
		}
		gotUUID = r1.ID
		var r2 domainIDPathReq
		if bad, ok := BindPath(c, &r2); !ok {
			t.Fatalf("domain.ID BindPath !ok badField=%q", bad)
		}
		gotDomain = r2.ID
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/u/"+sample)
	if gotUUID.String() != sample {
		t.Fatalf("uuid mismatch: %s", gotUUID)
	}
	if gotDomain.Value() != sample {
		t.Fatalf("domain.ID mismatch: %s", gotDomain.Value())
	}
}

func TestBindPath_ScalarTypes(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	var captured scalarsReq
	app.Get("/x/:page/:version/:rate/:flag", func(c *fiber.Ctx) error {
		var req scalarsReq
		if bad, ok := BindPath(c, &req); !ok {
			t.Fatalf("BindPath !ok badField=%q", bad)
		}
		captured = req
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x/42/7/1.5/true")
	if captured.Page != 42 || captured.Version != 7 || captured.Rate != 1.5 || captured.Flag != true {
		t.Fatalf("got %+v", captured)
	}
}

// ─── §9.2 type-conversion errors ────────────────────────────────────────────

func TestBindPath_BadUUID_ReturnsBadField(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/u/:id", func(c *fiber.Ctx) error {
		var req uuidPathReq
		bad, ok := BindPath(c, &req)
		if ok {
			t.Fatalf("expected BindPath !ok for non-uuid segment, got ok with field=%q", bad)
		}
		if bad != "id" {
			t.Fatalf("expected badField=id, got %q", bad)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/u/not-a-uuid")
}

func TestBindPath_BadInt_ReturnsBadField(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/x/:page", func(c *fiber.Ctx) error {
		var req struct {
			Page int64 `path:"page"`
		}
		bad, ok := BindPath(c, &req)
		if ok {
			t.Fatalf("expected !ok; got bad=%q", bad)
		}
		if bad != "page" {
			t.Fatalf("badField mismatch: %q", bad)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x/abc")
}

func TestBindPath_BadBool_ReturnsBadField(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/x/:flag", func(c *fiber.Ctx) error {
		var req struct {
			Flag bool `path:"flag"`
		}
		bad, ok := BindPath(c, &req)
		if ok || bad != "flag" {
			t.Fatalf("expected !ok flag, got ok=%v bad=%q", ok, bad)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x/maybe")
}

// ─── §9.X boot panics ───────────────────────────────────────────────────────

func TestPointerPathField_PanicsAtInspect(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on pointer field with path: tag")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "pointer") {
			t.Fatalf("panic missing 'pointer' hint: %v", r)
		}
	}()
	inspectPathTags(reflect.TypeOf(ptrPathReq{}))
}

func TestSlicePathField_PanicsAtInspect(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on slice field with path: tag")
		}
		if msg, _ := r.(string); !strings.Contains(msg, "slice") {
			t.Fatalf("panic missing 'slice' hint: %v", r)
		}
	}()
	inspectPathTags(reflect.TypeOf(slicePathReq{}))
}

func TestPathPlusJSON_PanicsAtInspect(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on field with both path: and json: tags")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "path:") || !strings.Contains(msg, "json:") {
			t.Fatalf("panic missing dual-tag mention: %v", r)
		}
	}()
	inspectPathTags(reflect.TypeOf(pathPlusJSONReq{}))
}

func TestDuplicateSegment_PanicsAtInspect(t *testing.T) {
	resetPathSchemaCache()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on two fields binding the same segment")
		}
	}()
	inspectPathTags(reflect.TypeOf(dupSegmentReq{}))
}

// ─── §9.X cache reuse ───────────────────────────────────────────────────────

func TestPathSchemaCache_Reuses(t *testing.T) {
	resetPathSchemaCache()
	t1 := reflect.TypeOf(stringPathReq{})
	s1 := inspectPathTags(t1)
	s2 := inspectPathTags(t1)
	if s1 != s2 {
		t.Fatal("expected cached pointer to be reused")
	}
}

// ─── §9.10 FullBody skips path: fields ──────────────────────────────────────

type strictPathReq struct {
	TenantID string `path:"tenantId"`
	Name     string `json:"name"`
}

func TestReflectExpectedJSONKeys_SkipsPathTaggedFields(t *testing.T) {
	keys := reflectExpectedJSONKeys(reflect.TypeOf(strictPathReq{}))
	if len(keys) != 1 || keys[0] != "name" {
		t.Fatalf("expected only [name], got %v", keys)
	}
}

// ─── §9.X BindPath argument validation ──────────────────────────────────────

func TestBindPath_NilRequest_NoOp(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error {
		bad, ok := BindPath(c, nil)
		if !ok || bad != "" {
			t.Fatalf("nil should be no-op; got bad=%q ok=%v", bad, ok)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x")
}

func TestBindPath_NonPointer_Panics(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c *fiber.Ctx) error {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic when passing non-pointer to BindPath")
			}
		}()
		var req stringPathReq
		BindPath(c, req) //nolint:govet // intentional value passing
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x")
}

// ─── helpers ────────────────────────────────────────────────────────────────

// resetPathSchemaCache clears the module-level cache between tests so a
// previous inspection (e.g., the boot-panic ones) does not poison a later
// test. The cache is private to the package — tests reach in via the same
// sync.Map identity.
func resetPathSchemaCache() {
	pathSchemaCache = sync.Map{}
}

func doRequest(t *testing.T, app *fiber.App, method, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("http.NewRequest failed: %v", err)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}
