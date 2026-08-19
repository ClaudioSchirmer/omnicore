// Package replay implements the omnicore-admin "replay-all-as-events"
// subcommand. It reads every active row from a configured aggregate table
// in A's configured relational database and inserts a synthetic INSERTED event into A's outbox
// for each one, which Debezium then forwards as a normal Kafka message.
// Every consumer subscribed to A's topic — existing replicas of B already
// running AND any new replica that subsequently joins — receives the
// replay as if it were a real INSERT.
//
// The replay runs inside A's process so the framework reuses A's existing
// config path (microservice.<profile>.yaml via APP_PROFILE) for the relational
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

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/bootstrap"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
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
		"Aggregate name to replay (required). Matches both the table name and the outbox aggregate_type. Example: users")
	filter := fs.String("filter", "",
		"Optional SQL WHERE expression appended to the SELECT (without the WHERE keyword). Example: \"active = true AND tenant_id = 'acme'\"")
	includeArchived := fs.Bool("include-archived", false,
		"Include rows where deleted_at IS NOT NULL (default: skip archived rows)")
	batchSize := fs.Int("batch-size", 1000,
		"Maximum number of rows fetched per page from the configured database (insert is still one row per outbox INSERT)")
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
	// Build through the relational-engine registry — the same seam bootstrap uses —
	// so the replay runs against whatever backend the service is configured for
	// (relational.dialect). A MySQL deployment needs the admin binary built with the
	// engine's build tag (-tags mysql); NewEngine returns a clear error otherwise.
	engine, err := core.NewEngine(cfg.Relational.Dialect, ctx, core.EngineConfig{DSN: cfg.Relational.DSN})
	if err != nil {
		return fmt.Errorf("replay-all-as-events: connect: %w", err)
	}
	defer engine.Close()

	return execute(ctx, engine.Querier(), engine.Dialect(), executeOptions{
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

// execute performs the replay against the supplied backend-neutral read surface
// (Querier + Dialect), so it runs identically on Postgres and MySQL. Extracted
// from Run, and narrowed to the two seam interfaces it actually uses, so tests
// can drive it with a fake without touching env / config.
func execute(ctx context.Context, q core.Querier, dialect core.Dialect, opt executeOptions) error {
	if !core.SafeIdentifier(opt.Aggregate) {
		return fmt.Errorf("replay-all-as-events: aggregate %q is not a valid SQL identifier", opt.Aggregate)
	}
	table := dialect.QuoteIdent(opt.Aggregate)
	pkCol := dialect.QuoteIdent("id")
	where := buildWhere(opt.IncludeArchived, opt.Filter)

	totalQuery := fmt.Sprintf("SELECT count(*) FROM %s%s", table, where)
	var total int64
	if err := q.QueryRow(ctx, totalQuery).Scan(&total); err != nil {
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

	// Keyset pagination over the ID — portable through the existing seam
	// (Dialect.ApplyLimit renders the row cap in each engine's native
	// position), where a LIMIT/OFFSET tail would be a PG/MySQL-ism (T-SQL
	// pages via OFFSET…FETCH with a different arg order). Keyset also never
	// re-scans skipped rows, so large replays stay O(rows).
	var written int64
	lastID := ""
	for {
		pageWhere := where
		var args []any
		if lastID != "" {
			cond := fmt.Sprintf("%s > %s", pkCol, dialect.Placeholder(1))
			if pageWhere == "" {
				pageWhere = " WHERE " + cond
			} else {
				pageWhere += " AND " + cond
			}
			args = append(args, dialect.EncodeArg(domain.NewID(lastID)))
		}
		selectQuery := dialect.ApplyLimit(
			fmt.Sprintf("SELECT * FROM %s%s ORDER BY %s", table, pageWhere, pkCol), opt.BatchSize)
		// QueryMaps is the dynamic-shape read the composer uses: it discovers the
		// column set at read time and normalizes uuid columns to canonical strings
		// on every backend (BINARY(16) → text, [16]byte → text), so the
		// payload + id below are dialect-agnostic.
		batch, err := q.QueryMaps(ctx, selectQuery, args...)
		if err != nil {
			return fmt.Errorf("replay-all-as-events: query batch after id=%q: %w", lastID, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			id, ok := stringField(row, "id")
			if !ok || id == "" {
				return fmt.Errorf("replay-all-as-events: row after id=%q missing id column", lastID)
			}
			payload, err := json.Marshal(row)
			if err != nil {
				return fmt.Errorf("replay-all-as-events: marshal payload for id=%s: %w", id, err)
			}
			if err := insertOutboxRow(ctx, q, dialect, opt.Aggregate, id, payload); err != nil {
				return fmt.Errorf("replay-all-as-events: insert outbox for id=%s: %w", id, err)
			}
			lastID = id
			written++
		}
		fmt.Fprintf(opt.Out, "replay-all-as-events: wrote %d / %d outbox rows\n", written, total)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(batch) < opt.BatchSize {
			break
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

// insertOutboxRow writes one synthetic INSERTED event into the framework outbox
// table through the neutral Querier. The surrogate id follows the framework id
// standard — a UUID v7 minted in Go, bound via Dialect.EncodeArg into the
// dialect's native id form (uuid text on PG, BINARY(16) elsewhere)
// and the payload binds as JSON with no PG-specific ::jsonb cast; aggregate_id is
// the row's uuid in text form, accepted by both the PG UUID and MySQL CHAR(36)
// columns. The column list mirrors each engine's own writeOutbox.
func insertOutboxRow(ctx context.Context, q core.Querier, dialect core.Dialect, aggregate, id string, payload []byte) error {
	rowID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("replay: uuid v7: %w", err)
	}
	sqlStr := fmt.Sprintf(
		"INSERT INTO outbox (id, aggregate_type, event_type, aggregate_id, payload, created_at) VALUES (%s, %s, %s, %s, %s, %s)",
		dialect.Placeholder(1), dialect.Placeholder(2), dialect.Placeholder(3), dialect.Placeholder(4), dialect.Placeholder(5), dialect.NowExpr(),
	)
	// Text bind — the payload column is text-shaped JSON on every dialect;
	// SQL Server refuses the implicit varbinary→NVARCHAR conversion a raw
	// []byte would require.
	return core.Exec(q, ctx, sqlStr, dialect.EncodeArg(domain.NewID(rowID.String())), aggregate, "INSERTED", id, string(payload))
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
