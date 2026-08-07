# OmniCore

> **CRITICAL RULES — read before touching anything in `omnicore/`.**
>
> 1. **Every change to this module needs explicit maintainer approval first.** Describe the change + motivation + impact, wait for approval, then edit. No exception for "obvious", "small", "cosmetic", or "a consumer needs it". If a consumer is urgent, work around it in the consumer — never patch the framework "for now".
> 2. **Never remove functionality without explicit confirmation.** "Appears redundant" / "new API covers it" / "looks like dead code" is not authorization. When a new feature impacts an old one, stop, describe the overlap, and offer via `AskUserQuestion`: *Remove / Deprecate / Keep both / Adapt to delegate*. Applies to any public surface (functions, endpoints, yaml fields, flags, defaults, struct fields, options).
> 3. **Canonical and manual routes stay feature-equivalent.** Every feature must work the same through the Auto/convention path AND the hand-written `pipeline.Handler` + explicit wiring path — same envelope, pipeline, audit/outbox/notification semantics, schema enforcement. Manual is the escape hatch for *wiring control*, not a poorer tier. If a feature can only fit one side, stop and offer options before coding one-sided.
> 4. **English everywhere** — code, comments, docs, identifiers, tests, logs, error strings. The only non-English text allowed is the seven translation catalogs in `application/translation/` (`ptbr`/`eng`/`esp`/`fra`/`deu`/`ita`/`nld`); the surrounding Go stays English. Chat may be any language.
> 5. **Verify, never guess.** Every claim about the code (signatures, behavior, defaults, existence) must be backed by a `Read`/`grep`, including while planning. A plan built on a guessed contract has no value. Say "I'm guessing — let me verify" and verify, rather than present inference as fact.
> 6. **The AI never writes git history.** No `commit`/`push`/`tag`/PR/release. At task start, get onto a coherent branch (`feature|fix|docs|refactor/<kebab-outcome>`): off `main` via `git checkout -b`, or rename an in-flight unmerged branch via `git branch -m` (never re-stack). Apply edits, then deliver one English commit-message suggestion as chat text. `git checkout -b` / `git branch -m` are the only git-writes allowed.
> 7. **This file is a lean orientation + doc index, not a surface dump.** It carries the working rules, the architecture/dependency boundaries, the cross-cutting invariants, and the Documentation Map. It does NOT restate the public surface (signatures, field lists, edge cases, examples) — that lives in `docs/content/sections/`. No file paths, function signatures, struct field dumps, or code samples; point to the mapped section instead. No "Phase N" labels, no changelog/dated entries, no "was X now Y", no references to removed APIs, no absence/TODO statements ("not yet", "future X", "currently only"). When this file contradicts the code or the docs, it is wrong — fix it in the same round as the change.
> 8. **95% is the minimum test coverage.** No production changes to enable testability without maintainer approval. `_test.go` files may cross DDD layers only if production imports already allow it.
>
> **Every approved change ships in one round with:** the code edit + unit tests (green `go build -tags 'postgres kafka' ./... && go vet -tags 'postgres kafka' ./... && go test -tags 'postgres kafka' ./... -count=1` is a precondition, not proof of working — then ask via `AskUserQuestion` whether to run the `omnicore-example-users` E2E suites) + a `CHANGELOG.md` `[Unreleased]` entry (public-surface changes only) + a `docs/` site update (the consumer-facing manual at `docs/content/sections/<id>.html` + a `changelog.html` entry; **the site and this file must tell the same story**). **After updating the HTML docs, reflect the change in CLAUDE.md only as a link** — add/adjust the Documentation Map row pointing to the affected section (and, if the change touches a cross-cutting boundary or invariant, update that terse line). Do NOT transcribe what changed into CLAUDE.md; the section is the record, this file is the index into it. A brand-new section (new nav entry) means a new Documentation Map row. **Then check the root `README.md`**: it is a public mirror of reality — when the change alters what the README enumerates (engines, transports, surfaces, stack, capabilities), update those enumerations in the same round; never inject into it information the manual does not already carry. Purely internal changes (private helper, refactor without API change, comment-only) may skip CHANGELOG/docs — record the rationale.
>
> **Changelog + release docs.** A `changelog.html` entry for a **breaking** change carries a standalone `<strong>breaking</strong>` marker (right after `<strong>Changed</strong> —`): `docs/assets/app.js` derives each release's severity by scanning entries for a `<strong>` whose text is exactly `breaking`; prose like "(breaking)" does NOT trigger it (the root `CHANGELOG.md` is free prose, not parsed). The full version-bump file checklist lives in `docs/README.md` under "Releasing" — the single source of truth; don't restate it here.

