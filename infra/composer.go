package infra

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Composer turns a ViewDefinition + a root key into the composed Mongo document
// the SyncEngine upserts. The document is column-keyed (a physical mirror of
// PostgreSQL); the Go↔column translation happens at read time in the reader.
//
// Per-source physical names come from each source's TableSchema (root via
// View.Schema, embeds via Source.Schema): the root key + each embed's key are
// the source's PK column, and the soft-delete filter uses the source's
// soft-delete column. A schema-less source falls back to id / deleted_at.
//
// Dispatch per embed on Source.IsMongo():
//   - PG source (FromSchema over a type-anchored schema): fetchRow / fetchWhere
//     against the connection pool.
//   - Mongo source (FromSchema over a type-less NewExternalSchema):
//     MongoDB.FindManyByField against the local DB.
type Composer struct {
	pg    *Postgres
	mongo *MongoDB
}

// NewComposer builds a Composer with PG dispatch only.
func NewComposer(pg *Postgres) *Composer {
	return &Composer{pg: pg}
}

// NewComposerWithMongo builds a Composer that dispatches PG sources via the pool
// AND Mongo sources via the supplied handle.
func NewComposerWithMongo(pg *Postgres, mongo *MongoDB) *Composer {
	return &Composer{pg: pg, mongo: mongo}
}

// schemaPK / schemaSoftDelete read the source's physical PK + soft-delete column
// straight from its TableSchema. The schema is mandatory on every view (root and
// embed), so there is no convention fallback — a view declared without a schema
// is rejected at boot, not silently mapped to "id"/"deleted_at".
func schemaPK(s *TableSchema) string                 { return s.PKColumn() }
func schemaSoftDelete(s *TableSchema) (string, bool) { return s.softDeleteColumn() }

func (c *Composer) Compose(ctx context.Context, view *ViewDefinition, rootID string) (bson.M, error) {
	includeArchived := !view.deleteOnArchive
	sd, _ := schemaSoftDelete(view.schema)
	row, err := c.fetchRow(ctx, view.rootTable, schemaPK(view.schema), rootID, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	if err := c.applyEmbeds(ctx, row, schemaPK(view.schema), view.embeds, includeArchived); err != nil {
		return nil, err
	}
	return row, nil
}

func (c *Composer) ComposeAll(ctx context.Context, view *ViewDefinition) ([]bson.M, error) {
	includeArchived := !view.deleteOnArchive
	sd, _ := schemaSoftDelete(view.schema)
	rows, err := c.fetchAll(ctx, view.rootTable, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := c.applyEmbeds(ctx, row, schemaPK(view.schema), view.embeds, includeArchived); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// applyEmbeds resolves each embed of the parent doc. parentPK is the parent
// source's PK column — the value matched against an EmbedMany child's FK.
func (c *Composer) applyEmbeds(ctx context.Context, doc bson.M, parentPK string, embeds []embedDef, includeArchived bool) error {
	for _, e := range embeds {
		if err := c.fetchEmbed(ctx, doc, parentPK, e, includeArchived); err != nil {
			return err
		}
	}
	return nil
}

func (c *Composer) fetchEmbed(ctx context.Context, doc bson.M, parentPK string, e embedDef, includeArchived bool) error {
	if e.source.IsMongo() {
		return c.fetchMongoEmbed(ctx, doc, parentPK, e)
	}
	return c.fetchPGEmbed(ctx, doc, parentPK, e, includeArchived)
}

func (c *Composer) fetchPGEmbed(ctx context.Context, doc bson.M, parentPK string, e embedDef, includeArchived bool) error {
	srcPK := schemaPK(e.source.schema)
	sd, _ := schemaSoftDelete(e.source.schema)

	if e.many {
		// One-to-many: the child holds the FK back to the parent; the value is
		// the parent's PK column.
		id, ok := doc[parentPK]
		if !ok || id == nil {
			return nil
		}
		idStr := fmt.Sprintf("%v", id)
		// One-to-many join key is the child FK declared on the source schema.
		rows, err := c.fetchWhere(ctx, e.source.table, e.JoinColumn(), idStr, sd, includeArchived)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := c.applyEmbeds(ctx, row, srcPK, e.source.embeds, includeArchived); err != nil {
				return err
			}
		}
		doc[e.field] = rows
		return nil
	}

	// One-to-one: the parent holds the FK pointing to the source's PK (.On).
	fk, ok := doc[e.JoinColumn()]
	if !ok || fk == nil {
		return nil
	}
	fkStr := fmt.Sprintf("%v", fk)
	row, err := c.fetchRow(ctx, e.source.table, srcPK, fkStr, sd, includeArchived)
	if err != nil {
		return err
	}
	if row != nil {
		if err := c.applyEmbeds(ctx, row, srcPK, e.source.embeds, includeArchived); err != nil {
			return err
		}
		doc[e.field] = row
	}
	return nil
}

func (c *Composer) fetchMongoEmbed(ctx context.Context, doc bson.M, parentPK string, e embedDef) error {
	if c.mongo == nil {
		return fmt.Errorf("composer: view embed on Mongo collection %q requires a MongoDB handle "+
			"(builder constructed without NewComposerWithMongo)", e.source.table)
	}
	srcPK := schemaPK(e.source.schema)
	if e.many {
		id, ok := doc[parentPK]
		if !ok || id == nil {
			return nil
		}
		docs, err := c.mongo.FindManyByField(ctx, e.source.table, e.JoinColumn(), id)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if err := c.applyEmbeds(ctx, d, srcPK, e.source.embeds, false); err != nil {
				return err
			}
		}
		doc[e.field] = docs
		return nil
	}
	fk, ok := doc[e.JoinColumn()]
	if !ok || fk == nil {
		return nil
	}
	docs, err := c.mongo.FindManyByField(ctx, e.source.table, "_id", fk)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}
	row := docs[0]
	if err := c.applyEmbeds(ctx, row, srcPK, e.source.embeds, false); err != nil {
		return err
	}
	doc[e.field] = row
	return nil
}

