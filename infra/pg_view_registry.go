package infra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
}

// pgExec is the minimal interface the registry helpers consume. Both
// *pgxpool.Pool, *pgxpool.Conn, *pgx.Conn, and pgx.Tx satisfy it. Keeps the
// helpers reusable across plain pool calls and transactions / pinned
// connections.
type pgExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SQL constants — exported so callers can build prepared statements or
// reuse the strings in diagnostics. The framework's internal helpers use
// these directly.
const (
	sqlReadViewRegistry = `
SELECT view_name, version, rebuild_hash, artifact_hash, combined_hash,
       previous_version, previous_combined_hash, previous_applied_at,
       status, started_at, pid, host,
       applied_at, applied_by, code_version
FROM omnicore_mongo_views
WHERE view_name = $1`

	sqlInitViewRegistry = `
INSERT INTO omnicore_mongo_views
  (view_name, version, rebuild_hash, artifact_hash, combined_hash,
   status, applied_at, applied_by, code_version)
VALUES
  ($1, $2, $3, $4, $5, 'done', $6, $7, NULLIF($8, ''))`

	sqlBeginRebuild = `
UPDATE omnicore_mongo_views
   SET status = 'processing',
       started_at = $2,
       pid = $3,
       host = $4
WHERE view_name = $1`

	sqlEndRebuild = `
UPDATE omnicore_mongo_views
   SET previous_version = version,
       previous_combined_hash = combined_hash,
       previous_applied_at = applied_at,
       version = $2,
       rebuild_hash = $3,
       artifact_hash = $4,
       combined_hash = $5,
       status = 'done',
       started_at = NULL,
       pid = NULL,
       host = NULL,
       applied_at = $6,
       applied_by = $7,
       code_version = NULLIF($8, '')
WHERE view_name = $1`

	sqlListNonDone = `
SELECT view_name, version, rebuild_hash, artifact_hash, combined_hash,
       previous_version, previous_combined_hash, previous_applied_at,
       status, started_at, pid, host,
       applied_at, applied_by, code_version
FROM omnicore_mongo_views
WHERE status <> 'done'
ORDER BY started_at ASC NULLS LAST`
)

// ReadViewRegistry loads the row for the given view. Returns (nil, nil)
// when no row exists for that view (caller distinguishes "first boot" vs
// "drifted" via the §9 decision matrix).
func ReadViewRegistry(ctx context.Context, exec pgExec, viewName string) (*ViewRegistryRow, error) {
	row := exec.QueryRow(ctx, sqlReadViewRegistry, viewName)
	out := ViewRegistryRow{}
	err := row.Scan(
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
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
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
func InitViewRegistry(ctx context.Context, exec pgExec, in InitViewRegistryInput) error {
	_, err := exec.Exec(ctx, sqlInitViewRegistry,
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

// BeginRebuild transitions the row to status='processing' at the start of
// a rebuild. Idempotent on re-entry: if a previous rebuild died at
// status='processing' (advisory lock auto-released, row left stale), this
// UPDATE rewrites started_at/pid/host with the current owner. Callers are
// expected to log the takeover via slog before calling this; the row write
// itself does not branch on the prior status.
//
// The advisory lock acquired in pg_view_lock.go is what guarantees only one
// owner can be in this state at a time — this UPDATE has no SQL state
// guard.
func BeginRebuild(ctx context.Context, exec pgExec, viewName string, now time.Time) error {
	pid := strconv.Itoa(os.Getpid())
	host, _ := os.Hostname()
	tag, err := exec.Exec(ctx, sqlBeginRebuild, viewName, now, pid, host)
	if err != nil {
		return fmt.Errorf("begin rebuild on view %q: %w", viewName, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("begin rebuild on view %q: registry row missing (call InitViewRegistry first)", viewName)
	}
	return nil
}

// EndRebuildInput carries the data EndRebuild needs to transition the row
// back to status='done'. captures previous_* from the row's current state
// (Postgres-side, via the UPDATE's SELECT-on-self semantics) so the caller
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
// writes the new hashes, captures previous_* from the row's prior state
// (the UPDATE reads version/combined_hash/applied_at before writing). This
// is the **last data write** of the rebuild sequence — a crash before this
// point leaves status at 'processing' with the OLD hashes, which the next
// boot detects as drift + takeover (§11.7).
func EndRebuild(ctx context.Context, exec pgExec, in EndRebuildInput) error {
	tag, err := exec.Exec(ctx, sqlEndRebuild,
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
		return fmt.Errorf("end rebuild on view %q: %w", in.ViewName, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("end rebuild on view %q: registry row missing", in.ViewName)
	}
	return nil
}

// ListNonDone returns every registry row whose status is not 'done' —
// typically empty in steady state, populated only when a rebuild is in
// flight. The partial index on status keeps the read O(1) regardless of
// total view count. Used by diagnostic helpers and by the §11.7 takeover
// detection at boot.
func ListNonDone(ctx context.Context, exec pgExec) ([]ViewRegistryRow, error) {
	rows, err := exec.Query(ctx, sqlListNonDone)
	if err != nil {
		return nil, fmt.Errorf("list non-done views: %w", err)
	}
	defer rows.Close()

	var out []ViewRegistryRow
	for rows.Next() {
		var r ViewRegistryRow
		if err := rows.Scan(
			&r.ViewName,
			&r.Version,
			&r.RebuildHash,
			&r.ArtifactHash,
			&r.CombinedHash,
			&r.PreviousVersion,
			&r.PreviousCombinedHash,
			&r.PreviousAppliedAt,
			&r.Status,
			&r.StartedAt,
			&r.PID,
			&r.Host,
			&r.AppliedAt,
			&r.AppliedBy,
			&r.CodeVersion,
		); err != nil {
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
