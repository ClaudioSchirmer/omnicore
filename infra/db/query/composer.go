package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// maxInClauseSize caps how many ids fetchByIDs binds into a single IN (...)
// predicate. Kept under the tightest backend ceiling (Oracle's ORA-01795 caps
// an expression list at 1000; SQL Server tolerates ~2100 parameters) so a
// rebuild batch of rebuildBatchSize ids is split into as many index-only IN
// lookups as needed rather than one oversized predicate that a backend rejects.
const maxInClauseSize = 900

// Composer turns a ViewDefinition + a root key into the composed Mongo document
// the SyncEngine upserts. The document is column-keyed (a physical mirror of
// PostgreSQL); the Go↔column translation happens at read time in the reader.
//
// Per-source physical names come from each source's core.TableSchema (root via
// View.Schema, embeds via Source.Schema): the root key + each embed's key are
// the source's PK column, and the soft-delete filter uses the source's
// soft-delete column. A schema-less source falls back to id / deleted_at.
//
// The root document and the aggregate's internal closure (siblings, SharedBase,
// own + base children) compose RELATIONALLY — fetchRow / fetchWhere against the
// engine's neutral read surface (core.Querier.QueryMaps + Dialect), so they
// compose the same way on any backend. Embeds are always EXTERNAL sources
// (FromSchema over a type-less core.NewExternalSchema): MongoDB.FindManyByField
// against the local DB. A write-anchored embed source is rejected at boot
// (ValidateViewSchemas) — internal data projects automatically, never via an embed.
//
// The relational reads go through infra.RelationalEngine (backend-neutral) rather
// than a concrete driver — the composer works identically on Postgres and MySQL.
// Statement bits ($n vs ?, identifier quoting, the uuid argument encoding) come from the engine's
// Dialect; the dynamic row→map read from core.Querier.QueryMaps.
type Composer struct {
	eng   core.RelationalEngine
	mongo ReadModelStore
	// resolver maps an embed's source collection to the physical collection it
	// currently resolves to. Nil on a relational-only Composer (NewComposer),
	// which never dispatches a Mongo embed; nil resolves to identity.
	resolver *ViewResolver
}

// NewComposer builds a Composer with relational dispatch only.
func NewComposer(eng core.RelationalEngine) *Composer {
	return &Composer{eng: eng}
}

