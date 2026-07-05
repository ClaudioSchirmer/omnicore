# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While in `0.x.y`, the public API may change between minor versions; breaking
changes are highlighted under **Changed**. Stable contract semantics arrive
with `1.0.0`.

## [Unreleased]

## [0.19.0] - 2026-07-05

### Fixed

- **`EmbedMany` over an external/upstream source now composes AND recomposes
  end to end.** It was declared-but-broken on two independent levels: (1) boot
  aborted because the §8.1 join-field-index guard demanded a covering index on
  the embedding view's bare child FK column, which the Mongo spec validator
  rejected — the composer emits that field only under the embed segment (e.g.
  `items.account_id`), never bare; (2) the upstream recompose-ripple discovered
  parents only via `FindIDsByField(view, joinField, upstreamID)` — the one-to-one
  shape — so a 1:N child change never found its parent to recompose. The ripple
  now branches by cardinality: a one-to-one `Embed` keeps the reverse scan,
  while a one-to-many `EmbedMany` resolves the parent by the CHANGED child's FK
  value → the parent `_id` (read from the doc state BEFORE and AFTER the change,
  so a moved or deleted child recomposes BOTH the old and the new parent). The
  §8.1 guard no longer requires a covering index for an `EmbedMany` (its reverse
  lookup is the parent primary key, always indexed). A view may now embed the
  same upstream collection both 1:1 and 1:N. This makes real the
  `Embed`/`EmbedMany` external-source support documented for both `query.View`
  and `query.SharedBaseView`.

## [0.18.0] - 2026-07-04

### Added

- **`query.ComposedView` — read-time composition (query-time JOIN of existing
  views).** The fourth composition primitive and the only one composing at READ
  time: never materialized, never synced, never rebuilt — no collection, no
  `Version(n)`, no schema-evolution entry, no recompose ripple. A read against
  the composed name reads the PRIMARY view exactly as a direct read would (the
  primary drives rows, filters, sort, search, pagination, total and cursors)
  and enriches each item by key, in batch, from the linked legs: `Link` (1:1 →
  sub-document, explicit `null` when absent; the PRIMARY holds the FK) and
  `LinkMany` (1:N → array in the declared `OrderBy` order, capped per parent by
  the `MaxLinkManyLimit` cascade per-link → yaml `query.maxLinkManyLimit` →
  100, with silent deterministic truncation; the LEG holds the FK). Legs are
  internal registered views (`JoinView`) or locally materialized upstream
  collections (`JoinUpstream` — a leg never reads another service's live
  storage; materialize first via `UpstreamSubscription`). Segment filters —
  wire nested groups and `ToCriteria` per-leg authorization overlays
  (segment-prefixed paths, e.g. `Filter["Notes.Kind"]="public"`) — shape
  segment content only and can never select or leak primary rows; a `?sort=`
  into a segment is rejected (400); `?includeArchived` propagates to every leg
  (no-op on a leg without soft-delete); `?onlyTotal` short-circuits before any
  leg fetch; `?fields=` projects into segments; cursors bind to the composed
  listing context (segment filters included). Boot-fatal validation
  (`query.ValidateComposedViews`): unknown FK/OrderBy columns, unregistered
  primary/leg views, an external leg without its subscription, a LinkMany FK
  without a covering index on the leg view (each page parent runs one
  find-by-FK subquery — un-indexed, that is a collection scan per parent),
  segment collisions, LinkMany-only knobs on a 1:1 link, name shadowing. Registration
  via the new `bootstrap.ComposingFeature` opt-in (`ComposedViews()`);
  `bootstrap.Run` installs the composition ON the framework reader by
  mutation (`mongo.MongoViewReader.SetComposedViews`, like `SetViews` — never a
  reassignment, so handlers that captured the reader earlier, e.g. GraphQL
  fields registered inside the consumer's `Wire()`, resolve composed names
  too), so consumption is unchanged by design — the
  composed name goes wherever a view name goes (Auto and manual handlers,
  GraphQL connections, CSV/XLSX export with one branch per leg;
  `ComposedViewDefinition` satisfies the export surface, delegating the export
  ceiling to its primary). New public surface: `query.ComposedView`,
  `query.ComposedViewDefinition`, `query.Leg` (`JoinView`, `JoinUpstream`,
  `FK`, `As`, `OrderBy`, `Desc`, `MaxLinkManyLimit`), `query.ComposedLink`,
  `query.ValidateComposedViews`, `query.FrameworkDefaultMaxLinkManyLimit`,
  `mongo.MongoViewReader.SetComposedViews` (+ `mongo.NewComposedViewReader`),
  `bootstrap.ComposingFeature`, yaml
  `query.maxLinkManyLimit`.

