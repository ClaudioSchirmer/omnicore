// Package replay implements the omnicore-admin "replay-all-as-events"
// subcommand. It reads every active row from a configured aggregate table
// in A's Postgres and inserts a synthetic INSERTED event into A's outbox
// for each one, which Debezium then forwards as a normal Kafka message.
// Every consumer subscribed to A's topic — existing replicas of B already
// running AND any new replica that subsequently joins — receives the
// replay as if it were a real INSERT.
//
// The replay runs inside A's process so the framework reuses A's existing
// config path (microservice.<profile>.yaml via APP_PROFILE) for Postgres
// DSN, outbox table identity, and serialization conventions. The CLI
// accepts NO flag for those — single source of truth with A's runtime.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/bootstrap"
	"github.com/ClaudioSchirmer/omnicore/infra"
)

// Run is the subcommand entry point invoked by cmd/omnicore-admin/main.go.
// Returns nil on success; a non-nil error is printed to stderr and the
// process exits with status 1.
func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("replay-all-as-events", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usage := newUsage(fs)
	fs.Usage = usage

	aggregate := fs.String("aggregate", "",
		"Aggregate name to replay (required). Matches both the PG table name and the outbox aggregate_type. Example: users")
	filter := fs.String("filter", "",
		"Optional SQL WHERE expression appended to the SELECT (without the WHERE keyword). Example: \"active = true AND tenant_id = 'acme'\"")
	includeArchived := fs.Bool("include-archived", false,
		"Include rows where deleted_at IS NOT NULL (default: skip archived rows)")
	batchSize := fs.Int("batch-size", 1000,
		"Maximum number of rows fetched per page from Postgres (insert is still one row per outbox INSERT)")
	dryRun := fs.Bool("dry-run", false,
		"Count the matching rows and print the summary without writing any outbox row")

	if err := fs.Parse(args); err != nil {
		// flag already printed its diagnostic
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *aggregate == "" {
		usage()
		return fmt.Errorf("replay-all-as-events: --aggregate is required")
	}
	if *batchSize < 1 {
		return fmt.Errorf("replay-all-as-events: --batch-size must be >= 1, got %d", *batchSize)
	}

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		return fmt.Errorf("replay-all-as-events: load config: %w", err)
	}
	pg, err := infra.NewPostgres(ctx, cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("replay-all-as-events: postgres connect: %w", err)
	}
	defer pg.Close()

	return execute(ctx, pg, executeOptions{
		Aggregate:       *aggregate,
		Filter:          *filter,
		IncludeArchived: *includeArchived,
		BatchSize:       *batchSize,
		DryRun:          *dryRun,
		Out:             os.Stdout,
	})
}

type executeOptions struct {
	Aggregate       string
	Filter          string
	IncludeArchived bool
	BatchSize       int
	DryRun          bool
	Out             io.Writer
}

