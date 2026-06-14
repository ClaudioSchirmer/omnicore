package audit

import "fmt"

// Destination names where the framework writes the AuditEvent produced
// after every successful write. The closed set of two destinations covers
// the design surface of `audit.destinations` in microservice.<profile>.yaml.
//
//   - DestinationSlog     — emit a structured slog line after COMMIT
//     (read-side / observability; lossy if the shipper drops events).
//   - DestinationDatabase — INSERT a row into `audit_events` inside the
//     same pgx.Tx that wrote the data row + outbox row (atomic; source of
//     truth for compliance and forensics).
//
// The two destinations are independent: a service can opt into both, either
// one, or none (the empty-list-as-off shape replaces a separate "disabled"
// enum). Default is both.
//
// The type lives in infra/audit (not bootstrap) so the Postgres persister
// can consume it without crossing the dependency boundary back to bootstrap.
// The YAML binding stays on Config (this package) and bootstrap.Config
// embeds it via `audit.Config`.
type Destination string

const (
	DestinationSlog     Destination = "slog"
	DestinationDatabase Destination = "database"
)

// Config is the `audit:` block of microservice.<profile>.yaml. Imported by
// bootstrap.Config as the type of its `Audit` field; methods are uppercase
// so callers across packages can invoke ApplyDefaults / Validate / Includes
// directly.
//
// The slice carries the explicit operator intent and the framework
// distinguishes three states by inspecting it:
//
//   - nil (the key is absent from YAML) → ApplyDefaults populates
//     [slog, database], the recommended posture that uses the always-present
//     audit_events table AND keeps the slog echo for ELK consumption.
//   - empty non-nil slice (the operator declared `destinations: []`) →
//     preserved verbatim, which turns audit off entirely. No row written,
//     no slog line emitted.
//   - populated slice → each item must be a known Destination value;
//     unknown tokens abort the boot naming the offender, duplicates abort
//     similarly so operator typos surface immediately.
type Config struct {
	// Destinations names where audit events are routed. nil means "use the
	// framework default" (see ApplyDefaults); a non-nil slice — including
	// the empty slice — is honored verbatim.
	Destinations []Destination `yaml:"destinations"`
}

// ApplyDefaults populates the recommended posture when the operator did not
// declare a `destinations:` key. A declared but empty slice is preserved as
// "audit off" — the operator made an explicit decision that the framework
// respects.
func (c *Config) ApplyDefaults() {
	if c.Destinations == nil {
		c.Destinations = []Destination{DestinationSlog, DestinationDatabase}
	}
}

// Validate enforces the closed-set membership rule for each entry plus
// uniqueness across the slice. Unknown tokens and duplicates abort the boot
// with a diagnostic naming the offender so operator typos surface
// immediately instead of degrading silently into wrong runtime behavior.
func (c *Config) Validate() error {
	seen := make(map[Destination]struct{}, len(c.Destinations))
	for _, d := range c.Destinations {
		switch d {
		case DestinationSlog, DestinationDatabase:
		default:
			return fmt.Errorf("audit.destinations: unknown value %q (allowed: %q, %q)",
				d, DestinationSlog, DestinationDatabase)
		}
		if _, ok := seen[d]; ok {
			return fmt.Errorf("audit.destinations: duplicate value %q", d)
		}
		seen[d] = struct{}{}
	}
	return nil
}

// Includes reports whether d is among the active destinations. Used by the
// Postgres persister to decide which write path runs for a given AuditEvent
// — the database branch lives inside the TX and the slog branch lives after
// COMMIT, both gated by this helper.
func (c *Config) Includes(d Destination) bool {
	for _, x := range c.Destinations {
		if x == d {
			return true
		}
	}
	return false
}
