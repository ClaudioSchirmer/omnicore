# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While in `0.x.y`, the public API may change between minor versions; breaking
changes are highlighted under **Changed**. Stable contract semantics arrive
with `1.0.0`.

## [Unreleased]

### Added

- **`TableSchema` — the single, mandatory, explicit Go-field↔physical-column
  map**, superseding the convention/inference model and the `RepoConfig` schema
  map from 0.11.0. Built with `NewTableSchema[T](table)` (type-anchored —
  validates each field against `T` at construction; a `Field` naming a missing
  or unexported field panics at boot) or `NewExternalSchema(table)` (type-less,
  for `FromMongo` upstream sources). Chainable builder: `PK(go, col)`,
  `FK(col)` (child), `Field(go, col)`, `SoftDelete(col)`, `CreatedAt(col)`,
  `UpdatedAt(col)`, `Child(*TableSchema)`. There is no name inference: every
  persisted field is declared, and an undeclared exported field is runtime-only
  by construction (never persisted, scanned, or audited).
- **`BaseAggregateRepository.WithSchema(*TableSchema)`** threads the one schema
  into the write binding AND the read loader (write SQL + criteria + auto-scan).
  Aggregate children come from the schema's `Child(...)` declarations.
- **The same `TableSchema` drives the Mongo read side.** `ViewDefinition.Schema(ts)`
  attaches the root map; `Source.Schema(ts)` + `Source.As(goSegment)` attach the
  embed's map and parent-side Go segment. The composer writes physical columns;
  the `MongoViewReader` translates each leaf back to its Go field name (and the
  embed doc field to its Go segment) using these schemas, so the typed Response
  speaks Go names with only `json:` tags.
- **Three managed columns by presence, not a bool** — calling
  `SoftDelete/CreatedAt/UpdatedAt(col)` enables; omitting disables. `created_at`
  and `updated_at` are actively stamped `NOW()` on write (no reliance on a DB
  default); on the read path they are readable under fixed logical Go names
  `CreatedAt`/`UpdatedAt`/`DeletedAt`.
- **`Source.SchemaDef() *TableSchema`** — exported accessor returning an embed
  source's schema (nil when declared without `.Schema(...)`); symmetric with
  `ViewDefinition.SchemaDef()`.
- **Schema-less `FromMongo` embed advisory** — `bootstrap.Run` emits a boot
  `slog.Warn` (`view.embed.schemaless`, naming the view + collection) for every
  `FromMongo` embed declared without a `.Schema(...)`, at any nesting depth and
  independent of whether any subscription is declared. Such an embed degrades the
  reader to identity pass-through (wire speaks the upstream's physical column
  names; soft-delete gate falls back to `deleted_at`). A warning, not an abort —
  pass-through is a legitimate mode for `RawDoc` projectors.

### Removed

- **`RepoConfig`, `SourceMap`, `ManagedColumn`** — replaced by `TableSchema`.
- **`WithChild[V]` / `WithChildAutoScan` / `WithConfig`** — children are declared
  on the schema via `Child(...)` and threaded via `WithSchema`.
- **`ViewOf[*T]()`** — views are declared explicitly via
  `View(name).Version(n).Root(table).Schema(ts).EmbedMany(field, From(...).On(fk).As(go).Schema(childTs))`.
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

- **Persistence names are no longer derived from Go identifiers.** Tables,
  columns, and child FKs are declared in the `TableSchema`; a typo is a boot
  panic, not a silent miss. **Breaking** — every consumer Repository and view
  must declare a `TableSchema` and call `WithSchema`.
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
  is map-blind, so a column rename never disturbs the timeline (unchanged from
  0.11.0).

## [0.11.0] - 2026-06-19

### Added

- **PG schema mapping — `RepoConfig` is now the single, complete per-Repository
  schema map.** Root mapping is declared flat on `RepoConfig`
  (`Table` / `PK` / `Fields` / `SoftDelete` / `UpdatedAt` / `CreatedAt`); each
  aggregate child declares its own `SourceMap` under `Children`, keyed by the Go
  child type name. New exported types: `SourceMap`, `ManagedColumn{Disabled, Column}`.
  Everything in the framework speaks the Go field name (PascalCase); the map is
  the only place a physical column/table name appears, and a column rename
  propagates to the Mongo read side automatically (composer `SELECT *`).
