package infra

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Composer turns a ViewDefinition + a root key into the composed Mongo
// document that the SyncEngine upserts. It dispatches per-embed on
// Source.IsMongo():
//
//   - PG source (default, produced by From): fetchRow / fetchWhere against
//     the connection pool.
//   - Mongo source (produced by FromMongo): MongoDB.FindManyByField against
//     the local Mongo database.
//
// The MongoDB handle is optional — Composer instances assembled without it
// (legacy test scaffolds, callers that never embed FromMongo sources) keep
// the PG-only behavior. When a Mongo-kind embed is encountered and the
// handle is nil, applyEmbeds returns a descriptive error rather than
// silently dropping the embed.
type Composer struct {
	pg    *Postgres
	mongo *MongoDB
}

// NewComposer builds a Composer with PG dispatch only. Use when no view
// declares a FromMongo embed (the common case under SyncEngine's existing
// path). Calls to Compose against a view that does carry a FromMongo embed
// will fail loudly.
func NewComposer(pg *Postgres) *Composer {
	return &Composer{pg: pg}
}

// NewComposerWithMongo builds a Composer that dispatches PG sources via the
// connection pool AND Mongo sources via the supplied handle. SyncEngine and
// UpstreamSubscriber both wire this constructor so cross-store embeds
// resolve correctly during recompose.
func NewComposerWithMongo(pg *Postgres, mongo *MongoDB) *Composer {
	return &Composer{pg: pg, mongo: mongo}
}

func (c *Composer) Compose(ctx context.Context, view *ViewDefinition, rootID string) (bson.M, error) {
	includeArchived := !view.deleteOnArchive
	row, err := c.fetchRow(ctx, view.rootTable, "id", rootID, includeArchived)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	if err := c.applyEmbeds(ctx, row, view.embeds, includeArchived); err != nil {
		return nil, err
	}
	return row, nil
}

func (c *Composer) ComposeAll(ctx context.Context, view *ViewDefinition) ([]bson.M, error) {
	includeArchived := !view.deleteOnArchive
	rows, err := c.fetchAll(ctx, view.rootTable, includeArchived)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := c.applyEmbeds(ctx, row, view.embeds, includeArchived); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func (c *Composer) applyEmbeds(ctx context.Context, doc bson.M, embeds []embedDef, includeArchived bool) error {
	for _, e := range embeds {
		if err := c.fetchEmbed(ctx, doc, e, includeArchived); err != nil {
			return err
		}
	}
	return nil
}

// fetchEmbed resolves one embed leaf against the right store and writes the
// result into the parent doc. Centralizes the PG-vs-Mongo dispatch so both
// applyEmbeds and any future caller share the same dispatch + recursion
// shape.
//
// Dispatch:
//   - e.source.IsMongo() == false → PG path (fetchRow / fetchWhere).
//   - e.source.IsMongo() == true  → Mongo path (FindManyByField /
//     FindManyByField+first-element). The Mongo path requires c.mongo
//     non-nil; otherwise returns a descriptive error.
//
// Recursion: every fetched row/doc passes through applyEmbeds so nested
// embeds resolve to the correct store independently of the parent — a
// FromMongo embed under a From parent is supported, and vice-versa.
//
// includeArchived semantics carry through unchanged: PG fetches honor the
// flag via the SQL WHERE clause; Mongo fetches do NOT filter by
// deleted_at — by spec, the upstream-projected Mongo collection mirrors
// the upstream's current state and the consuming view's archive policy is
// expressed on the view's root, not on the embed source.
func (c *Composer) fetchEmbed(ctx context.Context, doc bson.M, e embedDef, includeArchived bool) error {
	if e.source.IsMongo() {
		return c.fetchMongoEmbed(ctx, doc, e)
	}
	return c.fetchPGEmbed(ctx, doc, e, includeArchived)
}

func (c *Composer) fetchPGEmbed(ctx context.Context, doc bson.M, e embedDef, includeArchived bool) error {
	if e.many {
		// One-to-many: the child table holds the FK back to the root.
		// e.source.joinKey is the FK column on the child; the value comes
		// from the root's primary key, conventionally doc["id"].
		id, ok := doc["id"]
		if !ok || id == nil {
			return nil
		}
		idStr := fmt.Sprintf("%v", id)
		rows, err := c.fetchWhere(ctx, e.source.table, e.source.joinKey, idStr, includeArchived)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := c.applyEmbeds(ctx, row, e.source.embeds, includeArchived); err != nil {
				return err
			}
		}
		doc[e.field] = rows
		return nil
	}

	// One-to-one: the root holds the FK pointing to the source's id.
	// e.source.joinKey is the FK column on the root doc.
	fk, ok := doc[e.source.joinKey]
	if !ok || fk == nil {
		return nil
	}
	fkStr := fmt.Sprintf("%v", fk)
	row, err := c.fetchRow(ctx, e.source.table, "id", fkStr, includeArchived)
	if err != nil {
		return err
	}
	if row != nil {
		if err := c.applyEmbeds(ctx, row, e.source.embeds, includeArchived); err != nil {
			return err
		}
		doc[e.field] = row
	}
	return nil
}

