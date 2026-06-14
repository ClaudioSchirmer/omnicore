package infra

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
)

// Postgres is the persistence adapter. After construction the audit
// surface is configured via WithAudit — without that call the persister
// runs in a fully audit-disabled posture (no in-TX row, no slog echo)
// which is correct for tests + integration fixtures that construct the
// pool directly. bootstrap.Run wires it in production after Build.
type Postgres struct {
	pool        *pgxpool.Pool
	auditCfg    *audit.Config
	logger      *slog.Logger
	auditClaims []string
}

func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool: pool}, nil
}

// WithAudit configures the audit surface and returns the receiver so the
// call can chain at boot. nil cfg disables audit entirely on this Postgres
// (no in-TX write, no slog echo); a Config with destinations: [] yields
// the same posture by design (empty list = off). nil logger falls back to
// slog.Default() inside the echo path. The auditClaims allowlist controls
// which JWT claims surface on the actorClaims block — see auth.auditClaims.
func (p *Postgres) WithAudit(cfg *audit.Config, logger *slog.Logger, auditClaims []string) *Postgres {
	p.auditCfg = cfg
	p.logger = logger
	p.auditClaims = auditClaims
	return p
}

// auditEnabled reports whether the configured destinations slice carries
// any active destination. Used by the per-method audit branches to skip
// the BuildEvent call when there is nothing to do.
func (p *Postgres) auditEnabled() bool {
	return p.auditCfg != nil &&
		(p.auditCfg.Includes(audit.DestinationSlog) || p.auditCfg.Includes(audit.DestinationDatabase))
}

// writeAuditRow performs the in-TX INSERT into audit_events when the
// database destination is active. Returns nil quickly when audit is off or
// the event was never built (caller passes nil).
func (p *Postgres) writeAuditRow(ctx context.Context, tx pgx.Tx, ev *audit.AuditEvent) error {
	if ev == nil || p.auditCfg == nil || !p.auditCfg.Includes(audit.DestinationDatabase) {
		return nil
	}
	return audit.InsertAuditEvent(ctx, tx, *ev)
}

// echoAuditSlog emits the slog audit line post-commit when the slog
// destination is active. No-op when audit is off or the event was never
// built.
func (p *Postgres) echoAuditSlog(ctx domain.Context, ev *audit.AuditEvent) {
	if ev == nil || p.auditCfg == nil || !p.auditCfg.Includes(audit.DestinationSlog) {
		return
	}
	audit.EchoSlog(ctx, p.logger, *ev)
}

func (p *Postgres) Close() {
	p.pool.Close()
}

// Pool returns the underlying pgxpool so repositories can run custom SELECTs
// (FindByID with JOINs, paginated lookups, etc.) that don't fit the write
// API. Use only for reads — writes must go through Insert/Update/Archive/
// Delete/Unarchive to preserve the outbox + audit guarantees.
func (p *Postgres) Pool() *pgxpool.Pool {
	return p.pool
}

func validIdentifier(name string) string {
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			panic(fmt.Sprintf("infra: invalid SQL identifier %q", name))
		}
	}
	return name
}

// SafeIdentifier is the non-panicking counterpart of validIdentifier.
// Returns true when name matches the framework's SQL identifier
// allowlist (ASCII letters, digits, underscore). Used by the admin CLI
// to validate operator-supplied identifiers before composing SQL —
// failing fast with an error beats panicking inside a one-shot binary.
func SafeIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}
