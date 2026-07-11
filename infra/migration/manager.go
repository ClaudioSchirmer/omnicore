// Package migration provides management of SQL migration files via a wrapper
// over github.com/golang-migrate/migrate/v4. The outbox table is injected as
// migration 1 of the framework via embed.FS; the service provides its OWN
// migrations — an independent sequence starting at 0001 — in the
// cfg.Migrations.Dir directory (tracked in a separate table, so it does not
// continue the framework's numbering).
//
// The Manager operates 2 distinct migrate.Migrate instances in sequence:
//
//  1. Framework — iofs source (embedded), tracking table
//     "omnicore_framework_migrations". Applies 0001_outbox before the
//     service schema.
//  2. Service — file:// source (m.dir), tracking table
//     "omnicore_migrations". Applies the service's own files (0001+).
//
// Two tracking tables avoid version collision: framework and service can
// both have "version 1" without conflict because each has its own history.
package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source"
)

// Manager orchestrates framework migrations (embedded) + service migrations.
// The dialect selects the embedded framework source (embedded/<dialect>); the
// golang-migrate database driver is supplied by openDriver, set by the
// dialect-specific constructor (New / NewMySQL). Each constructor lives behind
// its engine build tag, so a single-engine build links exactly one database
// driver and its transitive SQL stack (pgx for Postgres, go-sql-driver for
// MySQL) — never both.
type Manager struct {
	dialect string
	dir     string

	// openDriver opens the golang-migrate database driver for trackingTable and
	// returns it alongside the migrate database name ("pgx5" / "mysql"). Set by
	// New (postgres build tag) or NewMySQL (mysql build tag); nil in a build with
	// neither engine, where Up/Down/Status are never reached (NewEngine aborts
	// boot first).
	openDriver func(trackingTable string) (database.Driver, string, error)
}

// Up applies all pending migrations — first the framework (embedded outbox),
// then the service. Re-calls with no pending changes return nil (ErrNoChange
// is absorbed). Failure at any stage marks dirty=true in the corresponding
// tracking table and blocks subsequent calls until Force.
func (m *Manager) Up(ctx context.Context) error {
	fwSrc, err := frameworkSourceFor(m.dialect)
	if err != nil {
		return err
	}
	if err := m.runUp("framework", fwSrc, frameworkTrackingTbl); err != nil {
		return err
	}

	svcSrc, err := serviceSource(m.dir)
	if err != nil {
		return err
	}
	return m.runUp("service", svcSrc, serviceTrackingTbl)
}

// Down reverts N service migrations. Does not touch the embedded outbox —
// reverting the outbox would break CDC and makes no sense at runtime.
func (m *Manager) Down(_ context.Context, steps int) error {
	if steps <= 0 {
		return errors.New("migration: steps must be > 0")
	}
	mig, err := m.openService()
	if err != nil {
		return err
	}
	defer closeMigrate(mig)

	if err := mig.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migration[service]: down %d steps: %w", steps, err)
	}
	return nil
}

// Status returns the current version and dirty state of the service. With no
// migrations applied → (0, false, nil).
func (m *Manager) Status(_ context.Context) (uint, bool, error) {
	mig, err := m.openService()
	if err != nil {
		return 0, false, err
	}
	defer closeMigrate(mig)

	v, dirty, err := mig.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migration[service]: version: %w", err)
	}
	return v, dirty, nil
}

// Force marks a version as applied with dirty=false in the service tracking
// table. Manual recovery after a migration failure that left an inconsistent
// state (e.g. SQL applied partially, connection dropped midway).
//
// Does not create or undo schema — just resets the tracking pointer.
// The user must manually correct the database state beforehand.
func (m *Manager) Force(_ context.Context, version int) error {
	mig, err := m.openService()
	if err != nil {
		return err
	}
	defer closeMigrate(mig)

	if err := mig.Force(version); err != nil {
		return fmt.Errorf("migration[service]: force %d: %w", version, err)
	}
	return nil
}

