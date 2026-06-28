# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While in `0.x.y`, the public API may change between minor versions; breaking
changes are highlighted under **Changed**. Stable contract semantics arrive
with `1.0.0`.

## [Unreleased]

### Added

- **Inbound request deadline.** New `http.requestTimeoutSeconds` config knob
  bounds how long a single inbound request may run before the framework cancels
  its context. The `AppContextMiddleware` derives the request's cancellation
  parent from `context.WithTimeout`, so the deadline propagates through the
  `AppContext` (which is the `context.Context` every handler receives) to `pgx`,
  `mongo` and outbound `httpclient` — a slow request releases its pool
  connection and goroutine the moment the deadline fires, instead of holding
  them indefinitely (the inbound counterpart to the outbound circuit breaker;
  what an edge/gateway timeout cannot do, since it cannot cancel work already
  running in-process). The cancellation also caps every outbound `httpclient`
  call at the request's remaining budget, for free (no httpclient change). The
  default is **30s**; an explicit `0` disables the deadline (the pre-deadline
  behavior, a request may run unbounded). A blown deadline surfaces as
  **504 Gateway Timeout** via the new `SemanticGatewayTimeout` and
  `RequestTimeoutNotification`, mapped in `pipeline.Run` from
  `context.DeadlineExceeded` so a timeout never masquerades as a 500. New
  public surface: `domain.SemanticGatewayTimeout`,
  `notifications.RequestTimeoutNotification`,
  `fwweb.WithRequestTimeout(time.Duration)`,
  `(*configuration.AppContext).SetParentIfAbsent`, and
  `bootstrap.FrameworkDefaultRequestTimeoutSeconds`. The deadline reaches the
  write-command pre-load too: the Update / Archive / Delete / Unarchive Auto
  handlers load the target aggregate under the request ctx via the optional
  `persistence.ScopedReaderProvider[T]` / `ScopedArchivedReaderProvider[T]`
  capabilities (helpers `persistence.LoadForWrite` / `LoadArchivedForWrite`),
  instead of the ctx-less `domain.Reader[T].FindByID` /
  `domain.ArchivedFinder[T].FindArchivedByID` that would run the load `SELECT` on
  `context.Background()`. `infra.BaseAggregateRepository[T]` implements both, so
  the canonical aggregate path is covered with no consumer code; a hand-rolled
  repository that implements neither degrades to the ctx-less load. The domain
  ports keep their pure ctx-less signatures — the ctx binds in application/infra,
  mirroring how `Scope(ctx)` binds writes (added public surface:
  `persistence.ScopedReaderProvider[T]`, `ScopedArchivedReaderProvider[T]`,
  `LoadForWrite`, `LoadArchivedForWrite`,
  `(*infra.BaseAggregateRepository[T]).ScopedReader` / `.ScopedArchivedReader`).
  So view/query reads, outbound httpclient and the full write path (mutation +
  pre-load) are covered; a direct call to the ctx-less
  `domain.Reader[T].FindByID` outside the Auto write handlers stays uncovered by
  design — the domain port takes no context.

- **Pluggable relational backend — MySQL alongside PostgreSQL.** The relational
  layer is now backend-agnostic: a `db.RelationalEngine` port decouples the write
  binding, read path, and composition root from the concrete driver, with the
  backend selected once at boot via the new `database.dialect` knob (default
  `postgres`, so every existing config stays valid). Engines self-register
  database/sql-style (`db.RegisterEngine` / `db.NewEngine`); `Deps.DB` and
  `BaseRepository.Engine` are the neutral handles, and `postgres.AsPostgres(engine)`
  recovers the concrete adapter for the few PG-bound escapes (pool, partitions).
  A complete MySQL engine ships behind the `mysql` build tag
  (`infra/db/engine/mysql`): selecting `database.dialect: mysql` +
  `go build -tags mysql` runs a service at feature parity with Postgres — flat and
  aggregate writes (root + children + outbox in one TX, symmetric
  archive/unarchive/delete cascade), `FindByID` / criteria reads, audit rows +
  domain-event publishing, the integration producer and consumer (dedup + failure
  registries), the composer + SyncEngine Mongo projection, operator-triggered
  Mongo-view rebuild + drift reconciliation, the migration runner, per-statement
  OpenTelemetry tracing, and the `omnicore-admin` tooling; a Postgres-only build
  never compiles the package nor links the MySQL driver. Several internal surfaces
  became backend-neutral to make this possible: a read seam
  (`db.Querier`/`Rows`/`Row` + `db.Dialect`, with `QueryMaps` for the composer's
  dynamic shape), the in-TX bridge (`db.UnwrapTx(TxHandle) db.Tx`, the neutral
  counterpart to the PG-only `UnwrapPgxTx`), a generic audit reader
  (`db.NewAuditReader`), and the rebuild mutex
  (`RelationalEngine.AcquireRebuildLock` — `pg_advisory_lock` on Postgres,
  `GET_LOCK` on MySQL); the `Dialect` renders placeholders (`$n`/`?`), identifier
  quoting, the case-insensitive LIKE clause, the upsert statement, and the UUID
  value codec per backend. MySQL specifics: primary keys are UUID v7 generated in
  Go and stored `BINARY(16)` (time-ordered for InnoDB locality); secondary UUID
  columns and raw-string id criteria round-trip through `BINARY(16)`, boolean
  fields keep type fidelity into Mongo, and case-insensitive criteria are
  collation-independent (`LOWER(col) LIKE LOWER(?)`); the DSN is normalized at
  construction (`parseTime`, `clientFoundRows`; `multiStatements` scoped to the
  migration connection only). Verified throughout by a `-tags=integration,mysql`
  suite against a real MySQL container.

### Changed

- **breaking** — the relational layer's public surface is now backend-neutral,
  replacing pgx-typed parameters with the `db.RelationalEngine` / `db.Querier` /
  `db.Dialect` / `db.Row` / `db.Rows` seam. Migration points:
  - The concrete engine package moved `infra/db/engine/pg` →
    `infra/db/engine/postgres` (`package pg` → `package postgres`) — update the
    import path and qualifier for the PG-only escapes (`postgres.AsPostgres`,
    `postgres.UnwrapPgxTx`, `*postgres.Postgres`, `postgres.NewPostgres`); no
    symbols changed.
  - `Deps.Postgres *infra.Postgres` → `Deps.DB infra.RelationalEngine`, and
    `BaseRepository[T].Postgres` → `.Engine RelationalEngine` (recover the pool via
    `infra.AsPostgres(d.DB)`; rename the literal field).
  - `WithAudit` / `WithEventPublisher` moved onto the `RelationalEngine` interface
    and now return `RelationalEngine`.
  - The audit read free functions (`audit.FindByID` / `FindByAggregate`) and the
    `pgExec` interface are removed — build a reader with `db.NewAuditReader(deps.DB)`.
  - `RootScanner[T]` / `ChildScanner` receive `db.Row` / `db.Rows` (was `pgx.Row` /
    `pgx.Rows`); the body's `row.Scan(...)` is unchanged.
  - The Mongo-view rebuild/drift surface neutralized: `DetectViewDrift` takes
    `RelationalEngine`; the registry helpers (`ReadViewRegistry`, `InitViewRegistry`,
    `BeginRebuild`, `EndRebuild`, `ListNonDone`) take `(db.Querier, db.Dialect)`;
    the PG-only advisory-lock helpers (`ViewLockKey`, `TryAcquireViewLock`,
    `ReleaseViewLock`, `ReadViewLockHolder`) are removed (use `AcquireRebuildLock`).
  - The engine-taking constructors and consumer-plane helpers now take a
    `RelationalEngine` (a `*Postgres` still satisfies it, so the canonical
    `bootstrap.Run` path is unaffected): `NewAggregateLoader`,
    `NewBaseAggregateRepository`, `NewComposer{,WithMongo}`, `NewSyncEngine`,
    `integration.Configure`, `NewUpstreamSubscriber`, `integration.NewConsumerPool`,
    `Receiver.RetryPendingFailures`. The failure/dedup helpers
    (`RecordUpstreamFailure`, `ResolveUpstreamFailures`,
    `ListPendingUpstreamFailures{,ByTopic}`, `RecordIntegrationFailure`,
    `ResolveIntegrationFailures`, `ListPendingIntegrationFailures{,ByGroup}`,
    `IsAlreadyProcessed`, `MarkProcessed`) take `(Querier[, Dialect])`.

- **breaking** — the embedded framework migrations collapsed from three versioned
  files (`0001_outbox` / `0002_integration_events` / `0003_outbox_traceparent`)
  into one flattened `0001_framework` per dialect (`embedded/postgres/`,
  `embedded/mysql/`), every table + column carrying a `COMMENT` (the MySQL flavor
  uses dialect-appropriate types — `UUID`→`CHAR(36)`, `JSONB`→`JSON`). A database
  that already applied the old framework versions must be reset
  (`docker compose down -v`) — done pre-1.0 deliberately so Postgres and MySQL
  share one clean initial schema. Service migrations (`0002+`) are unaffected.

- **UPDATE of a missing row now reports 404, not 500, on every backend.** A write
  verb (Update / PATCH, root or aggregate child) whose `WHERE id = …` matches no
  row — e.g. the row was deleted between the write command's pre-load and the write
  (a TOCTOU race) — now surfaces the canonical `RecordNotFoundNotification` (404)
  instead of a raw driver error (previously Postgres leaked `pgx.ErrNoRows` → 500).
  On MySQL the DSN forces `clientFoundRows=true`, so an idempotent no-op PUT of an
  existing row is not mistaken for a missing one.

## [0.16.0] - 2026-06-25

### Added

