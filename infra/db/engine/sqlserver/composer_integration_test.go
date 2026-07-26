//go:build integration && sqlserver

package sqlserver

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// The backend-neutral Composer reads the SQL Server engine through the read
// seam (Querier.QueryMaps + Dialect), so a Mongo view composes the same way it
// does on the other engines. This proves against a real SQL Server:
//
//	go test -tags=integration,sqlserver ./infra/db/engine/sqlserver/ -count=1
//
//   - QueryMaps decodes a BINARY(16) column back to a canonical uuid string;
//   - the root fetch encodes the (uuid-string) key into the 16-byte form so
//     the WHERE matches the stored BINARY(16) PK;
//   - the own-child projection re-encodes the parent id extracted from the
//     composed root doc (a string) back into bytes to match the child FK;
//   - a BIT column arrives as a native Go bool (no coercion needed — the
//     TINYINT(1) int64 path is MySQL-only).

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

type mcLineRow struct {
	ID  string
	Qty int
}

// TestSQLServerComposer_OwnChild proves the own-child auto path: the child is
// declared on the ROOT schema (no EmbedMany) and projects automatically,
// joined root.PK → child.FK with the BINARY(16) id re-encoded for the WHERE.
func TestSQLServerComposer_OwnChild(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE mc_lines (
		id BINARY(16) NOT NULL PRIMARY KEY,
		user_id BINARY(16) NOT NULL,
		qty INT NOT NULL,
		deleted_at DATETIME2(6) NULL
	)`); err != nil {
		t.Fatalf("create mc_lines: %v", err)
	}

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
			`INSERT INTO mc_lines (id, user_id, qty) VALUES (@p1, @p2, @p3)`,
			childID[:], rootBytes, qty); err != nil {
			t.Fatalf("insert line: %v", err)
		}
	}

	// Child declared on the ROOT schema (replicating flatSchema's fields) — no embed.
	rootWithChild := core.NewTableSchema[*flatPerson]("flat_persons").
		PK("id").Field("Name", "name").Field("Email", "email").Field("Phone", "phone").
		SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(core.NewTableSchema[mcLineRow]("mc_lines").PK("id").FK("user_id").
			Field("Qty", "qty").SoftDelete("deleted_at"))
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
			t.Fatalf("child user_id = %#v, want %q (BINARY(16) FK must decode to the root uuid)", l["user_id"], res.ID)
		}
	}
}

// SQL Server stores booleans as BIT and go-mssqldb yields a native Go bool on
// the dynamic compose read — the composer must hand the schema-declared bool
// column to BSON as a real bool. This is the engine where NO coercion is
// needed (MySQL's TINYINT(1) int64 path); the test locks that the value is a
// bool end to end, so a $jsonSchema bsonType:"bool" validator accepts it.
func TestSQLServerComposer_BoolColumn(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE flags (
		id BINARY(16) NOT NULL PRIMARY KEY,
		active BIT NOT NULL,
		name NVARCHAR(255) NOT NULL
	)`); err != nil {
		t.Fatalf("create flags: %v", err)
	}

	id := uuid.New()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO flags (id, active, name) VALUES (@p1, @p2, @p3)`, id[:], true, "on"); err != nil {
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
		t.Fatalf("active = %#v (%T), want bool(true)", doc["active"], doc["active"])
	}
	if got, ok := doc["name"].(string); !ok || got != "on" {
		t.Fatalf("name = %#v, want string \"on\"", doc["name"])
	}
}
