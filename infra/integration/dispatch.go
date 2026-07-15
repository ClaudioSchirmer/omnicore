package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/tracing"

	"github.com/google/uuid"
)

// DispatchOption is the functional-option type the producer Dispatch
// surface accepts. Each option carries a runtime-only value the
// framework cannot pre-declare in YAML: a TxHandle in-flight, an
// aggregate ID that changes per emission, request-scoped trace UUIDs.
// Everything declarative (event_type, aggregate type, schema version)
// lives in YAML and resolves via the eventKey lookup — same posture
// httpclient's CallConfig draws.
type DispatchOption func(*dispatchOpts)

type dispatchOpts struct {
	tx             persistence.TxHandle
	aggregateID    domain.ID
	hasAggregate   bool
	correlation    uuid.UUID
	hasCorrelation bool
	causation      uuid.UUID
	hasCausation   bool
}

// WithTx threads the in-flight transaction handle into the Dispatch call so the
// integration_events row lands in the same TX as the data write +
// outbox + audit. Canonical usage: from inside a BeforeCommit hook
// closure where the framework already opened the TX for the entity
// write. Omitting WithTx makes Dispatch run standalone — the row
// commits via the relational engine's single-statement autocommit on the
// package's engine pool, independent of any other write.
//
// TxHandle is a sealed marker (see application/persistence/tx.go); the
// framework infra layer is the only code path that unwraps it back to a
// live core.Tx (the canonical backend-neutral seam; pgx.Tx only via the PG-only
// escape hatch). Application code cannot pronounce SQL through the handle.
func WithTx(tx persistence.TxHandle) DispatchOption {
	return func(o *dispatchOpts) { o.tx = tx }
}

// WithAggregateID stamps the aggregate identity on the row. Required
// when the YAML entry declares an `aggregate:` field; rejected when the
// YAML entry omits aggregate (standalone events are aggregate-agnostic
// by definition). Mismatch surfaces as ErrIntegrationAggregateIDRequired
// before any database write runs.
func WithAggregateID(id domain.ID) DispatchOption {
	return func(o *dispatchOpts) {
		o.aggregateID = id
		o.hasAggregate = true
	}
}

// WithCorrelation overrides the framework-resolved correlation_id for
// the row. Default resolution: ctx.CorrelationID() when populated by the
// receiver path; NULL otherwise. Use the option when correlation comes
// through a non-framework carrier (HTTP header, vendor envelope) the
// receiver pipeline did not observe.
func WithCorrelation(id uuid.UUID) DispatchOption {
	return func(o *dispatchOpts) {
		o.correlation = id
		o.hasCorrelation = true
	}
}

// WithCausation overrides the framework-resolved causation_id. Default
// resolution: ctx.CausationID() when populated by the receiver path
// (the inbound event_id); NULL otherwise. Override is rare — useful
// when one cross-service flow spans multiple distinct inbound triggers
// and the dev wants to thread an upstream identifier explicitly.
func WithCausation(id uuid.UUID) DispatchOption {
	return func(o *dispatchOpts) {
		o.causation = id
		o.hasCausation = true
	}
}

