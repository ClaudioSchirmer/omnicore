package core

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Subqueries: the right-hand side of a comparison stops being only literals.
// These tests pin the rendered SQL (a subquery's source is ALWAYS aliased, its
// own archive gate is automatic, an Outer reference is qualified without
// changing a byte of the enclosing statement), the placeholder numbering across
// nesting levels, and every refusal — a wrong shape must produce an error and no
// SQL, never SQL that cannot run.

// ─── fixtures ────────────────────────────────────────────────────────────────

type subUser struct {
	ID    domain.ID
	Name  string
	Email *string
	// Tag lives on the 1:1 satellite, not on the owner's table — a sibling
	// carries a subset of the SAME entity's fields.
	Tag string
}

type subRolePerm struct {
	ID     domain.ID
	RoleID domain.ID
}

type subPhone struct {
	ID     domain.ID
	UserID domain.ID
	Number string
}

// userSchema is the enclosing statement's schema: a plain entity with the
// archive marker declared, so the gate is observable.
func subUserSchema() *TableSchema {
	return NewTableSchema[subUser]("users").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		DeletedAt("deleted_at")
}

// phoneSchema is the MANY side of a 1:N relation — the shape Exists exists for.
func subPhoneSchema() *TableSchema {
	return NewTableSchema[subPhone]("phones").
		ID("id").
		Field("UserID", "user_id").
		Field("Number", "number").
		DeletedAt("deleted_at")
}

// rolePermSchema declares NO archive marker, which is how the "no DeletedAt, no
// gate" half of the rule is observable.
func subRolePermSchema() *TableSchema {
	return NewTableSchema[subRolePerm]("role_permissions").
		ID("id").
		Field("RoleID", "role_id")
}

// outerResolver resolves the enclosing statement's fields against a real schema
// (unlike testResolver, which answers with a schema-less ResolvedField) — an
// Outer reference renders through the schema the field lives on, so the fixture
// has to carry one.
func outerResolver(s *TableSchema) FieldResolver { return s.Resolve }

func compilePG(t *testing.T, e criteria.Expr, s *TableSchema) (string, []any) {
	t.Helper()
	sql, args, err := CompileWhere(e, outerResolver(s), testPGDialect{}, s.IDKindOf)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
	}
	return sql, args
}

func compileErrPG(t *testing.T, e criteria.Expr, s *TableSchema) error {
	t.Helper()
	sql, _, err := CompileWhere(e, outerResolver(s), testPGDialect{}, s.IDKindOf)
	if err == nil {
		t.Fatalf("expected a refusal, got SQL %q", sql)
	}
	if sql != "" {
		t.Errorf("a refused predicate still produced SQL: %q", sql)
	}
	return err
}

// ─── rendering ───────────────────────────────────────────────────────────────