- **Distributed tracing (OpenTelemetry).** New opt-in `observability.tracing`
  block wires OTel across the framework; default off installs the no-op tracer
  so a service that does not declare it pays essentially nothing. Knobs:
  `enabled`, `exporter` (`otlp`|`stdout` debug-only|`none`), `endpoint`,
  `insecure` (plaintext OTLP/gRPC; profile default dev→`true`, else→`false` for
  TLS so a managed collector is reachable), `headers` (added to every OTLP
  export — the slot for a managed collector's auth token), `sampler`
  (`always_on`|`always_off`|`traceratio`|`parentbased_traceratio`; profile
  default dev→`always_on`, else→`parentbased_traceratio`), `ratio`,
  `serviceName` (defaults to `service`), and a per-subsystem `instrument`
  allowlist (`http`, `pgx`, `mongo`, `kafka`, `httpclient`). The OTLP resource
  merges `resource.Default()`, so `OTEL_RESOURCE_ATTRIBUTES` and host/SDK
  attributes reach the collector. The synchronous
  path is traced end to end — inbound server span → the business
  `dispatch <Command/Query>` span (inherited identically by Auto, manual, REST
  and GraphQL since all funnel through `pipeline.Dispatch`) → pgx / mongo /
  outbound httpclient spans, with the W3C `traceparent` injected on outbound
  calls so the downstream service continues the same trace. The async path
  re-links across Debezium/Kafka via a new `traceparent` carried on the
  `outbox` and `integration_events` rows; the SyncEngine, integration Receiver
  and UpstreamSubscriber open consumer spans linked to the producing trace.
  `AppContext.CorrelationID()` is kept equal to the active `trace_id`, so logs,
  traces and `integration_events.correlation_id` all join on one value; when
  tracing is enabled, slog records emitted with a span-carrying context (the
  `http.outbound` line, pipeline failures/exceptions, and any code using the
  `*Context` slog methods) gain `traceId`/`spanId`, and the audit event carries
  a `trace_id` mirrored to BOTH destinations — the in-TX `audit_events.trace_id`
  column and the slog audit echo's `traceId` attribute. Export is asynchronous
  and batched (off the request path; a down collector never back-pressures a
  call).
- **Framework migration `0003`.** Adds `outbox.traceparent`,
  `integration_events.traceparent` (W3C trace context carried to the consumer
  for cross-process trace linking) and `audit_events.trace_id` (a pivot column
  to jump from a forensic row to its trace). All nullable; existing rows and
  writes made with tracing disabled store NULL. To map `outbox.traceparent` to a
  Kafka header, add `traceparent:header:traceparent` to the Debezium Outbox
  Event Router's `table.fields.additional.placement`.

## [0.15.0] - 2026-06-25

### Added

- **httpclient: runtime service registration.** `*HttpClient` gains
  `RegisterIfAbsent(*Config) error`, `Unregister(name) bool`, `Count() int`
  and `Registered() []RegisteredService` so services (and their auth providers)
  can be wired **in code, after boot**, into the existing client — the missing
  piece for dynamic targets like customer-supplied webhooks whose URL + auth
  arrive at runtime from the DB. The merged service uses the same `Config` /
  `ServiceConfig` / `AuthProviderConfig` shapes the YAML decodes into and shares
  the **same token cache, connection pool, circuit breaker, retry and signing**
  as a YAML-declared one — so a `credentials-exchange` / `oauth2-client-credentials`
  provider registered once fetches its token through the normal middleware and
  reuses the warm cache on every subsequent `Call`, in a single developer call.
  `RegisterIfAbsent` is idempotent (a name already present — YAML or a prior
  registration — is left untouched, preserving its warm state) and validates the
  config the same way `New` does, returning the error at call time with
  all-or-nothing merge. `Count` / `Registered` / `Unregister` operate **only on
  runtime-registered entries** (YAML services are never listed or removable), and
  `RegisteredService` exposes `RegisteredAt` + `LastUsedAt` so the consumer can
  program any purge policy (e.g. LRU over `LastUsedAt`); the framework ships no
  implicit TTL/eviction — lifecycle stays the consumer's. The registry is held as
  one atomically-swapped snapshot (copy-on-write), so the hot read path stays
  lock-free and a registration never disturbs in-flight requests or warm provider
  state — the same pattern already used for the post-`New` cache swap. See the
  httpclient section.

### Changed

- **Wire wrapper naming unified across REST and GraphQL — `With…` carries a
  payload, `By…` is a bare id (breaking rename).** The id-carrying command/query
  wrappers used `WithID` to mean opposite things on the two surfaces — REST
  `HandleCommandWithID` was the *bodyless* verb, while GraphQL `MutationWithID`
  was the *body+id* verb — so the same token was a false friend across surfaces.
  Both now obey one compositional rule: `WithBody`/`WithBodyID` when a body is
  sent, `ByID` for a bodyless id-only verb.
  - **REST**: `HandleCommandWithID` → `HandleCommandByID` and `HandleQueryWithID`
    → `HandleQueryByID` (with their `…Spec` siblings). `HandleCommandWithBody`,
    `HandleCommandWithBodyID` and `HandleQueryWithParams` are unchanged (already
    compositional).
  - **GraphQL**: `Mutation` → `MutationWithBody`, `MutationWithID` →
    `MutationWithBodyID`, `Query` → `QueryWithParams`. `MutationByID` is unchanged.
    The GraphQL SDL / introspection type names (`Query`, `Mutation`) are untouched —
    only the Go builder functions were renamed.

  No name is reused with a flipped meaning (the ambiguous `WithID` is retired on
  both surfaces), so stale call sites fail to compile instead of silently changing
  behavior. Consumer migration is mechanical:
  `s/HandleCommandWithID/HandleCommandByID/`, `s/HandleQueryWithID/HandleQueryByID/`,
  `s/MutationWithID/MutationWithBodyID/`, plus `fwgraphql.Mutation` →
  `MutationWithBody` and `fwgraphql.Query` → `QueryWithParams`.

## [0.14.2] - 2026-06-24

### Fixed

- **GraphQL `last` / `before` now paginate backward per the Relay spec.** The
  connection arguments `first`/`after` (forward) and `last`/`before` (backward) were
  all collapsed onto `ReadCriteria.Limit` with no direction, so `last: N` returned the
  FIRST N instead of the LAST N, and a forward+backward argument mix (`first`+`last`,
  `last`+`after`, …) passed silently. `last` now sets the new `ReadCriteria.Backward`
  flag — the reader walks back from the end and returns the last N in canonical order
  (with `pageInfo.hasNextPage: false`, `hasPreviousPage` reflecting the remainder).
  Mixing forward and backward arguments, an `after`+`before` pair, or a non-positive
  page size is rejected before dispatch with a `SchemaViolationNotification`
  (`semantic: "Schema"`) — REST parity, and the `after`+`before` case is now a clean
  400 instead of reaching the reader's defense-in-depth 500. **REST is unchanged**: it
  never sets `Backward` and keeps inferring backward from a non-empty `before` cursor.

- **GraphQL error `extensions` now carry the REST envelope's `context`.** The REST
  error envelope groups messages under a translated `context` (e.g. `"User"`); the
  flat GraphQL `errors[]` has no grouping level, so the context was silently dropped
  — the one piece of notification data that survived on REST but not GraphQL.
  `errors[].extensions.context` now rides per message, closing the last data gap
  between the two surfaces (the envelope *shape* legitimately differs; the *data* no
  longer does). Emitted only when non-empty (omitempty parity), so services that
  don't name a context see a byte-identical envelope.

## [0.14.1] - 2026-06-24

### Fixed

- **GraphQL error `extensions` now mirror the REST `ErrorMessage` fully.** Domain
  notifications surfaced over GraphQL carried only `notificationKey` / `semantic` /
  `field` in `errors[].extensions`, silently dropping the translated `fieldLabel`
  (from the `labelKey` tag), the echoed `value`, and `funcName` — all of which the
  REST envelope already carries and which the shared `notifications.MessageDTO`
  already holds. GraphQL clients (and frontend-less channels relying on the
  human-readable label) now read the same fields on both surfaces. The three added
  keys are emitted only when non-empty (omitempty parity), so services that don't
  use them see a byte-identical envelope.

## [0.14.0] - 2026-06-24

### Added

- **Domain-event publishing wired into the persister** — domain events accumulated
  on an entity via `entity.RegisterEvent(DomainEvent{…})` are now forwarded
  post-commit, best-effort, through a configurable `events.Publisher` (default
  `events.SlogPublisher`, one flat slog line per event). It fires at the same
  post-commit position as the audit slog echo, on both the flat and aggregate
  write paths, so it is automatic for Auto and manual handlers alike and a no-op
  when the entity registered no events. Swap the transport (Kafka, etc.) via
  `pg.WithEventPublisher(publisher)`. The `events.Publisher` / `SlogPublisher`
  type existed before but was never invoked on any write path.

- **GraphQL count-only reads** — a connection selection of only `totalCount`
  (no `edges`, no `pageInfo`) now sets `ReadCriteria.OnlyTotal`, so the reader
  short-circuits to `CountDocuments` instead of materializing and discarding the
  full page — the GraphQL idiom for REST's `?onlyTotal=true`. The count still
  honors `where` / `search` / `includeArchived`; selecting `pageInfo` alongside
  `totalCount` forces the full read (its cursors derive from the page items); a
  pagination/sort argument (`first` / `last` / `after` / `before` / `orderBy`)
  passed with a `totalCount`-only selection is rejected with a
  `SchemaViolationNotification` (semantic Schema) — the GraphQL parity of REST's
  onlyTotal-vs-pagination 400. No schema change — no new argument. Closes the
  lone count-only parity gap between the GraphQL surface and REST.

## [0.13.0] - 2026-06-23

### Added

- **GraphQL endpoint (`web/graphql`)** — a web surface of its own that reuses the
  same application handlers REST consumes. A consumer attaches handlers to a
  registry: `fwgraphql.New(d.Pipeline).Register(fwgraphql.Query[TReq, R](
  "users", "User", h))` for reads (returning a Relay connection), and
  `Mutation[TReq](…)` / `MutationWithID[TReq](…)` / `MutationByID(…)` for writes.
  Only the reflection-only type params must be named — `TReq` (Request DTO) for
  every form, plus `R` (Response DTO) on `Query`; the command/result/query types
  are inferred from the handler + the `ToCommand`/`ToQuery` constraint (so
  `MutationByID` needs none). `Query`'s type-param list is ordered `[TReq, R, TQ]`
  so the inferable `TQ` trails and is elided. The SDL, the `where` input (the same
  `query:"X" filter:"ops"` operator allowlist as REST), the pagination /
  `orderBy` / `search` / `includeArchived` arguments, the mutation input objects
  (NonNull under `pipeline.FullBody`), and the criteria translation are all
  reflected from the same Request/Response DTOs. Parsing + validation ride
  `vektah/gqlparser/v2`; the framework owns the executor (selection-set trim,
  dispatch through the registered `pipeline.Handler`, `Page`/`Result` → wire) and
  introspection. **GraphQL is deliberately a separate surface from REST/OpenAPI:**
  it never goes through `openapi.Mount`/`MountRaw`, never appears in the Swagger
  document, and is not policed by the REST route scans — the only shared surface
  is the `pipeline.Handler` the resolvers dispatch to. `where` folds through the
  identical criteria emission, so `where: { name: { startswith: "Bo" } }`
  produces the same Mongo clause as the REST `?name.startswith=Bo`. GraphQL
  always returns HTTP 200 `{ data, errors }`; domain notifications map to
  `errors[].extensions{notificationKey, semantic, field}`. Opt in via
  `Wiring.GraphQL *graphql.Registry`; serving knobs (`path`, `uiPath`,
  `playground`, `introspection`, `rootRedirect`) live under `graphql:` in
  `microservice.<profile>.yaml`. The endpoint is authenticated by
  `AuthMiddleware` when `auth.mode: jwt`; the Layer-1 permission gate is
  declared per field via `fwgraphql.RequirePermission("resource:action")` (the
  GraphQL twin of `openapi.RequirePermission`) and enforced in the resolver
  behind the same `auth.authorization.enabled` master switch as REST (wired via
  `Registry.EnableAuthorization`, mirroring `EnableIntrospection`); a denied
  request returns HTTP 200 with the canonical `MissingPermissionNotification`
  (`semantic: "Forbidden"`, `field: "permission"`) in `errors[].extensions`,
  the same notification the REST gate returns as 403. Field-level read access is
  enforced too: the Relay node selection set (`edges { node { … } }`) is mapped to
  `ReadCriteria.Projection` before `ToCriteria`, so a field a `Query.ToCriteria`
  restricts (via `ReadCriteria.Restrict`) trips the same
  `FieldAccessForbiddenNotification` (`semantic: "Forbidden"`) the REST
  `?fields=` path returns when explicitly selected — and Mongo projects only the
  requested fields (pushdown), the same reader path `?fields=` uses. A passively
  unselected restricted field is scrubbed (never leaked) on either surface. Adds
  the `github.com/vektah/gqlparser/v2` dependency.

- **`queries.Page.ItemCursors []string`** — the per-row keyset cursor,
  positionally aligned with `Items`, filled by `MongoViewReader` from the same
  keyset tuple + context hash the edge cursors (`NextCursor`/`PrevCursor`) use.
  It lets a transport expose a cursor per element (the GraphQL Relay
  connection's `edges[].cursor`), which cannot be reconstructed above the reader
  once the physical keyset values are stripped from the returned Go-field-keyed
  items. REST ignores the field; it stays nil for count-only reads.

- **`infra.InvalidCursorError(cause error)`** — wraps a keyset-cursor rejection
  (undecodable, tuple-length mismatch, or context-hash mismatch) in the canonical
  Schema envelope via the kernel `SchemaViolationNotification`. The
  `MongoViewReader` now returns it instead of a plain `fmt.Errorf` for the three
  cursor-validation paths, so a surface that does not pre-validate the cursor
  (the GraphQL endpoint — the REST wrapper rejects it before dispatch) surfaces a
  legible Schema rejection (`errors[].extensions.semantic = "Schema"`,
  `notificationKey = "SchemaViolationNotification"`, `field = "cursor"`) instead
  of an opaque `500`/`Internal`. REST behavior is unchanged (it still pre-validates
  and reports the identical notification).

### Changed

- Internal: the read-side DTO reflection — the filter operator vocabulary
  (`Op*` constants, `knownOps`) and its criteria emission, the Request filter
  allowlist reflection, the Response projection map, and the sparse-render boot
  guard — is extracted into a single internal package (`web/queryschema`) now
  consumed by the REST wrappers, the OpenAPI generator, and the GraphQL schema
  builder. One ordered traversal (`queryschema.WalkRequest`) with two
  projections (the runtime allowlist + the OpenAPI parameter set) plus the
  GraphQL builder, so a new operator or a wire↔Go translation rule lives in
  exactly one place. No public-surface change: the `fwweb.Op*` constants are
  preserved as re-exports.

## [0.12.0] - 2026-06-22

### Added

- **`ReadCriteria.Restrict(goFieldPath)` — field-level read authorization.** A
  Query calls it inside `ToCriteria(ctx)`, after deciding from the `AppContext`
  identity that the caller may not see a field, to remove that field from the read
  entirely: it is not projected (so it never surfaces in the JSON **or** the
  tabular export — header included, thanks to the projection-aware export pruning),
  not sorted by, and not filtered on. If the request **actively** referenced the
  field — a `?sort=`, `?filters=`, or explicit `?fields=` on it — `Restrict`
  returns a 403 `*ApplicationError` (`FieldAccessForbiddenNotification`,
  `SemanticForbidden`): trying to use a hidden field is refused rather than
  silently ignored, which also closes the inference leak a dropped sort/filter
  would leave. A passive read (the field simply not requested) gets the silent
  omission. The decision stays in the application layer (the Query reads
  `Identity`); infra stays authz-blind. Pairs with the export projection fix.

### Fixed

- **Tabular export (CSV/XLSX) now respects the effective read projection** — so
  `ToCriteria` is the single source of truth for which fields surface in every
  format. Previously a field a Query removed from `ReadCriteria.Projection`
  vanished from the JSON and from the CSV/XLSX *values*, but its **column header
  survived**: the export pruned its column plan by the wire `?fields` alone,
  independent of `ToCriteria`. Now `queries.Page` carries a `Projection
  map[string]int` (stamped by `MongoViewReader.ReadPage` from the read criteria),
  and the export narrows its plan via the new `ExportPlan.PruneToProjection` (the
  Go-path counterpart of `Prune`, honoring include/exclude/whole-doc modes) — so
  the header drops too. `?fields` still drives the read projection and its
  validation; the export no longer needs the wire-token list for pruning. Build
  step toward field-level read `Hide()`.

### Changed

- **Application mappers now raise notifications by return — every fallible mapper
  returns `error`.** The Auto command/query contracts gain an `error` result on
  the developer-written boundary methods: `InsertCommand.ToEntity` →
  `(T, error)`; `FromEntity` → `(TResult, error)` on all six command contracts;
  `ApplyTo`/`ApplyPartiallyTo` → `error`; `FindByParamsQuery`/`FindByIDQuery`'s
  `ToCriteria` → `(ReadCriteria, error)`. `domain.GetUpdatable` /
  `GetPartialUpdatable` accept `apply func(T) error` (propagated before
  validation). This lets Application (and Infra) raise a notification from a
  mapper — e.g. an external-service failure inside `ToEntity` — via the idiomatic
  Go return path (`errors.As` at `pipeline.Run`), instead of being forced through
  the domain. The accumulate-then-gate facilitator stays domain-only, justified
  by the domain being the one sealed construction path (`ValidEntity` cannot be
  hand-built). Auto handlers propagate each mapper's `error`; `pipeline.Run` is
  unchanged. Breaking surface change — consumer mappers add `, error` and a
  `nil`/propagated return.

- **`TableSchema.PK` now takes only the column: `PK(column string)`** (was
  `PK(goName, column string)`). The Go side of the primary key is fixed to the
  `domain.Entity`/`BaseEntity` contract's `ID` (roots carry it privately and
  expose `GetID`/`SetID`; AVOs/children expose the exported `ID` field), so it
  was never a free parameter — only the physical column varies (`id`,
  `person_pk`, an upstream schema's own name). This aligns `PK` with the
  single-argument managed-column setters (`CreatedAt`/`UpdatedAt`/`SoftDelete`).
  Call sites change from `PK("ID", "id")` to `PK("id")`. Breaking surface change.

## [0.11.0] - 2026-06-19

### Added

- **File/download success responses on the canonical `Mount` path.**
  `openapi.RouteSpec` gains an optional `FileResponse *FileResponseSpec`
  (`{ContentType string}`): when set, the success status is documented as a raw
  file/stream of that content type (`{type: string, format: binary}`) instead of
  the JSON envelope, while the query/filter parameters (reflected from
  `RequestType`) and the standard error envelopes (401/422/500) render unchanged.
  Mutually exclusive with `Paged` and a non-nil `ResponseType` (boot panic at
  `Mount`). This completes `RouteSpec`'s response taxonomy
  (`{ResponseType envelope | Paged envelope | FileResponse}`) so a typed query
  route can return a file without leaving the canonical path or dropping to
  `MountRaw`. The tabular-export routes now mount via `Mount` (not `MountRaw`),
  so CSV/XLSX exports document their filters in Swagger.
- **Self-sufficient export `*Spec` wrappers** — `fwweb.HandleQueryAsCSVSpec` /
  `HandleQueryAsXLSXSpec` (and the generic `HandleQueryExportSpec`) return
  `(fiber.Handler, openapi.RouteSpec)` with `RequestType` + `FileResponse`
  prefilled, so the consumer mounts an export with the same `openapi.Mount` call
  as any JSON query route (symmetric with `HandleQueryWithParamsSpec`).
- **Export wrappers take `web.ExportView` + `web.ExportDeps`.** All four export
  wrappers (`HandleQueryExport{,Spec}`, `HandleQueryAsCSV{,Spec}`,
  `HandleQueryAsXLSX{,Spec}`) accept the view as a `web.ExportView` interface
  (the `*infra.ViewDefinition` satisfies it structurally, so `web` imports no
  `infra`) plus a `web.ExportDeps{Translator, MaxExportRows}` bundle
  pre-packaged on the new `bootstrap.Deps.Export` field. The wrapper resolves
  the plan, the row ceiling, and the download filename (`view.Name()`)
  internally, so the consumer threads `view, d.Export` at an export route
  instead of spelling out `view.ExportPlan()` + `d.Translator` +
  `view.ResolveMaxExportRows(d.Config.Query.MaxExportRows)` + a filename by hand.
- **`openapi.RouteSpec.OmittedQueryParams []string`** — query parameter names to
  drop from the generated OpenAPI parameters even though `RequestType` declares
  them. The export `*Spec` wrappers reuse the JSON list's Request DTO but ignore
  pagination at runtime, so they list `limit`/`after`/`before`/`onlyTotal` here;
  the spec assembler strips exactly those, keeping the honored filters / `fields`
  / `sort` / `search` / `includeArchived`. Swagger no longer advertises the four
  pagination knobs on a CSV/XLSX export — the spec stops claiming a control the
  export does not honor. Empty (no-op) for every other route.
- **Tabular export of a view query (CSV + XLSX, format-pluggable).**
  `fwweb.HandleQueryExport[TReq, TQ]` and the convenience `fwweb.HandleQueryAsCSV[TReq, TQ]`
  mount a route that streams the same view read as a paged GET — reusing the
  same Request DTO + query handler — rendered as a flat file. The layout is
  hierarchical: root columns start at column 0, each embed one column deeper
  (infinite nesting), with a blank separator line after each aggregate /
  sub-aggregate concludes (consecutive blanks collapse). Headers come from each
  column's `labelKey` resolved per
  `Accept-Language` (falling back to the Go field name). `?fields=` narrows the
  columns (allowlist driven by the view schema, not a Response DTO);
  filters / `?search` / `?sort` / `?includeArchived` behave like the JSON list;
  user pagination (`?limit` / `?after` / `?before` / `?onlyTotal`) is ignored —
  the export returns the full filtered set, capped at the resolved export
  ceiling (the wrapper sets the new `queries.ReadCriteria.BypassMaxLimit` so its
  operator-set ceiling is honored verbatim instead of being rejected by the
  per-view page `?limit` ceiling). The format is a pluggable `web/export.Encoder`;
  `export.CSV(export.WithDelimiter(r))` is the first encoder, with the field
  separator chosen at mount time. The format-neutral core —
  `queries.ExportPlan` (built by `infra.(*ViewDefinition).ExportPlan()`),
  `export.Generate`, and the `export.Encoder`/`Sink` boundary — means a new
  format is a new encoder with no change to the plan, the generator, or the
  HTTP wrapper.
- **XLSX (Excel) export** — `export.XLSX(export.WithSheetName(...))` encoder +
  the convenience wrapper `fwweb.HandleQueryAsXLSX[TReq, TQ]`, a drop-in sibling
  of `HandleQueryAsCSV` sharing the same plan, generator, and criteria handling.
  Header rows are bold and numeric/typed cells keep their type (`Cell.Value any`
  on the neutral `Row` carries the type through to the encoder). Built on
  `github.com/xuri/excelize/v2` via its streaming writer (memory bounded by
  `maxExportRows`); the per-level offset becomes the spreadsheet's own column
  offset. Adds `github.com/xuri/excelize/v2` as a dependency.
- **`ViewDefinition.MaxExportRows(n)` + `query.maxExportRows` yaml** — per-view
  and service-wide ceilings on the number of rows a tabular export streams,
  resolved via `ViewDefinition.ResolveMaxExportRows(yamlDefault)` (cascade:
  per-view override > yaml default > `infra.DefaultMaxExportRows` = 10000).
  Operational state — NOT part of `RebuildHash` / `ArtifactHash`, mirroring
  `MaxLimit`.
- **External-schema field labels.** `NewExternalSchema(table).Field(go, col, labelKey)`
  declares a header catalog key inline on a type-less view source (an upstream
  collection that has no Go struct to carry a `labelKey:"…"` tag — the
  "mini-domain"). External-only: passing a labelKey on a type-anchored
  `NewTableSchema[T]` is a boot panic, because that schema declares the label
  via the field's struct tag (never two ways to express one domain concept).
  `Field`'s signature gains an optional trailing `labelKey ...string` (backward
  compatible); the audit/export label resolver (`labelKeysByGoField`) now
  resolves both the inline label and the struct tag.
- **`domain.ToLowerCamel(s)`** — exported acronym-aware lowerCamel (mirrors the
  existing `PluralizeWord`), used by the export plan to derive a column's wire
  token (`ZipCode` → `zipCode`).
- **`TableSchema` — the single, mandatory, explicit Go-field↔physical-column
  map**, superseding the convention/inference model (and the never-released
  `RepoConfig` map that briefly preceded it on `main`). Built with
  `NewTableSchema[T](table)` (type-anchored —
  validates each field against `T` at construction; a `Field` naming a missing
  or unexported field panics at boot) or `NewExternalSchema(table)` (type-less,
  for external `FromSchema` upstream sources). Chainable builder: `PK(go, col)`,
  `FK(col)` (child), `Field(go, col)`, `SoftDelete(col)`, `CreatedAt(col)`,
  `UpdatedAt(col)`, `Child(*TableSchema)`. There is no name inference: every
  persisted field is declared, and an undeclared exported field is runtime-only
  by construction (never persisted, scanned, or audited). Aggregate depth is
  one level: a child schema that declares its own `Child(...)` (a grandchild)
  panics at `WithSchema` (write side — model the sub-collection as a separate
  aggregate), and an embed source whose schema carries `Child(...)` is a fatal
  `ValidateViewSchemas` error (read side — nest projections via `EmbedMany`/
  `Embed`, never the schema's `Child(...)`). Width (child types + instances)
  is unlimited. PK is mandatory, single-column, and has no default — every
  schema (root, child, embed source) must declare `PK(go, col)` (no `"ID"`/`"id"`
  guessing), which rejects empty names; an aggregate `Child(...)` must declare its FK
  (`.FK(col)`) or it panics, and on the read side an `EmbedMany` source without
  `.FK(col)` or a one-to-one `Embed` without `.On(col)` is a fatal
  `ValidateViewSchemas` error.
- **`BaseAggregateRepository.WithSchema(*TableSchema)`** threads the one schema
  into the write binding AND the read loader (write SQL + criteria + auto-scan).
  Aggregate children come from the schema's `Child(...)` declarations.
- **The same `TableSchema` drives the Mongo read side.** `ViewDefinition.Schema(ts)`
  attaches the root map; `fwinfra.FromSchema(ts)` constructs each embed source from a
  schema (table/collection, store kind, and `EmbedMany` join FK all derived from it).
  The composer writes physical columns; the `MongoViewReader` translates each leaf
  back to its Go field name using these schemas, so the typed Response speaks Go names
  with only `json:` tags.
- **Three managed columns by presence, not a bool** — calling
  `SoftDelete/CreatedAt/UpdatedAt(col)` enables; omitting disables. `created_at`
  and `updated_at` are actively stamped `NOW()` on write (no reliance on a DB
  default); on the read path they are readable under fixed logical Go names
  `CreatedAt`/`UpdatedAt`/`DeletedAt`. Column declarations are a strict bijection
  over the full physical column set: `PK`, every `Field`, and the three managed
  columns panic at construction when two map to the same physical column —
  enforced regardless of declaration order (a managed column declared after the
  field it collides with, or two managed slots sharing a column, fail loudly).
- **`fwinfra.FromSchema(*TableSchema) *Source`** — the single embed source
  constructor. Table/collection, store kind (type-anchored `NewTableSchema[T]` →
  local Postgres; type-less `NewExternalSchema` → external/Mongo — the schema's
  type IS the signal), and the `EmbedMany` join FK are all derived from the schema.
  A local embed derives its parent-side Go segment from the schema's Go type
  (pluralized for `EmbedMany`); `.As(...)` is an optional override there and is
  **required** on an external embed. `.On(key)` is one-to-one-`Embed`-only (the
  parent doc FK pointing at the source PK).
- **`infra.ValidateViewSchemas(views)`** — fatal boot enforcement (called by
  `bootstrap.Run`) that every view root and every embed declares a schema, and
  every external embed declares `.As(...)`. There is no optional / pass-through /
  schema-less mode.
- **`domain.PluralizeWord` exported** — used by infra to derive the local embed's
  Go segment (pluralized for `EmbedMany`).
- **Boot-time configuration guards (fail-fast on misconfigurations the boot
  already has full knowledge of).** Each aborts the boot with a single,
  aggregated diagnostic instead of letting the misconfiguration surface as a
  runtime error or a silent no-op:
  - **`auth.publicRoutes` are validated against the registered route set.** An
    entry that matches no registered `METHOD /path` (a typo, wrong method, or
    trailing slash) or that carries a Fiber path parameter / wildcard (which the
    exact-match `AuthMiddleware` can never honor — mark the route
    `Doc.Public=true` instead) aborts the boot. Runs after every route
    (features, `/health`, the OpenAPI spec/UI, the optional root redirect) is
    registered.
  - **Declared `integration.subscribes` entries must have a registered
    receiver.** A subscription declared in YAML with no matching
    `reg.From(source).On(eventKey, …)` (the inverse of the existing receiver→YAML
    check) would spin no consumer and silently drop every message; boot now
    aborts via `integration.ValidateSubscriptionsCovered`.
  - **`integration.subscribes.<src>.startFrom` / `defaults.startFrom` are
    enum-validated** (`earliest` | `latest`); a typo previously resolved
    silently to `latest`.
  - **Migration filenames without a parseable `{version}_{name}` prefix abort
    the boot** (`Manager.ValidateDownExists` + `MigrationFilenameInvalidNotification`).
    golang-migrate silently ignores such files, so the operator's SQL would never
    run while boot reported success.
  - **The aggregate boundary the domain declares (`AggregateChildren()`) and the
    children the `TableSchema` declares (`.Child(...)`) must name the same set**
    — `BaseAggregateRepository.WithSchema` panics on any drift (a child declared
    on only one side).
  - **`httpClient.authProviders.<name>.tokenEndpoint` is validated as an absolute
    URL** (`oauth2-client-credentials` + `credentials-exchange`), mirroring the
    `services.<name>.baseURL` check — a typo'd scheme / host-less value aborts the
    boot instead of failing on the first token acquisition.
  - **`httpClient` signing `signedHeaders` may not name the policy's own
    `signatureHeader` / `keyIdHeader`** — those are set after the canonical string
    is built, so signing them signs an always-empty value and every signed
    request would be rejected upstream.
  - **A Mongo view index key must name a column the composer emits**
    (`ValidateMongoSpec`) — an index on a typo'd / undeclared field (e.g.
    `Index("emial")` or a Go field name `addresses.zipCode` instead of the
    column `addresses.zip_code`) would be dead (never used); boot aborts naming
    the key and the emitted column set. Keys are validated against the root
    columns + each embed subtree + `_id`.
  - **A top-level `$jsonSchema.required` entry must name a column the composer
    emits** (`ValidateMongoSpec`) — a `required` field the document never carries,
    under the default `validationAction: error`, makes Mongo reject every
    SyncEngine upsert and silently freeze the projection; boot aborts instead.
- **`BaseRepository.WithSchema(*TableSchema)`** — the validated canonical way to
  bind a schema on a flat (non-aggregate) repository: runs the PK-declared,
  aggregate-depth, and `Modes()` ⟺ `SoftDelete` checks (the same the aggregate
  path runs) at construction instead of on the first write. Setting `r.Schema`
  directly remains supported as the unchecked escape hatch.

### Removed

- **`RepoConfig`, `SourceMap`, `ManagedColumn`** — replaced by `TableSchema`.
- **`WithChild[V]` / `WithChildAutoScan` / `WithConfig`** — children are declared
  on the schema via `Child(...)` and threaded via `WithSchema`.
- **`ViewOf[*T]()`** — views are declared explicitly via
  `View(name).Version(n).Root(table).Schema(ts).EmbedMany(field, FromSchema(childTs))`.
- **`From(string)` / `FromMongo(string)` string constructors and the
  `Source.Schema(ts)` method** — replaced by `FromSchema(ts)`, which derives the
  table, store kind, and `EmbedMany` join FK from the schema. (`Source.SchemaDef()`
  and the schema-less detection helper are gone with them.)
- **The `view.embed.schemaless` boot advisory + identity pass-through fallback** —
  schema is now mandatory on every view (root + every embed), not optional. There
  is no `slog.Warn` and no pass-through mode; a missing root schema or an external
  embed missing `.As(...)` is a fatal boot error via `infra.ValidateViewSchemas`.
- **The `view:"<docKey>"` Response struct tag** — the reader returns a Go-keyed
  document, so the Response carries only `json:` tags; there is no source-key
  override on the read projection.
- **Name-inference helpers for persistence** (`PascalToSnake`/`PluralizeSnake`
  used to derive tables/columns/FKs, `ColumnsOnly`/`ColumnSpec`) — gone.
  (`domain.PascalToSnake` itself is no longer used to map persistence names.)
- **The `transient:"-"` tag is removed entirely** — no longer read by any layer
  (persistence is driven by the explicit `TableSchema`; the field-label resolver
  dropped its vestigial opt-out). A field is persisted iff it is declared in the
  `TableSchema`; a field gets a label iff it carries a `label:` tag.

### Changed

- **The field-label struct tag is now `labelKey:"<catalogKey>"`** (was `label:`).
  The tag value has always been a *catalog key* the framework resolves to the
  rendered, locale-specific label — `FieldLabelKey` vs `FieldLabel` already names
  both ends internally; the tag now matches that vocabulary and stops colliding
  with domain fields literally named `Label`. The opt-out spelling becomes
  `labelKey:"-"`. **Breaking** — every consumer field declaring `label:"…"` must
  rename the tag to `labelKey:"…"`; resolution is otherwise unchanged.
- **Persistence names are no longer derived from Go identifiers.** Tables,
  columns, and child FKs are declared in the `TableSchema`; a typo is a boot
  panic, not a silent miss. **Breaking** — every consumer Repository and view
  must declare a `TableSchema` and call `WithSchema`.
- **Every view must declare a schema on the root AND on every embed** (breaking).
  The embed's table, join FK, and store kind come from the schema via `FromSchema`;
  `.On` is now one-to-one-`Embed`-only (no longer used by `EmbedMany`, whose FK
  comes from the schema); external embeds must declare `.As(...)`.
- **Read-side wire↔doc translation is now a two-hop pivot.** The web layer maps
  a wire path to the **Go field path** via the Response's `json:` tags;
  the `MongoViewReader` translates the Go path → physical Mongo column via the
  view's `TableSchema`. Filter keys are always Go field paths; sort/projection
  translate Go→column only with a typed Response (pass-through for `RawDoc` /
  `ParseCriteria`).
- **`Postgres.Insert/Update/Archive/Unarchive/Delete` and `Batch`, the
  `AggregateLoader`, and the criteria/scan internals take `*TableSchema`** instead
  of `*RepoConfig`. The aggregate-child notification path segment is now camelCase
  (`toLowerCamel(typeName)`, e.g. `OrderLine` → `orderLines`) — JSON wire output —
  replacing the snake_case `PluralizeSnake(PascalToSnake(...))` segment.
- **Audit emits the faithful domain field name** (Go field, not the column) and
  is map-blind, so a column rename never disturbs the timeline. `snapshot` keys
  and `changes[].field` now carry the raw Go field name (`Email`, `ZipCode`)
  instead of the snake_case column. **Breaking for consumers keying audit on the
  old snake_case field names** (e.g. ELK/BI pipelines).

### Fixed

- **Integration consumer topology: a source with ≥2 events no longer drops
  messages.** The Kafka consumer is now one reader per `(topic, consumerGroup)`
  that demultiplexes by `event_type` to the matching receiver, instead of one
  reader per receiver. Previously, `reg.From(s).On(A).On(B)` produced two readers
  sharing the same `(topic, consumerGroup)`; Kafka split the topic's partitions
  between them and — because the reader auto-commits — each silently dropped the
  events meant for the other (~half of every event type lost, no error). The
  fix reads every message exactly once and routes it by `event_type`; an event
  type matching no receiver is skipped (foreign event on the topic). Two
  receivers resolving to the same `(topic, consumerGroup, event_type)` now abort
  the boot (one event type cannot route to two handlers).

## [0.10.0] - 2026-06-17

### Changed

- **Moved the `criteria` package from `omnicore/criteria` to
  `omnicore/infra/criteria`.** The query DSL is consumed only by the `infra`
  layer (the framework loader/translator + consumers' own infra repository
  implementations), so nesting it under `infra` removes the stray root-level
  package. The package name is unchanged (`criteria`); only the import path
  moves — update `omnicore/criteria` → `omnicore/infra/criteria`.

## [0.9.0] - 2026-06-17

### Added

- **`omnicore/criteria/` package — backend-neutral query DSL for loading live
  domain aggregates from PostgreSQL by an arbitrary criterion.** A sealed
  expression tree (`Expr`) with a fluent builder — `Eq/Ne/In/Nin/Gt/Gte/Lt/Lte/
  Like/ILike/IsNull/NotNull`, `And/Or/Not`, sugar `Contains/StartsWith/EndsWith/
  Between` — wrapped in a `Query` carrying `WHERE` + `OrderBy`/`OrderByDesc` +
  `Limit` + an archived `Scope` (`Active`/`IncludeArchived`/`OnlyArchived`).
  `criteria.ByID(id)` is the primary-key shortcut. Pure (stdlib only, zero IO);
  the SQL translation lives behind the `Visitor` seam so other backends can be
  added without touching the tree. Consumed only inside `infra` repository
  implementations — `domain` and `application` keep business-vocabulary
  repository interfaces and never import `criteria`.
- **`AggregateLoader[T].FindOne(ctx, *criteria.Query)` and `FindAll(ctx,
  *criteria.Query)`** — load one (or `RecordNotFound`; error on >1) or many
  live aggregates (root + children) matching a criterion. `FindAll` batches
  children with `WHERE fk IN (...)` (one query per child type, not per root).
  Both honor the archived scope on root and children. Promoted on
  `BaseAggregateRepository[T]`. The single SQL-building path: by-id loads
  (`FindByID`/`FindArchivedByID`) and any alternate-key lookup all route through
  the engine.
- **Pure domain repository ports — `domain.Reader[T]`, `domain.Writer`,
  `domain.Repository[T]`.** `Reader[T]` = `FindByID` + `New`; `Writer` =
  `Insert/Update/Archive/Unarchive/Delete` taking only a ValidEntity
  (non-generic, no ctx); `Repository[T]` = `Reader[T] + Writer`. Pure (stdlib +
  google/uuid only) — what a consumer names for a read+write repository
  interface declared in the domain layer, with zero application import.
- **`persistence.ScopedRepository[T]` + `BaseRepository[T].Scope(ctx, opts...)
  domain.Writer`.** The write binding: reads stay direct on the handle
  (`domain.Reader[T]`), writes go through `Scope`, which binds the request ctx
  (cancellation → pgx, actor → audit) and the in-TX lifecycle hooks and returns
  a pure `domain.Writer`. The domain port never pronounces the ctx.
- **`persistence.RequestContext`** — request-scoped interface (`context.Context`
  + `ID()`/`ActorSubject()`/`ActorIssuer()`/`ActorClaims()`) the persistence and
  audit pipelines consume, satisfied by `*configuration.AppContext`. Relocated
  from the deleted `domain.Context`; `persistence.AnonymousActor` moved likewise.

### Changed

- **The write path is now Scope-bound.** Auto Command Handlers and the manual
  path call `repo.Scope(ctx, opts...).Insert(valid)` (etc.) instead of
  `repo.Insert(ctx, valid, opts...)`. Handlers depend on
  `persistence.ScopedRepository[T]` instead of the removed `persistence.Writer[T]`.
  Audit, cancellation, and the in-TX hook semantics are unchanged — the ctx +
  actor are captured by the bound writer internally.

### Removed

- **`domain.Context`** — deleted. The domain layer no longer declares a
  request-scoped context type (it carried `context.Context` + actor/claims, none
  of which are domain concepts). Relocated to `persistence.RequestContext`; the
  domain repository ports are now pure (no ctx in any signature).
- **`persistence.Writer[T]`** — replaced by `persistence.ScopedRepository[T]`
  (read port + `Scope`) on the handler side and the pure `domain.Writer` on the
  port side. Write call sites change from `repo.Insert(ctx, valid, opts...)` to
  `repo.Scope(ctx, opts...).Insert(valid)`.
- **`AggregateLoader[T].Load` / `LoadIncludingArchived`** — replaced by
  `FindOne(criteria.ByID(id))` / `FindOne(criteria.ByID(id).OnlyArchived())`.
  Small `infra`-API removal; the domain/application repository read contract
  (`Reader[T].FindByID`, `ArchivedFinder[T].FindArchivedByID`) is unchanged. A
  manual `WithRootScanner` used with `FindOne`/`FindAll` must now populate the
  entity id (scan it + `SetID`) — the framework no longer injects it on the
  criteria path (there is no input id).

## [0.8.0] - 2026-06-16

### Added

- **`omnicore/infra/cache/` package — generic byte-level key-value cache
  subsystem.** Single interface (`cache.Cache`) with three operations
  (`Get(ctx, key)`, `Set(ctx, key, value, ttl)`, `Delete(ctx, key)`) and
  two canonical implementations: in-process LRU+TTL (`cache.NewMemory`)
  and Redis (`cache.NewRedis`). Consumer code, domain services,
  infrastructure adapters, and the outbound httpclient all consult the
  same port via `bootstrap.Deps.Cache` (private) or
  `bootstrap.Deps.SharedCache` (cross-service).
  - Package-level typed helpers `cache.GetJSON[T]` /
    `cache.SetJSON[T]` round-trip Go values through `encoding/json`
    without polluting the interface. Both tolerate a nil `Cache` and
    degrade to no-op (consumer features can opt the cache in/out by
    declaring the YAML block).
  - `cache.RedisConfig` is exported so consumers can construct a
    Redis-backed cache programmatically (tests, alternative wiring)
    with the same diagnostics the YAML loader emits.
- **Top-level `cache:` block in `microservice.<profile>.yaml`** drives
  the framework's cache subsystem from the operator side:
  - `cache.store: memory | redis | custom` — backend selection. Default
    `memory` (in-process LRU+TTL) covers single-replica services. `redis`
    ships with the framework's `go-redis/v9`-backed adapter (lazy
    connection, JSON-encoded entries debug-able via `redis-cli GET`,
    per-op timeout governed by `timeoutMs`, namespace via `keyPrefix`).
    `custom` requires `bootstrap.Wiring.Cache` to be set.
  - `cache.shared:` sub-block declares a SECOND cache exposed on
    `Deps.SharedCache`. nil unless declared. `cache.shared.store:
    memory` is REJECTED at boot — an in-process LRU cannot honor cross-
    service reads. Supports `redis` and `custom` only.
  - `cache.redis.failMode: open | closed`. `open` (default) swallows
    transport errors + emits `slog.Warn "cache.redis.transport.error"`
    + returns miss (Get) / nil (Set/Delete) so the call proceeds to
    upstream. `closed` propagates the error.
  - `cache.maxEntries` caps the in-process LRU (only relevant for
    `store: memory`). 0 falls back to the framework default 10k.
