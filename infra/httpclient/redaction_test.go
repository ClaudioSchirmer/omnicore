package httpclient

import (
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

func TestRedactHeaders_DefaultBlockList(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer abc")
	h.Set("Proxy-Authorization", "Basic xyz")
	h.Set("Cookie", "sid=1")
	h.Set("Set-Cookie", "sid=1; Path=/")
	h.Set("X-API-Key", "secret")
	h.Set("X-Tenant", "acme")
	h.Set("User-Agent", "svc/1.0")
	out := redactHeaders(h, nil)
	for _, k := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key"} {
		if v := out.Get(k); v != redactedPlaceholder {
			t.Errorf("header %q should be redacted; got %q", k, v)
		}
	}
	if out.Get("X-Tenant") != "acme" {
		t.Errorf("X-Tenant should not be redacted; got %q", out.Get("X-Tenant"))
	}
	if out.Get("User-Agent") != "svc/1.0" {
		t.Errorf("User-Agent should not be redacted; got %q", out.Get("User-Agent"))
	}
}

func TestRedactHeaders_CaseInsensitiveLookup(t *testing.T) {
	h := http.Header{}
	h["authorization"] = []string{"Bearer abc"}
	out := redactHeaders(h, nil)
	if got := out["authorization"]; len(got) != 1 || got[0] != redactedPlaceholder {
		t.Errorf("lower-case authorization should be redacted; got %v", got)
	}
}

func TestRedactHeaders_PreservesMultiValue(t *testing.T) {
	h := http.Header{}
	h.Add("X-Trace", "a")
	h.Add("X-Trace", "b")
	out := redactHeaders(h, nil)
	if got := out["X-Trace"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("multi-value preserved: got %v", got)
	}
}

func TestRedactHeaders_NilSafe(t *testing.T) {
	if got := redactHeaders(nil, nil); len(got) != 0 {
		t.Errorf("nil should yield empty header")
	}
}

// --- redactBody ---------------------------------------------------------

func TestRedactBody_SimplePath(t *testing.T) {
	in := []byte(`{"password":"s3cr3t","name":"alice"}`)
	out := redactBody(in, []string{"$.password"})
	if !strings.Contains(string(out), `"password":"[REDACTED]"`) {
		t.Errorf("password not redacted: %s", out)
	}
	if !strings.Contains(string(out), `"name":"alice"`) {
		t.Errorf("name lost: %s", out)
	}
}

func TestRedactBody_NestedPath(t *testing.T) {
	in := []byte(`{"credentials":{"password":"x"},"keep":"v"}`)
	out := redactBody(in, []string{"$.credentials.password"})
	if !strings.Contains(string(out), `"password":"[REDACTED]"`) {
		t.Errorf("nested password not redacted: %s", out)
	}
	if !strings.Contains(string(out), `"keep":"v"`) {
		t.Errorf("non-target field lost: %s", out)
	}
}

func TestRedactBody_NonJSON_Verbatim(t *testing.T) {
	in := []byte("not json")
	out := redactBody(in, []string{"$.password"})
	if string(out) != "not json" {
		t.Errorf("non-JSON body changed: %s", out)
	}
}

func TestRedactBody_EmptyPaths_NoChange(t *testing.T) {
	in := []byte(`{"a":1}`)
	out := redactBody(in, nil)
	if string(out) != string(in) {
		t.Errorf("empty paths should not change body")
	}
}

// --- redactURL ----------------------------------------------------------

func TestRedactURL_ListedKeyMasked(t *testing.T) {
	keys := map[string]struct{}{"token": {}}
	got := redactURL("https://x.example.com/u?token=s3cr3t&name=alice", keys)
	if !strings.Contains(got, "token=%5BREDACTED%5D") && !strings.Contains(got, "token=[REDACTED]") {
		t.Errorf("token not masked: %s", got)
	}
	if !strings.Contains(got, "name=alice") {
		t.Errorf("non-listed param lost: %s", got)
	}
}

func TestRedactURL_NoQueryUnchanged(t *testing.T) {
	got := redactURL("https://x.example.com/u", map[string]struct{}{"token": {}})
	if got != "https://x.example.com/u" {
		t.Errorf("URL without query changed: %s", got)
	}
}

func TestRedactURL_EmptyKeys_NoChange(t *testing.T) {
	in := "https://x.example.com/u?token=secret"
	if got := redactURL(in, nil); got != in {
		t.Errorf("empty keys should not modify: %s", got)
	}
}

// --- resolveRedactionPolicy --------------------------------------------

func TestResolveRedactionPolicy_FrameworkDefaultsAlwaysPresent(t *testing.T) {
	policy := resolveRedactionPolicy(nil, nil)
	if _, ok := policy.headerSet[textproto.CanonicalMIMEHeaderKey("Authorization")]; !ok {
		t.Error("framework Authorization header default not present")
	}
	if _, ok := policy.queryKeys["token"]; !ok {
		t.Error("framework token query default not present")
	}
}

func TestResolveRedactionPolicy_DefaultsAndServiceExtend(t *testing.T) {
	d := &RedactionConfig{Headers: []string{"X-Defaults-Header"}, QueryKeys: []string{"q1"}}
	s := &RedactionConfig{Headers: []string{"X-Service-Header"}, QueryKeys: []string{"q2"}}
	policy := resolveRedactionPolicy(d, s)
	for _, k := range []string{"X-Defaults-Header", "X-Service-Header"} {
		if _, ok := policy.headerSet[textproto.CanonicalMIMEHeaderKey(k)]; !ok {
			t.Errorf("missing header %q", k)
		}
	}
	for _, k := range []string{"q1", "q2"} {
		if _, ok := policy.queryKeys[k]; !ok {
			t.Errorf("missing query key %q", k)
		}
	}
}

// --- validateRedactionConfig -------------------------------------------

func TestValidateRedactionConfig_BadJSONPath(t *testing.T) {
	cfg := &RedactionConfig{BodyJSONPath: []string{"password"}}
	errs := validateRedactionConfig("x", cfg)
	if len(errs) == 0 || !strings.Contains(errs[0], "$") {
		t.Errorf("expected $-prefix error; got %v", errs)
	}
}

func TestValidateRedactionConfig_EmptyEntries(t *testing.T) {
	cfg := &RedactionConfig{Headers: []string{""}, QueryKeys: []string{""}}
	errs := validateRedactionConfig("x", cfg)
	if len(errs) != 2 {
		t.Errorf("expected 2 errors; got %v", errs)
	}
}

// imports re-used
var _ = strings.Contains
var _ = textproto.CanonicalMIMEHeaderKey