func (c *Composer) fetchMongoEmbed(ctx context.Context, doc bson.M, e embedDef) error {
	if c.mongo == nil {
		return fmt.Errorf("composer: view embed on Mongo collection %q requires a MongoDB handle "+
			"(builder constructed without NewComposerWithMongo)", e.source.table)
	}
	// Mongo embeds do NOT cascade the includeArchived flag: the
	// upstream-projected collection mirrors the upstream's current state,
	// and the consuming view chooses its archive posture via
	// ViewDefinition.DeleteOnArchive() applied on the PG root only.
	if e.many {
		// EmbedMany: the parent's `id` is the value matched against the
		// embed source's joinKey field — the upstream-projected docs that
		// reference the parent root land in the slice.
		id, ok := doc["id"]
		if !ok || id == nil {
			return nil
		}
		docs, err := c.mongo.FindManyByField(ctx, e.source.table, e.source.joinKey, id)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if err := c.applyEmbeds(ctx, d, e.source.embeds, false); err != nil {
				return err
			}
		}
		doc[e.field] = docs
		return nil
	}
	// Embed (one-to-one): the parent doc carries the FK in the field named
	// by joinKey; the value is looked up by _id on the upstream-projected
	// collection.
	fk, ok := doc[e.source.joinKey]
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
	if err := c.applyEmbeds(ctx, row, e.source.embeds, false); err != nil {
		return err
	}
	doc[e.field] = row
	return nil
}

// Default fetches OMIT the `deleted_at IS NULL` filter — Mongo views mirror
// PostgreSQL symmetrically (archived rows survive in the projection with
// deleted_at populated) and the SyncEngine compose+upserts the document on
// ARCHIVED events. Views that opt in via ViewDefinition.DeleteOnArchive()
// flip includeArchived=false at the Compose entry point, which cascades
// through every embed so the WHERE deleted_at IS NULL filter is applied
// on root + every child source (hot-tier projection — there is no
// per-embed override). Framework convention is that every table
// participating in a view has a deleted_at column, so the filter is safe
// to apply whenever the opt-in is set.

func buildFetchSQL(verb, table, keyCol string, includeArchived bool) string {
	cond := " AND deleted_at IS NULL"
	if includeArchived {
		cond = ""
	}
	if keyCol == "" {
		// fetchAll: no key predicate, only the soft-delete clause.
		if includeArchived {
			return fmt.Sprintf("SELECT * FROM %s", validIdentifier(table))
		}
		return fmt.Sprintf("SELECT * FROM %s WHERE deleted_at IS NULL", validIdentifier(table))
	}
	suffix := ""
	if verb == "row" {
		suffix = " LIMIT 1"
	}
	return fmt.Sprintf("SELECT * FROM %s WHERE %s = $1%s%s",
		validIdentifier(table), validIdentifier(keyCol), cond, suffix)
}

func (c *Composer) fetchRow(ctx context.Context, table, keyCol, keyVal string, includeArchived bool) (bson.M, error) {
	rows, err := c.pg.pool.Query(ctx, buildFetchSQL("row", table, keyCol, includeArchived), keyVal)
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

func (c *Composer) fetchWhere(ctx context.Context, table, keyCol, keyVal string, includeArchived bool) ([]bson.M, error) {
	rows, err := c.pg.pool.Query(ctx, buildFetchSQL("where", table, keyCol, includeArchived), keyVal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgxRowsToMaps(rows)
}

func (c *Composer) fetchAll(ctx context.Context, table string, includeArchived bool) ([]bson.M, error) {
	rows, err := c.pg.pool.Query(ctx, buildFetchSQL("all", table, "", includeArchived))
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

// NormalizeSQLValue is the exported counterpart of normalizeSQLValue. Used
// by the admin CLI (cmd/omnicore-admin/replay) to normalize column values
// in the same way SyncEngine + Composer do, so the synthetic replay
// payloads land in the outbox with the same canonical UUID-as-string
// shape Debezium produces in steady state.
func NormalizeSQLValue(v any) any { return normalizeSQLValue(v) }

// normalizeSQLValue rewrites pgx's native Go representation of select types
// into the shape downstream code expects:
//
//   - UUID columns come back as [16]byte. Without normalization, fmt.Sprintf
//     ("%v", v) in applyEmbeds (used to compose the SQL placeholder for the
//     child FK lookup) renders the byte array as "[16 86 99 …]" and Postgres
//     rejects the query with SQLSTATE 22P02. BSON would also serialize the
//     [16]byte as binary rather than a canonical string. Converting once at
//     the source fixes both write paths in one place.
//
// Other types pass through unchanged.
func normalizeSQLValue(v any) any {
	switch t := v.(type) {
	case [16]byte:
		return uuid.UUID(t).String()
	default:
		return v
	}
}
