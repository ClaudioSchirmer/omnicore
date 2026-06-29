package query

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Composer turns a ViewDefinition + a root key into the composed Mongo document
// the SyncEngine upserts. The document is column-keyed (a physical mirror of
// PostgreSQL); the Go↔column translation happens at read time in the reader.
//
// Per-source physical names come from each source's core.TableSchema (root via
// View.Schema, embeds via Source.Schema): the root key + each embed's key are
// the source's PK column, and the soft-delete filter uses the source's
// soft-delete column. A schema-less source falls back to id / deleted_at.
//
// Dispatch per embed on Source.IsMongo():
//   - relational source (FromSchema over a type-anchored schema): fetchRow /
//     fetchWhere against the engine's neutral read surface (core.Querier.QueryMaps +
//     Dialect), so it composes the same way on any backend.
//   - Mongo source (FromSchema over a type-less core.NewExternalSchema):
//     MongoDB.FindManyByField against the local DB.
//
// The relational reads go through infra.RelationalEngine (backend-neutral) rather
// than a concrete driver — the composer works identically on Postgres and MySQL.
// Statement bits ($n vs ?, identifier quoting, the uuid argument encoding) come from the engine's
// Dialect; the dynamic row→map read from core.Querier.QueryMaps.
type Composer struct {
	eng   core.RelationalEngine
	mongo ReadModelStore
}

// NewComposer builds a Composer with relational dispatch only.
func NewComposer(eng core.RelationalEngine) *Composer {
	return &Composer{eng: eng}
}

// NewComposerWithMongo builds a Composer that dispatches relational sources via
// the engine AND Mongo sources via the supplied handle.
func NewComposerWithMongo(eng core.RelationalEngine, mongo ReadModelStore) *Composer {
	return &Composer{eng: eng, mongo: mongo}
}

// schemaPK / schemaSoftDelete read the source's physical PK + soft-delete column
// straight from its core.TableSchema. The schema is mandatory on every view (root and
// embed), so there is no convention fallback — a view declared without a schema
// is rejected at boot, not silently mapped to "id"/"deleted_at".
func schemaPK(s *core.TableSchema) string                 { return s.PKColumn() }
func schemaSoftDelete(s *core.TableSchema) (string, bool) { return s.SoftDeleteColumn() }