- **`bootstrap.Deps.Cache` and `bootstrap.Deps.SharedCache`** expose
  the resolved instances to every Feature. The httpclient cache
  middleware consumes `Deps.Cache` automatically — operators no longer
  declare a separate backend under `httpClient`.
- **`bootstrap.Wiring.Cache` and `bootstrap.Wiring.SharedCache`** are
  the escape hatches for `cache.store: custom` and
  `cache.shared.store: custom`. Mismatched wiring (e.g. `store: memory`
  + Wiring.Cache injected) fails the boot with a structural-coherence
  error so misconfiguration surfaces at startup, not at runtime.
- **`httpclient.WithCache(cache.Cache) Option`** binds the byte-level
  cache the GET cache middleware reads / writes through. Bootstrap
  forwards `Deps.Cache` automatically; manual lifecycles (tests,
  alternative wiring) call it directly. `httpclient.HttpClient.SetCache`
  is the runtime swap used by bootstrap to honor late `Wiring.Cache`
  injection without rebuilding the middleware chain.

### Changed

- **`httpClient.defaults.cache:` no longer carries the backend choice.**
  The block is reduced to POLICY knobs only: `enabled`, `defaultTTL`,
  `honorCacheControl`. The backend is read from `Deps.Cache` (declared
  at the top-level `cache:` block). Per-endpoint `cache: { ttl, varyOn }`
  and `cacheAcceptable: true | false` semantics are unchanged.
