package hydrate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// The hydrator reads through the engine's neutral surface only —
// Querier().QueryMaps + Dialect() — so these fixtures script a fake engine whose
// QueryMaps answers each SELECT with the column-keyed rows it would yield. That
// is the whole seam: no driver, no database, and the SQL each helper emits is
// visible to the assertions as the string it was asked for.

// ─── entities + schemas ──────────────────────────────────────────────────────

type orderEnt struct {
	domain.BaseEntity
	Name   string
	Active bool
	// Channel is owned by a SIBLING table (the owner's row, partitioned).
	Channel string
}

func (e *orderEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *orderEnt) BuildRules(string, domain.Service, *domain.Rules) {}

type lineEnt struct {
	domain.BaseEntity
	Label string
}

func (e *lineEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *lineEnt) BuildRules(string, domain.Service, *domain.Rules) {}

// CollectionName is the segment this child occupies inside the aggregate — the
// DOMAIN names it, and the read side nests the collection under exactly this.
func (e lineEnt) CollectionName() string { return "Lines" }

// lineSchema is an own aggregate child, joined root.id -> lines.order_id.
func lineSchema() *core.TableSchema {
	return core.NewTableSchema[*lineEnt]("lines").
		ID("id").
		ParentID("order_id").
		Field("Label", "label").
		DeletedAt("deleted_at")
}

// siblingSchema partitions the owner's row: same primary key, extra columns.
func siblingSchema() *core.TableSchema {
	// A sibling declares no primary key: it BORROWS the owner's, joined on the
	// same physical ID column name.
	return core.NewSiblingSchema[*orderEnt]("order_channels").
		Field("Channel", "channel")
}

// rootSchema is the plain aggregate: scalars, a bool column (the coercion case),
// managed timestamps, one own child and one sibling.
func rootSchema() *core.TableSchema {
	return core.NewTableSchema[*orderEnt]("orders").
		ID("id").
		Field("Name", "name").
		Field("Active", "active").
		DeletedAt("deleted_at").
		Revision("revision").
		Child(lineSchema()).
		Sibling(siblingSchema())
}

// flatSchema is the simplest possible source: no children, no siblings, no
// managed columns beyond the id.
func flatSchema() *core.TableSchema {
	return core.NewTableSchema[*lineEnt]("notes").ID("id").Field("Label", "label")
}

// ─── shared base ─────────────────────────────────────────────────────────────

type personEnt struct {
	domain.BaseEntity
	Name string
}

func (e *personEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *personEnt) BuildRules(string, domain.Service, *domain.Rules) {}

type addressEnt struct {
	domain.BaseEntity
	City string
}

func (e *addressEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *addressEnt) BuildRules(string, domain.Service, *domain.Rules) {}

func (e addressEnt) CollectionName() string { return "Addresses" }

// baseSchema is a shared identity carrying its own native child collection.
func baseSchema() *core.TableSchema {
	return core.NewSharedBaseSchema("persons").
		Revision("revision").
		ID("id").
		Field("Name", "name").
		NaturalID("name").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		Child(core.NewTableSchema[*addressEnt]("addresses").
			ID("id").
			ParentID("person_id").
			Field("City", "city").
			DeletedAt("deleted_at"))
}

type roleEnt struct {
	domain.BaseEntity
	Name  string // the base's shared field, scanned into the ROLE struct
	Grade string
}

func (e *roleEnt) Modes() []domain.EntityMode                       { return []domain.EntityMode{domain.ModeInsert} }
func (e *roleEnt) BuildRules(string, domain.Service, *domain.Rules) {}

// roleSchema is a specialization of the shared base, linked by person_id, with
// managed columns OF ITS OWN — the collision the base merge must resolve in
// favour of the role.
func roleSchema() *core.TableSchema {
	return core.NewTableSchema[*roleEnt]("students").
		ID("id").
		Field("Grade", "grade").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		Revision("revision").
		SharedBase(baseSchema(), "person_id")
}

// bareRoleSchema is a role declaring NO managed columns of its own — the other
// side of the skip guard: with nothing to shadow, the BASE's managed columns are
// the only ones there, so they must land.
func bareRoleSchema() *core.TableSchema {
	return core.NewTableSchema[*roleEnt]("interns").
		ID("id").
		Field("Grade", "grade").
		SharedBase(baseSchema(), "person_id")
}

