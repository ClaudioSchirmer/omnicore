package handlers

import (
	"strings"
	"testing"
)

// TestRequirePathID_NonEmpty asserts the helper returns silently when the
// path ID is populated — the common success path on every Auto handler
// dispatch.
func TestRequirePathID_NonEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RequirePathID panicked on non-empty input: %v", r)
		}
	}()
	RequirePathID("a1b2c3", "TestHandler")
}

// TestRequirePathID_Empty asserts the helper panics with a developer
// diagnostic carrying the handler name when the path ID is the empty
// string. Mirrors the §5.3 contract.
func TestRequirePathID_Empty(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got nil")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic value, got %T", r)
		}
		for _, want := range []string{
			"FATAL",
			"FancyHandler",
			"PathID()",
			"empty string",
			"path id: \"\"",
		} {
			if !strings.Contains(msg, want) {
				t.Fatalf("panic message missing %q:\n%s", want, msg)
			}
		}
	}()
	RequirePathID("", "FancyHandler")
}
