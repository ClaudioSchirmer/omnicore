package listfailures

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

func TestRenderText_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderText(&buf, nil, false); err != nil {
		t.Fatalf("renderText: %v", err)
	}
	if !strings.Contains(buf.String(), "no pending projection failures") {
		t.Errorf("expected empty-state message, got %q", buf.String())
	}
}

func TestRenderText_Populated(t *testing.T) {
	rows := []query.ProjectionFailureRecord{
		{
			Kind:          query.ProjectionFailureKindRipple,
			Topic:         "users.events",
			AggregateType: "orders",
			AggregateID:   "u1",
			Stage:         query.ProjectionFailureStageCompose,
			Error:         "boom",
			Attempt:       3,
			LastAttemptAt: time.Date(2026, 6, 11, 14, 0, 0, 0, time.UTC),
		},
	}
	var buf bytes.Buffer
	if err := renderText(&buf, rows, false); err != nil {
		t.Fatalf("renderText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"1 pending projection failure",
		"ripple",
		"users.events",
		"orders",
		"u1",
		"compose",
		"2026-06-11 14:00:00",
		"boom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderText_TruncatedHint(t *testing.T) {
	rows := []query.ProjectionFailureRecord{
		{Kind: query.ProjectionFailureKindRipple, Topic: "t", AggregateType: "v",
			AggregateID: "u", Stage: query.ProjectionFailureStageDiscover},
	}
	var buf bytes.Buffer
	if err := renderText(&buf, rows, true); err != nil {
		t.Fatalf("renderText: %v", err)
	}
	if !strings.Contains(buf.String(), "truncated") {
		t.Errorf("expected truncated marker, got %q", buf.String())
	}
}

func TestRenderJSON_Envelope(t *testing.T) {
	rows := []query.ProjectionFailureRecord{
		{Kind: query.ProjectionFailureKindEvent, Topic: "users.events", AggregateType: "users",
			AggregateID: "u1", EventType: "UPDATED", Error: "boom", Attempt: 2},
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, rows, true); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var envelope struct {
		Count     int                             `json:"count"`
		Truncated bool                            `json:"truncated"`
		Items     []query.ProjectionFailureRecord `json:"items"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if envelope.Count != 1 || !envelope.Truncated || len(envelope.Items) != 1 {
		t.Errorf("envelope shape wrong: %+v", envelope)
	}
	if envelope.Items[0].Topic != "users.events" || envelope.Items[0].Kind != query.ProjectionFailureKindEvent {
		t.Errorf("items[0] drifted: %+v", envelope.Items[0])
	}
}

func TestTruncateString(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 8, "hello..."},
		{"hi", 2, "hi"},
		{"abcdef", 3, "abc"},
		{"abcdef", 0, "abcdef"},
	}
	for _, tc := range cases {
		if got := truncateString(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateString(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
