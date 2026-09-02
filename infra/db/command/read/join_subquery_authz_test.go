package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The whole statement, not the fragment.
//
// An authorization read — every permission a user holds, directly through their
// roles and indirectly through their groups' roles — is the shape that motivated
// subqueries: five tables, two of them reached by declared joins and three by
// nested subqueries, one of those correlated. It used to take four round trips
// because a predicate could only compare against literals.
//
// The unit tests in infra/db/core pin the predicate. This one pins what the
// LOADER emits around it — the SELECT list, the join blocks under their derived
// aliases, the anchor qualification a declared join forces on every column, and
// the archive gates: one per subquery source plus the anchor's, none of them
// written by the caller.

type authzRolePermission struct {
	domain.AggregateRoot
	RoleID       domain.ID
	PermissionID domain.ID

	// Reached across the two declared joins.
	RoleKey      string
	ResourceName string
	ActionName   string
}

func (e *authzRolePermission) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert}
}
func (e *authzRolePermission) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *authzRolePermission) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }

func (e *authzNamed) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *authzNamed) BuildRules(string, domain.Service, *domain.Rules) {}
func (e *authzNamed) GetAggregateRoot() *domain.AggregateRoot          { return &e.AggregateRoot }

func authzRolePermissionSchema() *TableSchema {
	return NewTableSchema[*authzRolePermission]("role_permissions").
		ID("id").
		Field("RoleID", "role_id").
		Field("PermissionID", "permission_id").
		DeletedAt("deleted_at")
}

// The join targets. Both are ordinary entity schemas in a real service; a join
// takes the reduced form, which is the point of AsDirectSchema.
func authzRoleSchema() *TableSchema {
	return NewTableSchema[*authzNamed]("roles").
		ID("id").
		Field("Key", "role_key").
		DeletedAt("deleted_at").
		AsDirectSchema()
}

func authzPermissionSchema() *TableSchema {
	return NewTableSchema[*authzNamed]("permissions").
		ID("id").
		Field("Resource", "resource_name").
		Field("Action", "action_name").
		DeletedAt("deleted_at").
		AsDirectSchema()
}

type authzNamed struct {
	domain.AggregateRoot
	Key      string
	Resource string
	Action   string
}

// The subquery sources. Each declares DeletedAt, so each carries its own gate
// without the caller writing one.
type authzLink struct {
	ID      domain.ID
	UserID  domain.ID
	RoleID  domain.ID
	GroupID domain.ID
}

func authzLinkSchema(table string, cols ...string) *TableSchema {
	s := core.NewDirectSchema[authzLink](table).ID("id").DeletedAt("deleted_at")
	for _, c := range cols {
		switch c {
		case "user_id":
			s = s.Field("UserID", "user_id")
		case "role_id":
			s = s.Field("RoleID", "role_id")
		case "group_id":
			s = s.Field("GroupID", "group_id")
		}
	}
	return s
}

// authzCriteria is the §7 query: the roles a user holds directly, OR the roles
// they hold through a group that is itself active.
func authzCriteria(userID string) *criteria.Query {
	userRoles := authzLinkSchema("user_roles", "user_id", "role_id")
	groupRoles := authzLinkSchema("group_roles", "group_id", "role_id")
	userGroups := authzLinkSchema("user_groups", "user_id", "group_id")
	groups := core.NewDirectSchema[authzLink]("groups").ID("id").DeletedAt("deleted_at")

	return criteria.Where(criteria.Or(
		criteria.InSub("RoleID",
			criteria.Sub(userRoles).Select("RoleID").
				Where(criteria.Eq("UserID", userID))),

		criteria.InSub("RoleID",
			criteria.Sub(groupRoles).Select("RoleID").
				Where(criteria.InSub("GroupID",
					criteria.Sub(userGroups).Select("GroupID").
						Where(criteria.And(
							criteria.Eq("UserID", userID),
							criteria.Exists(criteria.Sub(groups).
								Where(criteria.Eq("ID", criteria.Outer("GroupID")))),
						))))),
	))
}

func authzLoader(t *testing.T) *AggregateLoader[*authzRolePermission] {
	t.Helper()
	return NewAggregateLoader[*authzRolePermission](nil, func() *authzRolePermission { return &authzRolePermission{} }).
		WithContextName("RolePermission").
		WithSchema(authzRolePermissionSchema()).
		WithJoins(
			InnerJoin(authzRoleSchema()).On("role_id").
				Field("RoleKey", "role_key"),
			InnerJoin(authzPermissionSchema()).On("permission_id").
				Field("ResourceName", "resource_name").
				Field("ActionName", "action_name"),
		)
}

func authzSQL(t *testing.T, q *criteria.Query) (string, []any) {
	t.Helper()
	var seenSQL string
	var seenArgs []any
	eng := fakeEngine(func(sql string, args []any) (Rows, error) {
		if seenSQL == "" {
			seenSQL, seenArgs = sql, args
		}
		return &fakeDBRows{}, nil
	})
	l := authzLoader(t)
	l.eng = eng
	if _, err := l.FindAll(context.Background(), q); err != nil {
		t.Fatalf("FindAll: %v", err)
	}
	return seenSQL, seenArgs
}

