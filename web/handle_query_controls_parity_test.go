package web

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// violationField reads the field name off the canonical 400 envelope.
func violationField(t *testing.T, body []byte) string {
	t.Helper()
	var parsed struct {
		Errors []struct {
			Messages []struct {
				Field           string `json:"field"`
				NotificationKey string `json:"notificationKey"`
			} `json:"messages"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	if len(parsed.Errors) == 0 || len(parsed.Errors[0].Messages) == 0 {
		t.Fatalf("expected a violation on the envelope, got %s", body)
	}
	return parsed.Errors[0].Messages[0].Field
}

// TestControlBool_ListAndByIDAgreeOnEverySpelling — the two REST read routes
// share one Request-DTO vocabulary, so they must answer the SAME way to the
// same control value. They did not: the paged wrapper compared strings while
// the by-id wrapper delegated to Fiber's binder, so `?includeArchived=1` was
// false on a listing and true on a by-id read of the same entity.
//
// The empty value is part of the contract, not an edge case: PRESENCE is the
// key being on the query string, so `?includeArchived=` is the control asked
// for with no answer and both routes refuse it. Reading it as "absent" on one
// side would rebuild the same disagreement on a different spelling.
func TestControlBool_ListAndByIDAgreeOnEverySpelling(t *testing.T) {
	app := fiber.New()
	pipe := newTestPipeline()
	app.Get("/users/:id", QueryByID(pipe, testFindIDRequest{}, rawItem, &capturingIDHandler{}))
	app.Get("/users", QueryWithParams(pipe, testFindParamsRequest{}, rawItem, &capturingParamsHandler{}))

	for _, tc := range []struct {
		value string
		want  int
	}{
		{"true", fiber.StatusOK},
		{"false", fiber.StatusOK},
		{"1", fiber.StatusBadRequest},
		{"t", fiber.StatusBadRequest},
		{"TRUE", fiber.StatusBadRequest},
		{"", fiber.StatusBadRequest},
	} {
		q := "?includeArchived=" + tc.value
		for path, label := range map[string]string{"/users": "listing", "/users/abc": "by-id"} {
			resp, err := app.Test(httptest.NewRequest("GET", path+q, nil))
			if err != nil {
				t.Fatalf("%s %s: %v", label, q, err)
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Errorf("%s %q → %d, want %d (body=%s)", label, q, resp.StatusCode, tc.want, body)
				continue
			}
			if tc.want == fiber.StatusBadRequest {
				if got := violationField(t, body); got != "includeArchived" {
					t.Errorf("%s %q must report the control, got %q", label, q, got)
				}
			}
		}
	}
}

// `?onlyTotal=` is the listing's own boolean control and takes the same cut.
func TestControlBool_OnlyTotalRefusesNonCanonicalSpellings(t *testing.T) {
	app := fiber.New()
	app.Get("/users", QueryWithParams(newTestPipeline(), testFindParamsRequest{}, rawItem, &capturingParamsHandler{}))

	for _, value := range []string{"1", "TRUE", ""} {
		resp, _ := app.Test(httptest.NewRequest("GET", "/users?onlyTotal="+value, nil))
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Errorf("?onlyTotal=%q → %d, want 400 (body=%s)", value, resp.StatusCode, body)
			continue
		}
		if got := violationField(t, body); got != "onlyTotal" {
			t.Errorf("?onlyTotal=%q must report the control, got %q", value, got)
		}
	}
}

// TestOrderBy_RepeatedPathIsRefused — the ordering terms become the reader's
// sort document, where a duplicated key is malformed and the store refuses the
// whole read. The refusal names the SECOND occurrence: that is the token the
// consumer has to remove, and the `-` prefix rides along verbatim.
func TestOrderBy_RepeatedPathIsRefused(t *testing.T) {
	app := fiber.New()
	app.Get("/users", QueryWithParams(newTestPipeline(), testFindParamsRequest{}, rawItem, &capturingParamsHandler{}))

	for _, tc := range []struct{ query, wantField string }{
		{"name,name", "orderBy[name]"},
		{"name,-name", "orderBy[-name]"},
		{"name,email,-name", "orderBy[-name]"},
	} {
		resp, _ := app.Test(httptest.NewRequest("GET", "/users?orderBy="+tc.query, nil))
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Errorf("?orderBy=%s → %d, want 400 (body=%s)", tc.query, resp.StatusCode, body)
			continue
		}
		if got := violationField(t, body); got != tc.wantField {
			t.Errorf("?orderBy=%s must report %q, got %q", tc.query, tc.wantField, got)
		}
	}

	// Distinct paths in the same ordering stay legal — the rule is about a
	// repeated KEY, not about multi-key sorting.
	resp, _ := app.Test(httptest.NewRequest("GET", "/users?orderBy=name,-email", nil))
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("a multi-key ordering over distinct paths must pass, got %d (%s)", resp.StatusCode, body)
	}
}