---

## What this is

Go framework library providing **DDD + CQRS infrastructure** for microservices. Services import it as a Go module dependency; OmniCore itself contains no service code.

- **Module path**: `github.com/ClaudioSchirmer/omnicore`
- **Local path**: `/Volumes/Lynx/Development/omnicore-stack/omnicore`
- **Maintainer**: Claudio Schirmer (`claudioschirmer@icloud.com`)
- **Reference consumer**: [`../omnicore-example-users`](../omnicore-example-users/CLAUDE.md) — sandbox service exercising every framework feature (its own `CLAUDE.md`; not covered here).
- **End-user manual = the source of truth for the public surface**: the [`docs/`](docs/) site, per-section pages under `docs/content/sections/`. This file is the agent/maintainer orientation; the manual carries the full surface (signatures, fields, edge cases, examples). **Keep both in sync.** When you need a contract, open the mapped section below — don't reconstruct it from memory.

## Stack

- Go ≥ 1.21 (`log/slog` + generics); toolchain pinned to `go 1.26.3`.
- Fiber v3 (HTTP), connectrpc.com/connect (gRPC surface), pgx v5 (Postgres), go-sql-driver (MySQL), microsoft/go-mssqldb (SQL Server), sijms/go-ora (Oracle, 23ai+), modernc.org/sqlite (SQLite — pure-Go/cgo-free, the self-executable MVP engine, tag-gated), mongo-driver v2 (MongoDB 5.2+ — `$sortArray` backs EmbedMany's materialized ordering), segmentio/kafka-go + nats.go (message-transport adapters, each tag-gated), google/uuid (canonical — don't add another uuid lib).

## Build and test

```
go build -tags 'postgres kafka' ./...                        # an engine tag (postgres|mysql|sqlserver|oracle) AND a transport tag (kafka|nats) are BOTH mandatory
go vet -tags 'postgres kafka' ./...
go test -tags 'postgres kafka' ./... -count=1                # unit suite; swap engine to -tags 'mysql kafka' for the MySQL build
go test -tags 'integration postgres kafka' ./... -count=1    # integration (needs docker compose up in ../omnicore-example-users/devops)
```

A build links a relational engine via build tag: `-tags postgres` / `-tags mysql` / `-tags sqlserver` / `-tags oracle` links exactly that one; any combination links those engines and selects the active dialect at runtime from `relational.dialect`. It also links a **message transport** the same way: `-tags kafka` links the Kafka/Redpanda adapter, `-tags nats` links the NATS JetStream adapter. No default on either axis — building without an engine tag OR without a transport tag aborts at boot. Tests sit beside the file under test (`foo.go` ↔ `foo_test.go`); integration tests opt in via `//go:build integration && <engine>` and carry the engine tag, so the unit suite also runs under the chosen tag. **Never `go mod tidy`** (prunes tag-gated deps).

→ Engine seam + transport seam, build tags, dialect/transport selection: `docs/content/sections/architecture.html`, `docs/content/sections/bootstrap.html`. Transport subsystem in depth (adapters, `transport:` config, durability, CDC relay): `docs/content/sections/transport.html`.

## Architecture — 4-layer DDD with strict boundaries

```
web/          HTTP transport only; openapi/ (OpenAPI 3.1 + Swagger), graphql/ (own surface),
              grpc/ (own surface, Connect — dedicated listener), authcore/ (shared JWT core),
              queryschema/ (shared read-side DTO reflection: REST + OpenAPI + GraphQL)
application/  configuration/ (AppContext), translation/, notifications/, pipeline/,
              persistence/, queries/, audit/ — Go-pure, no transport/infra tags
domain/       pure business rules, ZERO IO (stdlib only; identity is domain.ID, never a uuid.UUID field)
infra/        engines, persisters, outbox, mongo, composer, sync, audit impl, events,
              httpclient + grpcclient (outbound toolboxes) over shared resilience/ cores
```