// Dispatch is the canonical producer entry point — services emit
// integration events by composing this call. The framework:
//
//  1. Resolves eventKey against the loaded YAML's publishes block.
//     Unknown key → ErrIntegrationEventNotConfigured.
//  2. Validates the aggregate slot: YAML declared aggregate but caller
//     omitted WithAggregateID → ErrIntegrationAggregateIDRequired.
//  3. Marshals payload as JSON. Any encoding error surfaces verbatim.
//  4. Inserts one row into `integration_events`. With WithTx(tx) →
//     atomic with the framework's TX; without → standalone autocommit
//     on the package's relational-engine pool.
//  5. Emits a single slog.Info("integration.event.emitted", ...) line
//     post-INSERT for observability (operator log-tail consumers).
//
// The framework auto-fills event_id, thread_id, actor, created_at and
// resolves event_type/aggregate_type/event_version from YAML.
// correlation_id and causation_id default to ctx-derived values when the
// caller did not override via With*.
//
// Returns nil on success; any non-nil return aborts the caller's TX
// (when invoked from BeforeCommit) — the persister's defer rollback
// reverts every framework-issued write of the same call.
func Dispatch(
	ctx *configuration.AppContext,
	eventKey string,
	payload any,
	opts ...DispatchOption,
) error {
	if ctx == nil {
		return fmt.Errorf("integration.Dispatch: ctx is required")
	}
	c := snapshot()
	if c == nil || c.cfg == nil {
		return ErrIntegrationConfigNotInitialized
	}

	entry, ok := c.cfg.LookupPublish(eventKey)
	if !ok {
		return fmt.Errorf("%w: eventKey=%q", ErrIntegrationEventNotConfigured, eventKey)
	}

	o := &dispatchOpts{}
	for _, opt := range opts {
		opt(o)
	}

	if entry.Aggregate != "" && !o.hasAggregate {
		return fmt.Errorf("%w: eventKey=%q aggregate=%q", ErrIntegrationAggregateIDRequired, eventKey, entry.Aggregate)
	}

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("integration.Dispatch: marshal payload for eventKey=%q: %w", eventKey, err)
	}

	row := dispatchRow{
		EventID:      uuid.New(),
		EventType:    entry.EventType,
		Aggregate:    entry.Aggregate,
		AggregateID:  o.aggregateID,
		HasAggregate: o.hasAggregate,
		Version:      resolveVersion(entry.Version),
		Payload:      rawPayload,
		ThreadID:     ctx.ID(),
		Actor:        resolveActor(ctx),
		Correlation:  resolveCorrelation(ctx, o),
		Causation:    resolveCausation(ctx, o),
	}

	if err := writeIntegrationEvent(ctx, c, o.tx, row); err != nil {
		return fmt.Errorf("integration.Dispatch: write eventKey=%q: %w", eventKey, err)
	}

	emitDispatchEcho(c.logger, eventKey, entry, row)
	return nil
}

// resolveVersion enforces the framework default 1 when YAML left version
// unset (zero-value). Avoids stamping NULL on event_version (NOT NULL).
func resolveVersion(declared int) int {
	if declared <= 0 {
		return 1
	}
	return declared
}

// resolveActor reads the actor subject straight from ctx. ActorSubject()
// is contractually non-empty (real JWT `sub` or the sentinel
// `"anonymous"`), so the row never lands with an empty actor — matches
// the NOT NULL constraint at the schema level.
func resolveActor(ctx *configuration.AppContext) string {
	if a := ctx.ActorSubject(); a != "" {
		return a
	}
	return persistence.AnonymousActor
}

// resolveCorrelation picks WithCorrelation override first, then falls
// back to ctx.CorrelationID() — the slot the integration receiver
// pipeline populates from inbound events. Returns uuid.Nil when neither
// surfaces a value; writeIntegrationEvent maps uuid.Nil to NULL on the
// row.
func resolveCorrelation(ctx *configuration.AppContext, o *dispatchOpts) uuid.UUID {
	if o.hasCorrelation {
		return o.correlation
	}
	return ctx.CorrelationID()
}

// resolveCausation mirrors resolveCorrelation.
func resolveCausation(ctx *configuration.AppContext, o *dispatchOpts) uuid.UUID {
	if o.hasCausation {
		return o.causation
	}
	return ctx.CausationID()
}

// dispatchRow captures every column the INSERT writes — kept distinct
// from DispatchOption so the SQL layer never reaches into the option
// struct's internal shape.
type dispatchRow struct {
	EventID      uuid.UUID
	EventType    string
	Aggregate    string
	AggregateID  domain.ID
	HasAggregate bool
	Version      int
	Payload      []byte
	ThreadID     uuid.UUID
	Actor        string
	Correlation  uuid.UUID
	Causation    uuid.UUID
}

// integrationEventCols is the column list for the integration_events INSERT,
// fixed across dialects; only the placeholder grammar (rendered via the engine's
// Dialect) and the in-TX vs standalone execution differ.
var integrationEventCols = []string{
	"id", "event_id", "aggregate_type", "aggregate_id", "event_type", "event_version",
	"payload", "correlation_id", "causation_id", "thread_id", "actor", "traceparent",
}

