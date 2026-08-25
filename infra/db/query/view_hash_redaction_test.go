package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// A redacted field changes what the projected document CONTAINS, so it has to
// move the RebuildHash: that is the whole cleanup mechanism. A changed hash is
// DriftForgotToBump, which forces a Version bump, which forces a rebuild, and the
// rebuild recomposes every document — replacing the values an earlier policy had
// already written into the read model. Without this, declaring a field redacted
// would protect future writes while every document already projected kept its
// plaintext forever.

type hashRedactFixture struct {
	ID      string
	Name    string
	Account string
}

func (hashRedactFixture) CollectionName() string { return "HashRedactFixtures" }

// plainSchema / redactedSchema differ ONLY in the redaction declaration.
func plainRedactSchema() *core.TableSchema {
	return core.NewTableSchema[hashRedactFixture]("hash_redact").
		ID("id").
		Field("Name", "name").
		Field("Account", "account")
}

func redactedSchema(r core.Redactor) *core.TableSchema {
	return core.NewTableSchema[hashRedactFixture]("hash_redact").
		ID("id").
		Field("Name", "name").
		RedactedField("Account", "account", core.InSync(r), core.InAudit(core.Plain()))
}

func viewOver(s *core.TableSchema) *ViewDefinition {
	return View("hash_redact").Schema(s).Version(1)
}

func TestRebuildHash_DeclaringARedactionChangesTheShape(t *testing.T) {
	plain := viewOver(plainRedactSchema()).RebuildHash()
	redacted := viewOver(redactedSchema(core.RedactWith("***"))).RebuildHash()
	if plain == redacted {
		t.Fatal("declaring a RedactedField must change the RebuildHash — otherwise nothing forces the rebuild that replaces already-projected plaintext")
	}
}

func TestRebuildHash_ChangingTheStrategyChangesTheShape(t *testing.T) {
	a := viewOver(redactedSchema(core.RedactWith("***"))).RebuildHash()
	b := viewOver(redactedSchema(core.RedactKeepLast(4))).RebuildHash()
	if a == b {
		t.Fatal("changing the InSync strategy must change the RebuildHash — the documents already carry the old mask")
	}
	c := viewOver(redactedSchema(core.RedactKeepLast(2))).RebuildHash()
	if b == c {
		t.Fatal("changing the strategy's PARAMETER must change the RebuildHash")
	}
}

// The block is written only when a redaction exists, so a service that does not
// use the feature keeps hashing exactly as it did before the feature landed. If
// this regressed, every view in every existing service would report drift on its
// next boot.
func TestRebuildHash_NoRedactionHashesIdentically(t *testing.T) {
	a := viewOver(plainRedactSchema()).RebuildHash()
	b := viewOver(plainRedactSchema()).RebuildHash()
	if a != b {
		t.Fatalf("a schema with no redaction must hash deterministically: %q vs %q", a, b)
	}
	if len(plainRedactSchema().RedactionShape()) != 0 {
		t.Fatal("a schema with no RedactedField must render an EMPTY redaction shape, so the hash block is omitted")
	}
}

// A redaction declared on a SIBLING (or a child, or the shared base) must reach
// the hash too — writeSchemaShape recurses, and the projected document carries
// those columns flat at the top.
func TestRebuildHash_RedactionOnASiblingChangesTheShape(t *testing.T) {
	plain := View("hash_redact").Schema(
		core.NewTableSchema[hashRedactFixture]("hash_redact").ID("id").Field("Name", "name").
			Sibling(core.NewSiblingSchema[hashRedactFixture]("hash_redact_side").Field("Account", "account")),
	).Version(1).RebuildHash()

	redacted := View("hash_redact").Schema(
		core.NewTableSchema[hashRedactFixture]("hash_redact").ID("id").Field("Name", "name").
			Sibling(core.NewSiblingSchema[hashRedactFixture]("hash_redact_side").
				RedactedField("Account", "account", core.InSync(core.RedactWith("***")), core.InAudit(core.Plain()))),
	).Version(1).RebuildHash()

	if plain == redacted {
		t.Fatal("a redaction on a sibling must change the RebuildHash")
	}
}

// The AUDIT axis alone must NOT be invisible to the hash either: it does not
// change the document, but it is part of the declared shape and a reader of the
// registry should see the declaration move. (If this ever becomes undesirable,
// the fix is to split the fingerprint — not to drop it.)
func TestRebuildHash_AuditAxisParticipates(t *testing.T) {
	a := View("hash_redact").Schema(
		core.NewTableSchema[hashRedactFixture]("hash_redact").ID("id").Field("Name", "name").
			RedactedField("Account", "account", core.InSync(core.Plain()), core.InAudit(core.RedactWith("***"))),
	).Version(1).RebuildHash()
	b := View("hash_redact").Schema(
		core.NewTableSchema[hashRedactFixture]("hash_redact").ID("id").Field("Name", "name").
			RedactedField("Account", "account", core.InSync(core.Plain()), core.InAudit(core.Plain())),
	).Version(1).RebuildHash()
	if a == b {
		t.Fatal("the InAudit axis is part of the declared shape and must move the hash")
	}
}