### Dependency rules — NEVER violate

| Layer | May import | Must NOT import |
|---|---|---|
| `domain` | stdlib only (identity is `domain.ID`, never a `uuid.UUID` field) | everything else |
| `application/*` | `domain`, other `application/*` | `infra`, `web` |
| `infra` | `domain`, `application/*` | `web` |
| `web` | `domain`, `application/*` | `infra` directly |

- Cross-layer errors travel via `domain.NotificationCarrier` (an `error` carrying `[]*NotificationContext`), so layers never type-import each other's error structs. Catch with `errors.As(err, &carrier)`.
- **Three-name model**: wire (`json:`/`query:`/`filter:`/`path:` tags, only in `web/`) ↔ Go field name (every layer above infra) ↔ physical column (declared only in `infra/` via `TableSchema`). Wire tags never appear outside `web/requests/`.
- Identity stays in `application/` + `domain/` — infra never pronounces domain vocabulary (no tenant/authz at infra).

→ Layer model, boundaries, the relational-engine seam: `docs/content/sections/architecture.html`. Why Auto == manual: `docs/content/sections/handler-invariance.html`.

## Documentation map

For any contract, behavior, field list, or example, open the mapped file under `docs/content/sections/`. One concept → one section. The framework topics are exhaustively covered there; this file does not duplicate them.