func TestSubquery_Forms(t *testing.T) {
	users, phones, rp := subUserSchema(), subPhoneSchema(), subRolePermSchema()

	cases := []struct {
		name string
		e    criteria.Expr
		sql  string
		args int
	}{
		{
			"InSub",
			criteria.InSub("ID", criteria.Sub(phones).Select("UserID")),
			`id IN (SELECT phones_sq1.user_id FROM phones phones_sq1 ` +
				`WHERE phones_sq1.deleted_at IS NULL)`,
			0,
		},
		{
			"InSub with an inner predicate",
			criteria.InSub("ID", criteria.Sub(phones).Select("UserID").
				Where(criteria.Eq("Number", "555"))),
			`id IN (SELECT phones_sq1.user_id FROM phones phones_sq1 ` +
				`WHERE phones_sq1.number = $1 AND phones_sq1.deleted_at IS NULL)`,
			1,
		},
		{
			"EqSub over an aggregate",
			criteria.EqSub("Name", criteria.Sub(phones).SelectMax("Number")),
			`name = (SELECT MAX(phones_sq1.number) FROM phones phones_sq1 ` +
				`WHERE phones_sq1.deleted_at IS NULL)`,
			0,
		},
		{
			"GtSub with COUNT(*)",
			criteria.GtSub("Name", criteria.Sub(phones).SelectCount()),
			`name > (SELECT COUNT(*) FROM phones phones_sq1 ` +
				`WHERE phones_sq1.deleted_at IS NULL)`,
			0,
		},
		{
			"Exists projects nothing",
			criteria.Exists(criteria.Sub(phones).Where(criteria.Eq("Number", "555"))),
			`EXISTS (SELECT 1 FROM phones phones_sq1 ` +
				`WHERE phones_sq1.number = $1 AND phones_sq1.deleted_at IS NULL)`,
			1,
		},
		{
			"NotExists",
			criteria.NotExists(criteria.Sub(phones)),
			`NOT EXISTS (SELECT 1 FROM phones phones_sq1 ` +
				`WHERE phones_sq1.deleted_at IS NULL)`,
			0,
		},
		{
			"a source with no DeletedAt gets no gate",
			criteria.InSub("ID", criteria.Sub(rp).Select("RoleID")),
			`id IN (SELECT role_permissions_sq1.role_id FROM role_permissions role_permissions_sq1)`,
			0,
		},
		{
			"Any",
			criteria.GtSub("Name", criteria.Sub(rp).Select("RoleID").Any()),
			`name > ANY (SELECT role_permissions_sq1.role_id FROM role_permissions role_permissions_sq1)`,
			0,
		},
		{
			"All",
			criteria.GtSub("Name", criteria.Sub(rp).Select("RoleID").All()),
			`name > ALL (SELECT role_permissions_sq1.role_id FROM role_permissions role_permissions_sq1)`,
			0,
		},
		{
			"ordered and limited",
			criteria.EqSub("Name", criteria.Sub(rp).Select("RoleID").OrderByDesc("RoleID").Limit(1)),
			`name = (SELECT role_permissions_sq1.role_id FROM role_permissions role_permissions_sq1 ` +
				`ORDER BY role_permissions_sq1.role_id DESC LIMIT 1)`,
			0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args := compilePG(t, c.e, users)
			if sql != c.sql {
				t.Errorf("sql =\n  %s\nwant\n  %s", sql, c.sql)
			}
			if len(args) != c.args {
				t.Errorf("args = %d, want %d", len(args), c.args)
			}
		})
	}
}

func TestSubquery_ScopeOptOuts(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	t.Run("IncludeArchived drops the gate", func(t *testing.T) {
		sql, _ := compilePG(t, criteria.Exists(criteria.Sub(phones).IncludeArchived()), users)
		if strings.Contains(sql, "deleted_at") {
			t.Errorf("IncludeArchived still gated: %s", sql)
		}
	})

	t.Run("OnlyArchived inverts it", func(t *testing.T) {
		sql, _ := compilePG(t, criteria.Exists(criteria.Sub(phones).OnlyArchived()), users)
		if !strings.Contains(sql, `phones_sq1.deleted_at IS NOT NULL`) {
			t.Errorf("OnlyArchived gate missing: %s", sql)
		}
	})
}

// ─── correlation ─────────────────────────────────────────────────────────────

