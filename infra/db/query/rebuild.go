package query

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

const rebuildBatchSize = 1000

// RebuildConfig governs the per-view rebuild execution. Mirrors the
// mongo.rebuild yaml block — see bootstrap.MongoRebuildConfig and
// tasks/mongo_schema_evolution_2.md §10.
type RebuildConfig struct {
	// Orphan is "delete" or "warn" — see bootstrap.MongoRebuildConfig.
	Orphan string

	// ServiceName is stamped on the registry row's applied_by field for
	// forensics. Typically the cfg.Service value from bootstrap.LoadConfig.
	ServiceName string
}

// ExecuteRebuild runs the §10.1 sequence on one view, under the hybrid
// advisory-lock + status column primitive (PG pg_advisory_lock, MySQL GET_LOCK):
//
//  1. Acquire a pinned database connection from the pool (lock + status writes
//     must share the same connection; the advisory lock is bound to the
//     connection/session that acquired it).
//  2. Try the advisory lock; if held by another pod, abort with a
//     descriptive error.
//  3. Defer lock release. Auto-release on connection close is the safety
//     net.
//  4. UPDATE registry row to status='processing', started_at/pid/host.
//     When the prior status was already 'processing' (predecessor crashed),
//     emit slog.Warn before the UPDATE — §11.7 takeover.
//  5. Cleanup orphan fields (skip when collection empty).
//  6. Snapshot existing _id set from Mongo (collection no longer carries
//     reserved IDs).
//  7. Compose+upsert loop from the relational source: SELECT id FROM root_table, batch 1000,
//     Compose per id, mongo.Upsert.
//  8. Orphan reconciliation per cfg.Orphan setting.
//  9. UPDATE registry row to status='done', new hashes, captured previous_*.
//     This is the LAST data write.
//  10. slog.Info "view.rebuild.end".
//  11. Lock release fires via defer (and unpinning the connection releases
//     the advisory lock as the backstop).
//
// Returns a descriptive error on lock contention or any underlying
// Mongo/Postgres failure. Idempotent on _id — a rerun after a transient
// failure converges to the same state.
func (s *SyncEngine) ExecuteRebuild(ctx context.Context, plan DriftPlan, cfg RebuildConfig) error {
	view := plan.View
	collection := view.Name()
	// oldActive is the slot serving reads now; the rebuild builds the shadow slot
	// and flips readers to it, then reclaims oldActive. collection stays the
	// logical name for logging.
	oldActive := s.resolver.Active(collection)
	shadow := s.resolver.Shadow(collection)
	now := time.Now()

	switch cfg.Orphan {
	case "delete", "warn":
	default:
		return fmt.Errorf("ExecuteRebuild on view %q: cfg.Orphan %q invalid (want \"delete\" | \"warn\")", collection, cfg.Orphan)
	}

	// Step 1 — take the rebuild lock on a pinned session through the engine
	// seam. The lock is advisory + session-scoped on every backend (PG
	// pg_advisory_lock, MySQL GET_LOCK); the handle carries a Querier bound to
	// the pinned session so the status writes share the lock's connection.
	lock, err := s.eng.AcquireRebuildLock(ctx, collection)
	if err != nil {
		return fmt.Errorf("acquire rebuild lock on %q: %w", collection, err)
	}

	// Step 2 — defer release. Dropping the pinned session auto-releases the
	// lock too, but the explicit unlock surfaces errors via slog.
	defer func() {
		if releaseErr := lock.Release(ctx); releaseErr != nil {
			slog.WarnContext(ctx, "view.rebuild.lock_release_failed",
				slog.String("view", collection),
				slog.String("err", releaseErr.Error()))
		}
	}()

	// Step 3 — abort when another instance holds the lock.
	if !lock.Acquired() {
		if holder := lock.Holder(); holder != "" {
			return fmt.Errorf("rebuild lock on view %q held by %s — another instance is rebuilding", collection, holder)
		}
		return fmt.Errorf("rebuild lock on view %q held by another session (holder details unavailable) — another instance of the service is rebuilding this view", collection)
	}

	// regQ is the pinned-session Querier the status writes (BeginRebuild /
	// EndRebuild) run through; regD renders their dialect-specific bits.
	regQ := lock.Querier()
	regD := s.eng.Dialect()

	// Step 4 — detect takeover (§11.7) before writing status.
	if plan.Registry != nil && plan.Registry.Status == ViewRegistryStatusProcessing {
		holderPID, holderHost := "<unknown>", "<unknown>"
		if plan.Registry.PID != nil {
			holderPID = *plan.Registry.PID
		}
		if plan.Registry.Host != nil {
			holderHost = *plan.Registry.Host
		}
		holderStartedAt := "<unknown>"
		if plan.Registry.StartedAt != nil {
			holderStartedAt = plan.Registry.StartedAt.Format(time.RFC3339)
		}
		slog.WarnContext(ctx, "view.rebuild.takeover",
			slog.String("view", collection),
			slog.String("previous_started_at", holderStartedAt),
			slog.String("previous_pid", holderPID),
			slog.String("previous_host", holderHost))
	}

	// G5a — a crashed prior driver may have left a shadow flag + half-built
	// shadow collection. Clear the flag (ending any lingering dual-apply) and
	// drop the abandoned shadow before starting fresh, so the new rebuild builds
	// a clean slot and no pod keeps writing an orphaned one.
	if plan.Registry != nil && plan.Registry.ShadowCollection != nil {
		stale := PhysicalCollection{name: *plan.Registry.ShadowCollection}
		slog.WarnContext(ctx, "view.rebuild.takeover_shadow_reset",
			slog.String("view", collection), slog.String("stale_shadow", stale.String()))
		if err := abortSlotRebuild(ctx, regQ, regD, collection); err != nil {
			return fmt.Errorf("takeover %q: clear stale shadow flag: %w", collection, err)
		}
		if err := s.mongo.DropCollection(ctx, stale); err != nil {
			slog.WarnContext(ctx, "view.rebuild.takeover_shadow_drop_failed",
				slog.String("view", collection), slog.String("err", err.Error()))
		}
		if err := s.resolver.Refresh(ctx); err != nil {
			return fmt.Errorf("takeover %q: refresh after shadow reset: %w", collection, err)
		}
	}

	if err := BeginRebuild(ctx, regQ, regD, collection, now); err != nil {
		return err
	}

	slog.InfoContext(ctx, "view.rebuild.start",
		slog.String("view", collection),
		slog.String("from_hash", registryCombinedOrNone(plan.Registry)),
		slog.String("to_hash", plan.CurrentCombinedHash))

	// Step 5 — provision the shadow slot to the view's declared shape, record it
	// on the registry row (turning on dual-apply cluster-wide), and refresh so
	// this driver observes the shadow too.
	if err := s.mongo.ProvisionSlot(ctx, view, shadow); err != nil {
		return fmt.Errorf("rebuild %q: provision shadow %q: %w", collection, shadow, err)
	}
	if err := beginSlotRebuild(ctx, regQ, regD, collection, shadow.String()); err != nil {
		return err
	}
	if err := s.resolver.Refresh(ctx); err != nil {
		return fmt.Errorf("rebuild %q: refresh after begin: %w", collection, err)
	}

	// Step 6 — wait one lease so every pod's read-loop fence has observed the
	// dual-apply flag BEFORE the backfill reads anything (the G3 ordering
	// invariant): an event applied to the active slot only, after the backfill
	// passed that aggregate, would leave the shadow permanently gapped.
	if err := s.awaitFenceLease(ctx); err != nil {
		return err
	}

	// Step 7 — backfill the shadow from the relational source. Aggregates that
	// change during the backfill are carried into the shadow by dual-apply; the
	// backfill captures everything else.
	if err := s.backfillInto(ctx, view, shadow, ""); err != nil {
		return err
	}

	// Step 8 — verify the shadow (completeness + shape) before any reader can see
	// it. On failure the shadow is discarded and the flag cleared — the flip
	// never happens, so readers keep serving the untouched active slot.
	if err := s.verifyShadow(ctx, view, shadow); err != nil {
		s.discardShadow(ctx, regQ, regD, collection, shadow)
		return fmt.Errorf("rebuild %q: verify shadow: %w", collection, err)
	}

	// Step 9 — flip: one registry write moves the active pointer to the shadow
	// and clears the flag; refresh so this driver resolves reads to the new slot.
	if err := flipSlot(ctx, regQ, regD, collection); err != nil {
		return err
	}
	if err := s.resolver.Refresh(ctx); err != nil {
		return fmt.Errorf("rebuild %q: refresh after flip: %w", collection, err)
	}

	// Step 9 — UPDATE registry row to status='done' with new hashes.
	// previous_* are captured by the SQL itself (UPDATE reads current
	// state before writing). LAST data write — a crash before this leaves
	// the row at 'processing' with OLD hashes; next boot re-detects drift
	// + applies §11.7 takeover.
	if err := EndRebuild(ctx, regQ, regD, EndRebuildInput{
		ViewName:     collection,
		Version:      plan.CurrentVersion,
		RebuildHash:  plan.CurrentRebuildHash,
		ArtifactHash: plan.CurrentArtifactHash,
		CombinedHash: plan.CurrentCombinedHash,
		ServiceName:  cfg.ServiceName,
		Now:          time.Now(),
	}); err != nil {
		return err
	}

	// Step 11 — settle: wait one lease so dual-apply is off on every pod, then
	// reclaim the retired slot. A failed drop only leaks a collection the next
	// rebuild drop-and-recreates, so reclaim is best-effort.
	if err := s.awaitFenceLease(ctx); err != nil {
		return err
	}
	if err := s.mongo.DropCollection(ctx, oldActive); err != nil {
		slog.WarnContext(ctx, "view.rebuild.reclaim_failed",
			slog.String("view", collection),
			slog.String("retired", oldActive.String()),
			slog.String("err", err.Error()))
	}

	slog.InfoContext(ctx, "view.rebuild.end",
		slog.String("view", collection),
		slog.String("active", shadow.String()),
		slog.Duration("duration", time.Since(now)))

	return nil
}

