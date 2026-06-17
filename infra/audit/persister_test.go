package audit

import (
	"encoding/json"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// Unit tests cover the pure helpers that build the SQL parameters. The
// actual `INSERT INTO audit_events ...` round-trip against Postgres lives in
// the integration suite (see qa/audit.sh + framework `//go:build integration`
// tests) — pgx round-trips are observable behaviorally there, not via mocks.

func TestBuildAuditPayload_OmitsEmptyBlocks(t *testing.T) {
	payload, err := buildAuditPayload(AuditEvent{
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
	payload, err := buildAuditPayload(AuditEvent{
		ActorClaims: map[string]any{"roles": []string{"admin"}},
		Snapshot:    map[string]any{"name": "alice"},
		Changes:     []FieldChange{{Field: "name", From: "a", To: "b"}},
		Children: map[string][]ChildEvent{
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
