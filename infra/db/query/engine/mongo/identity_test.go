package mongo

import "testing"

// `_id` is this store's spelling of the identity and it stops at this package.
// Above the read seam every backing speaks ONE identity vocabulary: the Go field
// "ID". normalizeIdentity is where that is settled.
func TestNormalizeIdentity_LiftsTheStoreKeyOntoTheGoField(t *testing.T) {
	// A mirror of an upstream collection is stored under `_id` alone — no schema
	// column maps it, so ToGoDoc translated nothing.
	doc := normalizeIdentity(map[string]any{"_id": "u1", "Name": "ana"})
	if doc["ID"] != "u1" {
		t.Errorf("ID = %#v, want the store key lifted", doc["ID"])
	}
}

// A document whose schema maps an id column already carries "ID": the store key
// must not overwrite it.
func TestNormalizeIdentity_NeverOverwritesAnExistingID(t *testing.T) {
	doc := normalizeIdentity(map[string]any{"_id": "store-key", "ID": "translated"})
	if doc["ID"] != "translated" {
		t.Errorf("ID = %#v, want the translated value to win", doc["ID"])
	}
}

// A non-string store key is not an identity this layer can lift; leaving it alone
// is honest, and the document simply carries no "ID".
func TestNormalizeIdentity_IgnoresANonStringStoreKey(t *testing.T) {
	doc := normalizeIdentity(map[string]any{"_id": 42})
	if _, has := doc["ID"]; has {
		t.Errorf("a non-string store key must not become an ID, got %#v", doc["ID"])
	}
}

func TestNormalizeIdentity_NilDocIsANoOp(t *testing.T) {
	if got := normalizeIdentity(nil); got != nil {
		t.Errorf("nil must pass through, got %v", got)
	}
}
