# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While in `0.x.y`, the public API may change between minor versions; breaking
changes are highlighted under **Changed**. Stable contract semantics arrive
with `1.0.0`.

## [Unreleased]

### Added

- **Persistence lifecycle hooks.** New `application/persistence/` types
  declaring the in-TX hook contract — `TxHandle` / `CommandTag` / `Rows`
  / `Row` (the pgx-free surface exposed to hooks), `AfterBeginHook[T]` /
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
  hook types out of the domain layer so `domain.Repository[T]` stays
  pure (read-only port).
- **Infra adapters in `infra/tx_handle.go` and `infra/hook_dispatch.go`**
  — `pgxTxHandle` / `pgxRows` / `pgxRow` wrap `pgx.Tx` behind the
  application-layer interfaces; `AdaptWriteOptions[T]` translates typed
  `WriteOption[T]` slices into the type-erased dispatch struct the
  persister fires.
- **Observability slog line on hook error** — `persistence.hook.error`
  carrying `verb` / `hookSlot` / `entityType` / `threadId` / `error`,
  emitted as best-effort `slog.Warn` whenever a hook returns non-nil
  error.

### Changed

- **Auto Command Handlers dispatch.** All six handlers (insert / update /
  partial_update / archive / unarchive / delete) now type-assert against
  the optional `AfterBeginHookProvider[T]` / `BeforeCommitHookProvider[T]`
  at the top of `Handle` and forward the matching method values as
  `persistence.WriteOption[T]` to the `Writer.Method(ctx, valid, opts...)`
  call.
- **`Repo` field on every Auto handler** now references
  `persistence.Writer[T]` instead of `domain.Repository[T]`. The single
  `infra.BaseRepository[T]` struct continues to satisfy both ports so
  consumer code constructs one Repository and plugs it into either slot.
- **`domain.Repository[T]` reduced to the read port** (`FindByID` +
  `New`). The write surface lives at `persistence.Writer[T]` in the
  application layer where the variadic `WriteOption[T]` (referencing
  `*configuration.AppContext`) can live without violating the domain →
  stdlib layer rule.
- **Persister method signatures.** `Postgres.Insert / Update / Archive /
  Unarchive / Delete` add a `writeHook` parameter that the BaseRepository
  populates via `AdaptWriteOptions`. `infra.BaseRepository[T]`'s write
  methods gain the matching variadic `opts ...persistence.WriteOption[T]`.

### Removed

- **`application/persistence/Orchestrator[T]`** entirely — the type was a
  pass-through wrapper around the Repository with empty pre/post slots
  that never fired inside the TX. Auto and manual handlers now call the
  `persistence.Writer[T]` port (the read+write surface implemented by
  `BaseRepository[T]`) directly, threading `*AppContext` as the first
  argument and `WriteOption[T]` variadics as trailing arguments.
- **`Orchestrator.Insert/Update/Archive/Unarchive/Delete/FindByID`'s
  pre/post callback parameters** — the old `before` / `after` slots ran
  OUTSIDE the TX and could not deliver atomic in-TX side effects. The new
  `afterBegin` / `beforeCommit` hooks replace them with the correct firing
  positions and the correct payload (entity, id, TxHandle).
- **Write methods on `domain.Repository[T]`** — `Insert` / `Update` /
  `Delete` / `Archive` / `Unarchive` move to `persistence.Writer[T]`. The
  `domain.DefaultRepository[T]` placeholder is reduced to the read port.

### Deprecated

### Fixed

### Security

## [0.1.0] - 2026-06-13

Initial public release.

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

[0.1.0]: https://github.com/ClaudioSchirmer/omnicore/releases/tag/v0.1.0
