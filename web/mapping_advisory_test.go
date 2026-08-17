package web

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

// captureSlog swaps the default logger for one writing JSON into a buffer,
// so a test can assert on what the boot advisory actually emitted (or that it
// emitted nothing at all).
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// resetMappingAdvisory clears the per-pair dedupe so each test starts fresh.
func resetMappingAdvisory(t *testing.T) {
	t.Helper()
	warnedMappings.Range(func(k, _ any) bool {
		warnedMappings.Delete(k)
		return true
	})
}

type advOKResult struct {
	ID   string
	Name *string
	Tags []string
}

type advOKResponse struct {
	ID   *string  `json:"id,omitempty"`
	Name *string  `json:"name,omitempty"`
	Tags []string `json:"tags"`
}

type advBadResult struct {
	ID   string
	When time.Time
}

type advBadResponse struct {
	ID   string `json:"id"`
	When string `json:"when"` // time.Time → string: the JSON codec owns this
}

func TestMappingAdvisory_SilentWhenOptimized(t *testing.T) {
	resetMappingAdvisory(t)
	buf := captureSlog(t)
	warnMappingFallback(
		reflect.TypeOf(struct{}{}),
		reflect.TypeOf(advOKResult{}),
		reflect.TypeOf(advOKResponse{}))
	if buf.Len() != 0 {
		t.Fatalf("a compatible pair must log nothing, got: %s", buf.String())
	}
}

func TestMappingAdvisory_WarnsOnFallbackWithReason(t *testing.T) {
	resetMappingAdvisory(t)
	buf := captureSlog(t)
	warnMappingFallback(
		reflect.TypeOf(struct{}{}),
		reflect.TypeOf(advBadResult{}),
		reflect.TypeOf(advBadResponse{}))
	if buf.Len() == 0 {
		t.Fatal("an incompatible pair must emit the advisory")
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("advisory is not valid JSON: %v — %s", err, buf.String())
	}
	if rec["level"] != "WARN" {
		t.Fatalf("advisory must be a warning, got %v", rec["level"])
	}
	msg, _ := rec["msg"].(string)
	for _, want := range []string{"Response", "Result", "marshal+unmarshal", "Auto query handlers"} {
		if !strings.Contains(msg, want) {
			t.Errorf("advisory message must mention %q, got: %s", want, msg)
		}
	}
	reason, _ := rec["reason"].(string)
	if !strings.Contains(reason, "When") {
		t.Errorf("advisory must name the offending field, got reason: %s", reason)
	}
	if rec["response"] != "web.advBadResponse" || rec["result"] != "web.advBadResult" {
		t.Errorf("advisory must name both types, got %v / %v", rec["result"], rec["response"])
	}
}

func TestMappingAdvisory_OnePerPair(t *testing.T) {
	resetMappingAdvisory(t)
	buf := captureSlog(t)
	for i := 0; i < 3; i++ {
		warnMappingFallback(
			reflect.TypeOf(struct{}{}),
			reflect.TypeOf(advBadResult{}),
			reflect.TypeOf(advBadResponse{}))
	}
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; n != 1 {
		t.Fatalf("a pair must warn exactly once across mounts, got %d lines:\n%s", n, buf.String())
	}
}

func TestMappingAdvisory_NonStructShapesReport(t *testing.T) {
	resetMappingAdvisory(t)
	buf := captureSlog(t)
	warnMappingFallback(
		reflect.TypeOf(struct{}{}),
		reflect.TypeOf(map[string]any{}),
		reflect.TypeOf(advOKResponse{}))
	if !strings.Contains(buf.String(), "not a struct") {
		t.Fatalf("a non-struct Result must be reported, got: %s", buf.String())
	}
}

func TestTypeLabel_NilSafe(t *testing.T) {
	if got := typeLabel(nil); got != "<nil>" {
		t.Fatalf("typeLabel(nil) = %q", got)
	}
}