- **`BaseAggregateRepository.WithSchema(cfg RepoConfig)`** — declares the map
  once and threads it into both the write binding (`BaseRepository.Config`) and
  the read loader (`Loader.WithConfig`), eliminating the two-source split. Runs
  boot checks at construction: column bijection over each source's full column
  set (mapped fields + PK + enabled managed columns), every `Fields` key names a
  real persistable field, and `Modes()` ⟺ `SoftDelete`.
- **Managed `created_at` / `updated_at` are now actively stamped on INSERT**
  (`created_at = NOW()`, `updated_at = NOW()`). The framework no longer relies on
  a DB `DEFAULT NOW()` it does not own; each managed column is renamable and can
  be disabled via `ManagedColumn`.

### Fixed

- **The read/scan-back path now honors column renames (latent bug).** A field
  mapped to a non-convention column (e.g. `Email` → `mail`) was written and
  filtered correctly but scanned back by the convention column, so the row could
  not be read through `FindByID`/`FindOne`. The SELECT list and the scanner now
  share a per-Repository `mappedColumn → fieldIndex` resolver
  (`sourceColumnPlan`); root SELECT/`RETURNING`/`WHERE` and the criteria PK seed
  use the configured PK column.

### Changed

- **Audit emits the faithful domain field name.** `snapshot` keys and
  `changes[].field` now carry the raw Go field name (`Email`, `ZipCode`) instead
  of the snake_case column; field labels resolve by Go field name. Audit is
  map-blind — it does not consume `RepoConfig`, so a column rename never disturbs
  the timeline. **Breaking for consumers keying audit on the old snake_case
  field names** (e.g. ELK/BI pipelines).
- **`RepoConfig.FieldOverrides` / `ChildTableOverrides` / `ChildFKOverrides` are
  replaced** by `RepoConfig.Fields` (root) and per-child `SourceMap`
  (`Table` / `FK` / `Fields`) under `RepoConfig.Children`.
- **`scopeGate` / `childScopeFilter` / `newFieldResolver` (internal)** take the
  source's `SourceMap` so the soft-delete column and PK are resolved from the
  map; archive/unarchive/delete SQL key on the configured PK and soft-delete
  columns.

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

- **`omnicore/criteria/` package â backend-neutral query DSL for loading live
  domain aggregates from PostgreSQL by an arbitrary criterion.** A sealed
  expression tree (`Expr`) with a fluent builder â `Eq/Ne/In/Nin/Gt/Gte/Lt/Lte/
  Like/ILike/IsNull/NotNull`, `And/Or/Not`, sugar `Contains/StartsWith/EndsWith/
  Between` â wrapped in a `Query` carrying `WHERE` + `OrderBy`/`OrderByDesc` +
  `Limit` + an archived `Scope` (`Active`/`IncludeArchived`/`OnlyArchived`).
  `criteria.ByID(id)` is the primary-key shortcut. Pure (stdlib only, zero IO);
  the SQL translation lives behind the `Visitor` seam so other backends can be
  added without touching the tree. Consumed only inside `infra` repository
  implementations â `domain` and `application` keep business-vocabulary
  repository interfaces and never import `criteria`.
- **`AggregateLoader[T].FindOne(ctx, *criteria.Query)` and `FindAll(ctx,
  *criteria.Query)`** â load one (or `RecordNotFound`; error on >1) or many
  live aggregates (root + children) matching a criterion. `FindAll` batches
  children with `WHERE fk IN (...)` (one query per child type, not per root).
  Both honor the archived scope on root and children. Promoted on
  `BaseAggregateRepository[T]`. The single SQL-building path: by-id loads
  (`FindByID`/`FindArchivedByID`) and any alternate-key lookup all route through
  the engine.
