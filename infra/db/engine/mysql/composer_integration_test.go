//go:build integration && mysql

package mysql

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/mongo"
)

// Phase 4b integration test: the backend-neutral Composer reads the MySQL engine
// through the read seam (Querier.QueryMaps + Dialect), so a Mongo view composes
// the same way it does on Postgres. This proves the parts the seam extension
// de-risks against a real MySQL container (devops/docker-compose.yml `mysql`
// service, host :3307):
//
//	go test -tags=integration,mysql ./infra/db/mysql/ -count=1
//
//   - QueryMaps decodes a BINARY(16) column back to a canonical uuid string
//     (and other columns out of the driver's raw []byte into strings);
//   - the root fetch encodes the (uuid-string) key into the 16-byte form so the
//     WHERE matches the stored BINARY(16) PK;
//   - the one-to-many embed round-trips the parent id extracted from the composed
//     root doc (a string) back into bytes to match the child FK.

type addrRow struct {
	domain.BaseEntity
	Street string
}

func addrSchema() *core.TableSchema {
	return core.NewTableSchema[*addrRow]("addresses").
		PK("id").
		FK("user_id").
		Field("Street", "street").
		SoftDelete("deleted_at")
}

func TestMySQLComposer_RootWithEmbed(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	// addresses child table (FK BINARY(16) → flat_persons.id).
	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS addresses`); err != nil {
		t.Fatalf("drop addresses: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE addresses (
		id BINARY(16) PRIMARY KEY,
		user_id BINARY(16) NOT NULL,
		street VARCHAR(255) NOT NULL,
		deleted_at DATETIME NULL
	)`); err != nil {
		t.Fatalf("create addresses: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS addresses`) })

	// Root insert through the engine → BINARY(16) id, uuid-string back.
	person := &flatPerson{Name: "Alice", Email: "alice@compose"}
	ins, err := domain.GetInsertable(person, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, flatSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	rootID := res.ID
	rootUUID, err := uuid.Parse(rootID)
	if err != nil {
		t.Fatalf("root id not a uuid: %v", err)
	}
	rootBytes := rootUUID[:]

	// Two child rows referencing the root via BINARY(16) FK.
	for _, street := range []string{"1 Main St", "2 Oak Ave"} {
		childID := uuid.New()
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO addresses (id, user_id, street) VALUES (?, ?, ?)`,
			childID[:], rootBytes, street); err != nil {
			t.Fatalf("insert address %q: %v", street, err)
		}
	}

	// Compose the view through the MySQL engine (no Mongo embeds → NewComposer).
	view := mongo.View("flat_persons").Version(1).Root("flat_persons").
		Schema(flatSchema()).
		EmbedMany("addresses", mongo.FromSchema(addrSchema()))

	doc, err := mongo.NewComposer(eng).Compose(ctx, view, rootID)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc == nil {
		t.Fatal("Compose returned nil doc for an existing root")
	}

	// Root: BINARY(16) id decoded to the canonical uuid string; columns are
	// strings, not the driver's raw []byte.
	if got, ok := doc["id"].(string); !ok || got != rootID {
		t.Fatalf("root id = %#v, want string %q", doc["id"], rootID)
	}
	if got, ok := doc["name"].(string); !ok || got != "Alice" {
		t.Fatalf("root name = %#v, want string \"Alice\"", doc["name"])
	}

	// Embed: the parent id (a string in the composed doc) was re-encoded to
	// bytes to match the child FK, yielding both rows.
	lines, ok := doc["addresses"].([]bson.M)
	if !ok {
		t.Fatalf("addresses embed shape = %T", doc["addresses"])
	}
	if len(lines) != 2 {
		t.Fatalf("embedded addresses = %d, want 2 (doc=%v)", len(lines), doc)
	}
	for _, l := range lines {
		if uid, ok := l["user_id"].(string); !ok || uid != rootID {
			t.Fatalf("child user_id = %#v, want string %q", l["user_id"], rootID)
		}
		if _, ok := l["street"].(string); !ok {
			t.Fatalf("child street not a string: %#v", l["street"])
		}
	}
}

type flagRow struct {
	domain.BaseEntity
	Active bool
	Name   string
}

func flagSchema() *core.TableSchema {
	return core.NewTableSchema[*flagRow]("flags").
		PK("id").
		Field("Active", "active").
		Field("Name", "name")
}

// MySQL stores BOOL/BOOLEAN as TINYINT(1) and the driver yields int64(0/1) on the
// dynamic compose read. The composer must restore the schema-declared bool column
// to a real BSON bool (Postgres parity), not a number — otherwise a $jsonSchema
// bsonType:"bool" validator would reject the upsert and clients would see 0/1.
func TestMySQLComposer_BoolColumn(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS flags`); err != nil {
		t.Fatalf("drop flags: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE flags (
		id BINARY(16) PRIMARY KEY,
		active TINYINT(1) NOT NULL,
		name VARCHAR(255) NOT NULL
	)`); err != nil {
		t.Fatalf("create flags: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS flags`) })

	id := uuid.New()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO flags (id, active, name) VALUES (?, ?, ?)`, id[:], 1, "on"); err != nil {
		t.Fatalf("insert flag: %v", err)
	}

	view := mongo.View("flags").Version(1).Root("flags").Schema(flagSchema())
	doc, err := mongo.NewComposer(eng).Compose(ctx, view, id.String())
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if doc == nil {
		t.Fatal("Compose returned nil doc")
	}
	if got, ok := doc["active"].(bool); !ok || got != true {
		t.Fatalf("active = %#v (%T), want bool(true) — TINYINT(1) not coerced to bool", doc["active"], doc["active"])
	}
	if got, ok := doc["name"].(string); !ok || got != "on" {
		t.Fatalf("name = %#v, want string \"on\"", doc["name"])
	}
}
