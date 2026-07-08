//go:build nats

package nats

import (
	"testing"

	"github.com/nats-io/nats.go"
)

func TestUnwrap(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"users"`, "users"},       // JSON-quoted string (Debezium json header format)
		{"users", "users"},         // bare (not valid JSON) → trimmed passthrough
		{`""`, ""},                 // empty JSON string
		{"", ""},                   // empty
		{`"00-abc-def-01"`, "00-abc-def-01"},
	}
	for _, c := range cases {
		if got := unwrap(c.in); got != c.want {
			t.Errorf("unwrap(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFlattenHeadersAndHeaderString(t *testing.T) {
	h := nats.Header{
		"aggregate_type": {`"users"`},
		"event_type":     {`"INSERTED"`},
		"traceparent":    {`"00-abc-def-01"`},
		"aggregate_id":   {`"f5aaa9e7-17e1-41d3-8f9d-056d2d30f4a3"`},
	}
	out := flattenHeaders(h)
	if out["aggregate_type"] != "users" {
		t.Errorf("aggregate_type = %q, want users", out["aggregate_type"])
	}
	if out["event_type"] != "INSERTED" {
		t.Errorf("event_type = %q, want INSERTED", out["event_type"])
	}
	if out["aggregate_id"] != "f5aaa9e7-17e1-41d3-8f9d-056d2d30f4a3" {
		t.Errorf("aggregate_id = %q", out["aggregate_id"])
	}
	if headerString(h, "traceparent") != "00-abc-def-01" {
		t.Errorf("headerString(traceparent) = %q", headerString(h, "traceparent"))
	}
	if headerString(h, "absent") != "" {
		t.Errorf("absent header must be empty, got %q", headerString(h, "absent"))
	}
}