### Getting started
| Topic | Section | Essence |
|---|---|---|
| What the framework does, mental model | `overview.html` / `features.html` | Capability tour. |
| 4-layer DDD, dependency rules, write→read data flow, engine + transport seams | `architecture.html` | The boundaries above, in depth; the outbox → CDC → broker → Mongo spine, explicitly not event sourcing. |
| Auto path == manual path guarantee | `handler-invariance.html` | Convention and hand-wired routes are feature-equivalent (critical rule #3). |

### Requests & security
| Topic | Section | Essence |
|---|---|---|
| AppContext (UUID + language + Identity), request lifecycle, cancellation/`http.requestTimeoutSeconds` (→504) | `app-context.html` | Single per-request vehicle; `implements context.Context`; owns the cancellation parent. |
| JWT auth middleware (JWKS/PEM/external validator, revocation cache; validation core in `web/authcore`, shared with the gRPC shell) | `auth-middleware.html` | `auth.mode: jwt`; populates `Identity`; expired vs invalid split. |
| Authorization — 3 concentric layers (permission gate / `BuildRules` / tenant) | `authz-seams.html` | `resource:action`; `SemanticForbidden → 403`; no enforcement at infra. |

### Domain & persistence
| Topic | Section | Essence |
|---|---|---|
| Rules DSL, `BuildRules`, `EntityMode`/`Modes()`, `actionName`, notifications, parameterized vars, labels | `rules-dsl.html` | Mode-scoped closures; `Get*` family takes `actionName`; sealed `ValidEntity`. |
| Value objects — `ValueObject` (raw, writes `IsValid`) vs `EnumValueObject` (declares `Values()`, framework validates membership); `ValidateEnum`, `EnumByValue`, `EnumDescriptionKey`/`Translator.EnumDescription`, explicit-value rule; **persistence & read side** (a VO field persists as its underlying scalar and reconstructs on read — raw by conversion, enum by membership converge to `Unknown`; every schema position, both read backings, every surface) | `value-objects.html` | Two kinds by who owns the rule; enum = closed set, zero is `Unknown`, no hand-written `IsValid`; `ValidateValueObject` takes both; a VO field is a named type over a supported scalar, stored as the underlying (`domain.IsValueObject`/`ValueObjectValue`/`NewValueObjectValue` are the infra seam). |
| Aggregate root + value objects, transparent dispatch, symmetric cascade, child validation | `aggregate-persistence.html` | `AggregateRootProvider` opt-in; aggregate is the event unit (granularity B); depth = 1. |
| Lifecycle hooks (`AfterBegin`/`BeforeCommit`, positions A/D), `TxHandle`/`UnwrapTx` | `lifecycle-hooks.html` | In-TX side effects; sealed handle; `UnwrapPgxTx` is the PG-only escape hatch. |
| Old-state snapshot (`domain.Old[T]`) | `old-state.html` | Pre-write clone; captured by `Get*`; read by `BuildRules` + auditor. |

### Write side
| Topic | Section | Essence |
|---|---|---|
| CommandHandler, `Result[T]`, `Pipeline.Dispatch`, persistence ports (`ScopedRepository`/`Scope`), write-side composition catalog | `command-handler.html` | Reads direct, writes through `Scope(ctx)`; pure `domain.Writer`. |
| Auto command handlers + route constructors (`CommandWith*`/`CommandByID`), strict body (`pipeline.FullBody`), `path:` binding | `auto-handlers.html` | Cmd owns input (`ToEntity`/`ApplyTo`) + output (`FromEntity`); PUT≠PATCH by type. |
| Manual command handler (cross-service, side effects, custom envelope) | `custom-command-handler.html` | Implement `pipeline.Handler`; same wrappers; `WithBeforeCommit`/`WithAfterBegin`. |
| Concrete write lifecycle (BEGIN→hooks→write→outbox→audit→COMMIT→async) | `lifecycle-map.html` | One `pgx.Tx`: data + outbox + audit atomic; outbox → CDC relay → transport → SyncEngine. |
| Audit event shape, `kind` (snapshot/delta/transition), routing (`audit.destinations`) | `audit.html` | One event per write; `database` in-TX (authoritative) + `slog` post-commit. |

### Read side (CQRS)
| Topic | Section | Essence |
|---|---|---|
| QueryHandler, `ViewReader`, `ReadCriteria`/`Page`, CQRS split, read-side composition catalog, `ComposedView` (read-time join: primary + foreign-key legs, never materialized) | `query-side.html` | Eventually-consistent Mongo projections; documents, not aggregates; composed names read like view names. |
| Auto query handlers (`QueryWithParams`/`QueryByID`), filter operators, control keys (`fields`/`sort`/`after`/`before`/`limit`/`onlyTotal`), tabular export (CSV/XLSX) | `auto-query-handlers.html` | Allowlist by tag; projector `func(map[string]any) R`; keyset pagination. |
| Manual query handler (`NewQueryParser`, `ParseCriteria`, `RespondPaged`) | `custom-query-handler.html` | Escape hatch for bespoke parsing/envelopes. |
| Concrete read lifecycle, composer, SyncEngine, keep-by-default archive | `read-lifecycle-map.html` | Mongo mirrors PG; `DeleteOnArchive()` opt-in for hot-tier. |
| `SharedBaseView` — the all-in-one identity projection (base root + one sub-document per role) | `views.html` (SharedBaseView) | `SharedBaseView(name).Schema(base).Role(...)`; `_id` = base id; role events recompose; active-first segment pick. |

### Pipeline
| Topic | Section | Essence |
|---|---|---|
| Notification `Semantic` → HTTP status, error envelopes, Fiber router codes | `status-mapping.html` | Typed declaration IS registration; `SemanticValidation`→422 default. |

### Infrastructure
| Topic | Section | Essence |
|---|---|---|
| `TableSchema` — mandatory explicit Go-field↔column map; managed columns; the three-name contract (managed slots surface under FIXED Go names); a relational load surfaces id+revision+timestamps on the loaded entity (root AND child) via the embedded `domain.Managed` carrier; boot checks; drives write+criteria+scan+view | `table-schema.html` | `NewTableSchema[T]`; no inference; one declaration, every consumer. |
| Views — the read-side declaration: the three view kinds, the view-exclusive `NewExternalSchema`, `Embed`/`EmbedMany`/`EmbedInChild` (both leg kinds), `SharedBaseView`, `ComposedView`, and the SyncEngine/recompose fan-out | `views.html` | `View(name).Schema(...)` (root derived from schema); `SharedBaseView(name).Schema(base).Role(...)`; `ComposedView(name).Primary(...)`; a leg (`JoinUpstream(schema, go, ext)` / `JoinView(view, go, ext)`) carries the two segment names and serves BOTH families, the verb names the join via `.On(col)`; a `JoinView` leg's `Fields(cols...)` is the segment's materialization allowlist and per-consumer archive switch (Go names; JoinView-only; the mirror's cut is the subscription yaml `fields:` + its external schema); `Join*` = where the data comes from, `Embed*` vs `Link*` = when the join is paid (write vs read); the embed graph is acyclic and a view leg couples the source's `Version` into the embedder's rebuild hash; a view updates by its schema(s) and by every source it embeds; delivery is at-least-once with bounded retry, a UNIFIED relational failure ledger (`omnicore_projection_failures`: parked events + failed embed-segment ripples) with one automatic replay loop, revision-parity reconciliation and a `ProjectionHealth` liveness surface. |
| Relational view — a plain `query.View` marked `.RelationalSource(loader)`, served from the SoR (read-your-writes, no CDC) instead of the Mongo projection; the deliberate CQRS exception for dashboards / freshest reads / MVPs | `relational-view.html` | `View(name).RelationalSource(reader)` takes a `query.RelationalReader` — pass the aggregate's existing `repo.Loader` (one loader, shared with the repo; boot guard `BoundTable()==schema.Table()`). Full parity on ROOT + 1:1 satellite (sibling, shared base) read-side controls — the loader LEFT JOINs those in (id qualified to the anchor under the join); unsupported = Embed family + `SharedBaseView` (boot fail), `ComposedView`/`Link` (different type), and `?search=` / 1:N child- (or child-level-sibling) field filter+sort (→ 400 `RelationalCapabilityNotification`, `SemanticSchema`). Pagination is offset-in-cursor behind the same `after`/`before`/`limit` API. Flipping the backing is a shape change → bump `Version` (`DriftRelationalSync` = registry synced, no rebuild; dropping it rebuilds Mongo). No collection: SyncEngine/spec/reconcile skip it. |
| Cross-service communication — the channel decision matrix (sync internal → gRPC, sync external → httpclient, async facts → integration events, ANOTHER service's data in my views → `UpstreamSubscription`/`JoinUpstream` with ripple recompose + failure registry) | `service-to-service.html` | One canonical path per question; composition stays event-driven (B never reads A on the request path for VIEW data); composing MY OWN aggregates needs no broker hop (`JoinView` embed or `ComposedView` — see `views.html`). |
| Cache subsystem (`cache.Cache` port, Private vs Shared, memory/redis/custom) | `cache-subsystem.html` | DI-enforced split; typed `GetJSON`/`SetJSON` tolerate nil. |
| httpclient — outbound HTTP (`Call[Req,Resp]`, tag binding, middleware chain, retry/cache/breaker/idempotency/TLS/streaming/HMAC, auth providers) | `httpclient.html` | Per-service transport; handlers never import it (adapter in `infra/external/`). |
| gRPC (own surface via Connect: `reg.Register` + constructor family over the REST DTO seats — pb↔DTO bridge compiled at Register, boot-fails on mismatch, `Alias` for odd pairs, `MountRaw` for shapes that cannot mirror DTOs; feature-declared via `GRPCFeature.MountGRPC` (framework-built `Deps.GRPCRegistry`), dedicated listener, yaml `grpc:` block, Semantic→code table, `google.rpc` details, strict presence, `RequirePermission`, hand-written protos, shared `omnicore/v1/query.proto` components auto-converted — `filter:` tags = the wire operator allowlist, `PaginationInfo` list envelope (the REST `pagination` block mirrored field-for-field), `grpc.NewCriteria` as the raw-path companion; internal-plane posture `grpc.auth.mode` inherit/internal/mtls with attribution semantics + `idleTimeoutSeconds` recycling) + the outbound `infra/grpcclient` toolbox (yaml `grpcClient:`, `Deps.GRPCClient`, `grpcclient.For`, optional `pool`, resilience cores shared with httpclient via `infra/resilience`) | `grpc.html` | Fourth consumer of the same handlers over the same DTOs; auth rides `web/authcore` — one JWT core, two shells; one breaker/backoff implementation, two transports. |
| Integration events — async cross-service (`Dispatch`, `Receiver`, dedup, at-least-once) | `integration-events.html` | `integration_events` in-TX with the write; consumer handlers must be idempotent. |
| Message transport — the pluggable broker seam (`transport.Subscriber` port, the per-message `Completion` outcome contract, kafka/redpanda + nats adapters, `-tags kafka\|nats` build selection, `transport:` config, JetStream durability, the CDC-relay producer side) | `transport.html` | Consumer-side seam; a message advances only on `Done`, `Failed` guarantees redelivery; a build links exactly one adapter; producer path (outbox → CDC relay) unchanged. |

### Operations
| Topic | Section | Essence |
|---|---|---|
| bootstrap — `Run`/`Build`/`Serve`, `Deps`/`Wiring`/`Feature`, env vars, boot order | `bootstrap.html` | Reads exactly 4 env vars; everything else is consumer YAML. |
| `microservice.<profile>.yaml` full reference (`${...}` substitution, all blocks) | `yaml-reference.html` | One file per profile; `APP_PROFILE` selects it; only `dev` unlocks `auth.mode=disabled`. |
| OpenAPI 3.1 + Swagger UI (`*Spec` wrappers, `Mount`/`MountRaw`, generator coverage) | `openapi.html` | Reflection over the same DTOs; opt-in via `Wiring.OpenAPI`. |
| GraphQL (own surface, Relay connections, reflected schema, per-field authz; feature-declared via `GraphQLFeature.MountGraphQL` over the framework-built `Deps.GraphQLRegistry`) | `graphql.html` | Reuses application handlers only; not in Swagger; not policed by REST scans. |
| Relational migrations (numbered SQL, framework `0001`+`0002` per dialect, autoRun modes) | `migrations.html` | Framework sequence (`0001_framework` control plane + `0002_view_slots` blue-green pointers) in its own tracking table; the service's own files are an INDEPENDENT sequence starting at `0001` (separate table, no collision). |
| Mongo schema evolution (drift detection, `Version(N)`, online blue-green rebuild, `omnicore_mongo_views`) | `mongo-schema-evolution.html` | Backend-neutral control plane; advisory-lock + two-slot pointer; full rebuild is online blue-green (shadow + dual-apply + verify + flip); bump on rebuild-relevant change. |
| Logs — the always-on structured-JSON stdout channel + the external observability stack it feeds (collector → Elasticsearch/Loki → Kibana), audit-echo querying | `logs.html` | slog JSON to stdout, zero framework config; devops guide, not a surface; collector reads the stream directly. |
| Distributed tracing (OTel opt-in, `correlationID == trace_id`, async links) | `tracing.html` | `observability.tracing` default off = no-op; per-subsystem cost toggles. |

### Reference / About
| Topic | Section | Essence |
|---|---|---|
| Quick reference (where to add things, naming, surfaces) | `reference.html` | Consumer-facing index. |
| Service layout & naming — the recommended standard (directory skeleton, file granularity, per-layer naming, migration granularity, cross-cutting names) | `service-layout.html` | Baseline for developers, NORMATIVE for code generators; names/placement only — mechanics stay in the mapped sections. |
| Changelog | `changelog.html` | Per-release entries; `<strong>breaking</strong>` marker drives severity. |

## Cross-cutting invariants (must survive every change)

These constrain design decisions across the whole module. Each is detailed in the mapped section above.

1. **Sealed `ValidEntity`** — `Insertable`/`Updatable`/`Archivable`/`Unarchivable`/`Deletable`/`Batch` are produced ONLY by `domain` (private `entity()` seal), via the `Get*` family (each takes `actionName`). No low-level constructors.
2. **One TX for data + outbox + audit.** Each write opens one relational transaction containing the data write(s), exactly one outbox row per aggregate operation (granularity B), and the in-TX `audit_events` row when `database` routing is on. Custom repos must preserve this.
3. **Lifecycle hooks fire inside that TX, once per operation** — `afterBegin` before any framework write, `beforeCommit` after all writes and before COMMIT; same positions on flat and aggregate paths. Hook error rolls back (preserving type identity); hook panic rolls back and propagates to the single recover point in the pipeline.
4. **Domain has zero IO** — pure types, validation, rules; cross-layer errors only via `domain.NotificationCarrier`.
5. **Notifications are typed structs**; the human string comes from the translation layer at the boundary; `NotificationKey` (the struct name) and `Semantic` flow to the wire. Kernel notifications embed their layer's base (`Domain`/`Application`/`Infrastructure`NotificationBase) — never mix.
6. **Every Archivable has a symmetric Unarchivable**; cascade root↔children is symmetric and universal.
7. **Mongo mirrors the relational backend by default** — archived rows survive in the projection unless a view opts into `DeleteOnArchive()`; default reads hide them at EVERY level (the root `deleted_at` gate + the archived-entry strip on every segment: child arrays, roles, materialized embed segments and `EmbedInChild` enrichments), `?includeArchived` surfaces all of them at once. The strip applies to a segment if and ONLY IF the schema behind it declares `DeletedAt` — the declaration is what defines an archived state; a source declaring none is never filtered.
8. **`TableSchema` is the sole place physical names live** — mandatory, explicit, complete; an undeclared field is never persisted/scanned/audited; one declaration drives write + criteria + scan + Mongo view.
9. **The relational layer is backend-agnostic** (Postgres, MySQL, SQL Server, Oracle AND SQLite via the engine seam; Oracle floor: Database 23ai; SQLite is the pure-Go, single-node, MVP/self-executable backend). Say "the relational backend / SoR / control plane" — never "Postgres" for an agnostic concept. There is no engine-recovery cast — every backend is reached through the neutral `core.RelationalEngine` surface (`DB.Querier()` for custom reads); the only PG-specific escape hatch is the in-TX `UnwrapPgxTx`, one of the per-engine `Unwrap<Engine>Tx` family.
10. **Integration events are at-least-once** — dedup is best-effort; consumer handlers must be idempotent. No outer TX on the receiver path (each `Repo.Method` opens its own short TX, identical to the HTTP path).
11. **The `docs/` site is the source of truth for the public surface** — every approved surface change updates the mapped section + a `changelog.html` entry in the same round.
12. **Identity is a TYPE** — `domain.ID` (nullable ⇒ `*domain.ID`) is the id everywhere: entity contracts, the `domain.Managed` carrier embedded in roots (via `BaseEntity`) and in every AVO, `WriteResult`, `criteria.ByID`, and persisted fields, where the Go type alone drives each dialect's native id form (never a value-shape guess; a `string` field is text, always). The persistable field-type set is CLOSED — an unknown field type is a boot fail at the `Field(...)` declaration (`table-schema.html` is the canonical home of the set).

## Naming conventions

| Item | Convention | Example |
|---|---|---|
| Notification struct | `<What>Notification` | `UsernameAlreadyExistsNotification` |
| Translation key | identical to struct name | `"RequiredFieldNotification": "Required field."` |
| Enum description key | `<Type>.<VALUE>` | `"EntityMode.INSERT": "Inserir"` |
| Entity files (services) | lowercase singular | `customer.go` |
| Generic type param | `T` for value, `TEntity` for entity | `Result[T]`, `Repository[TEntity]` |

## Codebase-specific Go gotchas

Conceptual cautions not covered by the consumer manual (verify specifics in code before relying on them):

1. **Methods can't be generic** — generic pipeline/respond operations are top-level functions taking `*Pipeline`, not methods.
2. **`errors.As` with the carrier interface** catches every layer's error without importing each.
3. **Named return + `defer/recover`** is required wherever panics convert to an `Exception` result.
4. **`reflect.TypeOf(n).Name()` strips pointers** — `*Customer` and `Customer` both give `"Customer"` (notification/translation key resolution depends on this).
5. **Private-receiver base methods cross packages via promotion** — embedding a notification base in another package works because the seal method is promoted.
6. **`Fields`-style `map[string]any` has non-deterministic order** — SQL generation sorts keys for determinism.
7. **Identifier validation panics on bad input** — intentional SQL-injection defense; identifiers come from domain code, never user input.
8. **Aggregate value objects are value types, not pointers** — the change tracker matches them via the mandatory domain-declared `IsSameBusinessIdentity` (helper `domain.IsSameByBusinessFields` for the structural case), never `reflect.DeepEqual`.
9. **`slog` levels** — `Info` routine, `Warn` non-blocking failures (audit echo, best-effort writes), `Error` unhandled.
