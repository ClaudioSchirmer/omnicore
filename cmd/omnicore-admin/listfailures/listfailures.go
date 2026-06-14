// Package listfailures implements the omnicore-admin
// "upstream-list-failures" subcommand. It reads pending rows from
// omnicore_upstream_failures (populated by UpstreamSubscriber.ripple)
// and prints them as text or JSON for operator triage.
//
// Read-only — the framework binary has no Wiring of the consumer
// service B, so it cannot re-run ripple itself. The actual retry path
// lives on UpstreamSubscriber.RetryPendingFailures, which B exposes
// via cron/HTTP at its own discretion. This CLI is the inspection
// surface; the runtime API is the action surface.
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
	"github.com/ClaudioSchirmer/omnicore/infra"
)

const (
	formatText = "text"
	formatJSON = "json"
)

// Run is the subcommand entry point invoked by cmd/omnicore-admin/main.go.
// Returns nil on success; a non-nil error is printed to stderr and the
// process exits with status 1.
func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upstream-list-failures", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := newUsage(fs)
	fs.Usage = usage

	topic := fs.String("topic", "",
		"Filter by subscription_topic (default: every topic). Example: users.events")
	view := fs.String("view", "",
		"Filter by view_name (applied after the SQL query). Example: orders")
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
		return fmt.Errorf("upstream-list-failures: --format must be text or json, got %q", *format)
	}
	if *limit < 0 {
		return fmt.Errorf("upstream-list-failures: --limit must be >= 0, got %d", *limit)
	}

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		return fmt.Errorf("upstream-list-failures: load config: %w", err)
	}
	pg, err := infra.NewPostgres(ctx, cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("upstream-list-failures: postgres connect: %w", err)
	}
	defer pg.Close()

	return execute(ctx, pg, executeOptions{
		Topic:  *topic,
		View:   *view,
		Format: *format,
		Limit:  *limit,
		Out:    os.Stdout,
	})
}

type executeOptions struct {
	Topic  string
	View   string
	Format string
	Limit  int
	Out    io.Writer
}

// execute does the read + render. Extracted from Run so tests can drive
// it without touching env / config.
func execute(ctx context.Context, pg *infra.Postgres, opt executeOptions) error {
	var rows []infra.UpstreamFailureRecord
	var err error
	if opt.Topic != "" {
		rows, err = infra.ListPendingUpstreamFailuresByTopic(ctx, pg.Pool(), opt.Topic)
	} else {
		rows, err = infra.ListPendingUpstreamFailures(ctx, pg.Pool())
	}
	if err != nil {
		return fmt.Errorf("upstream-list-failures: list: %w", err)
	}
	if opt.View != "" {
		filtered := rows[:0]
		for _, r := range rows {
			if r.ViewName == opt.View {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
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
func renderJSON(out io.Writer, rows []infra.UpstreamFailureRecord, truncated bool) error {
	envelope := struct {
		Count     int                            `json:"count"`
		Truncated bool                           `json:"truncated"`
		Items     []infra.UpstreamFailureRecord  `json:"items"`
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
func renderText(out io.Writer, rows []infra.UpstreamFailureRecord, truncated bool) error {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no pending upstream failures")
		return nil
	}
	fmt.Fprintf(out, "%d pending upstream failure(s)", len(rows))
	if truncated {
		fmt.Fprint(out, " (truncated — pass --limit 0 for the full list)")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, strings.Repeat("-", 110))
	fmt.Fprintf(out, "%-25s %-20s %-36s %-10s %-3s %-19s %s\n",
		"TOPIC", "VIEW", "UPSTREAM_ID", "STAGE", "AT#", "LAST_ATTEMPT_AT", "ERROR")
	fmt.Fprintln(out, strings.Repeat("-", 110))
	for _, r := range rows {
		fmt.Fprintf(out, "%-25s %-20s %-36s %-10s %-3d %-19s %s\n",
			truncateString(r.SubscriptionTopic, 25),
			truncateString(r.ViewName, 20),
			truncateString(r.UpstreamID, 36),
			r.Stage,
			r.Attempt,
			r.LastAttemptAt.UTC().Format("2006-01-02 15:04:05"),
			truncateString(r.Error, 60),
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
		fmt.Fprintln(out, "omnicore-admin upstream-list-failures — list pending upstream recompose failures")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Usage:")
		fmt.Fprintln(out, "  omnicore-admin upstream-list-failures [flags]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Configuration is read from microservice.${APP_PROFILE}.yaml (override via OMNICORE_CONFIG_PATH).")
		fmt.Fprintln(out, "Postgres DSN comes from there — no flag duplicates it.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Read-only — this CLI does not re-run recompose. For actual retry use")
		fmt.Fprintln(out, "UpstreamSubscriber.RetryPendingFailures from the service binary (cron or HTTP endpoint).")
	}
}
