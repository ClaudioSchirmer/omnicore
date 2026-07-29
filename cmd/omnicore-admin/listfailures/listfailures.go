// Package listfailures implements the omnicore-admin "list-failures"
// subcommand. It reads pending rows from omnicore_projection_failures — the
// read-side's unified failure ledger (kind=event parked projection events,
// kind=ripple failed embed-segment refreshes) — and prints them as text or
// JSON for operator triage.
//
// Read-only — the framework binary has no Wiring of the consumer service, so
// it cannot replay rows itself. Replay is automatic: the service's parked-retry
// loop (the mongo.parkedRetry knob) sweeps both kinds on its cadence. This CLI
// is the inspection surface; the runtime loop is the action surface.
package listfailures

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/bootstrap"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

const (
	formatText = "text"
	formatJSON = "json"
)

// Run is the subcommand entry point invoked by cmd/omnicore-admin/main.go.
// Returns nil on success; a non-nil error is printed to stderr and the
// process exits with status 1.
func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list-failures", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := newUsage(fs)
	fs.Usage = usage

	kind := fs.String("kind", "",
		"Filter by kind — event | ripple (default: both)")
	topic := fs.String("topic", "",
		"Filter by topic/source (default: every source). Examples: users.events, view:products")
	view := fs.String("view", "",
		"Filter by the dependent view of ripple rows / the aggregate type of event rows")
	format := fs.String("format", formatText,
		"Output format — text | json (default text)")
	limit := fs.Int("limit", 100,
		"Maximum number of rows to print (default 100; pass 0 for unlimited)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *format != formatText && *format != formatJSON {
		usage()
		return fmt.Errorf("list-failures: --format must be text or json, got %q", *format)
	}
	if *kind != "" && *kind != string(query.ProjectionFailureKindEvent) && *kind != string(query.ProjectionFailureKindRipple) {
		return fmt.Errorf("list-failures: --kind must be event or ripple, got %q", *kind)
	}
	if *limit < 0 {
		return fmt.Errorf("list-failures: --limit must be >= 0, got %d", *limit)
	}

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		return fmt.Errorf("list-failures: load config: %w", err)
	}
	// Build through the relational-engine registry so inspection runs against the
	// configured backend (relational.dialect). The ledger reads already go
	// through the neutral Querier/Dialect; only construction was PG-bound. A MySQL
	// deployment needs the admin binary built with -tags mysql.
	engine, err := core.NewEngine(cfg.Relational.Dialect, ctx, core.EngineConfig{DSN: cfg.Relational.DSN})
	if err != nil {
		return fmt.Errorf("list-failures: connect: %w", err)
	}
	defer engine.Close()

	return execute(ctx, engine, executeOptions{
		Group:  cfg.Transport.SyncGroup,
		Kind:   *kind,
		Topic:  *topic,
		View:   *view,
		Format: *format,
		Limit:  *limit,
		Out:    os.Stdout,
	})
}

type executeOptions struct {
	Group  string
	Kind   string
	Topic  string
	View   string
	Format string
	Limit  int
	Out    io.Writer
}

// execute does the read + render through the backend-neutral seam. Extracted
// from Run so tests can drive it without touching env / config.
func execute(ctx context.Context, engine core.RelationalEngine, opt executeOptions) error {
	rows, err := query.ListPendingProjectionFailures(ctx, engine.Querier(), engine.Dialect(), opt.Group)
	if err != nil {
		return fmt.Errorf("list-failures: list: %w", err)
	}
	filtered := rows[:0]
	for _, r := range rows {
		if opt.Kind != "" && string(r.Kind) != opt.Kind {
			continue
		}
		if opt.Topic != "" && r.Topic != opt.Topic {
			continue
		}
		if opt.View != "" && r.AggregateType != opt.View {
			continue
		}
		filtered = append(filtered, r)
	}
	rows = filtered
	truncated := false
	if opt.Limit > 0 && len(rows) > opt.Limit {
		rows = rows[:opt.Limit]
		truncated = true
	}
	switch opt.Format {
	case formatJSON:
		return renderJSON(opt.Out, rows, truncated)
	default:
		return renderText(opt.Out, rows, truncated)
	}
}

// renderJSON writes the rows as a JSON object — `{count, truncated, items}`
// shape so downstream tooling never has to guess whether the array was capped.
func renderJSON(out io.Writer, rows []query.ProjectionFailureRecord, truncated bool) error {
	envelope := struct {
		Count     int                             `json:"count"`
		Truncated bool                            `json:"truncated"`
		Items     []query.ProjectionFailureRecord `json:"items"`
	}{
		Count:     len(rows),
		Truncated: truncated,
		Items:     rows,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(envelope)
}

// renderText writes a fixed-column listing. Columns chosen for triage
// signal — last_attempt_at + attempt + error help identify a stuck
// failure vs a transient one at a glance.
func renderText(out io.Writer, rows []query.ProjectionFailureRecord, truncated bool) error {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no pending projection failures")
		return nil
	}
	fmt.Fprintf(out, "%d pending projection failure(s)", len(rows))
	if truncated {
		fmt.Fprint(out, " (truncated — pass --limit 0 for the full list)")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, strings.Repeat("-", 125))
	fmt.Fprintf(out, "%-7s %-25s %-20s %-36s %-9s %-3s %-19s %s\n",
		"KIND", "TOPIC/SOURCE", "VIEW/TYPE", "ID", "STAGE", "AT#", "LAST_ATTEMPT_AT", "ERROR")
	fmt.Fprintln(out, strings.Repeat("-", 125))
	for _, r := range rows {
		fmt.Fprintf(out, "%-7s %-25s %-20s %-36s %-9s %-3d %-19s %s\n",
			r.Kind,
			truncateString(r.Topic, 25),
			truncateString(r.AggregateType, 20),
			truncateString(r.AggregateID, 36),
			r.Stage,
			r.Attempt,
			r.LastAttemptAt.UTC().Format("2006-01-02 15:04:05"),
			truncateString(r.Error, 55),
		)
	}
	return nil
}

func truncateString(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// newUsage returns a Usage function bound to the supplied FlagSet that
// prints the canonical help text. Extracted so help also fires when the
// user invokes the subcommand with no flags at all.
func newUsage(fs *flag.FlagSet) func() {
	return func() {
		out := fs.Output()
		fmt.Fprintln(out, "omnicore-admin list-failures — list pending rows of the unified projection failure ledger")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Usage:")
		fmt.Fprintln(out, "  omnicore-admin list-failures [flags]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Configuration is read from microservice.${APP_PROFILE}.yaml (override via OMNICORE_CONFIG_PATH).")
		fmt.Fprintln(out, "Database DSN comes from there — no flag duplicates it.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Read-only — replay is automatic: the service's parked-retry loop (mongo.parkedRetry)")
		fmt.Fprintln(out, "sweeps both kinds on its cadence.")
	}
}