func (c *Composer) Compose(ctx context.Context, view *ViewDefinition, rootID string) (Document, error) {
	includeArchived := !view.deleteOnArchive
	sd, _ := schemaSoftDelete(view.schema)
	row, err := c.fetchRow(ctx, view.schema, view.rootTable, schemaPK(view.schema), rootID, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	if err := c.mergeOwnerSiblings(ctx, row, view.schema, rootID, includeArchived); err != nil {
		return nil, err
	}
	if err := c.applyEmbeds(ctx, row, schemaPK(view.schema), view.embeds, includeArchived); err != nil {
		return nil, err
	}
	return row, nil
}

func (c *Composer) ComposeAll(ctx context.Context, view *ViewDefinition) ([]Document, error) {
	includeArchived := !view.deleteOnArchive
	sd, _ := schemaSoftDelete(view.schema)
	rows, err := c.fetchAll(ctx, view.schema, view.rootTable, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	pk := schemaPK(view.schema)
	for _, row := range rows {
		if err := c.mergeOwnerSiblings(ctx, row, view.schema, fmt.Sprintf("%v", row[pk]), includeArchived); err != nil {
			return nil, err
		}
		if err := c.applyEmbeds(ctx, row, pk, view.embeds, includeArchived); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// mergeOwnerSiblings merges each declared sibling's columns FLAT into the owner
// doc, fetched by the shared primary key. The document stays a flat mirror of
// the entity (siblings land at the owner's level, not nested) — the read-side
// reflection of how the write side partitioned the row. An absent sibling row
// leaves its fields omitted (never forced empty). Siblings carry no soft-delete
// (the owner's gate governs the row's visibility), so the sibling fetch passes
// an empty soft-delete column — no per-sibling filter. The shared PK column is
// already on the owner doc, so it is not re-copied. coerceTypes (inside
// fetchRow) restores bool fidelity on the sibling's own columns.
func (c *Composer) mergeOwnerSiblings(ctx context.Context, doc Document, ownerSchema *core.TableSchema, pkVal string, includeArchived bool) error {
	pkCol := schemaPK(ownerSchema)
	for _, sib := range ownerSchema.Siblings() {
		row, err := c.fetchRow(ctx, sib, sib.Table(), pkCol, pkVal, "", includeArchived)
		if err != nil {
			return err
		}
		if row == nil {
			continue
		}
		for col, val := range row {
			if col == pkCol {
				continue
			}
			doc[col] = val
		}
	}
	return nil
}

// applyEmbeds resolves each embed of the parent doc. parentPK is the parent
// source's PK column — the value matched against an EmbedMany child's FK.
func (c *Composer) applyEmbeds(ctx context.Context, doc Document, parentPK string, embeds []embedDef, includeArchived bool) error {
	for _, e := range embeds {
		if err := c.fetchEmbed(ctx, doc, parentPK, e, includeArchived); err != nil {
			return err
		}
	}
	return nil
}

func (c *Composer) fetchEmbed(ctx context.Context, doc Document, parentPK string, e embedDef, includeArchived bool) error {
	if e.source.IsMongo() {
		return c.fetchMongoEmbed(ctx, doc, parentPK, e)
	}
	return c.fetchPGEmbed(ctx, doc, parentPK, e, includeArchived)
}

func (c *Composer) fetchPGEmbed(ctx context.Context, doc Document, parentPK string, e embedDef, includeArchived bool) error {
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
		rows, err := c.fetchWhere(ctx, e.source.schema, e.source.table, e.JoinColumn(), idStr, sd, includeArchived)
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
	row, err := c.fetchRow(ctx, e.source.schema, e.source.table, srcPK, fkStr, sd, includeArchived)
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

func (c *Composer) fetchMongoEmbed(ctx context.Context, doc Document, parentPK string, e embedDef) error {
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
// the source has soft-delete AND archived rows are excluded. The dialect renders
// the placeholder ($1 on PG, ? on MySQL) and identifier quoting so the same
// composer drives any backend.
func buildFetchSQL(d core.Dialect, verb, table, keyCol, sdCol string, includeArchived bool) string {
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + d.QuoteIdent(sdCol) + " IS NULL"
	}
	if keyCol == "" {
		// fetchAll: no key predicate.
		if cond == "" {
			return fmt.Sprintf("SELECT * FROM %s", d.QuoteIdent(table))
		}
		return fmt.Sprintf("SELECT * FROM %s WHERE %s IS NULL", d.QuoteIdent(table), d.QuoteIdent(sdCol))
	}
	suffix := ""
	if verb == "row" {
		suffix = " LIMIT 1"
	}
	return fmt.Sprintf("SELECT * FROM %s WHERE %s = %s%s%s",
		d.QuoteIdent(table), d.QuoteIdent(keyCol), d.Placeholder(1), cond, suffix)
}

// encodeKey wraps the (always uuid-shaped) composer key as a domain.ID and runs
// it through the engine's argument codec: a uuid string on Postgres, the 16-byte
// BINARY(16) form on MySQL — so the WHERE matches the stored id column on either
// backend.
func (c *Composer) encodeKey(keyVal string) any {
	return c.eng.Dialect().EncodeArg(domain.NewID(keyVal))
}

func (c *Composer) fetchRow(ctx context.Context, schema *core.TableSchema, table, keyCol, keyVal, sdCol string, includeArchived bool) (Document, error) {
	d := c.eng.Dialect()
	results, err := c.eng.Querier().QueryMaps(ctx,
		buildFetchSQL(d, "row", table, keyCol, sdCol, includeArchived), c.encodeKey(keyVal))
	if err != nil || len(results) == 0 {
		return nil, err
	}
	row := Document(results[0])
	coerceTypes(row, schema)
	return row, nil
}

func (c *Composer) fetchWhere(ctx context.Context, schema *core.TableSchema, table, keyCol, keyVal, sdCol string, includeArchived bool) ([]Document, error) {
	d := c.eng.Dialect()
	results, err := c.eng.Querier().QueryMaps(ctx,
		buildFetchSQL(d, "where", table, keyCol, sdCol, includeArchived), c.encodeKey(keyVal))
	if err != nil {
		return nil, err
	}
	return toBsonMaps(results, schema), nil
}

func (c *Composer) fetchAll(ctx context.Context, schema *core.TableSchema, table, sdCol string, includeArchived bool) ([]Document, error) {
	d := c.eng.Dialect()
	results, err := c.eng.Querier().QueryMaps(ctx,
		buildFetchSQL(d, "all", table, "", sdCol, includeArchived))
	if err != nil {
		return nil, err
	}
	return toBsonMaps(results, schema), nil
}

// toBsonMaps converts the engine's neutral []map[string]any into []Document. Document
// IS map[string]interface{}, so each element conversion is free; the composer
// mutates these maps (adding embed fields) as Document downstream. Each row is run
// through coerceTypes so a value that lost type fidelity on the relational read
// reaches BSON in its Go-native form.
func toBsonMaps(ms []map[string]any, schema *core.TableSchema) []Document {
	out := make([]Document, len(ms))
	for i, m := range ms {
		out[i] = Document(m)
		coerceTypes(out[i], schema)
	}
	return out
}

// coerceTypes rewrites scanned values that lost type fidelity on the relational
// read into the Go-native form the BSON document expects. The case that matters
// is boolean: MySQL has no native boolean (BOOL/BOOLEAN is TINYINT(1)) and the
// driver yields int64(0/1) on the dynamic QueryMaps path, so a bool entity field
// would otherwise compose into Mongo as a number — diverging from Postgres, where
// pgx returns a real bool. The schema is type-anchored, so it knows which physical
// columns back a Go bool; those are coerced 0/1 → bool. A no-op on Postgres (the
// value is already a bool) and for an external/type-less schema (BoolColumns is
// empty). A SQL NULL (nil) stays nil.
func coerceTypes(row Document, schema *core.TableSchema) {
	if row == nil || schema == nil {
		return
	}
	for col := range schema.BoolColumns() {
		switch v := row[col].(type) {
		case int64:
			row[col] = v != 0
		case int:
			row[col] = v != 0
		}
	}
}