func TestSubquery_OuterReference(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	// The 1:N reverse filter: users holding at least one active phone.
	e := criteria.Exists(criteria.Sub(phones).
		Where(criteria.Eq("UserID", criteria.Outer("ID"))))

	sql, args := compilePG(t, e, users)
	want := `EXISTS (SELECT 1 FROM phones phones_sq1 ` +
		`WHERE phones_sq1.user_id = users.id AND phones_sq1.deleted_at IS NULL)`
	if sql != want {
		t.Errorf("sql =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("an outer reference bound %d args; it must bind none", len(args))
	}
}

func TestSubquery_EnclosingStatementRendersUnchanged(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	// The same enclosing predicate, with and without a correlated subquery beside
	// it. Whatever a subquery needs, it must not change how the statement around
	// it renders its own columns.
	plain, _ := compilePG(t, criteria.Eq("Name", "Bob"), users)
	withSub, _ := compilePG(t, criteria.And(
		criteria.Eq("Name", "Bob"),
		criteria.Exists(criteria.Sub(phones).Where(criteria.Eq("UserID", criteria.Outer("ID")))),
	), users)

	if plain != `name = $1` {
		t.Fatalf("baseline changed: %s", plain)
	}
	if !strings.HasPrefix(withSub, `(name = $1 AND `) {
		t.Errorf("the enclosing statement re-rendered its own column: %s", withSub)
	}
}

// ─── placeholder numbering ───────────────────────────────────────────────────

func TestSubquery_PlaceholderNumberingAcrossNesting(t *testing.T) {
	users, phones, rp := subUserSchema(), subPhoneSchema(), subRolePermSchema()

	e := criteria.And(
		criteria.Eq("Name", "Bob"),
		criteria.InSub("ID", criteria.Sub(phones).Select("UserID").
			Where(criteria.And(
				criteria.Eq("Number", "555"),
				criteria.InSub("UserID", criteria.Sub(rp).Select("RoleID").
					Where(criteria.Eq("RoleID", "r1"))),
			))),
	)

	sql, args := compilePG(t, e, users)
	if len(args) != 3 {
		t.Fatalf("args = %d, want 3", len(args))
	}
	for i, want := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(sql, want) {
			t.Errorf("placeholder %s missing (arg %d): %s", want, i+1, sql)
		}
	}
	// Emission order is what makes the numbering right: the outer literal first,
	// then the subquery's, then the nested one's.
	if got := []any{args[0], args[1], args[2]}; got[0] != "Bob" || got[1] != "555" || got[2] != "r1" {
		t.Errorf("args out of emission order: %v", got)
	}
	if strings.Index(sql, "$1") > strings.Index(sql, "$2") ||
		strings.Index(sql, "$2") > strings.Index(sql, "$3") {
		t.Errorf("placeholders are not in ascending textual order: %s", sql)
	}
}

func TestSubquery_PlaceholderNumberingContinuesAfterBase(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	// An UPDATE binds its SET list first; the predicate must continue that
	// numbering rather than restart, inside a subquery as much as outside it.
	sql, args, err := CompileWhereQualifiedFrom(
		criteria.InSub("ID", criteria.Sub(phones).Select("UserID").Where(criteria.Eq("Number", "555"))),
		outerResolver(users), testPGDialect{}, users.IDKindOf, ColQual{}, 2)
	if err != nil {
		t.Fatalf("CompileWhereQualifiedFrom: %v", err)
	}
	if len(args) != 1 {
		t.Fatalf("args = %d, want 1", len(args))
	}
	if !strings.Contains(sql, "$3") {
		t.Errorf("the subquery restarted its numbering instead of continuing after base=2: %s", sql)
	}
}

// ─── identity typing ─────────────────────────────────────────────────────────

func TestSubquery_InnerProbeLiftsTheSourcesIdentity(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()
	u := uuid.MustParse("018f8b2c-1d3e-7a9b-bc4d-5e6f7a8b9c0d")

	// A bare-string probe against the SUBQUERY's identity column binds in the
	// dialect's stored id form, read off the SUBQUERY's schema — not the
	// enclosing statement's.
	_, args, err := CompileWhere(
		criteria.Exists(criteria.Sub(phones).Where(criteria.Eq("UserID", u.String()))),
		outerResolver(users), testMySQLDialect{}, users.IDKindOf)
	if err != nil {
		t.Fatalf("CompileWhere: %v", err)
	}
	b, ok := args[0].([]byte)
	if !ok || len(b) != 16 {
		t.Fatalf("arg = %v (%T), want the 16-byte id form the subquery's schema declares", args[0], args[0])
	}
}

// ─── refusals ────────────────────────────────────────────────────────────────

