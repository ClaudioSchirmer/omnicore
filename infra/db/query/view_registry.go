package query

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/google/uuid"
)

// ViewRegistryStatus is the closed set of values the status column on
// omnicore_mongo_views carries. See tasks/mongo_schema_evolution_2.md §11.7
// for the state machine.
type ViewRegistryStatus string

const (
	// ViewRegistryStatusDone is the steady state. The view has a known
	// shape and no rebuild is in flight.
	ViewRegistryStatusDone ViewRegistryStatus = "done"

	// ViewRegistryStatusProcessing means a rebuild owns the advisory lock
	// and is actively writing. started_at, pid, host are populated.
	ViewRegistryStatusProcessing ViewRegistryStatus = "processing"
)

// codeVersionEnv carries an optional build-time identifier the framework
// stamps on the registry row for forensics ("which deploy ran this
// rebuild?"). Empty when unset; not a boot blocker.
const codeVersionEnv = "OMNICORE_CODE_VERSION"

// ViewRegistryRow mirrors one row of omnicore_mongo_views. Pointer fields
// (PreviousVersion, PreviousAppliedAt, StartedAt) are nullable in the
// schema; nil means "the column is NULL".
type ViewRegistryRow struct {
	ViewName             string
	Version              int
	RebuildHash          string
	ArtifactHash         string
	CombinedHash         string
	PreviousVersion      *int
	PreviousCombinedHash *string
	PreviousAppliedAt    *time.Time
	Status               ViewRegistryStatus
	StartedAt            *time.Time
	PID                  *string
	Host                 *string
	AppliedAt            time.Time
	AppliedBy            string
	CodeVersion          *string
	// ActiveCollection is the physical Mongo collection currently serving reads
	// for this view; nil (NULL) means the bare <ViewName> collection is active
	// (the pre-blue-green state). ShadowCollection is the slot being built during
	// an online rebuild; non-nil is the dual-apply signal and names the build
	// target, nil between rebuilds.
	ActiveCollection *string
	ShadowCollection *string
}

// The registry helpers run through the backend-neutral core.Querier (the engine's
// pool, or a RebuildLock's pinned-session Querier for the in-lock status writes)
// and render the only dialect-divergent bit — the positional placeholder — via
// core.Dialect. The framework column names are bare identifiers valid unquoted on
// both Postgres and MySQL, so no QuoteIdent is needed here. Scanning relies on
// both pgx and database/sql mapping a NULL column into a nil pointer field.

// registrySelectColumns is the shared projection used by ReadViewRegistry and
// ListNonDone — one source of truth for the column order the Scan calls expect.
const registrySelectColumns = `view_name, version, rebuild_hash, artifact_hash, combined_hash,
       previous_version, previous_combined_hash, previous_applied_at,
       status, started_at, pid, host,
       applied_at, applied_by, code_version,
       active_collection, shadow_collection`

// newControlPlaneID mints the framework-standard surrogate id for a
// control-plane row: a UUID v7 generated in Go, returned as a domain.ID so the
// caller binds it through Dialect.EncodeArg into the dialect's native id form
// (uuid text on PG, BINARY(16) elsewhere) — the same id discipline as every
// domain table; no AUTO_INCREMENT/IDENTITY/DB default anywhere.
func newControlPlaneID() (domain.ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return domain.ID{}, fmt.Errorf("db: uuid v7: %w", err)
	}
	return domain.NewID(u.String()), nil
}

func sqlReadViewRegistry(d core.Dialect) string {
	return `SELECT ` + registrySelectColumns + `
FROM omnicore_mongo_views
WHERE view_name = ` + d.Placeholder(1)
}

func sqlInitViewRegistry(d core.Dialect) string {
	return `INSERT INTO omnicore_mongo_views
  (id, view_name, version, rebuild_hash, artifact_hash, combined_hash,
   status, applied_at, applied_by, code_version)
VALUES
  (` + d.Placeholder(1) + `, ` + d.Placeholder(2) + `, ` + d.Placeholder(3) + `, ` + d.Placeholder(4) + `, ` + d.Placeholder(5) + `, ` + d.Placeholder(6) + `, 'done', ` + d.Placeholder(7) + `, ` + d.Placeholder(8) + `, NULLIF(` + d.Placeholder(9) + `, ''))`
}