// Pending returns the migration versions that exist on disk under m.dir
// but have not yet been applied (the version is > Status's current
// version). Used by bootstrap.Run when migrations.autoRun=false to build
// the strict-mode boot-abort diagnostic that names every pending file.
//
// A missing directory is treated as empty (no pending migrations).
// Duplicate filenames carrying the same version are deduplicated.
func (m *Manager) Pending(ctx context.Context) ([]uint, error) {
	current, _, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("migration[service]: read dir %q: %w", m.dir, err)
	}
	seen := make(map[uint]struct{})
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		v, ok := parseMigrationVersion(name)
		if !ok {
			continue
		}
		if v <= current {
			continue
		}
		seen[v] = struct{}{}
	}
	out := make([]uint, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// parseMigrationVersion extracts the leading integer from a migration
// filename like "0003_add_phone.up.sql" → 3. Returns (0, false) when the
// prefix is not a valid unsigned integer.
func parseMigrationVersion(name string) (uint, bool) {
	idx := strings.Index(name, "_")
	if idx <= 0 {
		return 0, false
	}
	prefix := name[:idx]
	var v uint64
	for _, c := range prefix {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return uint(v), true
}

// ValidateDownExists verifies that every *.up.sql in m.dir has an equivalent
// *.down.sql. Called at startup when autoRun=true. Framework embedded
// migrations already come versioned with .down — no validation needed.
//
// Returns *core.InfrastructureError carrying
// MigrationDownMissingNotification when there are missing files, with the
// file list in the message. A non-existent directory is treated as empty
// (not an error — the service may have no migrations of its own).
func (m *Manager) ValidateDownExists() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("migration: read dir %q: %w", m.dir, err)
	}

	ups := map[string]string{}     // base (e.g. "0002_init") → up filename
	downs := map[string]struct{}{} // base → present
	var malformed []string         // files whose name carries no parseable version prefix
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			ups[strings.TrimSuffix(name, ".up.sql")] = name
			if _, ok := parseMigrationVersion(name); !ok {
				malformed = append(malformed, name)
			}
		case strings.HasSuffix(name, ".down.sql"):
			downs[strings.TrimSuffix(name, ".down.sql")] = struct{}{}
			if _, ok := parseMigrationVersion(name); !ok {
				malformed = append(malformed, name)
			}
		}
	}

	// A filename without a parseable "{version}_{name}" prefix is silently
	// dropped by golang-migrate's loader — the SQL never runs yet boot
	// reports success. Surface it loudly rather than let it slip through.
	if len(malformed) > 0 {
		sort.Strings(malformed)
		cause := fmt.Errorf(`migration file(s) without a parseable "{version}_{name}" prefix: %s`,
			strings.Join(malformed, ", "))
		return core.FieldErrorWithCause("Migration", filepath.Clean(m.dir), cause,
			MigrationFilenameInvalidNotification{})
	}

	var missing []string
	for base, up := range ups {
		if _, ok := downs[base]; !ok {
			missing = append(missing, up)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)

	cause := fmt.Errorf("missing .down.sql for: %s", strings.Join(missing, ", "))
	return core.FieldErrorWithCause("Migration", filepath.Clean(m.dir), cause,
		MigrationDownMissingNotification{})
}

func (m *Manager) runUp(label string, src source.Driver, trackingTable string) error {
	mig, err := m.open(label, src, trackingTable)
	if err != nil {
		return fmt.Errorf("migration[%s]: open: %w", label, err)
	}
	defer closeMigrate(mig)

	if err := mig.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migration[%s]: up: %w", label, err)
	}
	return nil
}

func (m *Manager) openService() (*migrate.Migrate, error) {
	src, err := serviceSource(m.dir)
	if err != nil {
		return nil, err
	}
	return m.open("service", src, serviceTrackingTbl)
}

// open creates a migrate.Migrate isolated per (source, trackingTable). The
// database driver comes from openDriver — the only dialect-bound piece, set by
// the constructor behind its engine build tag (pgx5 for Postgres over the live
// pool, mysql for a dedicated *sql.DB).
func (m *Manager) open(name string, src source.Driver, trackingTable string) (*migrate.Migrate, error) {
	drv, dbName, err := m.openDriver(trackingTable)
	if err != nil {
		return nil, err
	}
	mig, err := migrate.NewWithInstance(name, src, dbName, drv)
	if err != nil {
		_ = drv.Close()
		return nil, err
	}
	return mig, nil
}

// closeMigrate closes both the source and database drivers. Errors from
// close are discarded: either a semantic error already happened earlier, or
// the pool stays alive (see comment in open) and there is nothing to
// recover here.
func closeMigrate(mig *migrate.Migrate) {
	_, _ = mig.Close()
}
