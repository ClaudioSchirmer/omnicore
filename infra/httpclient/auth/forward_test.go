package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type fakeBearerCarrier struct {
	context.Context
	token string
}

func (f fakeBearerCarrier) BearerToken() string { return f.token }

func newReq(ctx context.Context) *http.Request {
	r, _ := http.NewRequest("GET", "https://x.example.com/u", nil)
	return r.WithContext(ctx)
}

func TestForwardBearer_Apply_WithToken(t *testing.T) {
	p := NewForwardBearerProvider("fb", AttachConfig{})
	req := newReq(fakeBearerCarrier{Context: context.Background(), token: "abc"})
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer abc" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestForwardBearer_Apply_EmptyToken(t *testing.T) {
	p := NewForwardBearerProvider("fb", AttachConfig{})
	req := newReq(fakeBearerCarrier{Context: context.Background(), token: ""})
	err := p.Apply(req)
	if err == nil || !strings.Contains(err.Error(), "no bearer") {
		t.Errorf("expected no-bearer error; got %v", err)
	}
}

func TestForwardBearer_Apply_CtxWithoutCarrier(t *testing.T) {
	p := NewForwardBearerProvider("fb", AttachConfig{})
	req := newReq(context.Background())
	err := p.Apply(req)
	if err == nil || !strings.Contains(err.Error(), "requires AppContext") {
		t.Errorf("expected AppContext error; got %v", err)
	}
}

func TestForwardBearer_CustomAttach(t *testing.T) {
	p := NewForwardBearerProvider("fb", AttachConfig{Kind: AttachQuery, Name: "access_token", Format: "{token}"})
	req := newReq(fakeBearerCarrier{Context: context.Background(), token: "abc"})
	if err := p.Apply(req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := req.URL.Query().Get("access_token"); got != "abc" {
		t.Errorf("custom attach query = %q", got)
	}
}