// execute performs the replay against the supplied Postgres handle.
// Extracted from Run so tests can drive it without touching env / config.
func execute(ctx context.Context, pg *infra.Postgres, opt executeOptions) error {
	if !infra.SafeIdentifier(opt.Aggregate) {
		return fmt.Errorf("replay-all-as-events: aggregate %q is not a valid SQL identifier", opt.Aggregate)
	}
	where := buildWhere(opt.IncludeArchived, opt.Filter)
	totalQuery := fmt.Sprintf("SELECT count(*) FROM %s%s", quoteIdent(opt.Aggregate), where)
	var total int64
	if err := pg.Pool().QueryRow(ctx, totalQuery).Scan(&total); err != nil {
		return fmt.Errorf("replay-all-as-events: count rows: %w", err)
	}
	if total == 0 {
		fmt.Fprintf(opt.Out, "replay-all-as-events: no rows matched filter — nothing to do\n")
		return nil
	}
	fmt.Fprintf(opt.Out, "replay-all-as-events: %d row(s) matched on aggregate=%s, includeArchived=%v\n",
		total, opt.Aggregate, opt.IncludeArchived)
	if opt.DryRun {
		fmt.Fprintf(opt.Out, "replay-all-as-events: dry-run — no outbox rows written\n")
		return nil
	}

	selectQuery := fmt.Sprintf("SELECT * FROM %s%s ORDER BY id LIMIT $1 OFFSET $2",
		quoteIdent(opt.Aggregate), where)
	var written int64
	for offset := int64(0); offset < total; offset += int64(opt.BatchSize) {
		rows, err := pg.Pool().Query(ctx, selectQuery, opt.BatchSize, offset)
		if err != nil {
			return fmt.Errorf("replay-all-as-events: query batch at offset %d: %w", offset, err)
		}
		batch, batchErr := scanRowsToMaps(rows)
		if batchErr != nil {
			return fmt.Errorf("replay-all-as-events: decode batch at offset %d: %w", offset, batchErr)
		}
		for _, row := range batch {
			id, ok := stringField(row, "id")
			if !ok || id == "" {
				return fmt.Errorf("replay-all-as-events: row at offset %d missing id column", offset)
			}
			payload, err := json.Marshal(row)
			if err != nil {
				return fmt.Errorf("replay-all-as-events: marshal payload for id=%s: %w", id, err)
			}
			if err := insertOutboxRow(ctx, pg, opt.Aggregate, id, payload); err != nil {
				return fmt.Errorf("replay-all-as-events: insert outbox for id=%s: %w", id, err)
			}
			written++
		}
		fmt.Fprintf(opt.Out, "replay-all-as-events: wrote %d / %d outbox rows\n", written, total)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	fmt.Fprintf(opt.Out, "replay-all-as-events: done — %d outbox row(s) written for aggregate=%s\n",
		written, opt.Aggregate)
	return nil
}

// buildWhere assembles the WHERE clause from the includeArchived flag +
// the optional filter expression. Returns a string starting with " WHERE"
// or empty when no clause applies.
func buildWhere(includeArchived bool, filter string) string {
	var clauses []string
	if !includeArchived {
		clauses = append(clauses, "deleted_at IS NULL")
	}
	if strings.TrimSpace(filter) != "" {
		clauses = append(clauses, "("+filter+")")
	}
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(clauses, " AND ")
}

// insertOutboxRow writes one synthetic INSERTED event into the framework
// outbox table. Mirrors the shape framework migration 0001 declares.
func insertOutboxRow(ctx context.Context, pg *infra.Postgres, aggregate, id string, payload []byte) error {
	const sqlStr = `
		INSERT INTO outbox (id, aggregate_type, aggregate_id, event_type, payload, created_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, $5)
	`
	_, err := pg.Pool().Exec(ctx, sqlStr, aggregate, id, "INSERTED", payload, time.Now().UTC())
	return err
}

// quoteIdent wraps a SQL identifier in double quotes. infra.SafeIdentifier
// has already validated the input via the framework's allowlist so the
// quote is purely cosmetic (catches accidental keyword collisions in the
// generated SQL).
func quoteIdent(s string) string { return `"` + s + `"` }

// scanRowsToMaps walks pgx.Rows and produces a slice of column→value maps
// suitable for JSON marshaling. UUID columns are normalized to strings the
// same way infra.normalizeSQLValue does.
func scanRowsToMaps(rows pgx.Rows) ([]map[string]any, error) {
	defer rows.Close()
	descs := rows.FieldDescriptions()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		m := make(map[string]any, len(descs))
		for i, d := range descs {
			m[d.Name] = infra.NormalizeSQLValue(vals[i])
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// stringField extracts a string-typed column from a row map. Handles the
// canonical UUID/string cases and falls through to fmt.Sprint for any
// other shape (defense for non-canonical schemas).
func stringField(row map[string]any, key string) (string, bool) {
	v, ok := row[key]
	if !ok || v == nil {
		return "", false
	}
	switch s := v.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return strconv.Quote(fmt.Sprint(v)), true
	}
}

// newUsage returns a Usage function bound to the supplied FlagSet that
// prints the canonical help text. Extracted so help also fires when the
// user invokes the subcommand with no flags at all.
func newUsage(fs *flag.FlagSet) func() {
	return func() {
		out := fs.Output()
		fmt.Fprintln(out, "omnicore-admin replay-all-as-events — bootstrap a new B against an existing A")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Usage:")
		fmt.Fprintln(out, "  omnicore-admin replay-all-as-events --aggregate <name> [flags]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Configuration is read from microservice.${APP_PROFILE}.yaml (override via OMNICORE_CONFIG_PATH).")
		fmt.Fprintln(out, "Postgres DSN, outbox schema, and serialization come from there — no flags duplicate them.")
	}
}