func TestSubquery_Refusals(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	cases := []struct {
		name string
		e    criteria.Expr
		want string
	}{
		{
			"no projected item",
			criteria.InSub("ID", criteria.Sub(phones)),
			"no projected item",
		},
		{
			"two projected items",
			criteria.InSub("ID", criteria.Sub(phones).Select("UserID").SelectMax("Number")),
			"projects 2 items",
		},
		{
			"Exists with a Select",
			criteria.Exists(criteria.Sub(phones).Select("UserID")),
			"projects nothing",
		},
		{
			"no source",
			criteria.InSub("ID", criteria.Sub(nil).Select("UserID")),
			"Sub(nil)",
		},
		{
			"external schema",
			criteria.InSub("ID", criteria.Sub(NewExternalSchema("upstream").Field("K", "k")).Select("K")),
			"UPSTREAM service",
		},
		{
			"unknown field in the projection",
			criteria.InSub("ID", criteria.Sub(phones).Select("Nope")),
			"not a persisted field",
		},
		{
			"unknown field in the inner predicate",
			criteria.Exists(criteria.Sub(phones).Where(criteria.Eq("Nope", 1))),
			"not a persisted field",
		},
		{
			"Outer naming a field the enclosing statement lacks",
			criteria.Exists(criteria.Sub(phones).Where(criteria.Eq("UserID", criteria.Outer("Nope")))),
			"reaches exactly one level up",
		},
		{
			"Limit with no order",
			criteria.EqSub("Name", criteria.Sub(phones).Select("Number").Limit(1)),
			"no OrderBy",
		},
		{
			"an operator that takes no subquery",
			criteria.SubqueryComparison{Field: "Name", Op: criteria.OpLike, Sub: criteria.Sub(phones).Select("Number")},
			"does not take a subquery",
		},
		{
			"NinSub over a nullable column",
			criteria.NinSub("Name", criteria.Sub(users).Select("Email")),
			"NOT IN matches NO rows",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := compileErrPG(t, c.e, users)
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestSubquery_OuterOutsideASubqueryIsRefused(t *testing.T) {
	users := subUserSchema()
	err := compileErrPG(t, criteria.Eq("Name", criteria.Outer("ID")), users)
	if !strings.Contains(err.Error(), "outside a subquery") {
		t.Errorf("error = %q, want it to say the reference is outside a subquery", err)
	}
}

// A field of a SIBLING resolves in the enclosing statement, whose FROM carries
// the satellite, and must NOT resolve inside a subquery, whose FROM is one
// table. The message has to say which table it landed on, because the developer
// sees the same name working one line above.
func TestSubquery_SatelliteFieldIsRefusedInside(t *testing.T) {
	owner := NewTableSchema[subUser]("users").
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		DeletedAt("deleted_at").
		Sibling(NewSiblingSchema[subUser]("user_extras").Field("Tag", "tag"))

	if _, ok := owner.Resolve("Tag"); !ok {
		t.Fatal("fixture: the sibling field must resolve on the enclosing schema")
	}

	err := compileErrPG(t, criteria.InSub("ID", criteria.Sub(owner).Select("Tag")), owner)
	if !strings.Contains(err.Error(), "user_extras") || !strings.Contains(err.Error(), "ONE table") {
		t.Errorf("error = %q, want it to name the satellite table and the one-table rule", err)
	}
}

// ─── self-correlation ────────────────────────────────────────────────────────

// A subquery over the SAME table as the statement is exactly why the source is
// always aliased: without it, the inner FROM would shadow the outer one and the
// correlated reference would compare a row to itself.
func TestSubquery_SelfCorrelationIsUnambiguous(t *testing.T) {
	users := subUserSchema()

	sql, _ := compilePG(t, criteria.Exists(criteria.Sub(users).
		Where(criteria.And(
			criteria.Eq("Email", criteria.Outer("Email")),
			criteria.Ne("ID", criteria.Outer("ID")),
		))), users)

	want := `EXISTS (SELECT 1 FROM users users_sq1 ` +
		`WHERE (users_sq1.email = users.email AND users_sq1.id <> users.id) ` +
		`AND users_sq1.deleted_at IS NULL)`
	if sql != want {
		t.Errorf("sql =\n  %s\nwant\n  %s", sql, want)
	}
}

func TestSubAlias_HashesPastTheBudget(t *testing.T) {
	long := strings.Repeat("t", subAliasBudget)
	got := subAlias(long, 1)
	if len(got) > subAliasBudget {
		t.Errorf("alias %q is %d chars, over the budget", got, len(got))
	}
	if got == subAlias(strings.Repeat("u", subAliasBudget), 1) {
		t.Error("two different tables hashed to the same alias")
	}
	if short := subAlias("phones", 2); short != "phones_sq2" {
		t.Errorf("a short name must stay readable, got %q", short)
	}
}

// ─── the write path ──────────────────────────────────────────────────────────

func TestSubquery_WritePath(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	t.Run("another table renders on every engine", func(t *testing.T) {
		for _, d := range []Dialect{testPGDialect{}, testMySQLDialect{}} {
			sql, _, err := CompileWhereForWrite(
				criteria.InSub("ID", criteria.Sub(phones).Select("UserID")),
				outerResolver(users), d, users.IDKindOf, 0, "users")
			if err != nil {
				t.Fatalf("%T refused a subquery over another table: %v", d, err)
			}
			if !strings.Contains(sql, "phones") {
				t.Errorf("%T: subquery missing from the write predicate: %s", d, sql)
			}
		}
	})

	t.Run("the write target renders where the engine allows it", func(t *testing.T) {
		sql, _, err := CompileWhereForWrite(
			criteria.InSub("ID", criteria.Sub(users).Select("ID")),
			outerResolver(users), testPGDialect{}, users.IDKindOf, 0, "users")
		if err != nil {
			t.Fatalf("postgres refused a subquery on the write target: %v", err)
		}
		if !strings.Contains(sql, `users_sq1`) {
			t.Errorf("the self-referencing subquery did not render: %s", sql)
		}
	})

	t.Run("the write target is refused on MySQL, and only there", func(t *testing.T) {
		sql, _, err := CompileWhereForWrite(
			criteria.InSub("ID", criteria.Sub(users).Select("ID")),
			outerResolver(users), testMySQLDialect{}, users.IDKindOf, 0, "users")
		if err == nil {
			t.Fatalf("mysql accepted a subquery on the write target: %s", sql)
		}
		if sql != "" {
			t.Errorf("a refused write predicate still produced SQL: %q", sql)
		}
		if !strings.Contains(err.Error(), "1093") {
			t.Errorf("error = %q, want it to name the engine limitation", err)
		}
	})

	t.Run("a read is never subject to the write-target rule", func(t *testing.T) {
		if _, _, err := CompileWhere(
			criteria.InSub("ID", criteria.Sub(users).Select("ID")),
			outerResolver(users), testMySQLDialect{}, users.IDKindOf); err != nil {
			t.Errorf("mysql refused a self-referencing subquery on a READ: %v", err)
		}
	})
}

// ─── the other three dialects ────────────────────────────────────────────────

// The TREE is identical on every engine; what differs is the placeholder form,
// the identifier rendering and where the row cap goes. One subquery, four
// dialects, so a divergence shows up as a diff rather than as a surprise on the
// engine nobody ran locally.
func TestSubquery_PerDialectRendering(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()
	e := criteria.InSub("ID", criteria.Sub(phones).Select("UserID").
		Where(criteria.Eq("Number", "555")))

	cases := []struct {
		name    string
		dialect Dialect
		want    string
	}{
		{
			"postgres", testPGDialect{},
			`id IN (SELECT phones_sq1.user_id FROM phones phones_sq1 ` +
				`WHERE phones_sq1.number = $1 AND phones_sq1.deleted_at IS NULL)`,
		},
		{
			"mysql", testMySQLDialect{},
			"`id` IN (SELECT `phones_sq1`.`user_id` FROM `phones` `phones_sq1` " +
				"WHERE `phones_sq1`.`number` = ? AND `phones_sq1`.`deleted_at` IS NULL)",
		},
		{
			"sqlserver", testSQLServerDialect{},
			`[id] IN (SELECT [phones_sq1].[user_id] FROM [phones] [phones_sq1] ` +
				`WHERE [phones_sq1].[number] = @p1 AND [phones_sq1].[deleted_at] IS NULL)`,
		},
		{
			// Oracle folds identifiers to upper case — the derived alias goes
			// through the same QuoteIdent as every other identifier, so it folds
			// with them instead of standing out as the one lower-case name.
			"oracle", testOracleDialect{},
			`"ID" IN (SELECT "PHONES_SQ1"."USER_ID" FROM "PHONES" "PHONES_SQ1" ` +
				`WHERE "PHONES_SQ1"."NUMBER" = :1 AND "PHONES_SQ1"."DELETED_AT" IS NULL)`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := CompileWhere(e, outerResolver(users), c.dialect, users.IDKindOf)
			if err != nil {
				t.Fatalf("CompileWhere: %v", err)
			}
			if sql != c.want {
				t.Errorf("sql =\n  %s\nwant\n  %s", sql, c.want)
			}
			if len(args) != 1 {
				t.Errorf("args = %d, want 1", len(args))
			}
		})
	}
}

// The row cap is the one part of a subquery that is not a tail clause
// everywhere: SQL Server rewrites the SELECT head instead, and it must rewrite
// the SUBQUERY's head, not the statement's.
func TestSubquery_LimitPerDialect(t *testing.T) {
	users, rp := subUserSchema(), subRolePermSchema()
	e := criteria.EqSub("Name", criteria.Sub(rp).Select("RoleID").OrderBy("RoleID").Limit(1))

	cases := []struct {
		name    string
		dialect Dialect
		want    string
	}{
		{"postgres", testPGDialect{}, "LIMIT 1)"},
		{"mysql", testMySQLDialect{}, "LIMIT 1)"},
		{"oracle", testOracleDialect{}, "FETCH FIRST 1 ROWS ONLY)"},
		{"sqlserver", testSQLServerDialect{}, "(SELECT TOP 1 "},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, _, err := CompileWhere(e, outerResolver(users), c.dialect, users.IDKindOf)
			if err != nil {
				t.Fatalf("CompileWhere: %v", err)
			}
			if !strings.Contains(sql, c.want) {
				t.Errorf("sql = %s\nwant it to contain %q", sql, c.want)
			}
		})
	}
}