func TestAuthzQuery_WholeStatement(t *testing.T) {
	sql, args := authzSQL(t, authzCriteria("7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"))

	// ── the join blocks, under aliases derived from the foreign keys ──────────
	for _, want := range []string{
		"INNER JOIN roles j_role_id ON j_role_id.id = role_permissions.role_id",
		"INNER JOIN permissions j_permission_id ON j_permission_id.id = role_permissions.permission_id",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing join block:\n  want %s\n  in   %s", want, sql)
		}
	}

	// ── the SELECT list: the anchor's own columns qualified by its table (a
	//    declared join makes the anchor a namespace among others), plus one
	//    column per join field under that join's alias ─────────────────────────
	for _, want := range []string{
		"role_permissions.role_id",
		"role_permissions.permission_id",
		"j_role_id.role_key",
		"j_permission_id.resource_name",
		"j_permission_id.action_name",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing SELECT item %q in:\n%s", want, sql)
		}
	}

	// ── the predicate: two subqueries under OR, the second nesting a third,
	//    which carries a correlated EXISTS ─────────────────────────────────────
	wantWhere := "WHERE (role_permissions.role_id IN (" +
		"SELECT user_roles_sq1.role_id FROM user_roles user_roles_sq1 " +
		"WHERE user_roles_sq1.user_id = $1 AND user_roles_sq1.deleted_at IS NULL)" +
		" OR role_permissions.role_id IN (" +
		"SELECT group_roles_sq1.role_id FROM group_roles group_roles_sq1 " +
		"WHERE group_roles_sq1.group_id IN (" +
		"SELECT user_groups_sq2.group_id FROM user_groups user_groups_sq2 " +
		"WHERE (user_groups_sq2.user_id = $2 AND EXISTS (" +
		"SELECT 1 FROM groups groups_sq3 " +
		"WHERE groups_sq3.id = user_groups_sq2.group_id AND groups_sq3.deleted_at IS NULL)) " +
		"AND user_groups_sq2.deleted_at IS NULL) " +
		"AND group_roles_sq1.deleted_at IS NULL)) " +
		"AND role_permissions.deleted_at IS NULL"
	if !strings.Contains(sql, wantWhere) {
		t.Errorf("predicate mismatch.\n got: %s\nwant contains:\n%s", sql, wantWhere)
	}

	// ── five archive gates, none of them written by the caller ────────────────
	if got := strings.Count(sql, "deleted_at IS NULL"); got != 5 {
		t.Errorf("archive gates = %d, want 5 (the anchor plus one per subquery source):\n%s", got, sql)
	}

	// ── the user id is bound twice, in emission order, and nothing else is ────
	if len(args) != 2 {
		t.Fatalf("args = %d (%v), want 2", len(args), args)
	}
	for i, a := range args {
		if a != "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51" {
			t.Errorf("arg %d = %v, want the user id", i+1, a)
		}
	}
}

// The traversal is NOT archive-gated on the target: the scope governs which
// role_permissions rows come back, never the rows a foreign key reaches into.
// Five gates, and none of them on roles or permissions.
func TestAuthzQuery_JoinTargetsAreNotGated(t *testing.T) {
	sql, _ := authzSQL(t, authzCriteria("7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"))
	for _, alias := range []string{"j_role_id.deleted_at", "j_permission_id.deleted_at"} {
		if strings.Contains(sql, alias) {
			t.Errorf("the join target must not be archive-gated, found %q in:\n%s", alias, sql)
		}
	}
}

// The archive scope of the OUTER query moves the anchor's gate and leaves every
// subquery's own gate alone — each subquery carries its own scope.
func TestAuthzQuery_OuterScopeDoesNotReachIntoTheSubqueries(t *testing.T) {
	sql, _ := authzSQL(t, authzCriteria("7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51").IncludeArchived())

	// The column itself stays in the SELECT list — it is a mapped field. What must
	// be gone is the GATE.
	if strings.Contains(sql, "role_permissions.deleted_at IS NULL") {
		t.Errorf("IncludeArchived left the anchor gated:\n%s", sql)
	}
	if got := strings.Count(sql, "deleted_at IS NULL"); got != 4 {
		t.Errorf("subquery gates = %d, want the 4 that belong to the sources:\n%s", got, sql)
	}
}

// A join field is addressable in the predicate under the Go name the join
// declared — including beside a subquery, where the anchor and the joined
// namespace must each stay qualified by their own side.
func TestAuthzQuery_JoinFieldBesideASubquery(t *testing.T) {
	userRoles := authzLinkSchema("user_roles", "user_id", "role_id")
	q := criteria.Where(criteria.And(
		criteria.Eq("RoleKey", "admin"),
		criteria.InSub("RoleID", criteria.Sub(userRoles).Select("RoleID").
			Where(criteria.Eq("UserID", "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"))),
	))

	sql, args := authzSQL(t, q)
	if !strings.Contains(sql, "j_role_id.role_key = $1") {
		t.Errorf("the join field must resolve through its alias:\n%s", sql)
	}
	if !strings.Contains(sql, "role_permissions.role_id IN (SELECT user_roles_sq1.role_id") {
		t.Errorf("the anchor side of the subquery must stay qualified by its table:\n%s", sql)
	}
	if len(args) != 2 || args[0] != "admin" || args[1] != "7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51" {
		t.Errorf("args = %v, want [admin u-1] in emission order", args)
	}
}

// Ordering and paging ride on top of a subquery predicate untouched — the
// envelope is not part of the predicate algebra.
func TestAuthzQuery_EnvelopeRidesOnTop(t *testing.T) {
	sql, _ := authzSQL(t, authzCriteria("7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51").OrderBy("RoleID").Limit(50))

	if !strings.Contains(sql, "ORDER BY role_permissions.role_id ASC") {
		t.Errorf("ordering missing or unqualified:\n%s", sql)
	}
	if !strings.Contains(sql, "LIMIT 50") {
		t.Errorf("row cap missing:\n%s", sql)
	}
	if strings.Index(sql, "LIMIT 50") < strings.Index(sql, "user_roles_sq1") {
		t.Errorf("the cap must close the statement, not the subquery:\n%s", sql)
	}
}
