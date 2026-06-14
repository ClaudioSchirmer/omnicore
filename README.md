# omnicore

[![Go Reference](https://pkg.go.dev/badge/github.com/ClaudioSchirmer/omnicore.svg)](https://pkg.go.dev/github.com/ClaudioSchirmer/omnicore)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

DDD + CQRS infrastructure for Go microservices.

omnicore wires the layers a production service needs and gets out of the way: a sealed write side with transactional outbox and structured audit, a Mongo-projected read side with keyset pagination and sparse responses, Fiber-based HTTP with JWT auth and auto-generated OpenAPI, declarative outbound HTTP, and operational concerns (migrations, Mongo schema evolution, declarative YAML config) — all driven from one `microservice.<profile>.yaml` and one `Wire` function.

## Highlights

- **Trivial CRUD in one line.** Auto Command Handlers (`InsertCommandHandler`, `UpdateCommandHandler`, `PartialUpdateCommandHandler`, `ArchiveCommandHandler`, `UnarchiveCommandHandler`, `DeleteCommandHandler`) are generic over your entity + command + result. The 6 write verbs land with no handler code; manual handlers coexist for cross-service orchestration.
- **DDD that holds up.** Sealed `ValidEntity` types (`Insertable` / `Updatable` / `Archivable` / `Deletable` / `Unarchivable` / `Batch`) can only be produced by the `domain` package. The 4-layer architecture (`domain` / `application` / `infra` / `web`) enforces dependency direction at compile time via the `domain.NotificationCarrier` cross-layer error contract.
- **Aggregate-aware persistence with universal symmetric cascade.** Root archive cascades archive to children; root unarchive restores them; root delete relies on FK `ON DELETE CASCADE`. The framework infers table / column / FK from Go types via convention — your domain never pronounces SQL identifiers.
- **Transactional outbox by default.** Domain rows + outbox row + audit row land in one `pgx.Tx`. Debezium tails the outbox; the framework's `SyncEngine` projects to MongoDB views asynchronously. One outbox row per aggregate operation.
- **Read side that scales.** Composer-driven Mongo views; keyset pagination over `(sort..., _id)` with cursor context hashing (no cross-page drift on filter or sort changes); sparse responses via `?fields=` with boot-time guard; tag-driven projection via `fwresponses.AutoFromDoc[R]`; cross-service composition via `UpstreamSubscription` + `FromMongo`.
- **Mongo schema evolution.** Declarative `Version(N)` on every view, PG-backed registry (`omnicore_mongo_views`), drift detection across 8 cases (FreshInit / AlienData / MongoWiped / ArtifactOnly / ForgotToBump / RebuildRequired / Downgrade / None), advisory-lock-coordinated rebuild with `processing → done` status transitions and crash recovery.
- **First-class JWT auth.** Local validation against JWKS or PEM (RSA / ECDSA / Ed25519); optional RFC 7662 revocation check with opt-in positive-only cache; three-layer authorization (declarative `RequirePermission` gate on the route, programmatic owner checks in `BuildRules`, tenant scoping via claim presence + filter overlay).
- **Audit you can rely on.** One `AuditEvent` per write with SQL-grounded verbs (`insert` / `update` / `archive` / `unarchive` / `delete`), per-verb body discriminated by `kind` (`snapshot` / `delta` / `transition`), per-child ops following the same vocabulary. Routes via `audit.destinations`: `database` (atomic in-TX row), `slog` (post-commit echo), or both. JWT-claim allowlist controls actor surface.
- **Declarative outbound HTTP.** `httpclient` resolves services from YAML: retry with jitter, response cache, circuit breaker, HMAC request signing, OAuth2 (`client-credentials` / `credentials-exchange` with per-identity token cache), `forward-bearer` for downstream propagation, streaming (download / upload / multipart / SSE), inline auth for per-call credentials, per-call `CallConfig` overrides, and a `BaseURLResolver` plug for dynamic routing.
- **OpenAPI generated from your routes.** `openapi.Mount` / `MountRaw` introspect the same Go types your handlers consume — Request DTOs with `path:` / `query:` / `filter:` / `json:` tags, Response DTOs with `view:` overrides — and emit a complete `3.1.0` document plus Swagger UI. Optional language dropdown driven by `Accept-Language`.
- **i18n built in.** 7 catalogs (PT-BR, ENG, ESP, FRA, DEU, ITA, NLD), parameterized notifications (`tvar:"<name>"` struct tags), per-emit variable overrides, context-label translation.

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

- [`DOCS.html`](DOCS.html) — single-file public manual; the consumer's source of truth for every exported API.
- [`CHANGELOG.md`](CHANGELOG.md) — release notes (Keep a Changelog format, SemVer; the API may evolve through `0.x.y`).
- [`tasks/`](tasks/) — design documents for in-flight and shipped features.
- [`omnicore-example-users`](https://github.com/ClaudioSchirmer/omnicore-example-users) — canonical reference service that consumes every framework feature, plus end-to-end QA suites (`qa/e2e.sh`, `qa/auth.sh`, `qa/audit.sh`, `qa/httpclient.sh`, `qa/openapi.sh`, `qa/authz.sh`, `qa/schema_evolution.sh`) against real Postgres + Mongo + Kafka + Debezium + Keycloak.

## Stack

- Fiber v2 (HTTP)
- pgx v5 (PostgreSQL)
- mongo-driver v2 (MongoDB)
- segmentio/kafka-go (Kafka)
- golang-migrate/migrate v4 (SQL migrations)
- golang-jwt/jwt v5 + MicahParks/keyfunc (JWT)

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