- **`infra/httpclient/cache_middleware.go` is now a thin wrapper over
  `cache.Cache`.** The middleware encodes its internal `cacheEntry`
  (response body + headers + status + content-type + content-length +
  expiresAt) as JSON before storage and decodes on hit. Stored entries
  remain debug-able via `redis-cli GET <key>` + `json.loads`.
- **HTTP response cache TTL = 0 from `Cache-Control: max-age=0` no
  longer stores the response.** The new byte-cache layer treats `ttl == 0`
  as "no expiration" (the opposite of what the upstream asked for), so
  the middleware short-circuits the store. Pre-existing behavior was
  identical from the consumer's perspective (the entry expired
  immediately) — the explicit skip avoids polluting the cache with
  entries that would only be served once.

### Removed

- **`httpclient.Cache` interface** — replaced by the framework's
  top-level `cache.Cache`. Consumers who implemented custom backends
  via `httpclient.WithCacheStore` migrate to `cache.Cache` (same shape,
  with `Delete` added).
- **`httpclient.CacheEntry`** is now an internal type
  (`httpclient/cache_middleware.go::cacheEntry`). The previous public
  exposure was a draft contract from the in-flight feature branch and
  never shipped in a tagged release.
- **`httpclient.WithCacheStore(Cache) Option`** — replaced by
  `httpclient.WithCache(cache.Cache) Option`.
