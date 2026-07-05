package query

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"time"
)

const rebuildBatchSize = 1000

// sampleSizeForCleanup is the number of relational ids the rebuild
// composes during the cleanup step to derive the expected field set.
// Small enough to cost milliseconds; large enough that a single row
// with NULL columns does not skew the expected keys.
const sampleSizeForCleanup = 5

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

	if err := BeginRebuild(ctx, regQ, regD, collection, now); err != nil {
		return err
	}

	slog.InfoContext(ctx, "view.rebuild.start",
		slog.String("view", collection),
		slog.String("from_hash", registryCombinedOrNone(plan.Registry)),
		slog.String("to_hash", plan.CurrentCombinedHash))

	// Step 5 — cleanup (skip when collection empty).
	populated, err := s.mongo.HasDocuments(ctx, collection)
	if err != nil {
		return err
	}
	var orphanFields []string
	if populated {
		orphanFields, err = computeOrphanFields(ctx, s.mongo, s.composer, view)
		if err != nil {
			return err
		}
		if len(orphanFields) > 0 {
			if err := s.mongo.UnsetFields(ctx, collection, orphanFields); err != nil {
				return err
			}
		}
	}

	// Step 6 — snapshot _id set for orphan reconciliation.
	snapshot, err := s.mongo.SnapshotDocumentIDs(ctx, collection)
	if err != nil {
		return err
	}

	// Step 7 — compose+upsert from Postgres, tracking which ids we touched.
	upserted := 0
	flush := func(batch []string) error {
		for _, id := range batch {
			doc, composeErr := s.composer.Compose(ctx, view, id)
			if composeErr != nil {
				return composeErr
			}
			if doc == nil {
				continue
			}
			if upsertErr := s.mongo.Upsert(ctx, collection, id, doc); upsertErr != nil {
				return upsertErr
			}
			delete(snapshot, id)
			upserted++
		}
		return nil
	}

	// The root ids are read through the engine's neutral Querier (the pool, not
	// the lock's pinned session — a plain read needs no lock affinity). Each
	// scanned key is decoded via the dialect (identity on PG; BINARY(16) → uuid
	// string on MySQL), matching the canonical id form Compose expects. The
	// columns come from the view's root schema — the PK is whatever the schema
	// declares, and the scan order falls back to the PK when the root declares
	// no CreatedAt (e.g. a SharedBase root, whose timestamps are typically
	// DDL-defaulted rather than schema-declared).
	idDialect := s.eng.Dialect()
	q := rebuildScanSQL(view)
	rows, err := s.eng.Querier().Query(ctx, q)
	if err != nil {
		return err
	}
	batch := make([]string, 0, rebuildBatchSize)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, id)
		if len(batch) >= rebuildBatchSize {
			if err := flush(batch); err != nil {
				rows.Close()
				return err
			}
			batch = batch[:0]
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if err := flush(batch); err != nil {
		return err
	}

	// Step 8 — orphan reconciliation.
	orphanCount, err := reconcileOrphans(ctx, s.mongo, collection, snapshot, cfg.Orphan)
	if err != nil {
		return err
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

	slog.InfoContext(ctx, "view.rebuild.end",
		slog.String("view", collection),
		slog.Int("upserted", upserted),
		slog.Int("orphan_fields_unset", len(orphanFields)),
		slog.Int("orphan_docs", orphanCount),
		slog.Duration("duration", time.Since(now)))

	return nil
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

// computeOrphanFields runs the aggregation full-scan to discover every
// top-level field currently present in the collection, samples up to
// sampleSizeForCleanup real ids from Postgres to derive the expected
// field set via Compose, and returns the (observed - expected) set.
//
// Returns an empty slice when nothing diverges (steady state). When the
// collection has data but Postgres is empty (Compose returns nothing on
// every sample), returns an empty slice rather than the full observed
// set — the orphan reconciliation step will deleteMany every doc; there
// is no point in $unset-ing fields on docs that are about to disappear.
func computeOrphanFields(ctx context.Context, m ReadModelStore, c *Composer, view *ViewDefinition) ([]string, error) {
	observed, err := m.ObservedFieldNames(ctx, view.Name())
	if err != nil {
		return nil, err
	}
	if len(observed) == 0 {
		return nil, nil
	}
	expected, err := sampleExpectedFieldNames(ctx, c, view)
	if err != nil {
		return nil, err
	}
	if len(expected) == 0 {
		return nil, nil
	}

	// "_id" is always present and is never an orphan — Mongo's primary
	// key, not a domain field.
	expected["_id"] = struct{}{}

	var orphan []string
	for k := range observed {
		if _, ok := expected[k]; !ok {
			orphan = append(orphan, k)
		}
	}
	return orphan, nil
}

// sampleExpectedFieldNames composes a handful of real Postgres ids to
// derive the field set the current code emits. Returns an empty map when
// Postgres has no rows to sample — caller treats this as "skip cleanup"
// because the orphan reconciliation will deleteMany every Mongo doc.
func sampleExpectedFieldNames(ctx context.Context, c *Composer, view *ViewDefinition) (map[string]struct{}, error) {
	idDialect := c.eng.Dialect()
	schema := view.SchemaDef()
	if schema == nil {
		return nil, fmt.Errorf("sample ids: view %q declares no root .Schema(...)", view.Name())
	}
	q := "SELECT " + idDialect.QuoteIdent(schema.PKColumn()) + " FROM " + idDialect.QuoteIdent(view.rootTable) +
		" LIMIT " + fmt.Sprintf("%d", sampleSizeForCleanup)
	rows, err := c.eng.Querier().Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sample ids from %q: %w", view.rootTable, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	expected := make(map[string]struct{})
	for _, id := range ids {
		doc, err := c.Compose(ctx, view, id)
		if err != nil {
			return nil, fmt.Errorf("compose sample id %q on view %q: %w", id, view.Name(), err)
		}
		if doc == nil {
			continue
		}
		for k := range doc {
			expected[k] = struct{}{}
		}
	}
	return expected, nil
}

// reconcileOrphans handles the leftover _ids after the compose+upsert
// loop — documents whose Postgres source returned nil (hard-deleted, or
// archived in a DeleteOnArchive view). cfg.Orphan governs the action:
// "delete" issues a single deleteMany; "warn" emits a slog.Warn listing
// the ids and leaves them untouched.
func reconcileOrphans(ctx context.Context, m ReadModelStore, collection string, snapshot map[string]struct{}, mode string) (int, error) {
	if len(snapshot) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}

	switch mode {
	case "delete":
		n, err := m.DeleteByIDs(ctx, collection, ids)
		if err != nil {
			return 0, fmt.Errorf("delete orphans on %q: %w", collection, err)
		}
		return n, nil
	case "warn":
		slog.WarnContext(ctx, "view.rebuild.orphans_warning",
			slog.String("view", collection),
			slog.Int("orphan_count", len(ids)),
			slog.Any("orphan_ids", ids))
		return 0, nil
	default:
		return 0, errors.New("reconcileOrphans: invalid mode " + mode)
	}
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

	flush := func() error {
		for _, id := range batch {
			doc, err := s.composer.Compose(ctx, view, id)
			if err != nil {
				return err
			}
			if doc == nil {
				continue
			}
			if err := s.mongo.Upsert(ctx, view.name, id, doc); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
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
