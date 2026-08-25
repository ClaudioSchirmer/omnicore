// Package hydrate reads a whole aggregate out of the relational store and
// returns it as a column-keyed document: the root row, its siblings merged flat,
// its own children nested under their declared collection segment, and — when
// the root is a shared-base ROLE — the base's fields flattened in plus the base's
// native children.
//
// It is STORE-NEUTRAL on both ends. Its only inputs are a core.TableSchema (the
// one declaration that names every physical column, the id, the managed columns,
// the children, the siblings and the shared base) and a core.RelationalEngine
// (Querier + Dialect), so it composes identically on every supported backend. Its
// only output is a map[string]any. It knows nothing about views, projections,
// collections or documents-at-rest: it does not import query, and it never will.
//
// That neutrality is what makes it shareable. Its consumer today is the Mongo
// projection composer, which hydrates an aggregate here and then layers its
// external embeds on top; nothing in this package knows that, and nothing in it
// would have to change to serve a second caller.
//
// The relational READ engine is deliberately not that caller. It never selects
// rows of its own: it hands a criteria to the aggregate loader and maps the
// LOADED TYPED ENTITY into the same shape (engine/relational.BuildDocument), so
// the criteria→SQL translation stays in one place. The two therefore produce the
// same document from two different starting points — a scanned row here, a
// hydrated entity there — and their agreement is asserted, not structural: see
// TestRelationalDocParity_RootChildrenManaged, which compares both documents as
// canonical JSON and fails on any divergence, including a managed column added
// to a schema later.
package hydrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Document is one hydrated row: physical column names to scanned values, with
// nested child collections under their Go segment. An alias, not a defined type,
// so it converts freely to and from the map[string]any every caller already
// speaks.
type Document = map[string]any

// BaseRevisionField is the document watermark a shared BASE's revision lands
// under. The base's physical revision column is remapped onto it during the
// shared-base merge so it never collides with the ROLE's own revision. The role's
// own watermark field is chosen by the caller and passed to RemapRevision.
const BaseRevisionField = "_base_revision"

// MaxInClauseSize caps how many ids a single IN (...) predicate binds. Kept under
// the tightest backend ceiling (Oracle's ORA-01795 caps an expression list at
// 1000; SQL Server tolerates ~2100 parameters) so a large batch is split into as
// many index-only IN lookups as needed rather than one oversized predicate a
// backend rejects. Exported so a caller that sizes its own batches can align
// them with the chunking the fetches actually perform.
const MaxInClauseSize = 900

// Hydrator holds the relational engine every fetch reads through. One per
// service; safe for concurrent use (it owns no mutable state).
type Hydrator struct {
	eng core.RelationalEngine
}

// New builds a Hydrator over the neutral relational engine.
func New(eng core.RelationalEngine) *Hydrator { return &Hydrator{eng: eng} }

// Engine exposes the wrapped engine for a caller that needs the Querier or the
// Dialect directly (a selection step building its own WHERE, say).
func (h *Hydrator) Engine() core.RelationalEngine { return h.eng }

// SchemaPK / SchemaDeletedAt read a source's physical ID + DeletedAt column
// straight from its schema. The schema is mandatory on every source, so there is
// no convention fallback — nothing is silently mapped to "id"/"deleted_at".
func SchemaPK(s *core.TableSchema) string                { return s.IDColumn() }
func SchemaDeletedAt(s *core.TableSchema) (string, bool) { return s.DeletedAtColumn() }

// FetchRow reads a single row of table by keyCol = keyVal, applying the DeletedAt
// gate unless includeArchived. Nil (not an error) when nothing matches.
func (h *Hydrator) FetchRow(ctx context.Context, schema *core.TableSchema, table, keyCol, keyVal, sdCol string, includeArchived bool) (Document, error) {
	d := h.eng.Dialect()
	results, err := h.eng.Querier().QueryMaps(ctx,
		BuildFetchSQL(d, "row", table, readColsWithKey(schema, keyCol), keyCol, sdCol, includeArchived), h.EncodeKey(keyVal))
	if err != nil || len(results) == 0 {
		return nil, err
	}
	row := results[0]
	finishRow(row, schema)
	return row, nil
}

// FetchWhere reads every row of table matching keyCol = keyVal under the same
// gate — the 1:N companion of FetchRow.
func (h *Hydrator) FetchWhere(ctx context.Context, schema *core.TableSchema, table, keyCol, keyVal, sdCol string, includeArchived bool) ([]Document, error) {
	d := h.eng.Dialect()
	results, err := h.eng.Querier().QueryMaps(ctx,
		BuildFetchSQL(d, "where", table, readColsWithKey(schema, keyCol), keyCol, sdCol, includeArchived), h.EncodeKey(keyVal))
	if err != nil {
		return nil, err
	}
	return ToDocuments(results, schema), nil
}