- **Pure domain repository ports â `domain.Reader[T]`, `domain.Writer`,
  `domain.Repository[T]`.** `Reader[T]` = `FindByID` + `New`; `Writer` =
  `Insert/Update/Archive/Unarchive/Delete` taking only a ValidEntity
  (non-generic, no ctx); `Repository[T]` = `Reader[T] + Writer`. Pure (stdlib +
  google/uuid only) â what a consumer names for a read+write repository
  interface declared in the domain layer, with zero application import.
- **`persistence.ScopedRepository[T]` + `BaseRepository[T].Scope(ctx, opts...)
  domain.Writer`.** The write binding: reads stay direct on the handle
  (`domain.Reader[T]`), writes go through `Scope`, which binds the request ctx
  (cancellation â pgx, actor â audit) and the in-TX lifecycle hooks and returns
  a pure `domain.Writer`. The domain port never pronounces the ctx.
- **`persistence.RequestContext`** â request-scoped interface (`context.Context`
  + `ID()`/`ActorSubject()`/`ActorIssuer()`/`ActorClaims()`) the persistence and
  audit pipelines consume, satisfied by `*configuration.AppContext`. Relocated
  from the deleted `domain.Context`; `persistence.AnonymousActor` moved likewise.

### Changed

- **The write path is now Scope-bound.** Auto Command Handlers and the manual
  path call `repo.Scope(ctx, opts...).Insert(valid)` (etc.) instead of
  `repo.Insert(ctx, valid, opts...)`. Handlers depend on
  `persistence.ScopedRepository[T]` instead of the removed `persistence.Writer[T]`.
  Audit, cancellation, and the in-TX hook semantics are unchanged â the ctx +
  actor are captured by the bound writer internally.

### Removed

- **`domain.Context`** â deleted. The domain layer no longer declares a
  request-scoped context type (it carried `context.Context` + actor/claims, none
  of which are domain concepts). Relocated to `persistence.RequestContext`; the
  domain repository ports are now pure (no ctx in any signature).
- **`persistence.Writer[T]`** â replaced by `persistence.ScopedRepository[T]`
  (read port + `Scope`) on the handler side and the pure `domain.Writer` on the
  port side. Write call sites change from `repo.Insert(ctx, valid, opts...)` to
  `repo.Scope(ctx, opts...).Insert(valid)`.
- **`AggregateLoader[T].Load` / `LoadIncludingArchived`** â replaced by
  `FindOne(criteria.ByID(id))` / `FindOne(criteria.ByID(id).OnlyArchived())`.
  Small `infra`-API removal; the domain/application repository read contract
  (`Reader[T].FindByID`, `ArchivedFinder[T].FindArchivedByID`) is unchanged. A
  manual `WithRootScanner` used with `FindOne`/`FindAll` must now populate the
  entity id (scan it + `SetID`) â the framework no longer injects it on the
  criteria path (there is no input id).

## [0.8.0] - 2026-06-16

### Added

- **`omnicore/infra/cache/` package â generic byte-level key-value cache
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
  - `cache.store: memory | redis | custom` â backend selection. Default
    `memory` (in-process LRU+TTL) covers single-replica services. `redis`
    ships with the framework's `go-redis/v9`-backed adapter (lazy
    connection, JSON-encoded entries debug-able via `redis-cli GET`,
    per-op timeout governed by `timeoutMs`, namespace via `keyPrefix`).
    `custom` requires `bootstrap.Wiring.Cache` to be set.
  - `cache.shared:` sub-block declares a SECOND cache exposed on
    `Deps.SharedCache`. nil unless declared. `cache.shared.store:
    memory` is REJECTED at boot â an in-process LRU cannot honor cross-
    service reads. Supports `redis` and `custom` only.
  - `cache.redis.failMode: open | closed`. `open` (default) swallows
    transport errors + emits `slog.Warn "cache.redis.transport.error"`
    + returns miss (Get) / nil (Set/Delete) so the call proceeds to
    upstream. `closed` propagates the error.
  - `cache.maxEntries` caps the in-process LRU (only relevant for
    `store: memory`). 0 falls back to the framework default 10k.