- **`query.SharedBaseView` — the all-in-one identity projection.** A second
  read-side view kind, rooted at a SharedBase: one Mongo document per shared
  identity — the base's shared fields flat at the root, the base's native
  children nested at the root, and ONE SUB-DOCUMENT PER DECLARED ROLE
  (`SharedBaseView(personBase(), "persons").Role(UserSchema()).Role(EmployeeSchema())`,
  role count open-ended). `_id` = the base's deterministic PK (stable under
  shared-PK and separate-FK); document gate = the base's soft-delete (converged
  by the write side); an absent role is an explicit `null` segment ($set-safe);
  an archived role stores its `deleted_at` and hides on default reads
  (`?includeArchived` surfaces it); under separate-FK multiplicity the segment
  is picked active-first (else the most recently archived remnant). The
  SyncEngine subscribes to every role table's topic and recomposes the person
  document on role events — the base id resolved by identity (shared-PK), by
  source lookup (separate-FK, row alive) or from the DELETED payload's
  structural keys (the row is gone; the payload is a routing hint, never
  state) — and removes the document when the identity purges. Everything a
  regular view has applies unchanged: Version/rebuild hash (the role set
  participates — adding a role without bumping is forgot-to-bump; regular
  views' hashes are untouched), Indexes/validators, MaxLimit/MaxExportRows,
  DeleteOnArchive, filters/sort/projection through the role segments
  (`?user.userName=`, `?employee.dependents.relationship=`), GraphQL and
  CSV/XLSX (role branches). Declaration-time panics guard the shape (non-base
  root, role without `.SharedBase`, base-table mismatch, divergent
  declaration, duplicate segment); a role-less view is a boot error. Documented in
  [TableSchema → Shared-base view](#table-schema~shared-base-view).

- **Rebuild id-scan is now schema-driven.** The per-view rebuild used a
  hardcoded `SELECT id FROM <root> ORDER BY created_at`; it now reads the PK
  column from the view's root schema and falls back to the PK for the scan
  order when the root declares no CreatedAt (e.g. a SharedBase root). Behavior
  is unchanged for every existing view.

- **View index validation now recognizes own-child paths.** The
  composed-column allowlist (the `Index(...)`/`$jsonSchema` boot guard) did not
  walk a schema's own aggregate children, falsely rejecting a legitimate index
  on an own-child path (e.g. `dependents.name`). The walk now mirrors the
  composer exactly — own children included, and role segments on a
  SharedBaseView. Validation only loosens; no existing view changes behavior.

- **SharedBase — natural-key immutability is now enforced at the write layer.**
  The natural key derives the deterministic base id, so every SharedBase
  derivation (identity upsert, refcount, lifecycle convergence, CDC fan-out,
  lifecycle-payload FKs) assumes it never changes after insert — an assumption
  that was previously only a consumer convention (keep the field off update
  DTOs). A role `UPDATE` whose natural-key value diverges from the persisted
  identity is now rejected with the new `NaturalKeyImmutableNotification`
  (Semantic Validation → 422; translated in all seven catalogs): under the
  shared-PK model by pure arithmetic (the role id IS `UUIDv5(naturalKey)`, so
  the id derived from the request must equal the row id — zero queries), under
  the separate-FK model by one PK-indexed in-TX probe comparing the stored FK
  with the request-derived id (`SELECT fk = $derived FROM role WHERE pk = $id`)
  — which also covers a hand-rolled manual handler that skipped load-first,
  where an Old-snapshot comparison would be vacuous. Without the guard, a
  mutated key silently upserted a DIFFERENT identity (last-write-wins over a
  third party's shared fields) while the role row kept pointing at the old
  base. A missing role row skips the guard (the role UPDATE right after reports
  not-found exactly as before). See [TableSchema](#table-schema).

- **Relational connection-pool sizing — `relational.pool`.** A new optional
  `relational.pool` config block bounds the backend connection pool, applied
  uniformly to whichever engine is selected: `maxOpenConns` (cap on total open
  connections), `maxIdleConns` (retained idle connections), and
  `connMaxLifetimeSeconds` (recycle age). Each is tri-state — omit for the
  framework default, set explicitly to override. **`maxOpenConns` now defaults to
  `max(4, NumCPU)` for BOTH engines** (mirroring pgxpool's own default, so
  Postgres is behaviorally unchanged), which **bounds MySQL — whose `database/sql`
  pool was previously unlimited** — so a write burst applies backpressure
  (requests queue for a connection) instead of opening connections without limit
  until MySQL's `max_connections` rejects them, cascading to 500s. `maxIdleConns`
  defaults to `maxOpenConns` (keep the pool warm; avoids `database/sql`'s idle=2
  connection churn); an explicit `maxOpenConns: 0` opts back into an unlimited pool
  (Postgres cannot express unlimited and keeps its driver default). `maxIdleConns`
  is a `database/sql` knob — a no-op on Postgres, whose pgxpool governs idleness
  through `MinConns`/`MaxConnIdleTime`; `connMaxLifetimeSeconds` maps to pgxpool's
  `MaxConnLifetime` and `database/sql`'s `SetConnMaxLifetime`. New public surface:
  `core.PoolConfig`, `core.EngineConfig` (with its `Pool` field),
  `postgres.WithPool`, `bootstrap.FrameworkDefaultMaxOpenConns`.

### Changed

- **breaking** — **the embed join key is `Source.FK` — `Source.On` is
  removed.** The system speaks ONE join vocabulary, PK/FK: every relationship
  declares one FK pointing at the other side's PK, and the FK holder follows
  the multiplicity (1:1 `Embed` → the parent; child/`EmbedMany` → its own
  schema via `TableSchema.FK`; composed links per `Link`/`LinkMany`).
  Migration is a mechanical rename: `FromSchema(...).On("col")` →
  `FromSchema(...).FK("col")` — semantics, boot validation and the composer
  are unchanged. Out of scope: the integration-events receiver registration
  `reg.From(...).On(event, ...)` keeps its name (an event trigger — "when
  event X arrives" — not a join).

- **breaking** — the relational **engine constructor surface takes a
  `core.EngineConfig`** options struct instead of positional `(dsn, tracing)`
  arguments — the generalization the `EngineFactory` doc comment always
  anticipated, now that a second cross-engine knob (pool sizing) exists.
  `core.EngineFactory`, `core.NewEngine`, and `mysql.New` now take
  `(ctx, core.EngineConfig{DSN, Tracing, Pool})`; `postgres.NewPostgres` keeps its
  `(ctx, dsn, ...PostgresOption)` signature and gains a `WithPool` option. The
  canonical `bootstrap.Run` path is unaffected — this is transparent unless a
  consumer hand-wires the engine registry. Migration: a call to
  `core.NewEngine(dialect, ctx, dsn, tracing)` becomes
  `core.NewEngine(dialect, ctx, core.EngineConfig{DSN: dsn, Tracing: tracing})`.

- **Lifecycle outbox rows now carry payloads — full state on
  `ARCHIVED`/`UNARCHIVED`, structural keys on `DELETED`.** The bodyless verbs
  wrote their outbox row with a `NULL` payload, leaving CDC consumers
  (Debezium → external subscribers, including the framework's own
  `UpstreamSubscriber`) with nothing but the `aggregate_id` — in particular, an
  upstream `ARCHIVED` in keep mode (`deleteOnArchive: false`) could never land
  the archived state on the local document, despite the subscriber being
  written to expect `deleted_at` in the payload. Now:
  `ARCHIVED`/`UNARCHIVED` follow the `INSERTED`/`UPDATED` pattern — the full
  bound-field map (aggregates keep the `{root, children}` snapshot shape) plus
  the soft-delete column reflecting the verb's outcome (a Go-side UTC timestamp
  on archive — informational; the row's authoritative value is the
  database-stamped `NOW()` — and an explicit JSON `null` on unarchive) plus the
  shared-base FK when the role links its base through a separate column;
  `DELETED` carries the structural keys only (the row is gone) — the PK under
  its physical column name plus the shared-base FK — and the shared-base orphan
  purge's own `DELETED` row carries the base PK. Payload assembly never vetoes
  a write: an unresolvable natural key just omits the FK field. The local
  `SyncEngine` is unaffected (it re-reads the source by `aggregate_id`); the
  `outbox` table shape and the Debezium contract are unchanged — only the
  payload column's content grew. The base-table `UPDATED` fan-out rows (a
  SharedBase write through one role recomposing the other roles' views) still
  carry `NULL` — they are a local recompose trigger, not a consumer-facing
  snapshot. See [Lifecycle map](#lifecycle-map) and
  [Auto query handlers](#auto-query-handlers).

### Fixed

- **Graceful shutdown is now dependency-ordered end to end — every Kafka
  consumer's LeaveGroup goes out before the process exits.** The SyncEngine
  ran as a fire-and-forget goroutine outside the coordinated drain: on
  SIGTERM its deferred reader Close (which sends the consumer group's
  LeaveGroup) raced process exit, and losing that race left a ghost member
  holding the group slot — the NEXT boot's JoinGroup then blocked until the
  session timed the ghost out (tens of seconds), surfacing as "the first CDC
  event after a restart is late" (the QA-matrix flake signature). The
  UpstreamSubscriber had the partial form of the same gap (its Shutdown
  waited in-flight processing but not the supervisor's exit / reader Close).
  Now: `SyncEngine.Start` is idempotent and tracks a `done` channel that
  closes only after the loop's full deferred chain (worker queues drained →
  every in-flight compose+upsert finished → reader closed); the new
  `SyncEngine.Shutdown(drainCtx)` joins bootstrap's coordinated drain
  alongside http/integration/upstream (surfaced as `Deps.SyncEngine`,
  nil-safe); `UpstreamSubscriber.Shutdown` waits the supervisor's exit when
  started. This also closes a latent race where the relational/Mongo handles
  could close while sync workers were still composing. Coordination is by
  explicit dependency (channels/WaitGroups), never timing — the order is
  locked by unit tests and documented in the new "Graceful shutdown" part of
  the Bootstrap section.

- **Cursor pagination now composes with `ToCriteria` filter overlays.** The
  REST wrapper pre-compared the cursor's context hash against the
  PRE-`ToCriteria` wire criteria, while readers stamp cursors from the
  POST-`ToCriteria` context (identity overlays included) — so any paged query
  whose `ToCriteria` layered a security filter (tenant, owner, business gate)
  had every `?after=`/`?before=` rejected with 400 (page 1 always worked;
  GraphQL was unaffected — it never pre-validated). The wrapper now performs
  structural cursor checks only (decodability, tuple length vs sort); the
  context-hash validation is authoritative at the reader, post-`ToCriteria`,
  on every surface — a mid-navigation context change still gets the same
  canonical 400 (`SchemaViolationNotification`), never a silently wrong page.
  A developer adding a security overlay can no longer break pagination.

- **SharedBase — `/unarchive` now carries the same one-active-role veto as
  `POST`.** The framework invariant is at most ONE ACTIVE role row per identity
  per role table. It was enforced on INSERT only (an existing active role is a
  409), so under the separate-FK model — where an identity may keep archived
  remnants NEXT TO a newer active row (the active-only uniqueness contract) —
  unarchiving a remnant could produce two active roles for the same identity.
  The unarchive path now probes the role table for another ACTIVE row
  referencing the base (excluding the row being revived) and rejects with the
  same conflict notification (`EntityAlreadyAddedNotification`, 409) a POST
  raises; the whole unarchive rolls back. A no-op for the shared-PK model (the
  primary key caps the table at one row per identity) and for roles without a
  shared base. The docs now also spell out the two DDL uniqueness contracts on
  the separate-FK column — full `UNIQUE(fk)` (0:1 rows total; a remnant blocks
  re-POST) vs active-only uniqueness (Postgres partial unique index; MySQL
  unique generated column) — and that the index, not the framework's probe, is
  the arbiter when concurrent POSTs race. See [TableSchema](#table-schema).

- **Upstream composition — a `DELETED` upstream event now cascades even with an
  empty payload.** `UpstreamSubscriber.processMessage` decoded the message
  payload BEFORE dispatching by event type, so a hard delete — whose outbox row
  carries a `NULL` payload, surfaced by the CDC pipeline as a JSON scalar rather
  than an object — failed to decode into a map and returned early, silently
  skipping the cascade / anonymize / keep handling. Since those branches key off
  the aggregate id alone, `DELETED` is now dispatched before any payload decode;
  the payload decode is gated behind the payload-bearing verbs
  (`INSERTED` / `UPDATED` / `UNARCHIVED` / `ARCHIVED`). A real upstream delete now
  propagates to the local projection as documented. See
  [Service-to-service](#service-to-service).

## [0.17.0] - 2026-07-02

### Added

- **SharedBase safe orphan handling — KeepOrphan default, database-vetoable
  purge, audited destruction, engine-scoped role registry.** Destroying shared
  identity data is always a conscious, visible, physically-guarded act. (1) The `OrphanPolicy` **default is
  now `KeepOrphan`**: omission never destroys the identity; with `SoftDelete` on
  the base, hard-deleting the last referencing role row *archives* the orphaned
  base (+ its native children) — dormant, revived automatically if the same
  natural key returns — and without `SoftDelete` it simply stays.
  `DeleteWhenUnreferenced` is the explicit opt-in for physical erasure. (2) The
  orphan purge is **database-vetoable**: it runs under a savepoint
  (`SAVEPOINT omnicore_sb_purge`), and a foreign-key violation from ANY
  referencing table — including one outside the schema registry, e.g. another
  system sharing the database — cancels the purge (the base stays; the role
  delete still commits). The FK check is classified by the new
  `core.Dialect.IsForeignKeyViolation` (PG SQLSTATE 23503 / MySQL errno
  1451/1452). The same veto closes the probe-then-delete race against a
  concurrent role insert. Declare the role→base FKs as plain/`RESTRICT`
  constraints so the veto has teeth; `ON DELETE CASCADE` on a foreign table is
  that schema's explicit opt-in to the destruction. (3) An actual purge is
  **never invisible**: it emits its own in-TX audit event
  (`write.BuildSharedBasePurgeEvent` — the base table as `entityType`, the
  deterministic base id as `entityId`, kind `snapshot` with the shared fields)
  and its own `DELETED` outbox row for the base table, alongside the role's. (4)
  The refcount/lifecycle probes no longer depend on the consumer funneling every
  role through ONE `NewSharedBase` instance: `WithSchema` registers each
  shared-base role on an **engine-scoped registry keyed by the base's table**
  (`BaseEngine.RegisterSharedBaseRole`), and the probes read the union of the
  instance and engine registries — N identical `NewSharedBase` declarations
  behave exactly like one, no consumer singleton. Two *divergent* declarations
  of the same table (natural key, policy, soft-delete, fields, or children)
  panic at boot via `core.AssertSharedBaseEquivalent`. (5) A role hard-delete
  whose natural key resolves empty now fails loudly instead of converging on
  `UUIDv5("")` (the same guard the soft-write convergence already had).

- **A schema's own aggregate children auto-project into the Mongo view.** A view
  root's own `Child(...)` collections (and each child's siblings, merged FLAT) now
  nest into the composed document straight from the `TableSchema` — the read-side
  mirror of the write loader's `hydrateChildren`, joined `root.PK → child.FK`.
  Previously only a shared base's native children auto-projected; a root's own
  children reached the document only through an explicit `EmbedMany`, so a view
  declaring just its root silently dropped them. The child now projects wherever
  its schema is used (view root or embed source), under its pluralized child-type
  segment, filterable/sortable by dotted Go path. Two consequences: the former
  guard rejecting an embed source whose schema carries `Child(...)` is **removed**
  (those children now project instead of being ignored), and a redundant explicit
  embed of a child the schema already projects (same derived segment) is a new boot
  error. `EmbedMany`/`Embed` stay for composing sources the aggregate does not own
  (cross-service read models / derived projections). Auto-projected children (own
  and base-native) follow the root's soft-delete gate: archived child rows are
  hidden on default reads and surfaced only under `?includeArchived` — explicitly
  declared embeds are untouched, their lifecycle belonging upstream.

- **`TableSchema.ChildSchemas()`** — returns every declared aggregate child
  schema, ordered by table name (deterministic SQL on any engine). The aggregate
  hard-delete path uses it to clear each child table by FK explicitly; an
  out-of-package relational engine can enumerate the declared children through it.

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
  backend selected once at boot via the new mandatory `relational` block —
  `relational.dialect` (`postgres` | `mysql`, no default: the framework refuses
  to assume a backend, so an absent dialect aborts boot) plus `relational.dsn`
  (the connection string for the selected dialect). Engines self-register
  database/sql-style (`db.RegisterEngine` / `db.NewEngine`); `Deps.DB` and
  `BaseRepository.Engine` are the neutral handles, and `postgres.AsPostgres(engine)`
  recovers the concrete adapter for the few PG-bound escapes (pool, partitions).
  A complete MySQL engine ships behind the `mysql` build tag
  (`infra/db/engine/mysql`): selecting `relational.dialect: mysql` +
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

- **Relational entity specialization — Sibling and SharedBase.** A `TableSchema`
  node can now partition one flat Go entity across more than its own table,
  normalizing a DDD entity into third normal form on write and denormalizing it
  back on read. `core.NewSiblingSchema[T](table)` + `.Sibling(...)` declares a
  **1:1 shared-primary-key** secondary table holding a disjoint subset of the same
  entity's fields (a vertical split): written across owner + siblings in one TX
  (conditional materialization — skipped on INSERT, untouched on PATCH, deleted on
  a full PUT), merged back flat on read via `LEFT JOIN`, with sibling-aware
  criteria. `core.NewSharedBase(table)` + `.NaturalKey(col)` / `.OrphanPolicy(p)`
  and a role's `.SharedBase(base, fk)` declares the **party-role pattern** (N:1):
  one identity table shared by N independent role tables, deduplicated by a natural
  key whose value derives a deterministic UUIDv5 primary key (no read-back). A role
  links to the base either by a separate FK column or by sharing the base's id as its
  own primary key (`.SharedBase(base, "id")` → `role.id == base.id`, the PK enforcing
  the 0:1 with no separate FK). A shared base may own native **1:N children**
  (FK → the base id) shared by every
  role. The upsert-on-insert path is served by
  `read.NewSharedBaseRoleRepository[T]` + `handlers.SharedBaseInsertCommandHandler`
  (cold insert uses action name `"GetInsertable"`, warm reuse `"GetUpsertable"`),
  with a guard rejecting a blind insert that would duplicate an existing identity.
  An archived role is invisible to the insert probe (soft-delete is delete), so a
  POST never revives it — reactivation is the `/unarchive` verb's job.
  Lifecycle converges through the roles (archiving the last active role archives
  the base and its children; orphan convergence per `OrphanPolicy` — see the
  safe-orphan-handling entry), and a write
  through one role recomposes the Mongo views of every role of that identity
  (`fanOutSharedBase`). New public surface: `core.NewSiblingSchema`,
  `(*core.TableSchema).Sibling` / `.Siblings` / `.IsSecondary`,
  `core.NewSharedBase`, `.NaturalKey` / `.OrphanPolicy` / `.SharedBase` /
  `.IsSharedBase`, `core.OrphanPolicy` (`DeleteWhenUnreferenced` / `KeepOrphan`),
  `read.NewSharedBaseRoleRepository`, `handlers.SharedBaseInsertCommandHandler`,
  `pipeline.SharedBaseInsertCommand`, `persistence.SharedBaseInsertLoader`.
  Dialect-agnostic (Postgres + MySQL); boot guards reject illegal declarations.

- **Write-backed schema must be type-anchored — boot guard.**
  `BaseRepository.WithSchema` (and the aggregate repository, which delegates to
  it) now rejects a type-less `NewExternalSchema` as a repository root, panicking
  at construction. A schema that backs the write path must be anchored to a Go
  type: the persister reflects the entity to build the `INSERT`/`UPDATE`, and the
  read-side composer reflects it (`BoolColumns`) to restore type fidelity when it
  materializes the Mongo view — neither is possible without a struct. A type-less
  schema describes an *upstream* service's Mongo collection and is only ever a
  view *embed* source (`FromSchema`). Because the composer routes by the view
  root *table name* (the `.Root(table)` string), not by the schema's kind, a
  type-less root naming a real local table would otherwise be composed
  relationally with an empty `BoolColumns` and silently lose boolean fidelity on
  a backend without a native bool (MySQL `TINYINT(1)` → number) — this turns that
  latent divergence into a loud boot failure. Aggregate children were already
  covered (`Child(...)` rejects a type-less child at declaration), so the
  invariant *root + every child type-anchored* is now complete.

### Fixed

- **A legitimately EMPTY view no longer rebuilds on every boot.** The drift
  classifier read "registry matches + collection empty" as `DriftMongoWiped`
  and rebuilt — but a view whose aggregate has no rows yet is empty on BOTH
  sides, and it re-ran the wipe recovery (advisory lock + rebuild log, from and
  to hashes identical) on every single boot. `DetectViewDrift` now also probes
  the view's ROOT table (`SELECT 1 … LIMIT 1` through the neutral
  Querier/Dialect): collection empty + SoR empty → `DriftNone`; the rebuild
  fires only when the SoR actually has rows to mirror (a real wipe).
- **The reader hands Go-typed values — BSON datetimes become `time.Time`.**
  Consumers of the raw document (the tabular export, `RawDoc` handlers)
  received driver-typed BSON datetimes and rendered epoch milliseconds
  (`1425945600000`) where the JSON surface showed RFC3339. The
  `MongoViewReader` now normalizes BSON scalars recursively (datetime →
  `time.Time` UTC) before translation, the CSV encoder renders `time.Time` as
  RFC3339 (matching the JSON surface), and the XLSX encoder's existing
  typed-cell pass-through finally receives the `time.Time` it was written for.

### Changed

- **BREAKING: view embeds compose external data only; tabular export walks the
  full schema tree.** `Embed`/`EmbedMany` now boot-reject a write-anchored
  (`NewTableSchema[T]`) source — they compose ONLY external data (another
  service's read model via `UpstreamSubscription` / `FromSchema` over a type-less
  `NewExternalSchema`, or a derived projection). The relational cross-aggregate
  embed path (`fetchPGEmbed`) is removed: an aggregate's own data — root, siblings,
  SharedBase, and 1:N children — projects automatically from its `TableSchema`, so
  declaring it as an embed is the redundant second path this closes. One canonical
  path per case: internal data is automatic, embeds are external. Migration: a view
  embedding a local aggregate child via `EmbedMany("x", FromSchema(ChildSchema()))`
  drops the embed and declares the child with `.Child(ChildSchema())` on the root
  schema (it then auto-projects); a genuine cross-service embed already uses an
  external `NewExternalSchema` and is unaffected. Separately, tabular export
  (CSV/XLSX) now builds its column plan over the full tree — sibling and SharedBase
  columns fold in FLAT at the root level, and nested children contribute their own
  column groups — instead of the root's own fields only.

- **Aggregate child operations are decided by original + current status.** On an
  aggregate update, each child's persisted operation is now
  `domain.OperationOf(OriginalStatus, CurrentStatus)` (a new `AggregateItemOp`:
  `OpInsert` / `OpUpdate` / `OpDelete` / `OpNoop`), comparing where the item
  started against where it is now — not its current status in isolation. Two cases
  this corrects: a DB-loaded child re-added (`Constructor → Added`) is an **UPDATE**
  (audit `updated`), not an INSERT; a brand-new child added then removed before
  commit (`Added → Removed`) is **OpNoop** — no SQL and no audit children entry.
  The `GetAdded/Changed/RemovedItemsOf` helpers filter by the same rule. New public
  surface: `domain.AggregateItemOp` (`OpNoop` / `OpInsert` / `OpUpdate` /
  `OpDelete`) and `domain.OperationOf`.

- **Aggregate hard-delete cascades to children explicitly in Go.** Deleting an
  aggregate root now issues an explicit `DELETE` per declared child table (keyed
  on its FK to the root) before the root `DELETE`, all in one TX — mirroring the
  Go-owned symmetric cascade the archive/unarchive path already performs.
  Previously `deleteAggregate` issued only the root `DELETE` and relied on a
  database `ON DELETE CASCADE` declared in the consumer's migration. The
  framework now owns the cascade: it is correct and deterministic on every
  relational engine even when the FK omits `ON DELETE CASCADE` (which becomes an
  optional defense-in-depth safety-net, not a requirement), and children are
  enumerated from the schema's declared `ChildSchemas()` so every child table is
  cleared regardless of what the loaded aggregate carried. Behavior-preserving
  for services that already declared `ON DELETE CASCADE`.

- **breaking** — **a relational engine build tag is now mandatory.** Both engines
  are compiled behind build tags — Postgres under `-tags postgres`, MySQL under
  `-tags mysql` — so a binary links exactly one engine and its driver stack (pgx
  vs go-sql-driver), never both. Previously Postgres was always compiled in
  (untagged) and MySQL was the only tagged opt-in, so a `-tags mysql` build still
  carried pgx. Now `go build` / `test` / `run` and consumer services MUST pass
  `-tags postgres` or `-tags mysql`, matching `relational.dialect`. Building with
  **neither** tag registers no engine and aborts at boot (`db.NewEngine`: no engine
  registered for the dialect); building with **both** fails to compile (a guard in
  `infra/db/core`). The PG engine package (`infra/db/engine/postgres`), audit
  partition maintenance (`infra/audit/partitions.go`), the migration runner, and
  the bootstrap PG wiring now carry the `postgres` tag; the migration runner was
  restructured so each dialect's driver lives in its own `*_runner.go` behind its
  tag, and the dialect-bound boot steps moved into `bootstrap/engine_<dialect>.go`.
  No public signatures changed — `migration.New` / `NewMySQL`,
  `postgres.AsPostgres`, and the `db.RelationalEngine` seam are unchanged; what
  changed is that a build tag now selects which one is linked.

- **breaking** — the audit **model, read port, and label renderer** moved from
  `infra/audit` to a new `application/audit` package, closing a layering leak: an
  application/web consumer that reads the audit timeline (a manual handler over
  the `audit_events` table) previously had to import `infra/audit` to name the
  `Reader` port, the `AuditEvent` model, and `RenderLabels` — an
  `application → infra` (and `web → infra`) edge the dependency rules forbid.
  These now live beside the abstraction they belong to. `infra/audit` keeps the
  concrete reader, persister, echo, partitions, and `Config`, and depends on
  `application/audit` for the model + port (the correct `infra → application`
  direction). No behavior, signature, or wire change — pure package relocation.
  Migration: update imports of `AuditEvent`, `FieldChange`, `ChildEvent`,
  `Reader`, `ErrAuditNotFound`, `RenderLabels`, and `RenderLabelsInJSON` from
  `github.com/ClaudioSchirmer/omnicore/infra/audit` to
  `github.com/ClaudioSchirmer/omnicore/application/audit` (the package name stays
  `audit`; a file needing both — e.g. composition wiring `audit.NewReader` next
  to the moved `audit.Reader` — aliases one import).

- **breaking** — the relational backend config moved into a single mandatory
  `relational` block: `relational.dialect` (`postgres` | `mysql`) + `relational.dsn`.
  The former top-level `postgres.dsn` key is **removed**, and the dialect now has
  **no default** — an absent `relational.dialect` (or `relational.dsn`) aborts boot
  with `missing required config`. The framework no longer assumes Postgres. Migration:
  rename `postgres.dsn` → `relational.dsn` and add `relational.dialect: postgres`
  (the DSN is dialect-neutral, so it keeps its value). `cfg.Postgres.DSN` /
  `cfg.Database.Dialect` become `cfg.Relational.DSN` / `cfg.Relational.Dialect`.

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
