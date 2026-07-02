package core

import (
	"strings"
	"testing"
)

// AssertSharedBaseEquivalent is the boot gate that lets a shared base be
// re-declared identically per role file (no consumer singleton) while refusing
// two DIVERGENT declarations of one physical table. Each divergence axis must
// panic with a message naming what diverged.

func equivBase() *TableSchema {
	return NewSharedBase("pessoa").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document").
		SoftDelete("deleted_at").
		Child(NewTableSchema[equivAddr]("endereco").PK("id").FK("pessoa_id").Field("Street", "street").SoftDelete("deleted_at"))
}

type equivAddr struct {
	ID     string
	Street string
}

func (c equivAddr) GetID() string { return c.ID }

func mustPanicWith(t *testing.T, wantSub string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q, got none", wantSub)
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, wantSub) {
			t.Fatalf("panic %v must contain %q", r, wantSub)
		}
	}()
	fn()
}

func TestAssertSharedBaseEquivalent_IdenticalPasses(t *testing.T) {
	AssertSharedBaseEquivalent(equivBase(), equivBase()) // must not panic
}

func TestAssertSharedBaseEquivalent_DivergenceAxes(t *testing.T) {
	t.Run("different tables is a caller bug", func(t *testing.T) {
		mustPanicWith(t, "different tables", func() {
			AssertSharedBaseEquivalent(equivBase(), NewSharedBase("outra").PK("id").Field("Name", "name").NaturalKey("name"))
		})
	})
	t.Run("pk", func(t *testing.T) {
		b := equivBase()
		b.pkColumn = "uuid"
		mustPanicWith(t, "PK column", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
	t.Run("natural key", func(t *testing.T) {
		b := equivBase().NaturalKey("name")
		mustPanicWith(t, "NaturalKey", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
	t.Run("orphan policy", func(t *testing.T) {
		b := equivBase().OrphanPolicy(DeleteWhenUnreferenced)
		mustPanicWith(t, "OrphanPolicy", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
	t.Run("soft delete", func(t *testing.T) {
		b := equivBase()
		b.softDelete = "removed_at"
		mustPanicWith(t, "SoftDelete", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
	t.Run("field count", func(t *testing.T) {
		b := equivBase().Field("Extra", "extra")
		mustPanicWith(t, "field count", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
	t.Run("field column", func(t *testing.T) {
		a := NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("name")
		b := NewSharedBase("pessoa").PK("id").Field("Name", "full_name").NaturalKey("full_name")
		b.naturalKeyCol = "name" // align NK so the field divergence is what trips
		mustPanicWith(t, "field Name", func() { AssertSharedBaseEquivalent(a, b) })
	})
	t.Run("children count", func(t *testing.T) {
		b := equivBase()
		delete(b.children, "equivAddr")
		mustPanicWith(t, "native-children count", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
	t.Run("child shape", func(t *testing.T) {
		b := NewSharedBase("pessoa").
			PK("id").
			Field("Name", "name").
			Field("Document", "document").
			NaturalKey("document").
			SoftDelete("deleted_at").
			Child(NewTableSchema[equivAddr]("enderecos").PK("id").FK("pessoa_id").Field("Street", "street").SoftDelete("deleted_at"))
		mustPanicWith(t, "native child equivAddr", func() { AssertSharedBaseEquivalent(equivBase(), b) })
	})
}