// ─── coverage of the branches the happy paths do not reach ───────────────────

// A subquery two levels deep referencing the MIDDLE scope: the outer reference
// must carry that scope's alias, not the statement's table.
func TestSubquery_OuterReferenceFromTwoLevelsDeep(t *testing.T) {
	users, phones, rp := subUserSchema(), subPhoneSchema(), subRolePermSchema()

	e := criteria.Exists(criteria.Sub(phones).
		Where(criteria.Exists(criteria.Sub(rp).
			Where(criteria.Eq("RoleID", criteria.Outer("UserID"))))))

	sql, _ := compilePG(t, e, users)
	if !strings.Contains(sql, "role_permissions_sq2.role_id = phones_sq1.user_id") {
		t.Errorf("the inner reference did not resolve against the MIDDLE scope: %s", sql)
	}
}

func TestSubquery_NotInNullableGuard_OnlyFiresWhenItCan(t *testing.T) {
	users, phones := subUserSchema(), subPhoneSchema()

	// A non-nullable projected column is fine.
	if _, _, err := CompileWhere(
		criteria.NinSub("Name", criteria.Sub(phones).Select("Number")),
		outerResolver(users), testPGDialect{}, users.IDKindOf); err != nil {
		t.Errorf("NinSub over a non-nullable column was refused: %v", err)
	}

	// An aggregate projects no column to inspect, so the guard stands down
	// rather than guessing.
	if _, _, err := CompileWhere(
		criteria.NinSub("Name", criteria.Sub(phones).SelectCount()),
		outerResolver(users), testPGDialect{}, users.IDKindOf); err != nil {
		t.Errorf("NinSub over COUNT(*) was refused: %v", err)
	}
}