// FetchByIDs selects every row whose keyCol is in ids, applying the same
// DeletedAt gate as the single-row fetch. The id set is chunked at
// MaxInClauseSize so no single IN (...) predicate exceeds a backend's list
// ceiling; each chunk's placeholders are rendered through the dialect and each id
// is encoded exactly as the single-key path encodes it, so the WHERE matches the
// stored id column on every backend. Rows arrive in no guaranteed order — the
// caller keys each document by its ID column, never by position.
func (h *Hydrator) FetchByIDs(ctx context.Context, schema *core.TableSchema, table, keyCol string, ids []string, sdCol string, includeArchived bool) ([]Document, error) {
	d := h.eng.Dialect()
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + d.QuoteIdent(sdCol) + " IS NULL"
	}
	out := make([]Document, 0, len(ids))
	for start := 0; start < len(ids); start += MaxInClauseSize {
		end := start + MaxInClauseSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = d.Placeholder(i + 1)
			args[i] = h.EncodeKey(id)
		}
		sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)%s",
			selectList(d, readColsWithKey(schema, keyCol)), d.QuoteIdent(table), d.QuoteIdent(keyCol), strings.Join(placeholders, ", "), cond)
		results, err := h.eng.Querier().QueryMaps(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, ToDocuments(results, schema)...)
	}
	return out, nil
}

// FetchAll reads the whole table under the DeletedAt gate — no key predicate.
func (h *Hydrator) FetchAll(ctx context.Context, schema *core.TableSchema, table, sdCol string, includeArchived bool) ([]Document, error) {
	d := h.eng.Dialect()
	results, err := h.eng.Querier().QueryMaps(ctx,
		BuildFetchSQL(d, "all", table, readColsWithKey(schema, ""), "", sdCol, includeArchived))
	if err != nil {
		return nil, err
	}
	return ToDocuments(results, schema), nil
}

// FetchLatestArchived returns the most recently archived row referencing keyVal —
// the deterministic remnant pick when no active row exists.
func (h *Hydrator) FetchLatestArchived(ctx context.Context, schema *core.TableSchema, keyCol, keyVal, sdCol string) (Document, error) {
	d := h.eng.Dialect()
	sql := d.ApplyLimit(fmt.Sprintf("SELECT %s FROM %s WHERE %s = %s AND %s IS NOT NULL ORDER BY %s DESC",
		selectList(d, readColsWithKey(schema, keyCol)), d.QuoteIdent(schema.Table()), d.QuoteIdent(keyCol), d.Placeholder(1), d.QuoteIdent(sdCol), d.QuoteIdent(sdCol)), 1)
	results, err := h.eng.Querier().QueryMaps(ctx, sql, h.EncodeKey(keyVal))
	if err != nil || len(results) == 0 {
		return nil, err
	}
	row := results[0]
	finishRow(row, schema)
	return row, nil
}

// EncodeKey wraps the (always uuid-shaped) join key as a domain.ID and runs it
// through the engine's argument codec: a uuid string on Postgres, the 16-byte
// BINARY(16) form on MySQL — so the WHERE matches the stored id column on either
// backend.
func (h *Hydrator) EncodeKey(keyVal string) any {
	return h.eng.Dialect().EncodeArg(domain.NewID(keyVal))
}

// BuildFetchSQL renders the SELECT one keyed fetch runs: an explicit,
// dialect-quoted column list over table, an optional keyCol = ? predicate, the
// DeletedAt gate unless includeArchived, and LIMIT 1 when verb is "row". Exported
// because the gate it renders is a policy the CALLER decides (a projection that
// drops archived rows passes includeArchived=false), so the caller's own tests
// assert the SQL its policy produces.
func BuildFetchSQL(d core.Dialect, verb, table string, cols []string, keyCol, sdCol string, includeArchived bool) string {
	sel := selectList(d, cols)
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + d.QuoteIdent(sdCol) + " IS NULL"
	}
	if keyCol == "" {
		// FetchAll: no key predicate.
		if cond == "" {
			return fmt.Sprintf("SELECT %s FROM %s", sel, d.QuoteIdent(table))
		}
		return fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NULL", sel, d.QuoteIdent(table), d.QuoteIdent(sdCol))
	}
	sqlStr := fmt.Sprintf("SELECT %s FROM %s WHERE %s = %s%s",
		sel, d.QuoteIdent(table), d.QuoteIdent(keyCol), d.Placeholder(1), cond)
	if verb == "row" {
		sqlStr = d.ApplyLimit(sqlStr, 1)
	}
	return sqlStr
}