- **`bootstrap.Deps.Cache` and `bootstrap.Deps.SharedCache`** expose
  the resolved instances to every Feature. The httpclient cache
  middleware consumes `Deps.Cache` automatically â operators no longer
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
  immediately) â the explicit skip avoids polluting the cache with
  entries that would only be served once.

### Removed

- **`httpclient.Cache` interface** â replaced by the framework's
  top-level `cache.Cache`. Consumers who implemented custom backends
  via `httpclient.WithCacheStore` migrate to `cache.Cache` (same shape,
  with `Delete` added).
- **`httpclient.CacheEntry`** is now an internal type
  (`httpclient/cache_middleware.go::cacheEntry`). The previous public
  exposure was a draft contract from the in-flight feature branch and
  never shipped in a tagged release.
- **`httpclient.WithCacheStore(Cache) Option`** â replaced by
  `httpclient.WithCache(cache.Cache) Option`.
- **`httpClient.defaults.cache.store` and `httpClient.defaults.cache.redis`
  YAML keys** â moved to the top-level `cache:` block.

## [0.7.0] - 2026-06-16

### Added

- **Field labels â `label:"<catalogKey>"` struct tag on entity / value-object
  fields.** Resolves through the same `Translator.Render` already used for
  notification messages and produces translated human-readable identifiers
  alongside the technical `field` / column name on every reactive output:
  - **`MessageDTO.FieldLabel`** (new, `json:"fieldLabel,omitempty"`) carries
    the rendered string in the actor's locale next to `FieldName`. Channels
    without a frontend (e-mail, SMS, push) read it directly so the recipient
    sees "CEP Ã© invÃ¡lido" instead of "addresses[0].zipCode Ã© invÃ¡lido".
  - **`ErrorMessage.FieldLabel`** (new, `json:"fieldLabel,omitempty"`) on the
    web envelope â `ResponseFromContextDTOs` + `ResponseFromContexts` both
    propagate the value through so the wire HTTP response carries the
    rendered label as published by the consumer.
  - **`FieldChange.FieldLabelKey`** (new, `json:"fieldLabelKey,omitempty"`)
    carries the catalog key on every audit row (root delta + child cascade).
    Render-at-read fits compliance flows where the auditor reads in a locale
    that may differ from the actor's; the key persists across catalog
    evolution.
  - **`FieldChange.FieldLabel`** (new, `json:"fieldLabel,omitempty"`) is the
    read-time slot the audit renderer populates after consuming
    `FieldLabelKey`. Mutually exclusive with `FieldLabelKey` in practice â
    the in-flight write carries the key; the rendered read carries the text.
- **`audit.RenderLabels(ev, t, lang)` + `audit.RenderLabelsInJSON(doc, t, lang)`.**
  In-place audit read renderers. Walk every `FieldChange` (root + child
  cascade), pop `FieldLabelKey`, and write the translated string to
  `FieldLabel` via `Translator.Render(lang, key, nil)`. The typed
  variant operates on `*audit.AuditEvent` for in-process Go readers; the
  JSON variant operates on `map[string]any` for BI / SQL tools that parse
  the `audit_events.jsonb` payload directly. Catalog miss inherits the
  existing `Translator.Render` fallback (raw key + `slog.Warn` once per
  `(lang, key)`). Snapshot blocks are intentionally not touched â they
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
  PascalCase â camelCase emission side by side. Behavior unchanged â the
  docs were lagging.

### Changed

- **`NewRules` signature gained `entityType reflect.Type` (3rd arg).** All
  framework call sites updated (`entity_base.go` Ã 5, `aggregate_root.go` Ã 1,
  `runAggregateValidations` Ã 2). Consumer code does NOT call `NewRules`
  directly; the change is internal. Tests that exercise Rules in isolation
  pass `nil` to opt out of label resolution.

## [0.6.0] - 2026-06-16

### Added

- **Cross-service integration events â canonical write-side async path.**
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
- **`IntegrationFeature` interface** under `omnicore/bootstrap` â opt-in
  via type assertion (mirror of `ReadableFeature`). Bootstrap calls
  `MountReceivers(reg, deps)` on every feature implementing it during
  Phase Receivers, between Phase HTTP and ConsumerPool start.
