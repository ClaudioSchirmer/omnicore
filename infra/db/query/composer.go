package query

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/hydrate"
)

// Composer turns a ViewDefinition + a root key into the composed Mongo document
// the SyncEngine upserts. The document is column-keyed (a physical mirror of
// PostgreSQL); the Go↔column translation happens at read time in the reader.
//
// Per-source physical names come from each source's core.TableSchema (root via
// View.Schema, embeds via Source.Schema): the root key + each embed's key are
// the source's ID column, and the DeletedAt filter uses the source's
// DeletedAt column. A schema-less source falls back to id / deleted_at.
//
// The root document and the aggregate's internal closure (siblings, SharedBase,
// own + base children) compose RELATIONALLY — fetchRow / fetchWhere against the
// engine's neutral read surface (core.Querier.QueryMaps + Dialect), so they
// compose the same way on any backend. Embeds are always EXTERNAL sources
// (a JoinUpstream leg over a type-less core.NewExternalSchema): MongoDB.FindManyByField
// against the local DB. A write-anchored embed source is rejected at boot
// (ValidateViewSchemas) — internal data projects automatically, never via an embed.
//
// The relational reads go through infra.RelationalEngine (backend-neutral) rather
// than a concrete driver — the composer works identically on Postgres and MySQL.
// Statement bits ($n vs ?, identifier quoting, the uuid argument encoding) come from the engine's
// Dialect; the dynamic row→map read from core.Querier.QueryMaps.
type Composer struct {
	h     *hydrate.Hydrator
	mongo ReadModelStore
	// resolver maps an embed's source collection to the physical collection it
	// currently resolves to. Nil on a relational-only Composer (NewComposer),
	// which never dispatches a Mongo embed; nil resolves to identity.
	resolver *ViewResolver
}

// NewComposer builds a Composer with relational dispatch only.
func NewComposer(eng core.RelationalEngine) *Composer {
	return &Composer{h: hydrate.New(eng)}
}

// NewComposerWithMongo builds a Composer that dispatches relational sources via
// the engine AND Mongo sources via the supplied handle, resolving embed source
// collections through the shared resolver.
func NewComposerWithMongo(eng core.RelationalEngine, mongo ReadModelStore, resolver *ViewResolver) *Composer {
	return &Composer{h: hydrate.New(eng), mongo: mongo, resolver: resolver}
}

