# omnicore

[![Go Reference](https://pkg.go.dev/badge/github.com/ClaudioSchirmer/omnicore.svg)](https://pkg.go.dev/github.com/ClaudioSchirmer/omnicore)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

DDD + CQRS infrastructure for Go microservices.

omnicore wires the layers a production service needs and gets out of the way: a sealed write side with transactional outbox and structured audit, a Mongo-projected read side with keyset pagination and sparse responses, Fiber-based HTTP with JWT auth and auto-generated OpenAPI, declarative outbound HTTP, and operational concerns (migrations, Mongo schema evolution, declarative YAML config) — all driven from one `microservice.<profile>.yaml` and one `Wire` function.

## Highlights

- **Trivial CRUD in one line.** Auto Command Handlers (`InsertCommandHandler`, `UpdateCommandHandler`, `PartialUpdateCommandHandler`, `ArchiveCommandHandler`, `UnarchiveCommandHandler`, `DeleteCommandHandler`) are generic over your entity + command + result. The 6 write verbs land with no handler code; manual handlers coexist for cross-service orchestration.
- **DDD that holds up.** Sealed `ValidEntity` types (`Insertable` / `Updatable` / `Archivable` / `Deletable` / `Unarchivable` / `Batch`) can only be produced by the `domain` package. The 4-layer architecture (`domain` / `application` / `infra` / `web`) enforces dependency direction at compile time via the `domain.NotificationCarrier` cross-layer error contract.
- **Aggregate-aware persistence with universal symmetric cascade.** Root archive cascades archive to children; root unarchive restores them; root delete relies on FK `ON DELETE CASCADE`. Go fields map to physical columns via an explicit, mandatory `TableSchema` (no convention, no name inference) — your domain never pronounces SQL identifiers, and the one schema drives the write path, the criteria engine, the auto-scan read-back, and the Mongo view.
- **Transactional outbox by default.** Domain rows + outbox row + audit row land in one `pgx.Tx`. Debezium tails the outbox; the framework's `SyncEngine` projects to MongoDB views asynchronously. One outbox row per aggregate operation.
- **Read side that scales.** Composer-driven Mongo views; keyset pagination over `(sort..., _id)` with cursor context hashing (no cross-page drift on filter or sort changes); sparse responses via `?fields=` with boot-time guard; tag-driven projection via `fwresponses.AutoFromDoc[R]`; downloadable CSV / Excel exports of any view via `HandleQueryAsCSV` / `HandleQueryAsXLSX` (hierarchical, labelKey headers, `?fields=`-narrowed); cross-service composition via `UpstreamSubscription` + `FromSchema` over `NewExternalSchema`.
- **Mongo schema evolution.** Declarative `Version(N)` on every view, PG-backed registry (`omnicore_mongo_views`), drift detection across 8 cases (FreshInit / AlienData / MongoWiped / ArtifactOnly / ForgotToBump / RebuildRequired / Downgrade / None), advisory-lock-coordinated rebuild with `processing → done` status transitions and crash recovery.
- **First-class JWT auth.** Local validation against JWKS or PEM (RSA / ECDSA / Ed25519); optional RFC 7662 revocation check with opt-in positive-only cache; three-layer authorization (declarative `RequirePermission` gate on the route, programmatic owner checks in `BuildRules`, tenant scoping via claim presence + filter overlay).
- **Audit you can rely on.** One `AuditEvent` per write with SQL-grounded verbs (`insert` / `update` / `archive` / `unarchive` / `delete`), per-verb body discriminated by `kind` (`snapshot` / `delta` / `transition`), per-child ops following the same vocabulary. Routes via `audit.destinations`: `database` (atomic in-TX row), `slog` (post-commit echo), or both. JWT-claim allowlist controls actor surface.
- **Declarative outbound HTTP.** `httpclient` resolves services from YAML: retry with jitter, response cache, circuit breaker, HMAC request signing, OAuth2 (`client-credentials` / `credentials-exchange` with per-identity token cache), `forward-bearer` for downstream propagation, streaming (download / upload / multipart / SSE), inline auth for per-call credentials, per-call `CallConfig` overrides, and a `BaseURLResolver` plug for dynamic routing.
- **OpenAPI generated from your routes.** `openapi.Mount` / `MountRaw` introspect the same Go types your handlers consume — Request DTOs with `path:` / `query:` / `filter:` / `json:` tags, Response DTOs with `json:` tags (the column↔Go mapping lives in the view's `TableSchema`, never on the Response) — and emit a complete `3.1.0` document plus Swagger UI. Optional language dropdown driven by `Accept-Language`.
- **i18n built in.** 7 catalogs (PT-BR, ENG, ESP, FRA, DEU, ITA, NLD), parameterized notifications (`tvar:"<name>"` struct tags), per-emit variable overrides, context-label translation.
- **Handler invariance — one handler, many surfaces.** A handler is written once and stays invariant across the surface it is attached to; the surface decides the I/O shape, not the handler. A **command handler** (`pipeline.Handler[TCmd, TResult]`, Auto or hand-rolled) attaches to an HTTP route via `fwweb.HandleCommandWithBody`, to a cross-service Kafka receiver via `reg.From(source).On(eventKey, sample, handler)`, or to both at once — same Cmd, same `BuildRules`, same outbox + audit; the async path adds transactional emission (`fwintegration.Dispatch(...WithTx(tx))`), at-least-once delivery with per-consumer-group dedup, and an operator-driven retry surface. A **query handler** (`pipeline.Handler[TQ, queries.Page]`, Auto or hand-rolled) attaches to an HTTP/JSON list via `fwweb.HandleQueryWithParams`, to a **CSV** download via `fwweb.HandleQueryAsCSV`, or to an **Excel** download via `fwweb.HandleQueryAsXLSX` — same Query, same Request DTO + filter allowlist + `?fields=`; only the rendering (JSON envelope vs CSV vs `.xlsx`) changes, through a pluggable `web/export.Encoder`. Serve a new demand by attaching the existing handler to another wrapper — zero new business logic, zero rewrite. See the [documentation site](https://claudioschirmer.github.io/omnicore/#handler-invariance) → Handler invariance.
- **Cache subsystem that follows the handler.** A single `cache.Cache` port (`Get`/`Set`/`Delete` with `ctx` + TTL + error) is consumed by HTTP handlers, Kafka integration receivers, background goroutines, and the outbound httpclient response cache alike — **the same line of code, whichever transport delivered the request.** Two Deps slots: `Deps.Cache` (service-private; populated when `cache:` is declared) and `Deps.SharedCache` (cross-service; nil unless `cache.shared:` is declared). Three backends with a structural matrix: **memory** (in-process LRU+TTL — local only; rejected at boot for the shared slot), **redis** (both scopes — lazy connect, JSON entries debug-able via `redis-cli`, configurable `failMode: open\|closed` and per-op timeout), **custom** (`Wiring.Cache` / `Wiring.SharedCache` for proprietary backends — Memcached, Valkey, Hazelcast, AWS ElastiCache, internal REST-as-cache adapters). The choice between private and shared is at the dependency-injection level, not a flag on Set — the type system catches "did you mean private here?" at the call site.

## Install

```bash
go get github.com/ClaudioSchirmer/omnicore@latest
```

Requires Go 1.21+ (`log/slog` and generics).

## Quick Start

```go
package main

import (
    "log"

    "github.com/ClaudioSchirmer/omnicore/application/translation"
    "github.com/ClaudioSchirmer/omnicore/bootstrap"
)

func main() {
    if err := bootstrap.Run(Wire); err != nil {
        log.Fatal(err)
    }
}

func Wire(d bootstrap.Deps) bootstrap.Wiring {
    return bootstrap.Wiring{
        Translations: []translation.Module{translation.CoreENG()},
        Features:     []bootstrap.Feature{ /* your features */ },
    }
}
```

`bootstrap.Run` reads `microservice.${APP_PROFILE}.yaml` (`APP_PROFILE` is a required env var; `dev` and `prd` are canonical), wires Postgres + Mongo + Kafka + Fiber, applies pending migrations, auto-registers `GET /health`, mounts your Features, starts the `SyncEngine` if any feature declares views, and serves HTTP until `SIGINT`/`SIGTERM`.

A Feature implements `bootstrap.Feature` (or `bootstrap.ReadableFeature` when it has views). It owns one aggregate root: declares the entity, repository, command / query handlers, and HTTP routes — wired through its `Mount(app *fiber.App, d bootstrap.Deps)` method.

## A complete endpoint, end to end

A `POST /users` route that persists a `User` aggregate (root + addresses) with body validation, transactional outbox, audit row, and Mongo view projection — three files:

```go
// domain/user.go — pure business rules
type User struct {
    domain.AggregateRoot
    Name, Email string
    Phone       *string
}

func (u *User) Modes() []domain.EntityMode {
    return []domain.EntityMode{
        domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
        domain.ModeArchive, domain.ModeUnarchive,
    }
}

func (u *User) GetAggregateRoot() *domain.AggregateRoot          { return &u.AggregateRoot }
func (u *User) AggregateChildren() []domain.AggregateValueObject { return []domain.AggregateValueObject{Address{}} }

func (u *User) BuildRules(_ string, _ domain.Service, r *domain.Rules) {
    r.IfInsertOrUpdate(func() {
        if u.Email == "" {
            r.AddNotification("Email", domain.RequiredFieldNotification{})
        }
    })
}
```

```go
// application/commands/insert_user.go — application boundary
type InsertUserCommand struct {
    pipeline.CommandBase
    Name, Email string
    Phone       *string
}

func (c InsertUserCommand) ToEntity(_ *configuration.AppContext) *User {
    return &User{Name: c.Name, Email: c.Email, Phone: c.Phone}
}

func (c InsertUserCommand) FromEntity(_ *configuration.AppContext, u *User) InsertUserResult {
    return InsertUserResult{ID: *u.GetID(), Name: u.Name, Email: u.Email, Phone: u.Phone}
}

type InsertUserResult struct {
    ID    domain.ID
    Name  string
    Email string
    Phone *string
}
```

```go
// web/user_routes.go — register the route
users.Post("/", fwweb.HandleCommandWithBody(d.Pipeline,
    requests.InsertUserRequest{},
    requests.InsertUserResponse{}.FromResult,
    &handlers.InsertCommandHandler[*User, *InsertUserCommand, commands.InsertUserResult]{
        Repo: userRepo,
    },
    fiber.StatusCreated))
```

That single `HandleCommandWithBody` call gives you:

- Schema validation at the wire — malformed JSON or wrong type → `400` with `SchemaViolationNotification`.
- Business validation via `BuildRules` — invariant violations → `422` with the typed notification.
- Unique-constraint translation — PG `23505` mapped to `409` via the Repository's `Constraints` binding.
- Aggregate-aware persistence — `User` row + every `Address` row in a single `pgx.Tx`.
- Transactional outbox INSERT in the same transaction.
- Audit event built per-verb, written to `audit_events` in-TX when configured.
- Debezium Outbox Event Router publishes to Kafka; `SyncEngine` composes and upserts the Mongo view.
- A typed Result projected to the wire via `Response.FromResult` (no JSON tags below `web/`).

Swap `InsertCommandHandler` for `UpdateCommandHandler` (strict body via the `FullBody` marker), `PartialUpdateCommandHandler` (lenient PATCH), `ArchiveCommandHandler` / `UnarchiveCommandHandler` (state transitions with cascade), or `DeleteCommandHandler` — the rest of the wiring stays the same shape.

Manual handlers are a sibling path, not a poorer one: `fwweb.HandleCommandWith{Body,BodyID,ID}` and `fwweb.HandleQueryWith{Params,ID}` accept hand-written `pipeline.Handler[*Cmd, TResult]` implementations with the same envelope, the same notification semantics, the same audit guarantees.

## Documentation

- [Documentation site](https://claudioschirmer.github.io/omnicore/) — the public manual (published from [`docs/`](docs/) via GitHub Pages); the consumer's source of truth for every exported API.
- [`CHANGELOG.md`](CHANGELOG.md) — release notes (Keep a Changelog format, SemVer; the API may evolve through `0.x.y`).
- [`omnicore-example-users`](https://github.com/ClaudioSchirmer/omnicore-example-users) — canonical reference service that consumes every framework feature, plus end-to-end QA suites (`qa/e2e.sh`, `qa/auth.sh`, `qa/audit.sh`, `qa/httpclient.sh`, `qa/cache.sh`, `qa/openapi.sh`, `qa/authz.sh`, `qa/schema_evolution.sh`) against real Postgres + Mongo + Kafka + Debezium + Keycloak + Redis.

## Stack

- Fiber v3 (HTTP)
- pgx v5 (PostgreSQL)
- mongo-driver v2 (MongoDB)
- segmentio/kafka-go (Kafka)
- golang-migrate/migrate v4 (SQL migrations)
- golang-jwt/jwt v5 + MicahParks/keyfunc (JWT)

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