- **`httpClient.defaults.cache.store` and `httpClient.defaults.cache.redis`
  YAML keys** — moved to the top-level `cache:` block.

## [0.7.0] - 2026-06-16

### Added

- **Field labels — `label:"<catalogKey>"` struct tag on entity / value-object
  fields.** Resolves through the same `Translator.Render` already used for
  notification messages and produces translated human-readable identifiers
  alongside the technical `field` / column name on every reactive output:
  - **`MessageDTO.FieldLabel`** (new, `json:"fieldLabel,omitempty"`) carries
    the rendered string in the actor's locale next to `FieldName`. Channels
    without a frontend (e-mail, SMS, push) read it directly so the recipient
    sees "CEP é inválido" instead of "addresses[0].zipCode é inválido".
  - **`ErrorMessage.FieldLabel`** (new, `json:"fieldLabel,omitempty"`) on the
    web envelope — `ResponseFromContextDTOs` + `ResponseFromContexts` both
    propagate the value through so the wire HTTP response carries the
    rendered label as published by the consumer.
  - **`FieldChange.FieldLabelKey`** (new, `json:"fieldLabelKey,omitempty"`)
    carries the catalog key on every audit row (root delta + child cascade).
    Render-at-read fits compliance flows where the auditor reads in a locale
    that may differ from the actor's; the key persists across catalog
    evolution.
  - **`FieldChange.FieldLabel`** (new, `json:"fieldLabel,omitempty"`) is the
    read-time slot the audit renderer populates after consuming
    `FieldLabelKey`. Mutually exclusive with `FieldLabelKey` in practice —
    the in-flight write carries the key; the rendered read carries the text.