// buildFetchSQL builds the SELECT, applying the soft-delete filter on sdCol when
// the source has soft-delete AND archived rows are excluded.
func buildFetchSQL(verb, table, keyCol, sdCol string, includeArchived bool) string {
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + validIdentifier(sdCol) + " IS NULL"
	}
	if keyCol == "" {
		// fetchAll: no key predicate.
		if cond == "" {
			return fmt.Sprintf("SELECT * FROM %s", validIdentifier(table))
		}
		return fmt.Sprintf("SELECT * FROM %s WHERE %s IS NULL", validIdentifier(table), validIdentifier(sdCol))
	}
	suffix := ""
	if verb == "row" {
		suffix = " LIMIT 1"
	}
	return fmt.Sprintf("SELECT * FROM %s WHERE %s = $1%s%s",
		validIdentifier(table), validIdentifier(keyCol), cond, suffix)
}

func (c *Composer) fetchRow(ctx context.Context, table, keyCol, keyVal, sdCol string, includeArchived bool) (bson.M, error) {
	rows, err := c.pg.pool.Query(ctx, buildFetchSQL("row", table, keyCol, sdCol, includeArchived), keyVal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results, err := pgxRowsToMaps(rows)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	return results[0], nil
}

func (c *Composer) fetchWhere(ctx context.Context, table, keyCol, keyVal, sdCol string, includeArchived bool) ([]bson.M, error) {
	rows, err := c.pg.pool.Query(ctx, buildFetchSQL("where", table, keyCol, sdCol, includeArchived), keyVal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgxRowsToMaps(rows)
}

func (c *Composer) fetchAll(ctx context.Context, table, sdCol string, includeArchived bool) ([]bson.M, error) {
	rows, err := c.pg.pool.Query(ctx, buildFetchSQL("all", table, "", sdCol, includeArchived))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgxRowsToMaps(rows)
}

func pgxRowsToMaps(rows pgx.Rows) ([]bson.M, error) {
	fields := rows.FieldDescriptions()
	var result []bson.M
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		m := make(bson.M, len(fields))
		for i, f := range fields {
			m[f.Name] = normalizeSQLValue(vals[i])
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// NormalizeSQLValue is the exported counterpart of normalizeSQLValue, used by
// the admin replay CLI.
func NormalizeSQLValue(v any) any { return normalizeSQLValue(v) }

// normalizeSQLValue rewrites pgx's [16]byte UUID representation into the
// canonical string form both the SQL placeholder path and BSON expect. Other
// types pass through unchanged.
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case [16]byte:
		return uuid.UUID(t).String()
	default:
		return v
	}
}