// insertIntegrationEventSQL renders the INSERT with the dialect's positional
// placeholders. The PK follows the framework id standard (UUID v7 minted in
// Go, bound via Dialect.EncodeArg into the native id form); the wire-crossing
// uuid references (event_id, aggregate_id, correlation/causation/thread) are
// CHAR(36) on every dialect and bind as canonical uuid TEXT.
func insertIntegrationEventSQL(d core.Dialect) string {
	ph := make([]string, len(integrationEventCols))
	for i := range integrationEventCols {
		ph[i] = d.Placeholder(i + 1)
	}
	return "INSERT INTO integration_events (" + strings.Join(integrationEventCols, ", ") +
		") VALUES (" + strings.Join(ph, ", ") + ")"
}

// writeIntegrationEvent dispatches between the in-TX path (WithTx supplied → the
// row lands in the framework's open transaction via the canonical UnwrapTx
// bridge) and the standalone path (no WithTx → single-statement autocommit on the
// engine's pool). Both render the dialect's placeholders and run through the
// neutral seam, so the producer works on any backend.
func writeIntegrationEvent(
	ctx context.Context,
	c *client,
	tx persistence.TxHandle,
	row dispatchRow,
) error {
	rowID, err := newControlPlaneID()
	if err != nil {
		return fmt.Errorf("integration.Dispatch: %w", err)
	}

	// Resolve the dialect first: the Go-minted PK binds through EncodeArg into
	// the dialect's native id form, so the args depend on it.
	var (
		d    core.Dialect
		exec func(context.Context, string, ...any) error
	)
	if tx != nil {
		ntx := core.UnwrapTx(tx)
		d = ntx.Dialect()
		exec = ntx.Exec
	} else {
		if c.eng == nil {
			return fmt.Errorf("standalone Dispatch requires a relational engine; integration.Configure received nil")
		}
		d = c.eng.Dialect()
		exec = c.eng.Querier().Exec
	}

	args := []any{
		d.EncodeArg(rowID),
		row.EventID.String(),
		nullableString(row.Aggregate),
		nullableUUID(maybeAggregateUUID(row)),
		row.EventType,
		row.Version,
		// Text bind — the payload column is text-shaped JSON on every dialect;
		// SQL Server refuses the implicit varbinary→NVARCHAR conversion a raw
		// []byte would require.
		string(row.Payload),
		nullableUUID(row.Correlation),
		nullableUUID(row.Causation),
		row.ThreadID.String(),
		row.Actor,
		// W3C traceparent of the producing request so the Receiver can link the
		// consumed event back to this trace; NULL when tracing is off.
		nullableString(tracing.TraceparentFromContext(ctx)),
	}
	return exec(ctx, insertIntegrationEventSQL(d), args...)
}

// maybeAggregateUUID returns the aggregate id when HasAggregate, else
// uuid.Nil so nullableUUID emits NULL. Defensive: an aggregate-bound
// event MUST carry an id (the validation above rejects the missing
// case), but a standalone event lands here with HasAggregate=false and
// the slot stays NULL on the row.
func maybeAggregateUUID(row dispatchRow) uuid.UUID {
	if !row.HasAggregate {
		return uuid.Nil
	}
	parsed, err := uuid.Parse(row.AggregateID.String())
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

// nullableString returns *string so the database driver emits NULL on empty
// — needed because aggregate_type is the slot that distinguishes
// aggregate-bound events from standalone ones.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableUUID returns nil for uuid.Nil (→ NULL column) and the canonical TEXT
// form otherwise. Bound as text so the same value lands in a Postgres uuid column
// and a MySQL CHAR(36) column. correlation_id, causation_id, aggregate_id share it.
func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u.String()
}

// emitDispatchEcho is the producer-side observability line. Best-effort
// (slog never fails). Emitted post-INSERT so the line accurately
// reflects "this event landed in the database" — when the call is in-TX, this
// runs before COMMIT, but the row is still subject to rollback if the
// outer hook closure errors afterward; the slog echo is a hint, the database
// row is authoritative.
func emitDispatchEcho(logger *slog.Logger, eventKey string, entry PublishEvent, row dispatchRow) {
	if logger == nil {
		return
	}
	attrs := []any{
		"event_key", eventKey,
		"event_type", entry.EventType,
		"event_id", row.EventID.String(),
		"thread_id", row.ThreadID.String(),
		"actor", row.Actor,
		"version", row.Version,
	}
	if entry.Aggregate != "" {
		attrs = append(attrs, "aggregate_type", entry.Aggregate, "aggregate_id", row.AggregateID.String())
	}
	logger.Info("integration.event.emitted", attrs...)
}
