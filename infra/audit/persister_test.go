package audit

import (
	"encoding/json"
	"testing"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// Unit tests cover the pure helpers that build the SQL parameters. The
// actual `INSERT INTO audit_events ...` round-trip against Postgres lives in
// the integration suite (see qa/audit.sh + framework `//go:build integration`
// tests) — pgx round-trips are observable behaviorally there, not via mocks.

func TestBuildAuditPayload_OmitsEmptyBlocks(t *testing.T) {
	payload, err := buildAuditPayload(appaudit.AuditEvent{
		// All variable blocks empty — payload should be `{}`, not `{"snapshot":null,...}`.
	})
	if err != nil {
		t.Fatalf("buildAuditPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty event payload = %s, want {}", payload)
	}
}

func TestBuildAuditPayload_CarriesPopulatedBlocks(t *testing.T) {
	payload, err := buildAuditPayload(appaudit.AuditEvent{
		ActorClaims: map[string]any{"roles": []string{"admin"}},
		Snapshot:    map[string]any{"name": "alice"},
		Changes:     []appaudit.FieldChange{{Field: "name", From: "a", To: "b"}},
		Children: map[string][]appaudit.ChildEvent{
			"Address": {{ID: "a1", Op: "inserted", Snapshot: map[string]any{"city": "X"}}},
		},
	})
	if err != nil {
		t.Fatalf("buildAuditPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"actorClaims", "snapshot", "changes", "children"} {
		if _, ok := got[key]; !ok {
			t.Errorf("payload missing %q: %s", key, payload)
		}
	}
}

func TestNullableActor_AnonymousSentinelBecomesNull(t *testing.T) {
	if v := nullableActor(persistence.AnonymousActor); v != nil {
		t.Errorf("AnonymousActor → %v, want nil (NULL in DB)", v)
	}
	if v := nullableActor(""); v != nil {
		t.Errorf("empty string → %v, want nil", v)
	}
	if v := nullableActor("alice-uuid"); v != "alice-uuid" {
		t.Errorf("identified actor → %v, want passthrough", v)
	}
}

func TestNullableString_EmptyBecomesNull(t *testing.T) {
	if v := nullableString(""); v != nil {
		t.Errorf("empty → %v, want nil", v)
	}
	if v := nullableString("https://idp.test"); v != "https://idp.test" {
		t.Errorf("populated → %v, want passthrough", v)
	}
}

// The origin rides the payload blob, and survives the round-trip back through
// the reader — a forensic answer to "from where" is worthless if it only
// exists on the way in.
func TestAuditPayload_ClientIPRoundTrips(t *testing.T) {
	payload, err := buildAuditPayload(appaudit.AuditEvent{ClientIP: "198.51.100.23"})
	if err != nil {
		t.Fatalf("buildAuditPayload: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["clientIp"] != "198.51.100.23" {
		t.Fatalf("payload = %s, want clientIp", payload)
	}
	// …and back out through the reader's own decode view.
	var pl auditPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		t.Fatalf("reader unmarshal: %v", err)
	}
	if pl.ClientIP != "198.51.100.23" {
		t.Fatalf("reader decoded ClientIP = %q", pl.ClientIP)
	}
}

// An event with no origin must not grow a "clientIp": "" key — the payload
// scales with what exists, which is why the other blocks are elided too.
func TestAuditPayload_ClientIPElidedWhenEmpty(t *testing.T) {
	payload, err := buildAuditPayload(appaudit.AuditEvent{Snapshot: map[string]any{"name": "x"}})
	if err != nil {
		t.Fatalf("buildAuditPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["clientIp"]; ok {
		t.Errorf("empty origin emitted a key: %s", payload)
	}
}
