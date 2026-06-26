package httpclient

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers --------------------------------------------------------------

func streamingClient(t *testing.T, srv *httptest.Server, ep EndpointConfig) *HttpClient {
	t.Helper()
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: srv.URL, Endpoints: map[string]EndpointConfig{"call": ep}},
		},
	}
	c, err := New(cfg, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// --- 7.A Download streaming ----------------------------------------------

func TestStream_Download_DeliversReadCloser(t *testing.T) {
	payload := strings.Repeat("X", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", "1024")
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x",
		ResponseStream: true,
	})

	type req struct{}
	got, err := Call[req, StreamResponse](newCtx(t), c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer got.Body.Close()
	if got.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q", got.ContentType)
	}
	if got.ContentLength != 1024 {
		t.Errorf("ContentLength = %d", got.ContentLength)
	}
	data, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != payload {
		t.Errorf("body mismatch (got %d bytes)", len(data))
	}
}

func TestStream_Download_WrongRespType_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/x", ResponseStream: true,
	})
	type req struct{}
	type wrongResp struct {
		ID string `json:"id"`
	}
	_, err := Call[req, wrongResp](newCtx(t), c, "svc", "call", req{})
	if !errors.Is(err, ErrResponseDecode) {
		t.Fatalf("expected ErrResponseDecode for wrong Resp type, got %v", err)
	}
}

func TestStream_Download_BootRejects_CachePlusStream(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"svc": {BaseURL: "http://x", Endpoints: map[string]EndpointConfig{
			"call": {Method: "GET", Path: "/x", ResponseStream: true, Cache: &EndpointCacheConfig{}},
		}},
	}}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "responseStream") {
		t.Fatalf("expected boot reject of cache+responseStream, got %v", err)
	}
}

func TestStream_Download_BootRejects_StreamPlusSSE(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"svc": {BaseURL: "http://x", Endpoints: map[string]EndpointConfig{
			"call": {Method: "GET", Path: "/x", ResponseStream: true, ResponseSSE: true},
		}},
	}}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected boot reject of responseStream+responseSSE, got %v", err)
	}
}

// --- 7.B Upload streaming -------------------------------------------------

type uploadReq struct {
	UserID string    `http:"path,id"`
	Body   io.Reader `http:"body,stream"`
	Type   string    `http:"header,Content-Type"`
}

func TestStream_Upload_BodyPipedToServer(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{Method: "POST", Path: "/users/{id}/avatar"})

	payload := bytes.Repeat([]byte("PNG"), 512)
	_, err := Call[uploadReq, struct{}](newCtx(t), c, "svc", "call", uploadReq{
		UserID: "u42",
		Body:   bytes.NewReader(payload),
		Type:   "image/png",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Errorf("server received %d bytes, want %d", len(received), len(payload))
	}
}

func TestStream_Upload_RetryDisabled(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	// Endpoint declares retry; the streaming body must override and produce 1 attempt.
	c := streamingClient(t, srv, EndpointConfig{
		Method: "POST", Path: "/x",
		Retry: &RetryConfig{
			MaxAttempts: 5, Backoff: "constant",
			InitialDelay: Duration(1 * time.Millisecond),
			MaxDelay:     Duration(5 * time.Millisecond),
			RetryOn:      []string{"502"},
		},
		// Idempotency declared so YAML validation accepts POST + retry > 1.
		// The streaming body should still force maxAttempts: 1 at runtime.
		Idempotency: &IdempotencyConfig{Header: "X-Idempotency-Key", Source: "ctx"},
	})
	type req struct {
		Body io.Reader `http:"body,stream"`
	}
	_, _ = Call[req, struct{}](newCtx(t), c, "svc", "call", req{Body: strings.NewReader("data")})
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (streaming disables retry)", got)
	}
}

