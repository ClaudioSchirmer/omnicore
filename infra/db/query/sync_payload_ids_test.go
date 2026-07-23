package query

import "testing"

// The payload's "_ids" block drives routing: parsePayloadIDs feeds the
// role-event shared-base fan-out and the payload-first resolveBaseID.

func TestParsePayloadIDs(t *testing.T) {
	ids, ok := parsePayloadIDs([]byte(`{"name":"Ana","_ids":{"id":"r1","base_id":"b1","base_revision":42,"base_purged":true}}`))
	if !ok {
		t.Fatal("the payload must parse")
	}
	if ids.ID != "r1" || ids.BaseID != "b1" || ids.BaseRevision != 42 || !ids.BasePurged {
		t.Fatalf("ids = %+v", ids)
	}

	if _, ok := parsePayloadIDs(nil); ok {
		t.Error("an empty payload has no ids")
	}
	if _, ok := parsePayloadIDs([]byte(`{"name":"Ana"}`)); ok {
		t.Error("a payload without _ids must answer ok=false")
	}
	if _, ok := parsePayloadIDs([]byte(`null`)); ok {
		t.Error("a null payload must answer ok=false")
	}
	if _, ok := parsePayloadIDs([]byte(`not-json`)); ok {
		t.Error("a malformed payload must answer ok=false")
	}
}

func TestBuildViewIndex_BaseOfRole(t *testing.T) {
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	idx := buildViewIndex([]*ViewDefinition{view})
	if got := idx.baseOfRole["aluno"]; got != "pessoa" {
		t.Fatalf("baseOfRole[aluno] = %q, want pessoa — the role-event fan-out depends on it", got)
	}
}