func TestCompileWhereForWrite_NilPredicate(t *testing.T) {
	sql, args, err := CompileWhereForWrite(nil, outerResolver(subUserSchema()), testPGDialect{}, nil, 0, "users")
	if err != nil || sql != "" || args != nil {
		t.Errorf("a nil write predicate yielded (%q, %v, %v), want the empty fragment", sql, args, err)
	}
}

func TestFieldIsNullable(t *testing.T) {
	users := subUserSchema()

	for _, c := range []struct {
		field           string
		nullable, known bool
	}{
		{"Email", true, true},  // *string
		{"Name", false, true},  // string
		{"ID", false, true},    // domain.ID, the managed slot
		{"Nope", false, false}, // not a field of this schema
	} {
		nullable, known := users.fieldIsNullable(c.field)
		if nullable != c.nullable || known != c.known {
			t.Errorf("%s: (%v, %v), want (%v, %v)", c.field, nullable, known, c.nullable, c.known)
		}
	}

	// A type-less schema has no struct to ask, so the question is unanswerable
	// rather than answered false.
	if _, known := NewExternalSchema("up").Field("K", "k").fieldIsNullable("K"); known {
		t.Error("a type-less schema claimed to know its nullability")
	}
	if _, known := (*TableSchema)(nil).fieldIsNullable("K"); known {
		t.Error("a nil schema claimed to know its nullability")
	}
}