// ─── the scripted engine ─────────────────────────────────────────────────────

// scriptedEngine answers QueryMaps from a table-keyed script: the first table
// name appearing in the SQL picks the rows. It also records every statement, so
// a test can assert the SHAPE of the read (one IN per table, the gate applied)
// rather than only its result.
type scriptedEngine struct {
	rows map[string][]map[string]any
	err  error
	sqls []string
	args [][]any
}

func newScripted(rows map[string][]map[string]any) *scriptedEngine {
	return &scriptedEngine{rows: rows}
}

func (e *scriptedEngine) QueryMaps(_ context.Context, sql string, args ...any) ([]map[string]any, error) {
	e.sqls = append(e.sqls, sql)
	e.args = append(e.args, args)
	if e.err != nil {
		return nil, e.err
	}
	for table, rows := range e.rows {
		if strings.Contains(sql, " FROM "+table) {
			out := make([]map[string]any, len(rows))
			for i, r := range rows {
				cp := map[string]any{}
				for k, v := range r {
					cp[k] = v
				}
				out[i] = cp
			}
			return out, nil
		}
	}
	return nil, nil
}

func (e *scriptedEngine) Query(context.Context, string, ...any) (core.Rows, error) { return nil, nil }
func (e *scriptedEngine) QueryRow(context.Context, string, ...any) core.Row        { return nil }
func (e *scriptedEngine) Exec(context.Context, string, ...any) error               { return nil }

func (e *scriptedEngine) Querier() core.Querier { return e }
func (e *scriptedEngine) Dialect() core.Dialect { return pgLikeDialect{} }

func (scriptedEngine) Insert(persistence.RequestContext, domain.Insertable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (scriptedEngine) Update(persistence.RequestContext, domain.Updatable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (scriptedEngine) Archive(persistence.RequestContext, domain.Archivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (scriptedEngine) Unarchive(persistence.RequestContext, domain.Unarchivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (scriptedEngine) Delete(persistence.RequestContext, domain.Deletable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (e *scriptedEngine) WithAudit(*audit.Config, *slog.Logger, []string) core.RelationalEngine {
	return e
}
func (e *scriptedEngine) WithEventPublisher(events.Publisher) core.RelationalEngine { return e }
func (e *scriptedEngine) AcquireRebuildLock(context.Context, string) (core.RebuildLock, error) {
	return nil, nil
}
func (scriptedEngine) Close() {}

// pgLikeDialect renders Postgres-shaped SQL — enough for the assertions to read
// as the statement a real backend would receive.
type pgLikeDialect struct{}

func (pgLikeDialect) Placeholder(n int) string      { return fmt.Sprintf("$%d", n) }
func (pgLikeDialect) QuoteIdent(name string) string { return name }
func (pgLikeDialect) EncodeArg(val any) any {
	if id, ok := val.(domain.ID); ok {
		return id.Value()
	}
	return val
}
func (pgLikeDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (pgLikeDialect) ILikeClause(col, ph string) string   { return col + " ILIKE " + ph }
func (pgLikeDialect) LikeClause(col, ph string) string    { return col + " LIKE " + ph }
func (pgLikeDialect) NowExpr() string                     { return "NOW()" }
func (pgLikeDialect) UTCNowExpr() string                  { return "NOW() AT TIME ZONE 'UTC'" }
func (pgLikeDialect) ApplyLimit(sql string, n int) string { return fmt.Sprintf("%s LIMIT %d", sql, n) }
func (pgLikeDialect) ApplyLimitOffset(sql string, limit, offset int) string {
	return fmt.Sprintf("%s LIMIT %d OFFSET %d", sql, limit, offset)
}
func (pgLikeDialect) Savepoint(name string) string                                    { return "SAVEPOINT " + name }
func (pgLikeDialect) RollbackToSavepoint(name string) string                          { return "ROLLBACK TO SAVEPOINT " + name }
func (pgLikeDialect) ReleaseSavepoint(name string) string                             { return "RELEASE SAVEPOINT " + name }
func (pgLikeDialect) IsUniqueViolation(error) (string, bool)                          { return "", false }
func (pgLikeDialect) IsForeignKeyViolation(error) (string, bool)                      { return "", false }
func (pgLikeDialect) BuildUpsert(string, []string, []string, []core.UpsertSet) string { return "" }

func (pgLikeDialect) AllowsSubqueryOnWriteTarget() bool { return true }