- **`audit.RenderLabels(ev, t, lang)` + `audit.RenderLabelsInJSON(doc, t, lang)`.**
  In-place audit read renderers. Walk every `FieldChange` (root + child
  cascade), pop `FieldLabelKey`, and write the translated string to
  `FieldLabel` via `Translator.Render(lang, key, nil)`. The typed
  variant operates on `*audit.AuditEvent` for in-process Go readers; the
  JSON variant operates on `map[string]any` for BI / SQL tools that parse
  the `audit_events.jsonb` payload directly. Catalog miss inherits the
  existing `Translator.Render` fallback (raw key + `slog.Warn` once per
  `(lang, key)`). Snapshot blocks are intentionally not touched — they
  carry `map[col]value` with no schema for labels.
- **`audit.FindByID(ctx, exec, id)` + `audit.FindByAggregate(ctx, exec, entityType, aggregateID)`.**
  Canonical reader helpers for the `audit_events` table. Forensic lookups
  by row id and timeline reads by aggregate (index-served by
  `audit_events_entity_timeline_idx`). Compose with `audit.RenderLabels`
  for translated read in three lines. Both take the minimal `pgExec`
  interface (`*pgxpool.Pool` / `*pgxpool.Conn` / `*pgx.Conn` / `pgx.Tx`
  satisfy it). `ErrAuditNotFound` sentinel exported for the miss path on
  `FindByID`.
- **`Rules.entityType reflect.Type`** plus a third parameter on `NewRules`
  (internal framework signature). `r.AddNotification` reads the field's
  `label` tag at emit and writes the catalog key onto
  `NotificationMessage.LabelKey`; the convert layer renders it via
  `Translator.Render(lang, key, nil)` next to the existing Message render.
  Same caching shape as the `tvar` extraction (`sync.Map` per `reflect.Type`).
- **Documentation of the existing three-path field-name override surface.**
  CLAUDE.md + DOCS.html now describe `AddFieldNameAlias` (entity-stable
  rename), `ChangeFieldName` (request-conditional rename), and the default
  PascalCase → camelCase emission side by side. Behavior unchanged — the
  docs were lagging.

### Changed

- **`NewRules` signature gained `entityType reflect.Type` (3rd arg).** All
  framework call sites updated (`entity_base.go` × 5, `aggregate_root.go` × 1,
  `runAggregateValidations` × 2). Consumer code does NOT call `NewRules`
  directly; the change is internal. Tests that exercise Rules in isolation
  pass `nil` to opt out of label resolution.

## [0.6.0] - 2026-06-16

### Added

