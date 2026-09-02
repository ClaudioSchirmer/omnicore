package web

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/gofiber/fiber/v3"
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
	app.Get("/x/:id", func(c fiber.Ctx) error {
		var req emptyReq
		v := BindPath(c, &req)
		if v != nil {
			t.Fatalf("BindPath returned !ok with empty Request (field=%q)", v.Field)
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
	app.Get("/users/:email", func(c fiber.Ctx) error {
		var req stringPathReq
		v := BindPath(c, &req)
		if v != nil {
			t.Fatalf("BindPath unexpected !ok field=%q", v.Field)
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
	app.Get("/tenants/:tenantId/users/:userId", func(c fiber.Ctx) error {
		var req multiSegmentReq
		v := BindPath(c, &req)
		if v != nil {
			t.Fatalf("BindPath !ok field=%q", v.Field)
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
	app.Get("/u/:id", func(c fiber.Ctx) error {
		var r1 uuidPathReq
		if v := BindPath(c, &r1); v != nil {
			t.Fatalf("uuid BindPath !ok field=%q", v.Field)
		}
		gotUUID = r1.ID
		var r2 domainIDPathReq
		if v := BindPath(c, &r2); v != nil {
			t.Fatalf("domain.ID BindPath !ok field=%q", v.Field)
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
	app.Get("/x/:page/:version/:rate/:flag", func(c fiber.Ctx) error {
		var req scalarsReq
		if v := BindPath(c, &req); v != nil {
			t.Fatalf("BindPath !ok field=%q", v.Field)
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
	app.Get("/u/:id", func(c fiber.Ctx) error {
		var req uuidPathReq
		v := BindPath(c, &req)
		if v == nil {
			t.Fatal("expected a violation for a non-uuid segment, got nil")
		}
		if v.Field != "id" {
			t.Fatalf("expected field=id, got %q", v.Field)
		}
		// An identity segment on a READ names no record: the same 404 the
		// by-id wrappers answer, not the generic schema violation.
		if _, isUnknown := v.Notification.(domain.UnknownIDAddressNotification); !isUnknown {
			t.Fatalf("expected UnknownIDAddressNotification on a read, got %T", v.Notification)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/u/not-a-uuid")
}

func TestBindPath_BadInt_ReturnsBadField(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/x/:page", func(c fiber.Ctx) error {
		var req struct {
			Page int64 `path:"page"`
		}
		v := BindPath(c, &req)
		if v == nil {
			t.Fatal("expected a violation for a non-numeric segment, got nil")
		}
		if v.Field != "page" {
			t.Fatalf("field mismatch: %q", v.Field)
		}
		// Not an identity segment: an int that is not a number stays the
		// canonical schema violation, which a nil Notification spells.
		if v.Notification != nil {
			t.Fatalf("expected the canonical schema violation, got %T", v.Notification)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x/abc")
}

func TestBindPath_BadBool_ReturnsBadField(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Get("/x/:flag", func(c fiber.Ctx) error {
		var req struct {
			Flag bool `path:"flag"`
		}
		v := BindPath(c, &req)
		if v == nil || v.Field != "flag" {
			t.Fatalf("expected a violation on flag, got %+v", v)
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
	app.Get("/x", func(c fiber.Ctx) error {
		if v := BindPath(c, nil); v != nil {
			t.Fatalf("nil should be a no-op; got %+v", v)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "GET", "/x")
}

func TestBindPath_NonPointer_Panics(t *testing.T) {
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error {
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
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// TestBindPath_BadUUIDOnWrite_IsMalformedID is the write-side twin of
// TestBindPath_BadUUID_ReturnsBadField: the SAME malformed segment, refused as
// a request-shape violation because the caller stated an intention about a
// record instead of asking for one.
func TestBindPath_BadUUIDOnWrite_IsMalformedID(t *testing.T) {
	resetPathSchemaCache()
	app := fiber.New()
	app.Patch("/u/:id", func(c fiber.Ctx) error {
		var req uuidPathReq
		v := BindPath(c, &req)
		if v == nil {
			t.Fatal("expected a violation for a non-uuid segment, got nil")
		}
		if _, isMalformed := v.Notification.(domain.MalformedIDNotification); !isMalformed {
			t.Fatalf("expected MalformedIDNotification on a write, got %T", v.Notification)
		}
		return c.SendString("ok")
	})
	doRequest(t, app, "PATCH", "/u/not-a-uuid")
}