// schemaPK / schemaDeletedAt read the source's physical ID + DeletedAt column
// straight from its core.TableSchema. The schema is mandatory on every view (root and
// embed), so there is no convention fallback — a view declared without a schema
// is rejected at boot, not silently mapped to "id"/"deleted_at".
func (c *Composer) Compose(ctx context.Context, view *ViewDefinition, rootID string) (Document, error) {
	includeArchived := !view.deleteOnArchive
	sd, _ := hydrate.SchemaDeletedAt(view.schema)
	row, err := c.h.FetchRow(ctx, view.schema, view.RootTable(), hydrate.SchemaPK(view.schema), rootID, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	hydrate.RemapRevision(row, view.schema, docRevisionField)
	if view.isSharedBaseView {
		if err := c.composeBaseRootedRow(ctx, view, row, rootID, includeArchived); err != nil {
			return nil, err
		}
		return row, nil
	}
	if err := c.h.MergeOwnerSiblings(ctx, row, view.schema, rootID, includeArchived); err != nil {
		return nil, err
	}
	if err := c.h.MergeSharedBase(ctx, row, view.schema, includeArchived); err != nil {
		return nil, err
	}
	if err := c.h.MergeSharedBaseChildren(ctx, row, view.schema, includeArchived); err != nil {
		return nil, err
	}
	if err := c.h.MergeOwnChildren(ctx, row, view.schema, includeArchived); err != nil {
		return nil, err
	}
	if err := c.applyEmbeds(ctx, row, hydrate.SchemaPK(view.schema), view.embeds, includeArchived); err != nil {
		return nil, err
	}
	if err := c.applyChildEmbeds(ctx, row, view.childEmbeds); err != nil {
		return nil, err
	}
	return row, nil
}

func (c *Composer) ComposeAll(ctx context.Context, view *ViewDefinition) ([]Document, error) {
	includeArchived := !view.deleteOnArchive
	sd, _ := hydrate.SchemaDeletedAt(view.schema)
	rows, err := c.h.FetchAll(ctx, view.schema, view.RootTable(), sd, includeArchived)
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
// The returned documents carry their _id in the root ID column, identical to
// Compose, so the caller keys the upsert the same way on either path.
func (c *Composer) ComposeBatch(ctx context.Context, view *ViewDefinition, ids []string) ([]Document, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	includeArchived := !view.deleteOnArchive
	sd, _ := hydrate.SchemaDeletedAt(view.schema)
	rows, err := c.h.FetchByIDs(ctx, view.schema, view.RootTable(), hydrate.SchemaPK(view.schema), ids, sd, includeArchived)
	if err != nil {
		return nil, err
	}
	if err := c.composeRows(ctx, view, rows, includeArchived); err != nil {
		return nil, err
	}
	return rows, nil
}

// composeRows runs the merge chain over already-fetched root rows — the shared
// body of ComposeAll and ComposeBatch. It is SET-BASED: each step fetches its
// related source for the WHOLE batch in one IN (...) query and groups in memory
// (composer_batch.go), so a rebuild pays one round trip per related table per
// batch instead of one per aggregate — the difference between minutes and hours
// at millions of rows, largest on the engines with the heaviest per-query cost.
// The per-doc result is identical to the per-row chain (Compose): the steps run
// in the same order — siblings, shared base, base children, own children, embeds
// — just fanned across every row at each step. (The per-row helpers stay for the
// single-root Compose, where there is no N+1 to collapse.)
func (c *Composer) composeRows(ctx context.Context, view *ViewDefinition, rows []Document, includeArchived bool) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		hydrate.RemapRevision(row, view.schema, docRevisionField)
	}
	if view.isSharedBaseView {
		if err := c.composeBaseRootedRowsBatched(ctx, view, rows, includeArchived); err != nil {
			return err
		}
	} else {
		if err := c.h.MergeOwnerSiblingsBatch(ctx, rows, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.h.MergeSharedBaseBatch(ctx, rows, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.h.MergeSharedBaseChildrenBatch(ctx, rows, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.h.MergeOwnChildrenBatch(ctx, rows, view.schema, includeArchived); err != nil {
			return err
		}
		if err := c.applyEmbedsBatch(ctx, rows, hydrate.SchemaPK(view.schema), view.embeds, includeArchived); err != nil {
			return err
		}
	}
	// EmbedInChild enrichment — after the native child arrays are merged, for both
	// the shared-base and regular paths. Per-row (one $in per row per child-embed):
	// correct and idempotent; collapsing it across the whole batch is a deferred
	// optimization. No-op when the view declares no child-embeds.
	if len(view.childEmbeds) > 0 {
		for _, row := range rows {
			if err := c.applyChildEmbeds(ctx, row, view.childEmbeds); err != nil {
				return err
			}
		}
	}
	return nil
}

// composeBaseRootedRow fills a SharedBaseView document from its already-fetched
// base row: the base's native children nest at the root (mergeOwnChildren — the
// base IS the schema that owns them), then ONE SUB-DOCUMENT PER DECLARED ROLE,
// then the external embeds. A role's sub-document carries the role row (chosen
// active-first, see fetchRoleRow) with its siblings merged flat and its own
// children nested — keyed on the CHOSEN ROLE ROW's ID, never the base id (under
// the separate-ParentID model they differ). An absent role writes an explicit nil
// segment: the store's Upsert is $set, so a vanished role must overwrite its
// stale segment rather than silently survive it.
func (c *Composer) composeBaseRootedRow(ctx context.Context, view *ViewDefinition, row Document, baseID string, includeArchived bool) error {
	base := view.schema
	if err := c.h.MergeOwnChildren(ctx, row, base, includeArchived); err != nil {
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
		rolePK := hydrate.SchemaPK(r.schema)
		if err := c.h.MergeOwnerSiblings(ctx, roleRow, r.schema, fmt.Sprintf("%v", roleRow[rolePK]), includeArchived); err != nil {
			return err
		}
		if err := c.h.MergeOwnChildren(ctx, roleRow, r.schema, includeArchived); err != nil {
			return err
		}
		hydrate.RemapRevision(roleRow, r.schema, docRevisionField)
		row[r.segment] = roleRow
	}
	if err := c.applyEmbeds(ctx, row, hydrate.SchemaPK(base), view.embeds, includeArchived); err != nil {
		return err
	}
	// EmbedInChild enriches the BASE's native children (validated at boot). Role
	// sub-documents are never targeted.
	return c.applyChildEmbeds(ctx, row, view.childEmbeds)
}

// fetchRoleRow selects THE role row that represents a specialization of the
// identity — deterministic under the separate-ParentID multiplicity the write side
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
// A role without DeletedAt has no archived state (hard delete is delete), so
// a single fetch by ParentID decides it.
func (c *Composer) fetchRoleRow(ctx context.Context, r roleDef, baseID string, includeArchived bool) (Document, error) {
	_, fkCol, _ := r.schema.SharedBaseRef()
	sd, hasSD := hydrate.SchemaDeletedAt(r.schema)
	if !hasSD {
		return c.h.FetchRow(ctx, r.schema, r.schema.Table(), fkCol, baseID, "", true)
	}
	active, err := c.h.FetchRow(ctx, r.schema, r.schema.Table(), fkCol, baseID, sd, false)
	if err != nil || active != nil {
		return active, err
	}
	if !includeArchived {
		return nil, nil
	}
	return c.h.FetchLatestArchived(ctx, r.schema, fkCol, baseID, sd)
}

// applyEmbeds resolves each embed of the parent doc. parentPK is the parent
// source's ID column — the value matched against an EmbedMany child's ParentID.
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
			"(builder constructed without NewComposerWithMongo)", e.leg.Collection())
	}
	if e.many {
		id, ok := doc[parentPK]
		if !ok || id == nil {
			return nil
		}
		docs, err := c.mongo.FindManyByField(ctx, c.resolver.Active(e.leg.Collection()), e.JoinColumn(), id)
		if err != nil {
			return err
		}
		doc[e.Field()] = trimDocsToFields(docs, embedTrimSet(e))
		return nil
	}
	// An unresolved 1:1 (null ParentID, or the source doc gone) writes an EXPLICIT
	// null: view documents are $set-merged, so omitting the key would leave a
	// stale sub-document from a previous resolution in place forever — the
	// documented contract is "null when unset/unresolved".
	fk, ok := doc[e.JoinColumn()]
	if !ok || fk == nil {
		doc[e.Field()] = nil
		return nil
	}
	docs, err := c.mongo.FindManyByField(ctx, c.resolver.Active(e.leg.Collection()), "_id", fk)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		doc[e.Field()] = nil
		return nil
	}
	doc[e.Field()] = trimToFields(docs[0], embedTrimSet(e))
	return nil
}

