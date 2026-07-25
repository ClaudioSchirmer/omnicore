package query

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// rebuildBatchSize is the default number of root ids composed + bulk-upserted per
// batch when yaml leaves mongo.rebuild.batchSize unset. rebuildWorkers is the
// default number of concurrent compose+write workers driving the backfill
// pipeline when mongo.rebuild.workers is unset — see backfillInto.
const (
	rebuildBatchSize = 1000
	rebuildWorkers   = 4
)

// ErrRebuildLockHeld is returned by ExecuteRebuild when another live instance
// holds the per-view rebuild lock. It is NOT a failure: the boot path treats it
// as "this pod is a follower — serve the current active slot and pick up the
// driver's flip at runtime via the resolver's lease refresh", rather than
// aborting the boot. Detect it with errors.Is(err, ErrRebuildLockHeld).
var ErrRebuildLockHeld = errors.New("view rebuild lock held by another instance")

// RebuildConfig governs the per-view rebuild execution. Mirrors the
// mongo.rebuild yaml block — see bootstrap.MongoRebuildConfig and
// tasks/mongo_schema_evolution_2.md §10.
type RebuildConfig struct {
	// Orphan is "delete" or "warn" — see bootstrap.MongoRebuildConfig.
	Orphan string

	// ServiceName is stamped on the registry row's applied_by field for
	// forensics. Typically the cfg.Service value from bootstrap.LoadConfig.
	ServiceName string

	// Workers is the number of concurrent compose+write workers the backfill
	// pipeline runs (mongo.rebuild.workers). 0 → the framework default
	// (rebuildWorkers). Each worker composes a batch from the relational source
	// and bulk-upserts it; they run independently because every root document is
	// independent. The relational pool should carry at least Workers+1
	// connections (one is pinned by the streaming root-id scan).
	Workers int

	// BatchSize is the number of root ids composed + bulk-upserted per batch
	// (mongo.rebuild.batchSize). 0 → the framework default (rebuildBatchSize).
	BatchSize int
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

	// Step 3 — another instance holds the lock. Return the ErrRebuildLockHeld
	// sentinel so the boot path treats this pod as a FOLLOWER (serve the active
	// slot, wait for the driver's flip) instead of aborting.
	if !lock.Acquired() {
		if holder := lock.Holder(); holder != "" {
			return fmt.Errorf("rebuild lock on view %q held by %s — another instance is rebuilding: %w", collection, holder, ErrRebuildLockHeld)
		}
		return fmt.Errorf("rebuild lock on view %q held by another session (holder details unavailable) — another instance is rebuilding this view: %w", collection, ErrRebuildLockHeld)
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

	// Step 5 — the shadow ALWAYS starts empty: drop whatever occupies the slot
	// first. A retired slot whose one-lease reclaim never ran (the process shut
	// down before the lease elapsed) still holds the PREVIOUS generation's
	// documents — G5a only covers registry-FLAGGED shadows, so an unreclaimed
	// retiree is invisible to it. Reusing it without the drop resurrects
	// leftover documents: verify's reverse pass deletes the ones its snapshot
	// saw, but its deliberate late-write protection (a doc arriving after the
	// snapshot is presumed a dual-applied concurrent write) cannot tell a
	// leftover from a live write — so the slot must be clean BY CONSTRUCTION,
	// never by reconciliation. Then provision the declared shape and record the
	// slot on the registry row (turning on dual-apply cluster-wide).
	if err := s.mongo.DropCollection(ctx, shadow); err != nil {
		return fmt.Errorf("rebuild %q: drop stale shadow %q: %w", collection, shadow, err)
	}
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
	if err := s.backfillInto(ctx, view, shadow, "", cfg.Workers, cfg.BatchSize, regQ); err != nil {
		// A partial/aborted backfill must not linger. The pipeline already stopped
		// every worker and returned the first error (no goroutine outlives Wait),
		// but the dual-apply flag + half-built shadow are still live cluster-wide.
		// Clear the flag and drop the shadow — symmetric with the verify-failure
		// path — so no pod keeps dual-applying to a shadow that will never flip and
		// the next rebuild starts clean. The active slot is never touched, so
		// readers keep serving authoritative data: no flip, no data loss.
		s.discardShadow(ctx, regQ, regD, collection, shadow)
		return fmt.Errorf("rebuild %q: backfill shadow: %w", collection, err)
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
	log.Printf("rebuilding view %s from table %s", view.name, view.RootTable())
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
	// Operator-triggered path: no RebuildConfig and no rebuild lock, so the
	// pipeline runs at the framework defaults (0 → rebuildWorkers / rebuildBatchSize)
	// and the scan uses the shared pool (nil scanQ).
	return s.backfillInto(ctx, view, s.resolver.Active(view.name), since, 0, 0, nil)
}

// backfillInto streams the view's root PKs and drives a bounded producer/consumer
// pipeline: the scanning goroutine (this one) accumulates ids into batches and
// hands each to a pool of `workers` goroutines, each of which composes its batch
// from the relational source in one set-based read and bulk-upserts the composed
// documents into target — the active slot for an in-place operator rebuild, or a
// shadow slot for the blue-green driver. Overlapping the relational scan+compose
// with the Mongo write (and fanning independent batches across workers) collapses
// the wall-clock from Σ(read+compose+write) toward the slowest single stage.
//
// since != "" scopes the scan to rows changed at/after that watermark
// (incremental); "" is a full backfill. An id whose root vanished between the
// scan and the compose is absent from the composed set, so it is simply never
// written (blue-green verify reconciles it). Every root document is independent,
// so batch order does not matter; the upsert is idempotent on _id.
//
// workers/batchSize come from mongo.rebuild (0 → the framework defaults).
//
// scanQ is the querier the streaming root-id scan runs on. The blue-green driver
// passes the lock's PINNED session (already reserved for this rebuild and idle
// during the backfill), so the long-lived scan cursor does NOT consume a second
// pool connection — leaving the whole pool for the workers' composer queries.
// The minimum pool for progress is then 2 (one for lock+scan, one for a
// composer) instead of 3, and there is no path where the scan cursor starves the
// composers. A nil scanQ (the operator ad-hoc path, no lock) falls back to the
// shared pool. Either way the pool should carry at least workers+1 connections to
// let all workers compose concurrently.
func (s *SyncEngine) backfillInto(ctx context.Context, view *ViewDefinition, target PhysicalCollection, since string, workers, batchSize int, scanQ core.Querier) error {
	if workers <= 0 {
		workers = rebuildWorkers
	}
	if batchSize <= 0 {
		batchSize = rebuildBatchSize
	}
	if scanQ == nil {
		scanQ = s.eng.Querier()
	}

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
	table := idDialect.QuoteIdent(view.RootTable())
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
		// Full backfill: NO ORDER BY. Every live root is composed exactly once
		// regardless of scan order, so ordering buys nothing but forces the engine
		// to materialize a full sort of the root table (O(n log n) + temp space)
		// before the first row can flow — a real cost at millions of rows. Dropping
		// it lets the first batch start immediately and streams straight off the
		// scan. Safety is unchanged: the scan reads one consistent snapshot (each
		// live row exactly once, order-independent), a row changed during the scan
		// is carried by dual-apply, and verify's forward-completeness pass composes
		// any id the snapshot missed — duplicate composes are idempotent on _id.
		q = "SELECT " + pkCol + " FROM " + table
	}

	rows, err := scanQ.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	pkColName := schema.PKColumn()

	// A cancelable child context lets the first failing worker/scan stop the whole
	// pipeline; batches is bounded so a slow Mongo write back-pressures the scan.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	batches := make(chan []string, workers)
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		errMu.Unlock()
	}

	// The batch write is GUARDED (consultGuardedStages via BulkApplyProjection),
	// never a plain $set: a live write racing this batch dual-applies to the
	// shadow with a fresher revision, and a full-document overwrite landing
	// after it would silently regress the slot that is about to flip. The
	// guarded pipeline makes the stale batch a no-op on that document instead.
	composeAndWrite := func(batch []string) error {
		composed, err := s.composer.ComposeBatch(ctx, view, batch)
		if err != nil {
			return err
		}
		if len(composed) == 0 {
			return nil
		}
		items := make([]IdentifiedStages, 0, len(composed))
		for _, doc := range composed {
			items = append(items, IdentifiedStages{ID: fmt.Sprintf("%v", doc[pkColName]), Stages: consultGuardedStages(view, doc)})
		}
		return s.mongo.BulkApplyProjection(ctx, target, items)
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range batches {
				if ctx.Err() != nil {
					continue // drain the channel without work once cancelled
				}
				if err := composeAndWrite(batch); err != nil {
					fail(err)
				}
			}
		}()
	}

	// Producer: scan the root ids, cut fixed-size batches, and hand each (a fresh
	// slice — the sent batch is owned by a worker) to the pool. A scan error or a
	// cancelled context stops production; close(batches) then drains the workers.
	batch := make([]string, 0, batchSize)
	send := func() bool {
		select {
		case batches <- batch:
			batch = make([]string, 0, batchSize)
			return true
		case <-ctx.Done():
			return false
		}
	}
	for rows.Next() {
		if ctx.Err() != nil {
			break
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			fail(err)
			break
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			fail(err)
			break
		}
		batch = append(batch, id)
		if len(batch) >= batchSize {
			if !send() {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		fail(err)
	} else if ctx.Err() == nil && len(batch) > 0 {
		send()
	}

	close(batches)
	wg.Wait()
	return firstErr
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
	return "SELECT " + validIdentifier(pkCol) + " FROM " + validIdentifier(view.RootTable()) +
		" ORDER BY " + validIdentifier(orderCol)
}