- **`Deps.IntegrationRegistry` + `Deps.UpstreamSubscribers`.**
  Consumer admin surfaces walk both slices to expose retry
  endpoints. The upstream subscriber slice was previously documented
  as "not surfaced on Deps" â gap closed in the same round as the
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
  omitted â events emitted inside a receiver handler automatically
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
  drainTimeoutSeconds` budget â drains that exceed surface as
  `slog.Warn` lines so the operator knows what did not finish.
- **Documentation: outbound HTTP error handling pattern.** New `Outbound error
  handling` subsection under `httpclient â outbound HTTP` in `DOCS.html`
  documents the canonical translation path for `*HttpError` returned by
  `httpclient.Call`: handlers wrap the failure with a service-defined
  notification via `exception.SingleNotificationError` /
  `exception.FieldErrorWithCause` (or `infra.FieldErrorWithCause` when the
  mapping lives inside the adapter). Untranslated failures keep falling through
  `pipeline.Run` to the canonical 500 `InternalServerErrorNotification`
  envelope â by design, since only the consumer knows the domain semantic of an
  upstream error. No runtime change; clarifies an existing surface and
  discourages per-service `respondWithError` helpers that duplicate the
  framework's canonical envelope.

## [0.5.0] - 2026-06-15

### Changed

- **Upgrade Fiber v2 â v3.** Breaking change throughout the HTTP layer:
  - Handler signature now uses `fiber.Ctx` (interface), no pointer. Every
    `func(c *fiber.Ctx) error` in the public surface becomes
    `func(c fiber.Ctx) error`.
  - `c.BodyParser(&req)` / `c.QueryParser(&req)` replaced by the unified Bind
    API: `c.Bind().Body(&req)` / `c.Bind().Query(&req)`.
  - `c.UserContext()` removed upstream â `fiber.Ctx` now implements
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

- **`web.CORS(origins ...string)`** â removed. Services and bootstrap call
  `cors.New(cors.Config{AllowOrigins: []string{...}, ...})` directly, the
  Fiber v3 idiomatic pattern.
- **`web.Logger() fiber.Handler`** â removed. Bootstrap calls
  `logger.New()` directly.
- **`web.RateLimit(max int) fiber.Handler`** â removed. Services call
  `limiter.New(limiter.Config{Max: max})` directly.

  Rationale: these three wrappers were thin delegations over Fiber middleware
  with no omnicore-specific value. Removing them aligns the framework with
  the Fiber v3 documented surface and reduces API drift. `web.Recover()` is
  kept because it carries omnicore-specific logic (slog-integrated
  `StackTraceHandler` that emits structured panic logs).

## [0.4.0] - 2026-06-14

### Added

- **Parameterized notifications** â translation messages can carry runtime
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
- `infra.UnwrapPgxTx(persistence.TxHandle) pgx.Tx` â the single authorized
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
  `Exec` / `Query` / `QueryRow` surface â the new shape makes "application
  is SQL-free" a type-system guarantee instead of a documentation rule.

### Deprecated

### Removed

- `persistence.TxHandle.Exec` / `Query` / `QueryRow` methods â replaced by
  the sealed-marker shape above. Hooks that need an in-TX side effect now
  declare a port in `application/` (or `domain/`) and implement the SQL in
  an `infra/` adapter that calls `UnwrapPgxTx`.
- `persistence.CommandTag`, `persistence.Rows`, `persistence.Row` types â
  removed alongside the SQL methods they served. Adapters that need
  command-tag / iterator / single-row semantics consume the corresponding
  `pgx` / `pgconn` types directly through the unwrapped `pgx.Tx`.

### Fixed

### Security

## [0.3.0] - 2026-06-13

Initial public release of the rewritten history. Skips `v0.1.0` /
`v0.2.0` â both versions are frozen on `proxy.golang.org` pointing
to content from a prior repo that no longer exists.

### Added

- **DDD layering with enforced boundaries** â `domain` (pure rules, zero I/O),
  `application` (pipeline, orchestrator, queries), `infra` (Postgres + outbox,
  Mongo, Kafka SyncEngine, Composer, Audit), `web` (Fiber transport). Cross-layer
  error contract via `domain.NotificationCarrier`.
- **Sealed `ValidEntity` types** (`Insertable` / `Updatable` / `Archivable` /
  `Deletable` / `Unarchivable` / `Batch`) produced only by `domain` package
  functions; compile-time enforcement via private `entity()` method.
- **`AggregateRoot` with universal symmetric cascade** â root archive â children
  archive, root unarchive â children unarchive, root delete â FK ON DELETE
  CASCADE. Top-level primitives `AddAggregateChild` / `ChangeAggregateChild` /
  `RemoveAggregateChild` / `ReplaceAggregateChildrenOf` with declarative
  boundary via `AggregateChildren() []AggregateValueObject`.
- **Old-state snapshot** captured automatically by `Get*` functions; exposed
  via `Entity.Old()` and the typed `domain.Old[T](e) T` wrapper. Consumed by
  `BuildRules` for transition-aware invariants and by the auditor for change
  computation.
- **Rules DSL** (`r.IfInsert` / `r.IfUpdate` / `r.IfDelete` / `r.IfInsertOrUpdate` /
  `r.IfDisplay`) â mode-scoped validation closures. Archive/Unarchive fire
  `IfUpdate` with a distinct `actionName` for state-transition branching.
- **Notification system** with typed structs (translation key = struct name),
  scoped `NotificationContext`, path-aware field names for nested aggregates,
  manual override via `ChangeFieldName`. Wire format carries `NotificationKey`
  + `Semantic` so clients can branch UI without parsing status codes.
- **Result[T] and Pipeline** â discriminated value (`Success` / `Failure` /
  `Exception`); generic top-level `Run[T]` and `Dispatch[TReq, TRes]`.
- **Auto Command Handlers** â `InsertCommandHandler` / `UpdateCommandHandler` /
  `PartialUpdateCommandHandler` / `ArchiveCommandHandler` /
  `UnarchiveCommandHandler` / `DeleteCommandHandler`. Cmd declares
  `ToEntity(ctx) T` (Insert) or `ApplyTo(ctx, T)` / `ApplyPartiallyTo(ctx, T)`
  (others) + `FromEntity(ctx, T) TResult` on every verb.
- **Auto Query Handlers** â `FindByIDQueryHandler` and `FindByParamsQueryHandler`
  with full read-side feature set: sparse responses via `?fields=`, sort
  allowlist via `?sort=`, keyset pagination via `?after=` / `?before=`, count-only
  mode via `?onlyTotal=true`, per-view `?limit=` ceiling cascade, full filter
  operator catalog (`eq`, `ne`, `in`, `nin`, `gte`/`lte`/`gt`/`lt`, `startswith`,
  `contains`, `ieq`, `iin`, `istartswith`, `icontains`, â¦), nested embed groups.
- **Route wrappers** â `HandleCommandWithBody{,ID}`, `HandleCommandWithID`,
  `HandleQueryWithParams`, `HandleQueryWithID`. Universal URL-segment binding
  via `path:"X"` struct tag. Strict body marker `pipeline.FullBody`. Schema
  violations â 400; domain rejections â 422; not-found â 404; recovered
  panic â 500. All emitted through the canonical `Response` envelope.
- **OpenAPI 3.1 + Swagger UI auto-generated** from the same Go types the
  HTTP wrappers consume. Reflection-driven projection of `json:` / `path:` /
  `query:` / `filter:` / `view:` / `example:` tags into the schema. Optional
  language selector dropdown in the UI. Inline favicon (SVG) and apple-touch
  data URI links to suppress browser fallback 404s.
- **AuthMiddleware** for JWT validation â JWKS (`MicahParks/keyfunc`),
  PEM-encoded public key, external introspection (RFC 7662) with optional
  in-memory positive-only cache. Four canonical modes: `prd` (JWKS), `prd-pem`,
  `prd-external`, `prd-external-cached`.
- **Authorization** in three layers â Layer 1 coarse-grained declarative gate
  (`fwopenapi.RequirePermission("users:write")`), Layer 2 fine-grained
  programmatic rules in `BuildRules`, Layer 3 cross-cutting tenant scoping.
  Boot-time validation rejects non-public routes without permission when
  authorization is enabled.
- **Audit dual-destination** â `audit_events` row inside the same `pgx.Tx`
  as the data write + outbox row (atomic source of truth) plus optional
  post-commit slog echo for observability. Per-verb event shape: `snapshot`
  (insert/delete), `delta` (update), `transition` (archive/unarchive).
  Children block carries SQL-grounded ops (`inserted` / `updated` / `archived` /
  `unarchived` / `deleted`).
- **`httpclient` package** â declarative outbound HTTP. Per-service YAML
  describes baseURL, timeout, endpoints (method/path/codecs). Typed `Call[Req,
  Resp]` generic with `http:"..."` tag binding (path / query / header / headers /
  body+codec). Codecs: JSON, XML, form-urlencoded.
  Middleware chain: correlation â logging â auth â idempotency â cache â
  retry â breaker â signing â transport. Auth providers: `none`,
  `header-static`, `bearer-static`, `basic`, `forward-bearer`,
  `oauth2-client-credentials`, `credentials-exchange` (with per-tenant
  `requestFieldsFromCtx`). Retry with backoff strategies + RFC 7231
  Retry-After. In-memory LRU+TTL cache. Per-(service,endpoint) circuit
  breaker. RFC-style idempotency key injection. HMAC-SHA256 request signing
  (AWS SigV4-lite canonical string). TLS + connection pool tuning per
  service. Streaming (download / upload / multipart / SSE). `BaseURLResolver`
  plug-in for dynamic routing. `NewFake` test harness.
- **Cross-service composition** â `UpstreamSubscription` (YAML or
  `Wiring.UpstreamSubscriptions`) materializes another service's Kafka events
  into a local Mongo collection. `fwinfra.FromMongo("collection").On("fk")`
  embeds it into local views. `UpstreamSubscriber` runs the consumer, applies
  filter allowlist, dispatches by event type, and triggers downstream
  recompose-ripple on every embedding view. Failure isolation: per-doc
  errors logged + counted + persisted to `omnicore_upstream_failures`
  (queryable list of currently stale entities). `RetryPendingFailures`
  runtime API + `omnicore-admin upstream-list-failures` inspection CLI.
- **Mongo schema evolution** â `Version(N)` mandatory per `ViewDefinition`.
  Three-mode `mongo.rebuild.autoRun`: `check` / `true` / `false` (profile-aware
  defaults). PG-backed control plane (`omnicore_mongo_views`) with hybrid
  concurrency (`pg_advisory_lock` + `status='processing'` column). Eight-branch
  drift detection at boot (DriftNone / DriftFreshInit / DriftAlienData /
  DriftMongoWiped / DriftArtifactOnly / DriftForgotToBump / DriftRebuildRequired
  / DriftDowngrade). Rebuild orchestration via `SyncEngine.ExecuteRebuild`:
  advisory lock + status transitions + cleanup + compose+upsert + orphan
  reconciliation + EndRebuild on a pinned `pgxpool.Conn`.
- **Declarative MongoDB surface** â fluent builders on `ViewDefinition`:
  `Indexes` (single / compound / unique / partial / sparse / TTL / text /
  2dsphere / hashed), `JSONSchema`, `Collation`, `Capped`, `TimeSeries`.
  `ApplyMongoSpecs` idempotent at boot. Per-view `MaxLimit(N)` override.
- **Read-side keep-by-default** â `ViewDefinition` mirrors PostgreSQL
  symmetrically by default (archived rows survive in Mongo with `deleted_at`
  populated). Opt-in `.DeleteOnArchive()` for hot-tier projections that drop
  archived rows.
- **Migrations** via `golang-migrate/migrate v4`. Framework-embedded migration
  0001 creates `outbox` + `omnicore_mongo_views`. Services start at `0002+`.
  `.down.sql` mandatory (validated at boot). Three-mode `migrations.autoRun`
  (`check` / `true` / `false`) symmetric to Mongo rebuild. Strict mode aborts
  boot in non-dev profiles on drift.
- **Declarative YAML config** â `microservice.${APP_PROFILE}.yaml` per profile.
  Substitution syntax: `${VAR:default}`, `${file:/path}`, `${vault:store#field}`
  (vault via pluggable `bootstrap.SecretResolver`). Profile names beyond
  `dev` / `prd` accepted (`prd-pem`, `prd-external-cached`, â¦). Strict YAML
  decoding on critical blocks rejects unknown keys at boot.
- **`bootstrap.Run(wire)`** orchestrates the whole service boot: loads YAML,
  builds singletons (`Postgres`, `Mongo`, `Translator`, `Pipeline`, `ViewReader`,
  `QueryHandler`, `HttpClient`, `OpenAPIRegistry`), runs migrations, registers
  middlewares + `/health`, mounts features, starts SyncEngine if any view was
  collected, serves HTTP until SIGINT/SIGTERM. Sibling `bootstrap.Build()` +
  `bootstrap.Serve(ctx, deps, wiring)` for custom lifecycle.
- **Built-in translations** â seven languages: PT-BR, English, Spanish,
  French, German, Italian, Dutch. Consumer-supplied catalogs compose on top
  via `Wiring.Translations`.
- **`cmd/omnicore-admin`** â operational CLI. Subcommands:
  `replay-all-as-events` (synthetic INSERTED outbox events for backfill of
  cross-service consumers), `upstream-list-failures` (read-only triage of
  the failure registry).
- **Persistence lifecycle hooks** â new `application/persistence/` types
  declaring the in-TX hook contract: `TxHandle` / `CommandTag` / `Rows` /
  `Row` (the pgx-free surface exposed to hooks), `AfterBeginHook[T]` /
  `BeforeCommitHook[T]` (function types), `AfterBeginHookProvider[T]` /
  `BeforeCommitHookProvider[T]` (detected by Auto handlers via type
  assertion; Cmds satisfy these by declaring `AfterBegin(ctx, t, tx)` /
  `BeforeCommit(ctx, t, id, tx)` methods â no prefix, mirroring Go's idiom
  for struct methods named after the event they respond to),
  `WriteOption[T]` / `WithAfterBegin[T]` / `WithBeforeCommit[T]`
  (functional options threaded into write methods â `With*` idiom for the
  free-function counterparts; each surface follows its own Go convention).
  Hooks fire INSIDE the persister's TX at position A (BEFORE any framework
  write) and position D (AFTER data + outbox + audit, BEFORE COMMIT) on
  both the flat path (`infra/executor.go`) and the aggregate path
  (`infra/aggregate_persister.go`), with symmetric semantics; granularity B
  (single firing per `repo.Method()` call). A non-nil error from either
  hook rolls the TX back; `domain.NotificationCarrier` identity reaches
  the wire envelope verbatim.
- **`persistence.Writer[T]` port** â typed write surface carrying the
  variadic options. `infra.BaseRepository[T]` implements it; Auto Command
  Handlers consume it for the `Repo` field. Keeps the AppContext-bearing
  hook types out of the domain layer so `domain.Repository[T]` stays the
  read-only port.
- **Infra adapters in `infra/tx_handle.go` and `infra/hook_dispatch.go`**
  â `pgxTxHandle` / `pgxRows` / `pgxRow` wrap `pgx.Tx` behind the
  application-layer interfaces; `AdaptWriteOptions[T]` translates typed
  `WriteOption[T]` slices into the type-erased dispatch struct the
  persister fires.
- **Observability slog line on hook error** â `persistence.hook.error`
  carrying `verb` / `hookSlot` / `entityType` / `threadId` / `error`,
  emitted as best-effort `slog.Warn` whenever a hook returns non-nil
  error.

[0.10.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.10.0
[0.9.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.9.0
[0.8.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.8.0
[0.7.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.7.0
[0.6.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.6.0
[0.5.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.5.0
[0.4.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.4.0
[0.3.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.3.0