// Placeholders are numbered in APPEARANCE order (SET columns first, the WHERE
// key last) so the statement works on BOTH dialects: Postgres binds $n by
// number, MySQL binds each `?` positionally — so the arg order at the call site
// must match the order the placeholders appear in the text. (A PG-only layout
// with `view_name = $1` trailing would silently bind the wrong column on MySQL.)
func sqlBeginRebuild(d core.Dialect) string {
	return `UPDATE omnicore_mongo_views
   SET status = 'processing',
       started_at = ` + d.Placeholder(1) + `,
       pid = ` + d.Placeholder(2) + `,
       host = ` + d.Placeholder(3) + `
WHERE view_name = ` + d.Placeholder(4)
}

func sqlEndRebuild(d core.Dialect) string {
	return `UPDATE omnicore_mongo_views
   SET previous_version = version,
       previous_combined_hash = combined_hash,
       previous_applied_at = applied_at,
       version = ` + d.Placeholder(1) + `,
       rebuild_hash = ` + d.Placeholder(2) + `,
       artifact_hash = ` + d.Placeholder(3) + `,
       combined_hash = ` + d.Placeholder(4) + `,
       status = 'done',
       started_at = NULL,
       pid = NULL,
       host = NULL,
       applied_at = ` + d.Placeholder(5) + `,
       applied_by = ` + d.Placeholder(6) + `,
       code_version = NULLIF(` + d.Placeholder(7) + `, '')
WHERE view_name = ` + d.Placeholder(8)
}

// sqlListNonDone orders unfinished rows oldest-first. The leading CASE key is
// the portable NULLS-LAST idiom: MySQL has no NULLS LAST clause, and a bare
// "started_at IS NULL" sort key is a PG/MySQL-ism (T-SQL has no boolean
// expressions outside predicates) — ANSI CASE 1/0 sorts identically on all.
const sqlListNonDone = `SELECT ` + registrySelectColumns + `
FROM omnicore_mongo_views
WHERE status <> 'done'
ORDER BY CASE WHEN started_at IS NULL THEN 1 ELSE 0 END, started_at ASC`

// scanRegistryRow scans one neutral row into a ViewRegistryRow in the column
// order of registrySelectColumns.
func scanRegistryRow(s interface{ Scan(...any) error }, out *ViewRegistryRow) error {
	return s.Scan(
		&out.ViewName,
		&out.Version,
		&out.RebuildHash,
		&out.ArtifactHash,
		&out.CombinedHash,
		&out.PreviousVersion,
		&out.PreviousCombinedHash,
		&out.PreviousAppliedAt,
		&out.Status,
		&out.StartedAt,
		&out.PID,
		&out.Host,
		&out.AppliedAt,
		&out.AppliedBy,
		&out.CodeVersion,
		&out.ActiveCollection,
		&out.ShadowCollection,
	)
}

// ReadViewRegistry loads the row for the given view. Returns (nil, nil) when no
// row exists for that view (caller distinguishes "first boot" vs "drifted" via
// the §9 decision matrix). Uses Query+Next rather than QueryRow so the no-rows
// case is detected neutrally — pgx and database/sql disagree on the no-rows
// sentinel error.
func ReadViewRegistry(ctx context.Context, q core.Querier, d core.Dialect, viewName string) (*ViewRegistryRow, error) {
	rows, err := q.Query(ctx, sqlReadViewRegistry(d), viewName)
	if err != nil {
		return nil, fmt.Errorf("read view registry %q: %w", viewName, err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read view registry %q: %w", viewName, err)
		}
		return nil, nil
	}
	out := ViewRegistryRow{}
	if err := scanRegistryRow(rows, &out); err != nil {
		return nil, fmt.Errorf("read view registry %q: %w", viewName, err)
	}
	return &out, nil
}

// InitViewRegistryInput carries the data InitViewRegistry needs to write a
// brand-new row at status='done'. Used by the DriftFreshInit branch under
// autoRun=true and by the §14.9 manual-reconcile-init SQL pattern.
type InitViewRegistryInput struct {
	ViewName     string
	Version      int
	RebuildHash  string
	ArtifactHash string
	CombinedHash string
	ServiceName  string
	Now          time.Time
}

// InitViewRegistry inserts the initial row for a view at status='done'.
// Fails on conflict — the caller must check whether a row already exists
// (via ReadViewRegistry) before deciding to init.
func InitViewRegistry(ctx context.Context, q core.Querier, d core.Dialect, in InitViewRegistryInput) error {
	rowID, err := newControlPlaneID()
	if err != nil {
		return fmt.Errorf("init view registry %q: %w", in.ViewName, err)
	}
	err = q.Exec(ctx, sqlInitViewRegistry(d),
		d.EncodeArg(rowID),
		in.ViewName,
		in.Version,
		in.RebuildHash,
		in.ArtifactHash,
		in.CombinedHash,
		in.Now,
		FormatRegistryAppliedBy(in.ServiceName),
		os.Getenv(codeVersionEnv),
	)
	if err != nil {
		return fmt.Errorf("init view registry %q: %w", in.ViewName, err)
	}
	return nil
}