- **Cross-service integration events — canonical write-side async path.**
  New package `omnicore/infra/integration` carries the producer surface
  (`Dispatch(ctx, eventKey, payload, opts...)` with `WithTx`/`WithAggregateID`/
  `WithCorrelation`/`WithCausation`) and the consumer surface (`Registry`,
  `Receiver`, `ConsumerPool`, `RequestWithCommand` via reflection on the
  wire DTO's `ToCommand()`). Wire `event_type` strings live in YAML;
  Go-side code references the YAML keys (`eventKey`, `sourceKey`) so a
  wire rename is a YAML edit, not a code sweep. Handlers are invariant
  across transports: a single `pipeline.Handler[TCmd, TResult]` Mounts
  on HTTP via `fwweb.HandleCommandWithBody` AND on Kafka via
  `reg.From(source).On(eventKey, sample, handler)`.
- **`IntegrationFeature` interface** under `omnicore/bootstrap` — opt-in
  via type assertion (mirror of `ReadableFeature`). Bootstrap calls
  `MountReceivers(reg, deps)` on every feature implementing it during
  Phase Receivers, between Phase HTTP and ConsumerPool start.
- **`Deps.IntegrationRegistry` + `Deps.UpstreamSubscribers`.**
  Consumer admin surfaces walk both slices to expose retry
  endpoints. The upstream subscriber slice was previously documented
  as "not surfaced on Deps" — gap closed in the same round as the
  integration receivers since the admin retry pattern is identical
  across the two surfaces.
- **YAML blocks `integration:` and `shutdown:`** under
  `microservice.<profile>.yaml`. `integration.publishes.events.<key>`
  declares producer-side wire metadata; `integration.subscribes.<src>.
  events.<key>` declares subscriber-side wire metadata. `integration.
  defaults` seeds consumer-group / worker / startFrom across sources.
  `shutdown.drainTimeoutSeconds` caps the coordinated drain (default
  30s).
- **Embedded migration `0002_integration_events.{up,down}.sql`.**
  Creates three tables: `integration_events` (producer-side
  authoritative store; written in the same TX as the data row +
  outbox + audit when `WithTx(tx)` is supplied), `omnicore_integration_
  failures` (consumer-side failure registry, mirrors `omnicore_
  upstream_failures` shape for parity in operator tooling), and
  `omnicore_integration_processed` (per-(event_id, consumer_group)
  dedup table with BRIN index for time-window pruning).
- **`AppContext.CorrelationID` / `CausationID` accessors + setters.**
  Concurrent-safe via the existing `sync.RWMutex`. Receiver pipeline
  populates from inbound event metadata; outbound `Dispatch` reads
  them as fallback when `WithCorrelation` / `WithCausation` are
  omitted — events emitted inside a receiver handler automatically
  carry the inbound trace chain.
- **`UpstreamSubscriber.Shutdown(ctx) error`.** Drains in-flight
  ripple ops under the supplied drain context. Fills the previously
  documented gap where a SIGTERM mid-ripple would drop the in-flight
  recompose on the floor.

### Changed

- **`bootstrap.Run` Phase Receivers + coordinated drain.** After
  Phase HTTP (`f.Mount`) bootstrap iterates every `IntegrationFeature`
  and calls `MountReceivers(reg, deps)`. The ConsumerPool then starts
  one supervisor per receiver before `app.Listen`. On SIGINT/SIGTERM
  the HTTP server, integration consumer pool, and upstream
  subscribers drain in parallel under the shared `shutdown.
  drainTimeoutSeconds` budget — drains that exceed surface as
  `slog.Warn` lines so the operator knows what did not finish.
- **Documentation: outbound HTTP error handling pattern.** New `Outbound error
  handling` subsection under `httpclient — outbound HTTP` in `DOCS.html`
  documents the canonical translation path for `*HttpError` returned by
  `httpclient.Call`: handlers wrap the failure with a service-defined
  notification via `exception.SingleNotificationError` /
  `exception.FieldErrorWithCause` (or `infra.FieldErrorWithCause` when the
  mapping lives inside the adapter). Untranslated failures keep falling through
  `pipeline.Run` to the canonical 500 `InternalServerErrorNotification`
  envelope — by design, since only the consumer knows the domain semantic of an
  upstream error. No runtime change; clarifies an existing surface and
  discourages per-service `respondWithError` helpers that duplicate the
  framework's canonical envelope.

## [0.5.0] - 2026-06-15

### Changed

- **Upgrade Fiber v2 → v3.** Breaking change throughout the HTTP layer:
  - Handler signature now uses `fiber.Ctx` (interface), no pointer. Every
    `func(c *fiber.Ctx) error` in the public surface becomes
    `func(c fiber.Ctx) error`.
  - `c.BodyParser(&req)` / `c.QueryParser(&req)` replaced by the unified Bind
    API: `c.Bind().Body(&req)` / `c.Bind().Query(&req)`.
  - `c.UserContext()` removed upstream — `fiber.Ctx` now implements
    `context.Context` directly. `AppContext.SetParent(c)` replaces
    `AppContext.SetParent(c.UserContext())`.
  - `app.Add(method, path, handler)` now takes `[]string` for methods:
    `app.Add([]string{method}, path, handler)`.
  - `c.Redirect(uri, status)` replaced by builder chain:
    `c.Redirect().Status(status).To(uri)`.
  - `app.Test(req, -1)` (timeout disable) replaced by
    `app.Test(req, fiber.TestConfig{Timeout: 0, FailOnTimeout: false})`.
  - `fiber.Config.DisableStartupMessage` moved to `fiber.ListenConfig`:
    `app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true})`.
  - `cors.Config.AllowOrigins` is now `[]string` (was comma-separated string).
  - `recover` middleware's `StackTraceHandler` signature updated to
    `func(c fiber.Ctx, e any)`.

  Consumer services must upgrade in lock-step after the framework tag is cut.

- **Bump `github.com/jackc/pgx/v5` from v5.9.2 to v5.10.0.** No breaking
  changes. Brings security hardening (cap server-supplied SCRAM iteration
  count, bound binary decoders against malicious server input,
  `CancelRequest` over TLS when primary connection used TLS), a few opt-in
  features (`require_auth` to restrict accepted auth methods,
  `ParseConfigOptions.ConnStringAllowedKeys`, `StructArgs` /
  `StrictStructArgs` for `@`-named queries, `pgxpool` expiration check
  before acquire, `ErrConnClosed` sentinel), and several fixes
  (`"char"` OID 18 binary scanning, typed-nil `driver.Valuer` in array /
  composite codecs, race on context cancellation).

### Removed

- **`web.CORS(origins ...string)`** — removed. Services and bootstrap call
  `cors.New(cors.Config{AllowOrigins: []string{...}, ...})` directly, the
  Fiber v3 idiomatic pattern.
- **`web.Logger() fiber.Handler`** — removed. Bootstrap calls
  `logger.New()` directly.
- **`web.RateLimit(max int) fiber.Handler`** — removed. Services call
  `limiter.New(limiter.Config{Max: max})` directly.

  Rationale: these three wrappers were thin delegations over Fiber middleware
  with no omnicore-specific value. Removing them aligns the framework with
  the Fiber v3 documented surface and reduces API drift. `web.Recover()` is
  kept because it carries omnicore-specific logic (slog-integrated
  `StackTraceHandler` that emits structured panic logs).

## [0.4.0] - 2026-06-14

### Added

- **Parameterized notifications** — translation messages can carry runtime
  variables substituted from notification payload values. Notifications
  declare `tvar:"<name>"` struct tags on exported fields; catalog entries
  use the matching `{<name>}` placeholders; the rendering layer
  (`application/notifications/convert.go`) auto-resolves and interpolates
  during pipeline translation. Per-emit overrides via
  `r.AddNotificationWithVars(field, n, vars, value...)`; escape hatch for
  unexported / computed values via a `TranslationVars() map[string]string`
  method on the notification. Context labels (`NotificationContext.Context()`)
  carry their own variables via `ctx.SetVars(map[string]string{...})`,
  surfaced through `ctx.ContextVars()`. New API surface:
  `domain.ExtractVarsFromTags(n)`, `domain.MessageVars(msg)`,
  `domain.TranslationVarsProvider` interface, `Vars` field on
  `NotificationMessage`, `Translator.Render(lang, key, vars)`,
  package-level `translation.Interpolate(s, vars)`. Backwards-compatible:
  notifications without `tvar` and catalog entries without `{...}`
  placeholders behave identically to the prior `Get` path. Scanner
  whitelists `{[A-Za-z_][A-Za-z0-9_]*}`; placeholders missing from the
  var map are left literal and `slog.Warn`-ed once per
  `(lang, key, placeholder)` tuple.
- `infra.UnwrapPgxTx(persistence.TxHandle) pgx.Tx` — the single authorized
  bridge from the opaque `persistence.TxHandle` token to the underlying
  `pgx.Tx`. Lives in `infra/` so only adapters in that layer can call it;
  panics with a descriptive diagnostic on a foreign `TxHandle`
  implementation. Infra-layer port adapters now use this helper to recover
  the live transaction and execute SQL on behalf of an application-layer
  port whose method receives a `TxHandle`.

### Changed

- **`persistence.TxHandle` is now a sealed marker** with no public methods.
  Application code receives the handle and threads it to a port; the port's
  adapter in `infra/` calls `UnwrapPgxTx` to obtain the `pgx.Tx`. The
  sealing method on the interface is unexported, so only the framework's
  own `infra/pgxTxHandle` satisfies it. Removes a code path where the
  application layer could pronounce SQL via the handle's previous
  `Exec` / `Query` / `QueryRow` surface — the new shape makes "application
  is SQL-free" a type-system guarantee instead of a documentation rule.

### Deprecated

### Removed

- `persistence.TxHandle.Exec` / `Query` / `QueryRow` methods — replaced by
  the sealed-marker shape above. Hooks that need an in-TX side effect now
  declare a port in `application/` (or `domain/`) and implement the SQL in
  an `infra/` adapter that calls `UnwrapPgxTx`.
- `persistence.CommandTag`, `persistence.Rows`, `persistence.Row` types —
  removed alongside the SQL methods they served. Adapters that need
  command-tag / iterator / single-row semantics consume the corresponding
  `pgx` / `pgconn` types directly through the unwrapped `pgx.Tx`.

### Fixed

### Security

## [0.3.0] - 2026-06-13

Initial public release of the rewritten history. Skips `v0.1.0` /
`v0.2.0` — both versions are frozen on `proxy.golang.org` pointing
to content from a prior repo that no longer exists.

### Added

- **DDD layering with enforced boundaries** — `domain` (pure rules, zero I/O),
  `application` (pipeline, orchestrator, queries), `infra` (Postgres + outbox,
  Mongo, Kafka SyncEngine, Composer, Audit), `web` (Fiber transport). Cross-layer
  error contract via `domain.NotificationCarrier`.
- **Sealed `ValidEntity` types** (`Insertable` / `Updatable` / `Archivable` /
  `Deletable` / `Unarchivable` / `Batch`) produced only by `domain` package
  functions; compile-time enforcement via private `entity()` method.
- **`AggregateRoot` with universal symmetric cascade** — root archive → children
  archive, root unarchive → children unarchive, root delete → FK ON DELETE
  CASCADE. Top-level primitives `AddAggregateChild` / `ChangeAggregateChild` /
  `RemoveAggregateChild` / `ReplaceAggregateChildrenOf` with declarative
  boundary via `AggregateChildren() []AggregateValueObject`.
- **Old-state snapshot** captured automatically by `Get*` functions; exposed
  via `Entity.Old()` and the typed `domain.Old[T](e) T` wrapper. Consumed by
  `BuildRules` for transition-aware invariants and by the auditor for change
  computation.
- **Rules DSL** (`r.IfInsert` / `r.IfUpdate` / `r.IfDelete` / `r.IfInsertOrUpdate` /
  `r.IfDisplay`) — mode-scoped validation closures. Archive/Unarchive fire
  `IfUpdate` with a distinct `actionName` for state-transition branching.
- **Notification system** with typed structs (translation key = struct name),
  scoped `NotificationContext`, path-aware field names for nested aggregates,
  manual override via `ChangeFieldName`. Wire format carries `NotificationKey`
  + `Semantic` so clients can branch UI without parsing status codes.
- **Result[T] and Pipeline** — discriminated value (`Success` / `Failure` /
  `Exception`); generic top-level `Run[T]` and `Dispatch[TReq, TRes]`.
- **Auto Command Handlers** — `InsertCommandHandler` / `UpdateCommandHandler` /
  `PartialUpdateCommandHandler` / `ArchiveCommandHandler` /
  `UnarchiveCommandHandler` / `DeleteCommandHandler`. Cmd declares
  `ToEntity(ctx) T` (Insert) or `ApplyTo(ctx, T)` / `ApplyPartiallyTo(ctx, T)`
  (others) + `FromEntity(ctx, T) TResult` on every verb.
- **Auto Query Handlers** — `FindByIDQueryHandler` and `FindByParamsQueryHandler`
  with full read-side feature set: sparse responses via `?fields=`, sort
  allowlist via `?sort=`, keyset pagination via `?after=` / `?before=`, count-only
  mode via `?onlyTotal=true`, per-view `?limit=` ceiling cascade, full filter
  operator catalog (`eq`, `ne`, `in`, `nin`, `gte`/`lte`/`gt`/`lt`, `startswith`,
  `contains`, `ieq`, `iin`, `istartswith`, `icontains`, …), nested embed groups.
- **Route wrappers** — `HandleCommandWithBody{,ID}`, `HandleCommandWithID`,
  `HandleQueryWithParams`, `HandleQueryWithID`. Universal URL-segment binding
  via `path:"X"` struct tag. Strict body marker `pipeline.FullBody`. Schema
  violations → 400; domain rejections → 422; not-found → 404; recovered
  panic → 500. All emitted through the canonical `Response` envelope.
- **OpenAPI 3.1 + Swagger UI auto-generated** from the same Go types the
  HTTP wrappers consume. Reflection-driven projection of `json:` / `path:` /
  `query:` / `filter:` / `view:` / `example:` tags into the schema. Optional
  language selector dropdown in the UI. Inline favicon (SVG) and apple-touch
  data URI links to suppress browser fallback 404s.
- **AuthMiddleware** for JWT validation — JWKS (`MicahParks/keyfunc`),
  PEM-encoded public key, external introspection (RFC 7662) with optional
  in-memory positive-only cache. Four canonical modes: `prd` (JWKS), `prd-pem`,
  `prd-external`, `prd-external-cached`.
- **Authorization** in three layers — Layer 1 coarse-grained declarative gate
  (`fwopenapi.RequirePermission("users:write")`), Layer 2 fine-grained
  programmatic rules in `BuildRules`, Layer 3 cross-cutting tenant scoping.
  Boot-time validation rejects non-public routes without permission when
  authorization is enabled.
- **Audit dual-destination** — `audit_events` row inside the same `pgx.Tx`
  as the data write + outbox row (atomic source of truth) plus optional
  post-commit slog echo for observability. Per-verb event shape: `snapshot`
  (insert/delete), `delta` (update), `transition` (archive/unarchive).
  Children block carries SQL-grounded ops (`inserted` / `updated` / `archived` /
  `unarchived` / `deleted`).
- **`httpclient` package** — declarative outbound HTTP. Per-service YAML
  describes baseURL, timeout, endpoints (method/path/codecs). Typed `Call[Req,
  Resp]` generic with `http:"..."` tag binding (path / query / header / headers /
  body+codec). Codecs: JSON, XML, form-urlencoded.
  Middleware chain: correlation → logging → auth → idempotency → cache →
  retry → breaker → signing → transport. Auth providers: `none`,
  `header-static`, `bearer-static`, `basic`, `forward-bearer`,
  `oauth2-client-credentials`, `credentials-exchange` (with per-tenant
  `requestFieldsFromCtx`). Retry with backoff strategies + RFC 7231
  Retry-After. In-memory LRU+TTL cache. Per-(service,endpoint) circuit
  breaker. RFC-style idempotency key injection. HMAC-SHA256 request signing
  (AWS SigV4-lite canonical string). TLS + connection pool tuning per
  service. Streaming (download / upload / multipart / SSE). `BaseURLResolver`
  plug-in for dynamic routing. `NewFake` test harness.
- **Cross-service composition** — `UpstreamSubscription` (YAML or
  `Wiring.UpstreamSubscriptions`) materializes another service's Kafka events
  into a local Mongo collection. `fwinfra.FromMongo("collection").On("fk")`
  embeds it into local views. `UpstreamSubscriber` runs the consumer, applies
  filter allowlist, dispatches by event type, and triggers downstream
  recompose-ripple on every embedding view. Failure isolation: per-doc
  errors logged + counted + persisted to `omnicore_upstream_failures`
  (queryable list of currently stale entities). `RetryPendingFailures`
  runtime API + `omnicore-admin upstream-list-failures` inspection CLI.
- **Mongo schema evolution** — `Version(N)` mandatory per `ViewDefinition`.
  Three-mode `mongo.rebuild.autoRun`: `check` / `true` / `false` (profile-aware
  defaults). PG-backed control plane (`omnicore_mongo_views`) with hybrid
  concurrency (`pg_advisory_lock` + `status='processing'` column). Eight-branch
  drift detection at boot (DriftNone / DriftFreshInit / DriftAlienData /
  DriftMongoWiped / DriftArtifactOnly / DriftForgotToBump / DriftRebuildRequired
  / DriftDowngrade). Rebuild orchestration via `SyncEngine.ExecuteRebuild`:
  advisory lock + status transitions + cleanup + compose+upsert + orphan
  reconciliation + EndRebuild on a pinned `pgxpool.Conn`.
- **Declarative MongoDB surface** — fluent builders on `ViewDefinition`:
  `Indexes` (single / compound / unique / partial / sparse / TTL / text /
  2dsphere / hashed), `JSONSchema`, `Collation`, `Capped`, `TimeSeries`.
  `ApplyMongoSpecs` idempotent at boot. Per-view `MaxLimit(N)` override.
- **Read-side keep-by-default** — `ViewDefinition` mirrors PostgreSQL
  symmetrically by default (archived rows survive in Mongo with `deleted_at`
  populated). Opt-in `.DeleteOnArchive()` for hot-tier projections that drop
  archived rows.
- **Migrations** via `golang-migrate/migrate v4`. Framework-embedded migration
  0001 creates `outbox` + `omnicore_mongo_views`. Services start at `0002+`.
  `.down.sql` mandatory (validated at boot). Three-mode `migrations.autoRun`
  (`check` / `true` / `false`) symmetric to Mongo rebuild. Strict mode aborts
  boot in non-dev profiles on drift.
- **Declarative YAML config** — `microservice.${APP_PROFILE}.yaml` per profile.
  Substitution syntax: `${VAR:default}`, `${file:/path}`, `${vault:store#field}`
  (vault via pluggable `bootstrap.SecretResolver`). Profile names beyond
  `dev` / `prd` accepted (`prd-pem`, `prd-external-cached`, …). Strict YAML
  decoding on critical blocks rejects unknown keys at boot.
- **`bootstrap.Run(wire)`** orchestrates the whole service boot: loads YAML,
  builds singletons (`Postgres`, `Mongo`, `Translator`, `Pipeline`, `ViewReader`,
  `QueryHandler`, `HttpClient`, `OpenAPIRegistry`), runs migrations, registers
  middlewares + `/health`, mounts features, starts SyncEngine if any view was
  collected, serves HTTP until SIGINT/SIGTERM. Sibling `bootstrap.Build()` +
  `bootstrap.Serve(ctx, deps, wiring)` for custom lifecycle.
- **Built-in translations** — seven languages: PT-BR, English, Spanish,
  French, German, Italian, Dutch. Consumer-supplied catalogs compose on top
  via `Wiring.Translations`.
- **`cmd/omnicore-admin`** — operational CLI. Subcommands:
  `replay-all-as-events` (synthetic INSERTED outbox events for backfill of
  cross-service consumers), `upstream-list-failures` (read-only triage of
  the failure registry).
- **Persistence lifecycle hooks** — new `application/persistence/` types
  declaring the in-TX hook contract: `TxHandle` / `CommandTag` / `Rows` /
  `Row` (the pgx-free surface exposed to hooks), `AfterBeginHook[T]` /
  `BeforeCommitHook[T]` (function types), `AfterBeginHookProvider[T]` /
  `BeforeCommitHookProvider[T]` (detected by Auto handlers via type
  assertion; Cmds satisfy these by declaring `AfterBegin(ctx, t, tx)` /
  `BeforeCommit(ctx, t, id, tx)` methods — no prefix, mirroring Go's idiom
  for struct methods named after the event they respond to),
  `WriteOption[T]` / `WithAfterBegin[T]` / `WithBeforeCommit[T]`
  (functional options threaded into write methods — `With*` idiom for the
  free-function counterparts; each surface follows its own Go convention).
  Hooks fire INSIDE the persister's TX at position A (BEFORE any framework
  write) and position D (AFTER data + outbox + audit, BEFORE COMMIT) on
  both the flat path (`infra/executor.go`) and the aggregate path
  (`infra/aggregate_persister.go`), with symmetric semantics; granularity B
  (single firing per `repo.Method()` call). A non-nil error from either
  hook rolls the TX back; `domain.NotificationCarrier` identity reaches
  the wire envelope verbatim.
- **`persistence.Writer[T]` port** — typed write surface carrying the
  variadic options. `infra.BaseRepository[T]` implements it; Auto Command
  Handlers consume it for the `Repo` field. Keeps the AppContext-bearing
  hook types out of the domain layer so `domain.Repository[T]` stays the
  read-only port.
- **Infra adapters in `infra/tx_handle.go` and `infra/hook_dispatch.go`**
  — `pgxTxHandle` / `pgxRows` / `pgxRow` wrap `pgx.Tx` behind the
  application-layer interfaces; `AdaptWriteOptions[T]` translates typed
  `WriteOption[T]` slices into the type-erased dispatch struct the
  persister fires.
- **Observability slog line on hook error** — `persistence.hook.error`
  carrying `verb` / `hookSlot` / `entityType` / `threadId` / `error`,
  emitted as best-effort `slog.Warn` whenever a hook returns non-nil
  error.

[0.12.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.12.0
[0.11.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.11.0
[0.10.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.10.0
[0.9.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.9.0
[0.8.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.8.0
[0.7.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.7.0
[0.6.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.6.0
[0.5.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.5.0
[0.4.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.4.0
[0.3.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.3.0