// awaitFenceLease waits one bounded-staleness lease so every pod's read-loop
// fence has re-read the registry — before the backfill (so dual-apply is on
// everywhere) and after the flip (so it is off everywhere). A zero lease (tests)
// returns immediately.
func (s *SyncEngine) awaitFenceLease(ctx context.Context) error {
	wait := s.resolver.lease
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// discardShadow aborts a rebuild whose shadow failed verification: clear the flag
// (ending dual-apply cluster-wide) and drop the abandoned shadow. Best-effort —
// a leaked shadow is drop-and-recreated by the next rebuild.
func (s *SyncEngine) discardShadow(ctx context.Context, q core.Querier, d core.Dialect, viewName string, shadow PhysicalCollection) {
	if err := abortSlotRebuild(ctx, q, d, viewName); err != nil {
		slog.WarnContext(ctx, "view.rebuild.discard_flag_failed",
			slog.String("view", viewName), slog.String("err", err.Error()))
	}
	if err := s.mongo.DropCollection(ctx, shadow); err != nil {
		slog.WarnContext(ctx, "view.rebuild.discard_drop_failed",
			slog.String("view", viewName), slog.String("err", err.Error()))
	}
	if err := s.resolver.Refresh(ctx); err != nil {
		slog.WarnContext(ctx, "view.rebuild.discard_refresh_failed",
			slog.String("view", viewName), slog.String("err", err.Error()))
	}
}

// InitRegistryOnly is the fast path for the DriftFreshInit case under
// autoRun=true: no registry row exists AND the Mongo collection is empty.
// The framework writes the initial registry row with the spec hashes; no
// document write happens because there is nothing to compose. SyncEngine
// will populate the collection organically as events flow.
func (s *SyncEngine) InitRegistryOnly(ctx context.Context, plan DriftPlan, serviceName string) error {
	return InitViewRegistry(ctx, s.eng.Querier(), s.eng.Dialect(), InitViewRegistryInput{
		ViewName:     plan.View.Name(),
		Version:      plan.CurrentVersion,
		RebuildHash:  plan.CurrentRebuildHash,
		ArtifactHash: plan.CurrentArtifactHash,
		CombinedHash: plan.CurrentCombinedHash,
		ServiceName:  serviceName,
		Now:          time.Now(),
	})
}

// RefreshRegistryArtifactOnly is the fast path for the DriftArtifactOnly
// case under autoRun=true: only the index declaration changed.
// ApplyMongoSpecs already brought the indexes to the declared shape;
// only the registry row needs updating. No rebuild loop, no lock
// (technically safe to run without the advisory lock — the change is a
// metadata UPDATE and the index reconciliation already happened).
//
// The UPDATE captures previous_* the same way EndRebuild does so audit
// trail is preserved across the metadata-only transition.
func (s *SyncEngine) RefreshRegistryArtifactOnly(ctx context.Context, plan DriftPlan, serviceName string) error {
	return EndRebuild(ctx, s.eng.Querier(), s.eng.Dialect(), EndRebuildInput{
		ViewName:     plan.View.Name(),
		Version:      plan.CurrentVersion,
		RebuildHash:  plan.CurrentRebuildHash,
		ArtifactHash: plan.CurrentArtifactHash,
		CombinedHash: plan.CurrentCombinedHash,
		ServiceName:  serviceName,
		Now:          time.Now(),
	})
}

func registryCombinedOrNone(r *ViewRegistryRow) string {
	if r == nil {
		return "<none>"
	}
	return r.CombinedHash
}

// RebuildView / RebuildViewSince / RebuildAllViews are the operator-
// triggered rebuild paths kept from the predecessor design. They do NOT
// take the registry/lock path — they exist for ad-hoc operator use (e.g.
// "rebuild this view from scratch but don't touch the registry"). The
// boot-time rebuild path goes through ExecuteRebuild + the drift
// reconciliation in bootstrap.

func (s *SyncEngine) RebuildView(ctx context.Context, view *ViewDefinition) error {
	log.Printf("rebuilding view %s from table %s", view.name, view.rootTable)
	return s.rebuildFromTable(ctx, view, "")
}

func (s *SyncEngine) RebuildViewSince(ctx context.Context, view *ViewDefinition, since time.Time) error {
	log.Printf("rebuilding view %s since %s", view.name, since.Format(time.RFC3339))
	return s.rebuildFromTable(ctx, view, since.Format(time.RFC3339))
}

func (s *SyncEngine) RebuildAllViews(ctx context.Context) error {
	seen := map[string]bool{}
	walk := func(views []*ViewDefinition) error {
		for _, v := range views {
			if seen[v.name] {
				continue
			}
			seen[v.name] = true
			if err := s.RebuildView(ctx, v); err != nil {
				return err
			}
		}
		return nil
	}
	for _, views := range s.index.byPGTable {
		if err := walk(views); err != nil {
			return err
		}
	}
	for _, views := range s.index.byMongoColl {
		if err := walk(views); err != nil {
			return err
		}
	}
	return nil
}

func (s *SyncEngine) rebuildFromTable(ctx context.Context, view *ViewDefinition, since string) error {
	return s.backfillInto(ctx, view, s.resolver.Active(view.name), since)
}

// backfillInto streams the view's root PKs, composes each batch from the
// relational source in one set-based read, and bulk-upserts the composed
// documents into target — the active slot for an in-place operator rebuild, or a
// shadow slot for the blue-green driver. since != "" scopes the scan to rows
// changed at/after that watermark (incremental); "" is a full backfill. An id
// whose root vanished between the scan and the compose is absent from the
// composed set, so it is simply never written (blue-green verify reconciles it).
func (s *SyncEngine) backfillInto(ctx context.Context, view *ViewDefinition, target PhysicalCollection, since string) error {
	var q string
	var args []any

	idDialect := s.eng.Dialect()
	schema := view.SchemaDef()
	if schema == nil {
		return fmt.Errorf("rebuild %q: view declares no root .Schema(...)", view.name)
	}
	// Identifiers go through the dialect's QuoteIdent (backtick on MySQL, bare on
	// Postgres) and the column names come from the schema — never hardcoded — so a
	// reserved-word table (e.g. `order`) or a renamed PK/timestamp column (PK("key"))
	// rebuilds correctly on every backend.
	table := idDialect.QuoteIdent(view.rootTable)
	pkCol := idDialect.QuoteIdent(schema.PKColumn())
	if since != "" {
		updatedCol := schema.UpdatedAtColumn()
		if updatedCol == "" {
			return fmt.Errorf("rebuild %q since %s: schema declares no UpdatedAt column to scan incrementally", view.name, since)
		}
		uq := idDialect.QuoteIdent(updatedCol)
		q = "SELECT " + pkCol + " FROM " + table + " WHERE " + uq + " >= " + idDialect.Placeholder(1) + " ORDER BY " + uq
		args = []any{since}
	} else {
		order := pkCol
		if createdCol := schema.CreatedAtColumn(); createdCol != "" {
			order = idDialect.QuoteIdent(createdCol)
		}
		q = "SELECT " + pkCol + " FROM " + table + " ORDER BY " + order
	}

	rows, err := s.eng.Querier().Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	batch := make([]string, 0, rebuildBatchSize)
	pkColName := schema.PKColumn()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		composed, err := s.composer.ComposeBatch(ctx, view, batch)
		if err != nil {
			return err
		}
		batch = batch[:0]
		if len(composed) == 0 {
			return nil
		}
		docs := make([]IdentifiedDocument, 0, len(composed))
		for _, doc := range composed {
			docs = append(docs, IdentifiedDocument{ID: fmt.Sprintf("%v", doc[pkColName]), Doc: doc})
		}
		return s.mongo.BulkUpsert(ctx, target, docs)
	}

	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			return err
		}
		batch = append(batch, id)
		if len(batch) >= rebuildBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	return flush()
}

// rebuildScanSQL builds the id-scan SELECT a rebuild walks: the view's root PK
// (whatever the schema declares) ordered by the root's CreatedAt, falling back
// to the PK when the root declares none — e.g. a SharedBase root, whose
// timestamps are typically DDL-defaulted rather than schema-declared. The scan
// order only needs to be deterministic; creation order is a courtesy.
func rebuildScanSQL(view *ViewDefinition) string {
	pkCol := view.schema.PKColumn()
	orderCol := view.schema.CreatedAtColumn()
	if orderCol == "" {
		orderCol = pkCol
	}
	return "SELECT " + validIdentifier(pkCol) + " FROM " + validIdentifier(view.rootTable) +
		" ORDER BY " + validIdentifier(orderCol)
}
