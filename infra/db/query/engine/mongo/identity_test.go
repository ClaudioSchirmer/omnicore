package mongo

import "testing"

// `_id` is this store's spelling of the identity and it stops at this package.
// Above the read seam every backing speaks ONE identity vocabulary: the Go field
// "ID". normalizeIdentity is where that is settled.
func TestNormalizeIdentity_LiftsTheStoreKeyOntoTheGoField(t *testing.T) {
	// A mirror of an upstream collection is stored under `_id` alone — no schema
	// column maps it, so ToGoDoc translated nothing.
	doc := normalizeIdentity(map[string]any{"_id": "u1", "Name": "ana"}, true)
	if doc["ID"] != "u1" {
		t.Errorf("ID = %#v, want the store key lifted", doc["ID"])
	}
}

// A document whose schema maps an id column already carries "ID": the store key
// must not overwrite it.
func TestNormalizeIdentity_NeverOverwritesAnExistingID(t *testing.T) {
	doc := normalizeIdentity(map[string]any{"_id": "store-key", "ID": "translated"}, true)
	if doc["ID"] != "translated" {
		t.Errorf("ID = %#v, want the translated value to win", doc["ID"])
	}
}

// A non-string store key is not an identity this layer can lift; leaving it alone
// is honest, and the document simply carries no "ID".
func TestNormalizeIdentity_IgnoresANonStringStoreKey(t *testing.T) {
	doc := normalizeIdentity(map[string]any{"_id": 42}, true)
	if _, has := doc["ID"]; has {
		t.Errorf("a non-string store key must not become an ID, got %#v", doc["ID"])
	}
}

func TestNormalizeIdentity_NilDocIsANoOp(t *testing.T) {
	if got := normalizeIdentity(nil, true); got != nil {
		t.Errorf("nil must pass through, got %v", got)
	}
}

// A selection that does not KEEP the identity gets none, under either spelling.
// This is the seat ReadCriteria.Restrict("ID") depends on: the exclusion removes
// the schema's id column from the projection, but Mongo returns `_id` on every
// document regardless, and lifting it back onto "ID" served the restricted field
// spelled exactly as the DTO fills it.
func TestNormalizeIdentity_NotKept_DropsBothSpellings(t *testing.T) {
	doc := normalizeIdentity(map[string]any{"_id": "u1", "Name": "ana"}, false)
	for _, key := range []string{"_id", "ID"} {
		if _, leaked := doc[key]; leaked {
			t.Errorf("%q survived a selection that does not keep the identity: %#v", key, doc)
		}
	}
	if doc["Name"] != "ana" {
		t.Errorf("only the identity leaves, got %#v", doc)
	}
}

// Including the spelling ToGoDoc already translated from the schema's own id
// column — an exclusion of "ID" removes that column from the projection, but a
// composed read may have forced it back in to join on.
func TestNormalizeIdentity_NotKept_DropsATranslatedID(t *testing.T) {
	doc := normalizeIdentity(map[string]any{"_id": "u1", "ID": "u1", "Name": "ana"}, false)
	if _, leaked := doc["ID"]; leaked {
		t.Errorf("the translated ID survived: %#v", doc)
	}
}

func TestNormalizeIdentity_NotKept_NilDocIsStillANoOp(t *testing.T) {
	if got := normalizeIdentity(nil, false); got != nil {
		t.Errorf("nil must pass through, got %v", got)
	}
}