// readColsWithKey is the schema's read columns PLUS the key column the fetch
// groups or maps rows by — the caller reads row[keyCol] to bucket results, so the
// key MUST come back in the row. ReadColumns names a table's OWN columns, but the
// join key can be a column the schema does not list among them (a sibling's
// shared-ID join column, say, owned by its parent), so it is unioned in
// explicitly. keyCol == "" (FetchAll) adds nothing.
func readColsWithKey(schema *core.TableSchema, keyCol string) []string {
	cols := schema.ReadColumns()
	if keyCol == "" {
		return cols
	}
	for _, c := range cols {
		if c == keyCol {
			return cols
		}
	}
	return append(cols, keyCol)
}

// selectList renders an explicit, dialect-quoted column list for a read — never
// SELECT * (see core.TableSchema.ReadColumns for why: a named result type stays
// stable across an online ADD COLUMN). Falls back to "*" only if a caller passes
// no columns, which a real schema never does (it always has at least an ID).
func selectList(d core.Dialect, cols []string) string {
	if len(cols) == 0 {
		return "*"
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = d.QuoteIdent(c)
	}
	return strings.Join(quoted, ", ")
}

// ToDocuments converts the engine's neutral []map[string]any into []Document.
// Document IS map[string]any, so each element conversion is free; callers mutate
// these maps downstream. Each row is run through CoerceTypes so a value that lost
// type fidelity on the relational read reaches the caller in its Go-native form.
func ToDocuments(ms []map[string]any, schema *core.TableSchema) []Document {
	out := make([]Document, len(ms))
	for i, m := range ms {
		out[i] = m
		finishRow(out[i], schema)
	}
	return out
}

// RemapRevision moves a schema's physical revision column into the document
// watermark named by watermarkField. Every path that produces a hydrated document
// must carry the SAME watermark fields, or two paths would diverge doc-a-doc and
// the revision guards would not survive a rebuild or an overwrite.
func RemapRevision(row Document, schema *core.TableSchema, watermarkField string) {
	if row == nil || schema == nil {
		return
	}
	rc := schema.RevisionColumn()
	if rc == "" {
		return
	}
	if v, ok := row[rc]; ok {
		row[watermarkField] = v
		delete(row, rc)
	}
}

// CoerceTypes rewrites scanned values that lost type fidelity on the relational
// read into the Go-native form the document expects. The case that matters is
// boolean: MySQL has no native boolean (BOOL/BOOLEAN is TINYINT(1)) and the driver
// yields int64(0/1) on the dynamic QueryMaps path, so a bool entity field would
// otherwise surface as a number — diverging from Postgres, where pgx returns a
// real bool. The schema is type-anchored, so it knows which physical columns back
// a Go bool; those are coerced 0/1 → bool. A no-op on Postgres (the value is
// already a bool) and for an external/type-less schema (BoolColumns is empty). A
// SQL NULL (nil) stays nil.
// finishRow is what every fetch in this package runs over a scanned row before
// handing it out: type coercion, then the InSync redaction any RedactedField
// declares.
//
// This package is the RIGHT home for that second step and the relational read
// engine is deliberately not (see the package doc): hydrate exists only to feed
// the Mongo projection, so redacting here reaches the composer, the batch
// composer, the consult path, the embed ripple and — through the composer — the
// REBUILD, which is the one that would otherwise reintroduce in a recomposition
// exactly what the payload excluded. The entity loader does not read through
// here, so the domain keeps the real value.
//
// Coercion runs FIRST: a redactor that masks a string needs the value already in
// its Go-native form.
func finishRow(row Document, schema *core.TableSchema) {
	CoerceTypes(row, schema)
	schema.RedactSyncColumns(row)
}

func CoerceTypes(row Document, schema *core.TableSchema) {
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

// KeyOf returns the stringified join-key value of col on doc, or "" when the
// column is absent or NULL (a doc without the key is skipped by every merge).
func KeyOf(doc Document, col string) string {
	v, ok := doc[col]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// CollectKeys gathers the non-empty join keys of col across docs (duplicates kept
// — FetchInGrouped dedupes before binding the IN predicate).
func CollectKeys(docs []Document, col string) []string {
	keys := make([]string, 0, len(docs))
	for _, d := range docs {
		if k := KeyOf(d, col); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// EmptyIfNil normalizes an absent group to a non-nil empty slice, so a childless
// root hydrates an empty array (matching FetchWhere's []Document{}), never a
// missing/null field a reader would mishandle.
func EmptyIfNil(rows []Document) []Document {
	if rows == nil {
		return []Document{}
	}
	return rows
}