// BeginRebuild transitions the row to status='processing' at the start of a
// rebuild. Idempotent on re-entry: if a previous rebuild died at
// status='processing' (advisory lock auto-released, row left stale), this UPDATE
// rewrites started_at/pid/host with the current owner. Callers are expected to
// log the takeover via slog before calling this; the row write itself does not
// branch on the prior status.
//
// The advisory lock acquired via RelationalEngine.AcquireRebuildLock is what
// guarantees only one owner can be in this state at a time — this UPDATE has no
// SQL state guard. The registry row is guaranteed present by the caller's drift
// detection (it found the row) or a prior InitViewRegistry; the neutral Querier
// surface does not report rows-affected, so no "row missing" guard is asserted
// here (it was unreachable in the live control flow).
func BeginRebuild(ctx context.Context, q core.Querier, d core.Dialect, viewName string, now time.Time) error {
	pid := strconv.Itoa(os.Getpid())
	host, _ := os.Hostname()
	// Arg order matches the placeholder appearance order in sqlBeginRebuild
	// (started_at, pid, host, then the WHERE view_name) — required for MySQL's
	// positional `?` binding.
	if err := q.Exec(ctx, sqlBeginRebuild(d), now, pid, host, viewName); err != nil {
		return fmt.Errorf("begin rebuild on view %q: %w", viewName, err)
	}
	return nil
}

// EndRebuildInput carries the data EndRebuild needs to transition the row
// back to status='done'. captures previous_* from the row's current state
// (engine-side, via the UPDATE's read-before-write semantics) so the caller
// doesn't have to read the row twice.
type EndRebuildInput struct {
	ViewName     string
	Version      int
	RebuildHash  string
	ArtifactHash string
	CombinedHash string
	ServiceName  string
	Now          time.Time
}

// EndRebuild transitions the row from status='processing' to status='done',
// writes the new hashes, captures previous_* from the row's prior state (the
// UPDATE reads version/combined_hash/applied_at before writing). This is the
// **last data write** of the rebuild sequence — a crash before this point leaves
// status at 'processing' with the OLD hashes, which the next boot detects as
// drift + takeover (§11.7). As with BeginRebuild, the row's presence is the
// caller's invariant; no rows-affected guard is asserted on the neutral surface.
func EndRebuild(ctx context.Context, q core.Querier, d core.Dialect, in EndRebuildInput) error {
	// Arg order matches the placeholder appearance order in sqlEndRebuild
	// (the SET columns first, the WHERE view_name last) — required for MySQL's
	// positional `?` binding.
	err := q.Exec(ctx, sqlEndRebuild(d),
		in.Version,
		in.RebuildHash,
		in.ArtifactHash,
		in.CombinedHash,
		in.Now,
		FormatRegistryAppliedBy(in.ServiceName),
		os.Getenv(codeVersionEnv),
		in.ViewName,
	)
	if err != nil {
		return fmt.Errorf("end rebuild on view %q: %w", in.ViewName, err)
	}
	return nil
}

// ListNonDone returns every registry row whose status is not 'done' — typically
// empty in steady state, populated only when a rebuild is in flight. The partial
// index on status keeps the read O(1) regardless of total view count. Used by
// diagnostic helpers and by the §11.7 takeover detection at boot.
func ListNonDone(ctx context.Context, q core.Querier) ([]ViewRegistryRow, error) {
	rows, err := q.Query(ctx, sqlListNonDone)
	if err != nil {
		return nil, fmt.Errorf("list non-done views: %w", err)
	}
	defer rows.Close()

	var out []ViewRegistryRow
	for rows.Next() {
		var r ViewRegistryRow
		if err := scanRegistryRow(rows, &r); err != nil {
			return nil, fmt.Errorf("scan non-done view row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate non-done views: %w", err)
	}
	return out, nil
}

// FormatRegistryAppliedBy is the canonical applied_by string the framework
// writes to the registry: "<service>@pid:<process_id>" for framework writes;
// "unknown@pid:<process_id>" when ServiceName is empty. The §14 diagnostics
// instruct operators to use "manual-reconcile-*" prefixes for hand-written
// rows, so the column always discriminates between framework and operator
// writes on inspection.
func FormatRegistryAppliedBy(service string) string {
	pid := strconv.Itoa(os.Getpid())
	if service == "" {
		return "unknown@pid:" + pid
	}
	return service + "@pid:" + pid
}
