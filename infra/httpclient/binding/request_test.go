package binding

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func meta(method, path string, headers map[string]string) EndpointMeta {
	return EndpointMeta{
		Method:        method,
		Path:          path,
		RequestCodec:  "json",
		ResponseCodec: "json",
		Headers:       headers,
	}
}

func TestBuildRequest_PathSubstitution(t *testing.T) {
	type req struct {
		ID string `http:"path,id"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/users/{id}", nil), req{ID: "abc"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if r.URL.String() != "https://api.example.com/users/abc" {
		t.Errorf("URL = %q", r.URL)
	}
}

func TestBuildRequest_PathEscaping(t *testing.T) {
	type req struct {
		ID string `http:"path,id"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/users/{id}", nil), req{ID: "a/b c"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if !strings.Contains(r.URL.String(), "/users/a%2Fb%20c") {
		t.Errorf("path component not escaped: %q", r.URL)
	}
}

func TestBuildRequest_QuerySingle(t *testing.T) {
	type req struct {
		Q string `http:"query,q"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/search", nil), req{Q: "go"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := r.URL.Query().Get("q"); got != "go" {
		t.Errorf("q = %q", got)
	}
}

func TestBuildRequest_QuerySingleEmpty_Omitted(t *testing.T) {
	type req struct {
		Q string `http:"query,q"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/search", nil), req{Q: ""})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if r.URL.RawQuery != "" {
		t.Errorf("empty query value should be omitted; got RawQuery=%q", r.URL.RawQuery)
	}
}

func TestBuildRequest_QueryCSV(t *testing.T) {
	type req struct {
		Tags []string `http:"query,tags,csv"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/x", nil), req{Tags: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := r.URL.Query().Get("tags"); got != "a,b,c" {
		t.Errorf("tags csv = %q", got)
	}
}

func TestBuildRequest_QueryMulti(t *testing.T) {
	type req struct {
		Tags []string `http:"query,tags,multi"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/x", nil), req{Tags: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := r.URL.Query()["tags"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("tags multi = %v", got)
	}
}

func TestBuildRequest_HeaderTagAndDefaultsCascade(t *testing.T) {
	type req struct {
		Tenant string `http:"header,X-Tenant"`
	}
	defaults := map[string]string{"User-Agent": "svc/1.0", "X-Tenant": "default"}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/x", defaults), req{Tenant: "acme"})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if got := r.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("tag header should override defaults: got %q", got)
	}
	if got := r.Header.Get("User-Agent"); got != "svc/1.0" {
		t.Errorf("defaults header lost: got %q", got)
	}
}

func TestBuildRequest_HeadersMap(t *testing.T) {
	type req struct {
		Extra map[string]string `http:"headers"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/x", nil), req{Extra: map[string]string{"X-A": "1", "X-B": "2"}})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if r.Header.Get("X-A") != "1" || r.Header.Get("X-B") != "2" {
		t.Errorf("headers map not applied: %+v", r.Header)
	}
}

func TestBuildRequest_BodyJSON(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	type req struct {
		ID   string `http:"path,id"`
		Body body   `http:"body,json"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("POST", "/users/{id}", nil), req{ID: "1", Body: body{Name: "Ada"}})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(b) != `{"name":"Ada"}` {
		t.Errorf("body = %q", b)
	}
	if r.ContentLength != int64(len(b)) {
		t.Errorf("ContentLength mismatch: %d vs %d", r.ContentLength, len(b))
	}
}

func TestBuildRequest_FullCombo(t *testing.T) {
	type body struct {
		Active bool `json:"active"`
	}
	type req struct {
		ID     string            `http:"path,id"`
		Q      string            `http:"query,q"`
		Tenant string            `http:"header,X-Tenant"`
		Extra  map[string]string `http:"headers"`
		Body   body              `http:"body,json"`
	}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("PUT", "/users/{id}", map[string]string{"User-Agent": "svc/1.0"}),
		req{ID: "u1", Q: "search", Tenant: "acme", Extra: map[string]string{"X-Trace": "abc"}, Body: body{Active: true}})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if r.Method != "PUT" {
		t.Errorf("Method = %q", r.Method)
	}
	if !strings.HasSuffix(r.URL.Path, "/users/u1") {
		t.Errorf("path = %q", r.URL.Path)
	}
	if r.URL.Query().Get("q") != "search" {
		t.Errorf("q lost")
	}
	if r.Header.Get("X-Tenant") != "acme" || r.Header.Get("X-Trace") != "abc" || r.Header.Get("User-Agent") != "svc/1.0" {
		t.Errorf("headers cascade failed: %+v", r.Header)
	}
}

func TestBuildRequest_NilPointer(t *testing.T) {
	type req struct {
		ID string `http:"path,id"`
	}
	var rp *req
	_, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/users/{id}", nil), rp)
	if err == nil {
		t.Fatal("expected error for nil pointer request")
	}
}

func TestBuildRequest_NotStruct(t *testing.T) {
	_, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/x", nil), 42)
	if err == nil {
		t.Fatal("expected error for non-struct request")
	}
}

func TestBuildRequest_BadEndpointMeta(t *testing.T) {
	type req struct{}
	if _, err := BuildRequest(context.Background(), "https://api.example.com",
		EndpointMeta{Method: "", Path: "/x"}, req{}); err == nil {
		t.Error("expected error for missing method")
	}
	if _, err := BuildRequest(context.Background(), "https://api.example.com",
		EndpointMeta{Method: "GET", Path: "x"}, req{}); err == nil {
		t.Error("expected error for path without leading /")
	}
}

func TestBuildRequest_ContextWired(t *testing.T) {
	type req struct{}
	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")
	r, err := BuildRequest(ctx, "https://api.example.com",
		meta("GET", "/x", nil), req{})
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if r.Context().Value(ctxKey("k")) != "v" {
		t.Errorf("context not propagated to *http.Request")
	}
}

func TestBuildRequest_AcceptsStructPointer(t *testing.T) {
	type req struct {
		ID string `http:"path,id"`
	}
	rp := &req{ID: "abc"}
	r, err := BuildRequest(context.Background(), "https://api.example.com",
		meta("GET", "/users/{id}", nil), rp)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if !strings.HasSuffix(r.URL.Path, "/users/abc") {
		t.Errorf("path = %q", r.URL.Path)
	}
}

// _ keeps http imported when none of the test bodies reference http directly.
var _ = http.NoBody