func TestStream_Upload_RejectsWhenSigningEnabled(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"svc": {
			BaseURL: "http://x",
			Endpoints: map[string]EndpointConfig{
				"call": {Method: "POST", Path: "/x"},
			},
			Signing: &SigningConfig{
				Type: "hmac-sha256", Secret: "s",
				SignedHeaders:   []string{"host"},
				TimestampHeader: "X-Date", SignatureHeader: "X-Sig",
			},
		},
	}}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type req struct {
		Body io.Reader `http:"body,stream"`
	}
	_, err = Call[req, struct{}](newCtx(t), c, "svc", "call", req{Body: strings.NewReader("data")})
	if err == nil || !strings.Contains(err.Error(), "signs requests") {
		t.Fatalf("expected signing-vs-streaming rejection, got %v", err)
	}
}

// --- 7.C Multipart upload -------------------------------------------------

func TestStream_Multipart_FieldsAndFile(t *testing.T) {
	type seen struct {
		field       string
		filename    string
		fileMime    string
		fileContent []byte
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediatype, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediatype != "multipart/form-data" {
			t.Errorf("server: bad Content-Type %q", r.Header.Get("Content-Type"))
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("server: NextPart: %v", err)
				return
			}
			body, _ := io.ReadAll(p)
			if p.FormName() == "category" {
				got.field = string(body)
			}
			if p.FileName() != "" {
				got.filename = p.FileName()
				got.fileMime = p.Header.Get("Content-Type")
				got.fileContent = body
			}
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{Method: "POST", Path: "/uploads"})

	type req struct {
		Body Multipart `http:"body,multipart"`
	}
	_, err := Call[req, struct{}](newCtx(t), c, "svc", "call", req{
		Body: Multipart{
			Fields: []MultipartField{{Name: "category", Value: "id-proof"}},
			Files: []MultipartFile{{
				Name: "file", Filename: "passport.pdf", MimeType: "application/pdf",
				Content: strings.NewReader("PDF-DATA"),
			}},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.field != "id-proof" {
		t.Errorf("field = %q", got.field)
	}
	if got.filename != "passport.pdf" || got.fileMime != "application/pdf" || string(got.fileContent) != "PDF-DATA" {
		t.Errorf("file part mismatch: %+v", got)
	}
}

// --- 7.D SSE --------------------------------------------------------------

func TestStream_SSE_ParsesEventsAndDispatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("server: missing Accept header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: tick\ndata: 1\nid: a\n\n: comment line\nevent: tock\ndata: line1\ndata: line2\nretry: 2500\n\n")
	}))
	defer srv.Close()
	c := streamingClient(t, srv, EndpointConfig{
		Method: "GET", Path: "/events", ResponseSSE: true,
	})

	type req struct{}
	got, err := Call[req, SSEResponse](newCtx(t), c, "svc", "call", req{})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer got.Close()

	var events []SSEvent
	timeout := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-got.Events:
			if !ok {
				break loop
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatalf("timeout waiting for SSE events (got %d)", len(events))
		}
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d (%+v)", len(events), events)
	}
	if events[0].Event != "tick" || string(events[0].Data) != "1" || events[0].ID != "a" {
		t.Errorf("event[0] = %+v", events[0])
	}
	if events[1].Event != "tock" || string(events[1].Data) != "line1\nline2" {
		t.Errorf("event[1] = %+v", events[1])
	}
	if events[1].Retry != 2500*time.Millisecond {
		t.Errorf("event[1].Retry = %v, want 2.5s", events[1].Retry)
	}
}

func TestStream_SSE_BootRejects_CachePlusSSE(t *testing.T) {
	cfg := &Config{Services: map[string]ServiceConfig{
		"svc": {BaseURL: "http://x", Endpoints: map[string]EndpointConfig{
			"call": {Method: "GET", Path: "/x", ResponseSSE: true, Cache: &EndpointCacheConfig{}},
		}},
	}}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "responseSSE") {
		t.Fatalf("expected boot reject of cache+responseSSE, got %v", err)
	}
}