// NewComposerWithMongo builds a Composer that dispatches relational sources via
// the engine AND Mongo sources via the supplied handle, resolving embed source
// collections through the shared resolver.
func NewComposerWithMongo(eng core.RelationalEngine, mongo ReadModelStore, resolver *ViewResolver) *Composer {
	return &Composer{eng: eng, mongo: mongo, resolver: resolver}
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
	if view.isSharedBaseView {
		if err := c.composeBaseRootedRow(ctx, view, row, rootID, includeArchived); err != nil {
			return nil, err
		}
		return row, nil
	}
	if err := c.mergeOwnerSiblings(ctx, row, view.schema, rootID, includeArchived); err != nil {
		return nil, err
	}
	if err := c.mergeSharedBase(ctx, row, view.schema, includeArchived); err != nil {
		return nil, err
	}
	if err := c.mergeSharedBaseChildren(ctx, row, view.schema, includeArchived); err != nil {
		return nil, err
	}
	if err := c.mergeOwnChildren(ctx, row, view.schema, includeArchived); err != nil {
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
	if err := c.composeRows(ctx, view, rows, includeArchived); err != nil {
		return nil, err
	}
	return rows, nil
}

// ComposeBatch composes exactly the roots named by ids — the batched companion
// of the per-id Compose the rebuild loop drives. It fetches the whole batch of
// root rows in one IN (...) lookup (chunked at maxInClauseSize) instead of one
// SELECT per id, then runs the identical merge chain per row. The dominant
// per-root round trip of a large rebuild collapses from one-per-row to
// one-per-batch; the aggregate's inner reads (siblings, children, roles,
// embeds) stay per-row, so a rich aggregate still pays for its closure. A root
// whose id has no live row (hard-deleted, or archived under DeleteOnArchive)
// simply does not appear in the result — the caller reconciles it as an orphan.
// The returned documents carry their _id in the root PK column, identical to
// Compose, so the caller keys the upsert the same way on either path.
func (c *Composer) ComposeBatch(ctx context.Context, view *ViewDefinition, ids []string) ([]Document, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	includeArchived := !view.deleteOnArchive
	sd, _ := schemaSoftDelete(view.schema)
	rows, err := c.fetchByIDs(ctx, view.schema, view.rootTable, schemaPK(view.schema), ids, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	if err := c.composeRows(ctx, view, rows, includeArchived); err != nil {
		return nil, err
	}
	return rows, nil
}

// composeRows runs the per-row merge chain over already-fetched root rows —
// the shared body of ComposeAll and ComposeBatch. SharedBase roots recompose
// their role sub-documents; a plain root merges siblings, shared base, base
// children, own children, then external embeds.
func (c *Composer) composeRows(ctx context.Context, view *ViewDefinition, rows []Document, includeArchived bool) error {
	pk := schemaPK(view.schema)
	if view.isSharedBaseView {
		for _, row := range rows {
			if err := c.composeBaseRootedRow(ctx, view, row, fmt.Sprintf("%v", row[pk]), includeArchived); err != nil {
				return err
			}
		}
		return nil
	}
	for _, row := range rows {
		if err := c.mergeOwnerSiblings(ctx, row, view.schema, fmt.Sprintf("%v", row[pk]), includeArchived); err != nil {
			return err
		}
		if err := c.mergeSharedBase(ctx, row, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.mergeSharedBaseChildren(ctx, row, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.mergeOwnChildren(ctx, row, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.applyEmbeds(ctx, row, pk, view.embeds, includeArchived); err != nil {
			return err
		}
	}
	return nil
}

// composeBaseRootedRow fills a SharedBaseView document from its already-fetched
// base row: the base's native children nest at the root (mergeOwnChildren — the
// base IS the schema that owns them), then ONE SUB-DOCUMENT PER DECLARED ROLE,
// then the external embeds. A role's sub-document carries the role row (chosen
// active-first, see fetchRoleRow) with its siblings merged flat and its own
// children nested — keyed on the CHOSEN ROLE ROW's PK, never the base id (under
// the separate-FK model they differ). An absent role writes an explicit nil
// segment: the store's Upsert is $set, so a vanished role must overwrite its
// stale segment rather than silently survive it.
func (c *Composer) composeBaseRootedRow(ctx context.Context, view *ViewDefinition, row Document, baseID string, includeArchived bool) error {
	base := view.schema
	if err := c.mergeOwnChildren(ctx, row, base, includeArchived); err != nil {
		return err
	}
	for _, r := range view.roles {
		roleRow, err := c.fetchRoleRow(ctx, r, baseID, includeArchived)
		if err != nil {
			return err
		}
		if roleRow == nil {
			row[r.segment] = nil
			continue
		}
		rolePK := schemaPK(r.schema)
		if err := c.mergeOwnerSiblings(ctx, roleRow, r.schema, fmt.Sprintf("%v", roleRow[rolePK]), includeArchived); err != nil {
			return err
		}
		if err := c.mergeOwnChildren(ctx, roleRow, r.schema, includeArchived); err != nil {
			return err
		}
		row[r.segment] = roleRow
	}
	return c.applyEmbeds(ctx, row, schemaPK(base), view.embeds, includeArchived)
}

// fetchRoleRow selects THE role row that represents a specialization of the
// identity — deterministic under the separate-FK multiplicity the write side
// admits (archived remnants NEXT TO at most one active row):
//
//  1. the ACTIVE row (fk = baseID AND deleted_at IS NULL) when one exists —
//     the write-side one-active-role invariant caps it at one;
//  2. otherwise, when archived rows compose at all (keep mode), the MOST
//     RECENTLY archived remnant (ORDER BY deleted_at DESC) — the document
//     represents the CURRENT state of each specialization; remnant history is
//     not enumerated here (the role views with ?includeArchived cover it);
//  3. otherwise nil (the caller writes the explicit null segment).
//
// A role without SoftDelete has no archived state (hard delete is delete), so
// a single fetch by FK decides it.
func (c *Composer) fetchRoleRow(ctx context.Context, r roleDef, baseID string, includeArchived bool) (Document, error) {
	_, fkCol, _ := r.schema.SharedBaseRef()
	sd, hasSD := schemaSoftDelete(r.schema)
	if !hasSD {
		return c.fetchRow(ctx, r.schema, r.schema.Table(), fkCol, baseID, "", true)
	}
	active, err := c.fetchRow(ctx, r.schema, r.schema.Table(), fkCol, baseID, sd, false)
	if err != nil || active != nil {
		return active, err
	}
	if !includeArchived {
		return nil, nil
	}
	return c.fetchLatestArchived(ctx, r.schema, fkCol, baseID, sd)
}

// fetchLatestArchived returns the most recently archived row referencing the
// base — the deterministic remnant pick when no active row exists.
func (c *Composer) fetchLatestArchived(ctx context.Context, schema *core.TableSchema, keyCol, keyVal, sdCol string) (Document, error) {
	d := c.eng.Dialect()
	sql := d.ApplyLimit(fmt.Sprintf("SELECT * FROM %s WHERE %s = %s AND %s IS NOT NULL ORDER BY %s DESC",
		d.QuoteIdent(schema.Table()), d.QuoteIdent(keyCol), d.Placeholder(1), d.QuoteIdent(sdCol), d.QuoteIdent(sdCol)), 1)
	results, err := c.eng.Querier().QueryMaps(ctx, sql, c.encodeKey(keyVal))
	if err != nil || len(results) == 0 {
		return nil, err
	}
	row := results[0]
	coerceTypes(row, schema)
	return row, nil
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

// mergeSharedBaseChildren nests the shared base's NATIVE children (base-children)
// into the role document — the person-native collections (e.g. a person's
// addresses) shared across every role. They are fetched by the base-child's FK to
// the base id, which is the role's FK to the base already on the doc (the role row
// carries pessoa_id). Each collection lands under its derived Go segment (the same
// name BuildViewNode registers, so ToGoDoc translates it). No-op without a shared
// base or base children.
func (c *Composer) mergeSharedBaseChildren(ctx context.Context, doc Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	baseChildren := base.ChildSchemas()
	if len(baseChildren) == 0 {
		return nil
	}
	baseID, present := doc[fkCol]
	if !present || baseID == nil {
		return nil
	}
	idStr := fmt.Sprintf("%v", baseID)
	for _, bc := range baseChildren {
		sd, _ := schemaSoftDelete(bc)
		rows, err := c.fetchWhere(ctx, bc, bc.Table(), bc.FKColumn(), idStr, sd, includeArchived)
		if err != nil {
			return err
		}
		doc[childDocSegment(bc)] = rows
	}
	return nil
}

// mergeOwnChildren nests the schema's OWN aggregate children (schema.Child(...))
// into the document — the read-side mirror of hydrateChildren on the write side.
// Unlike base-children (keyed on the base's deterministic id via
// mergeSharedBaseChildren), an own child is joined on root.PK → child.FK: the
// child's FK column matched against the PK value already on the doc. Each
// collection lands under its derived Go segment (the same name newViewNode
// registers, so ToGoDoc translates it). Every fetched child row also gets its
// siblings merged FLAT (shape #4 — the child-sibling merge). No-op when the schema
// declares no own children. Applied on the root path (Compose/ComposeAll); embeds
// are external (type-less) and carry no children, so this runs only at the root.
func (c *Composer) mergeOwnChildren(ctx context.Context, doc Document, schema *core.TableSchema, includeArchived bool) error {
	children := schema.ChildSchemas()
	if len(children) == 0 {
		return nil
	}
	pkVal, present := doc[schemaPK(schema)]
	if !present || pkVal == nil {
		return nil
	}
	idStr := fmt.Sprintf("%v", pkVal)
	for _, child := range children {
		sd, _ := schemaSoftDelete(child)
		rows, err := c.fetchWhere(ctx, child, child.Table(), child.FKColumn(), idStr, sd, includeArchived)
		if err != nil {
			return err
		}
		childPK := schemaPK(child)
		for _, row := range rows {
			if err := c.mergeOwnerSiblings(ctx, row, child, fmt.Sprintf("%v", row[childPK]), includeArchived); err != nil {
				return err
			}
		}
		doc[childDocSegment(child)] = rows
	}
	return nil
}

// mergeSharedBase merges a role's shared identity (SharedBase) FLAT into the role
// document, fetched by the role's FK to the base's deterministic id. Like a
// sibling, the base fields land at the role's level (the doc stays flat). The
// base PK column equals the FK value already on the doc, so it is not re-copied.
//
// The base's MANAGED columns (soft-delete, created_at, updated_at) never
// overwrite the role's own: the document represents the ROLE, whose lifecycle
// and timestamps are authoritative (the base's are derived — it converges from
// its roles). Without this guard, a two-role identity with ONE archived role
// would compose the ACTIVE base's NULL deleted_at over the role's archived
// timestamp, hiding the archival from the reader's soft-delete gate, and every
// role doc would carry the person's creation timestamps instead of its own.
// Each managed column of the base is skipped only when the role declares its
// own column of the same kind.
func (c *Composer) mergeSharedBase(ctx context.Context, doc Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	fk, present := doc[fkCol]
	if !present || fk == nil {
		return nil
	}
	row, err := c.fetchRow(ctx, base, base.Table(), base.PKColumn(), fmt.Sprintf("%v", fk), "", includeArchived)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	skip := map[string]bool{base.PKColumn(): true}
	if col, ok := base.SoftDeleteColumn(); ok {
		if _, roleHas := schema.SoftDeleteColumn(); roleHas {
			skip[col] = true
		}
	}
	if col := base.CreatedAtColumn(); col != "" && schema.CreatedAtColumn() != "" {
		skip[col] = true
	}
	if col := base.UpdatedAtColumn(); col != "" && schema.UpdatedAtColumn() != "" {
		skip[col] = true
	}
	for col, val := range row {
		if skip[col] {
			continue
		}
		doc[col] = val
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

// fetchEmbed resolves one embed. Every embed source is EXTERNAL — another
// service's read model (UpstreamSubscription / FromMongo) or a derived
// projection — so composition is always against the local Mongo store. A
// write-anchored embed source is rejected at boot (ValidateViewSchemas): the
// aggregate's own data (root / siblings / SharedBase / own children) projects
// automatically from the TableSchema, never through an embed. `includeArchived`
// is unused here — the Mongo read model already reflects the upstream's archive
// state — but stays on the signature threaded from the root compose call.
func (c *Composer) fetchEmbed(ctx context.Context, doc Document, parentPK string, e embedDef, includeArchived bool) error {
	return c.fetchMongoEmbed(ctx, doc, parentPK, e)
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
		docs, err := c.mongo.FindManyByField(ctx, c.resolver.Active(e.source.table), e.JoinColumn(), id)
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
	docs, err := c.mongo.FindManyByField(ctx, c.resolver.Active(e.source.table), "_id", fk)
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
	sqlStr := fmt.Sprintf("SELECT * FROM %s WHERE %s = %s%s",
		d.QuoteIdent(table), d.QuoteIdent(keyCol), d.Placeholder(1), cond)
	if verb == "row" {
		sqlStr = d.ApplyLimit(sqlStr, 1)
	}
	return sqlStr
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
	row := results[0]
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

// fetchByIDs selects every row whose keyCol is in ids, applying the same
// soft-delete gate as the single-row fetch. The id set is chunked at
// maxInClauseSize so no single IN (...) predicate exceeds a backend's list
// ceiling; each chunk's placeholders are rendered through the dialect and each
// id is encoded exactly as the single-key path encodes it, so the WHERE matches
// the stored id column on every backend. Rows arrive in no guaranteed order —
// the caller keys each document by its PK column, never by position.
func (c *Composer) fetchByIDs(ctx context.Context, schema *core.TableSchema, table, keyCol string, ids []string, sdCol string, includeArchived bool) ([]Document, error) {
	d := c.eng.Dialect()
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + d.QuoteIdent(sdCol) + " IS NULL"
	}
	out := make([]Document, 0, len(ids))
	for start := 0; start < len(ids); start += maxInClauseSize {
		end := start + maxInClauseSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = d.Placeholder(i + 1)
			args[i] = c.encodeKey(id)
		}
		sql := fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s)%s",
			d.QuoteIdent(table), d.QuoteIdent(keyCol), strings.Join(placeholders, ", "), cond)
		results, err := c.eng.Querier().QueryMaps(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, toBsonMaps(results, schema)...)
	}
	return out, nil
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
		out[i] = m
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
