//go:build integration && mysql

package mysql

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
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
//     WHERE matches the stored BINARY(16) ID;
//   - the one-to-many embed round-trips the parent id extracted from the composed
//     root doc (a string) back into bytes to match the child ParentID.

// The relational EmbedMany integration test (root + BINARY(16) ParentID embed) was
// removed with the relational embed path. Own-child projection with the same
// BINARY(16) ParentID re-encoding is covered by TestMySQLComposer_OwnChild below.

type flagRow struct {
	domain.BaseEntity
	Active bool
	Name   string
}

func flagSchema() *core.TableSchema {
	return core.NewTableSchema[*flagRow]("flags").
		ID("id").
		Field("Active", "active").
		Field("Name", "name")
}

type mcLineRow struct {
	ID  string
	Qty int
}

// TestMySQLComposer_OwnChild proves the Phase-1 own-child auto path on MySQL: the
// child is declared on the ROOT schema (no EmbedMany) and projects automatically,
// joined root.ID → child.ParentID with the BINARY(16) id re-encoded for the WHERE.
func TestMySQLComposer_OwnChild(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS mc_lines`); err != nil {
		t.Fatalf("drop mc_lines: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE mc_lines (
		id BINARY(16) PRIMARY KEY,
		user_id BINARY(16) NOT NULL,
		qty INT NOT NULL,
		deleted_at DATETIME NULL
	)`); err != nil {
		t.Fatalf("create mc_lines: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS mc_lines`) })

	person := &flatPerson{Name: "Bob", Email: "bob@compose"}
	ins, err := domain.GetInsertable(person, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, flatSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	rootUUID, err := uuid.Parse(res.ID.Value())
	if err != nil {
		t.Fatalf("root id not a uuid: %v", err)
	}
	rootBytes := rootUUID[:]
	for _, qty := range []int{3, 5} {
		childID := uuid.New()
		if _, err := raw.ExecContext(ctx,
			`INSERT INTO mc_lines (id, user_id, qty) VALUES (?, ?, ?)`,
			childID[:], rootBytes, qty); err != nil {
			t.Fatalf("insert line: %v", err)
		}
	}

	// Child declared on the ROOT schema (replicating flatSchema's fields) — no embed.
	rootWithChild := core.NewTableSchema[*flatPerson]("flat_persons").
		ID("id").Field("Name", "name").Field("Email", "email").Field("Phone", "phone").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(core.NewTableSchema[mcLineRow]("mc_lines").ID("id").ParentID("user_id").
			Field("Qty", "qty").DeletedAt("deleted_at"))
	view := query.View("flat_persons").Version(1).Schema(rootWithChild)

	doc, err := query.NewComposer(eng).Compose(ctx, view, res.ID.Value())
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	seg := domain.PluralizeWord("mcLineRow")
	lines, ok := doc[seg].([]query.Document)
	if !ok {
		t.Fatalf("own child %q shape = %T (doc=%v)", seg, doc[seg], doc)
	}
	if len(lines) != 2 {
		t.Fatalf("auto-projected own children = %d, want 2", len(lines))
	}
	for _, l := range lines {
		if uid, ok := l["user_id"].(string); !ok || uid != res.ID.Value() {
			t.Fatalf("child user_id = %#v, want %q (BINARY(16) ParentID must decode to the root uuid)", l["user_id"], res.ID)
		}
	}
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

	view := query.View("flags").Version(1).Schema(flagSchema())
	doc, err := query.NewComposer(eng).Compose(ctx, view, id.String())
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