// applyChildEmbeds resolves every EmbedInChild of the view against the ROOT doc
// (or the SharedBaseView base doc): for each declared child-embed it enriches
// each element of the native child array (doc[<childSegment>]) with a 1:1
// external lookup by the element's own ParentID, landing the source document under the
// child-embed's field. Set-based per child-embed — ONE $in against the source
// collection for all elements of this doc, keyed by the source _id. A missing /
// nil ParentID, or an unresolved source, writes an EXPLICIT null on that element (the
// $set-merge contract, identical to a root 1:1 embed). It runs ONLY on the root
// native children (the view's declared child-embeds target those, validated at
// boot); role sub-documents of a SharedBaseView are never enriched here.
func (c *Composer) applyChildEmbeds(ctx context.Context, doc Document, childEmbeds []childEmbedDef) error {
	if len(childEmbeds) == 0 {
		return nil
	}
	if c.mongo == nil {
		return fmt.Errorf("composer: EmbedInChild requires a MongoDB handle " +
			"(builder constructed without NewComposerWithMongo)")
	}
	for _, ce := range childEmbeds {
		arr, ok := doc[ce.ChildSegment()].([]Document)
		if !ok || len(arr) == 0 {
			continue
		}
		fkCol := ce.ParentIDColumn()
		field := ce.Field()
		values := make([]any, 0, len(arr))
		seen := map[string]struct{}{}
		for _, el := range arr {
			v, has := el[fkCol]
			if !has || v == nil {
				continue
			}
			key := fmt.Sprintf("%v", v)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			values = append(values, v)
		}
		byID := make(map[string]Document, len(values))
		if len(values) > 0 {
			srcDocs, err := c.mongo.FindManyByFieldIn(ctx, c.resolver.Active(ce.Source().Table()), "_id", values)
			if err != nil {
				return err
			}
			allow := childEmbedTrimSet(ce)
			for _, sd := range srcDocs {
				byID[fmt.Sprintf("%v", sd["_id"])] = trimToFields(sd, allow)
			}
		}
		for _, el := range arr {
			v, has := el[fkCol]
			if !has || v == nil {
				el[field] = nil
				continue
			}
			if sd, ok := byID[fmt.Sprintf("%v", v)]; ok {
				el[field] = sd
			} else {
				el[field] = nil
			}
		}
	}
	return nil
}
