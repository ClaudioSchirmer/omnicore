# omnicore

[![Go Reference](https://pkg.go.dev/badge/github.com/ClaudioSchirmer/omnicore.svg)](https://pkg.go.dev/github.com/ClaudioSchirmer/omnicore)
[![Go 1.21+](https://img.shields.io/badge/go-1.21%2B-00ADD8.svg)](https://go.dev)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-GitHub%20Pages-24292e.svg)](https://claudioschirmer.github.io/omnicore/)

**DDD + CQRS infrastructure for Go microservices.**

Write your domain once. omnicore wires the rest — the transactional write side, the
Mongo-projected read side, and every transport surface — from a single
`microservice.<profile>.yaml` and one `Wire` function.

📖 **[Full documentation →](https://claudioschirmer.github.io/omnicore/)** · the links below jump straight to each topic.

---

## Why omnicore

- ⚡ **The 6 CRUD verbs land with zero handler code** — insert, update, partial-update, archive, unarchive, delete.
- 🗄️ **Backend-agnostic relational core** — PostgreSQL, MySQL, SQL Server, Oracle *and* SQLite (pure-Go, self-executable MVP), behind one engine seam; your domain never names a vendor.
- 🧱 **DDD that the compiler enforces** — 4 layers, one direction, sealed domain types.
- 🔁 **One handler, five surfaces** — REST, gRPC, GraphQL, the message broker (Kafka or NATS), and file exports share the *same* handler instance.
- 📬 **Correct-by-construction writes** — data + outbox + audit commit in one transaction, always.
- 🛠️ **Batteries included** — auth, authz, cache, outbound HTTP, migrations, schema evolution, tracing, i18n.

---

## Capabilities at a glance

Each row links to its manual page.

### Write side
| Capability | In one line | Docs |
|---|---|---|
| Auto command handlers | The 6 write verbs, generic over your entity — no handler code | [auto-handlers](https://claudioschirmer.github.io/omnicore/#auto-handlers) |
| Sealed domain | `ValidEntity` only produced by `domain`; boundaries checked at compile time | [architecture](https://claudioschirmer.github.io/omnicore/#architecture) |
| Aggregate persistence | Root + children in one write; universal symmetric archive/unarchive cascade | [aggregate-persistence](https://claudioschirmer.github.io/omnicore/#aggregate-persistence) |
| Transactional outbox | Domain rows + outbox row + audit row in a single `pgx.Tx` | [write lifecycle](https://claudioschirmer.github.io/omnicore/#lifecycle-map) |
| Criteria — the query DSL | One backend-neutral tree behind every relational read and every predicated write: Go field names, parameterized values, the envelope (order, limit/offset, archive scope) on the query rather than in the predicate — and **subqueries**, where `InSub` / `EqSub` / `Exists` compare against another SELECT and `criteria.Outer("ID")` correlates, which is what expresses a filter on the MANY side of a 1:N | [criteria](https://claudioschirmer.github.io/omnicore/#criteria) |
| Tables without an aggregate | `core.NewDirectSchema[T]` + `read.NewDirectRepository[T]` point the same criteria engine, read joins and aggregate DSL at a table with no entity over it — a control table, or an aggregate's child counted as a fact. The read keeps its full horizontal reach; the write is one statement against the anchor table, with no outbox, audit, revision guard or cascade — including `Upsert`, keyed on a declared conflict target rather than on the identity | [direct-schema](https://claudioschirmer.github.io/omnicore/#direct-schema) |
| Audit | One event per write, answering who AND from where; `snapshot` / `delta` / `transition` bodies; DB + slog routing; optional framework-served read endpoint over the trail | [audit](https://claudioschirmer.github.io/omnicore/#audit) |

### Read side (CQRS)
| Capability | In one line | Docs |
|---|---|---|
| Mongo projections | Eventually-consistent views; keyset pagination; sparse `?fields=` responses; a typed application Result — the document never leaves the application layer | [query-side](https://claudioschirmer.github.io/omnicore/#query-side) |
| Read joins | `r.WithJoins(read.InnerJoin(schema.AsDirectSchema()).On("fk").Field("Go", "col"))` on the repository lets every loader read — and any relational view over it — filter, sort and load across a foreign key into another aggregate; never a write path | [read-joins](https://claudioschirmer.github.io/omnicore/#read-joins) |
| Relational views | `query.RelationalView(name, loader)` serves a read model straight from the SoR — read-your-writes with no CDC lag, for dashboards / freshest queries / MVPs; root + 1:1 satellite controls, no embed/link/composed/shared | [relational-view](https://claudioschirmer.github.io/omnicore/#relational-view) |
| Projection integrity | At-least-once delivery with a per-message outcome; failed events retry, park in a relational ledger and auto-replay; opt-in revision-parity reconciliation audits every view against its source | [views](https://claudioschirmer.github.io/omnicore/#views) |
| Auto query handlers | Filter-operator allowlist by tag; one Response DTO drives every surface — the JSON body, the `?fields=` vocabulary and the CSV / Excel columns | [auto-query-handlers](https://claudioschirmer.github.io/omnicore/#auto-query-handlers) |
| View composition | Join by key with the same two legs (a local view or another service's mirrored data), choosing when you pay: materialized on write (`Embed`) or composed per request (`Link`) — with a per-leg field allowlist (`Fields`) that doubles as the segment's archive switch | [views](https://claudioschirmer.github.io/omnicore/#views) |
| Cross-service reads | Another service's data projected locally from its event stream — never a call on the request path | [service-to-service](https://claudioschirmer.github.io/omnicore/#service-to-service) |
| Schema evolution | Declarative `Version(N)`, drift detection, online blue-green rebuild (zero-downtime) | [mongo-schema-evolution](https://claudioschirmer.github.io/omnicore/#mongo-schema-evolution) |

### Transports & surfaces
| Capability | In one line | Docs |
|---|---|---|
| **Handler invariance** | One handler serves every surface below — attach, don't rewrite | [handler-invariance](https://claudioschirmer.github.io/omnicore/#handler-invariance) |
| REST + OpenAPI | Fiber routes; OpenAPI 3.1 + Swagger UI generated from the same DTOs | [openapi](https://claudioschirmer.github.io/omnicore/#openapi) |
| gRPC | Own surface over Connect; pb↔DTO bridge boot-checked at register | [grpc](https://claudioschirmer.github.io/omnicore/#grpc) |
| GraphQL | Own surface, Relay connections, per-field authz, reflected schema | [graphql](https://claudioschirmer.github.io/omnicore/#graphql) |
| Integration events | Async cross-service facts; at-least-once, idempotent consumers | [integration-events](https://claudioschirmer.github.io/omnicore/#integration-events) |

### Security, config & ops
| Capability | In one line | Docs |
|---|---|---|
| JWT authentication | Local validation (JWKS / PEM, RSA·ECDSA·Ed25519); optional revocation cache | [auth-middleware](https://claudioschirmer.github.io/omnicore/#auth-middleware) |
| Token issuance | A service mints its own JWTs — key rotation, opaque rotating refresh tokens; validate side unchanged | [token-issuance](https://claudioschirmer.github.io/omnicore/#token-issuance) |
| Authorization | 3 concentric layers: permission gate · owner rules · tenant scoping | [authz-seams](https://claudioschirmer.github.io/omnicore/#authz-seams) |
| Outbound HTTP | `httpclient` from YAML: retry, cache, breaker, HMAC, OAuth2, streaming | [httpclient](https://claudioschirmer.github.io/omnicore/#httpclient) |
| Cache | One `cache.Cache` port; private vs shared enforced by DI; memory/redis/custom | [cache-subsystem](https://claudioschirmer.github.io/omnicore/#cache-subsystem) |
| Request origin | `http.trustProxy` declares the proxies in front, and the client address is then resolved **rightmost-untrusted** — an edge that appends cannot let a caller forge it. Absent, the spoof-proof socket peer. Reaches `c.IP()`, the access log, the server span, `AppContext.ClientIP()` and the audit trail from one declaration | [yaml](https://claudioschirmer.github.io/omnicore/#yaml-reference) · [app-context](https://claudioschirmer.github.io/omnicore/#app-context) |
| Bootstrap & YAML | `Run` / `Build` / `Serve`; one profile file drives everything | [bootstrap](https://claudioschirmer.github.io/omnicore/#bootstrap) · [yaml](https://claudioschirmer.github.io/omnicore/#yaml-reference) |
| Migrations · Tracing · i18n | Numbered SQL migrations · OTel opt-in · 7 translation catalogs | [migrations](https://claudioschirmer.github.io/omnicore/#migrations) · [tracing](https://claudioschirmer.github.io/omnicore/#tracing) |

---

## Relational backends — one seam, many engines

The relational layer is **backend-agnostic by design**. **PostgreSQL**, **MySQL**, **SQL Server**,
**Oracle** (Oracle Database 23ai or higher) and **SQLite** are all first-class today: you link one at build time with a
build tag and select the active dialect at runtime via `relational.dialect`. All are consumers of a single *engine seam* — the domain and
application layers never name a vendor, write raw SQL, or pronounce a physical identifier, so a
service's business code is identical whichever engine backs it.

**SQLite** is the pure-Go (cgo-free), single-node, MVP/self-executable backend: `CGO_ENABLED=0 go build -tags sqlite`
gives a single static binary that boots against a plain `app.db` file — no Docker, and (combined with the infra-optional
boot) no Mongo and no broker. It serves relational views only (SQLite has no CDC source), the deliberate degraded
posture for standing up a working service before the distributed stack is in place.

Because every engine plugs into that same seam, **adding a new relational backend is an isolated
seam implementation, not a change that ripples through your services** — SQL Server joined
PostgreSQL and MySQL through exactly that seam, Oracle followed the same way, and SQLite after it: one more
engine package behind its build tag, each time.

→ [Architecture · the engine seam](https://claudioschirmer.github.io/omnicore/#architecture) · [Bootstrap · build tags & dialect](https://claudioschirmer.github.io/omnicore/#bootstrap)

---

## Handler invariance — attach, don't rewrite

A handler is a pure `pipeline.Handler[TReq, TRes]`. It never learns which surface invoked it.
The surface decides only the I/O shape; everything below the boundary — `BuildRules`
validation, the sealed `ValidEntity`, the outbox, the audit row — is identical. Serve a new
channel by attaching the same instance to another wrapper.

| Handler family | Attaches to | Same instance across |
|---|---|---|
| **Command** (write) | HTTP · broker (Kafka/NATS) · GraphQL mutation · gRPC unary | one `pipeline.Handler[TCmd, TResult]` |
| **Query** (read) | HTTP · CSV · Excel · GraphQL query · gRPC unary | one `pipeline.Handler[TQ, queries.Page]` |

```go
// one handler instance...
h := &handlers.InsertCommandHandler[*User, *commands.InsertUserCommand, commands.InsertUserResult]{Repo: repo}

fwweb.CommandWithBody(d.Pipeline, requests.InsertUserRequest{},                 // ...over HTTP
    requests.InsertUserResponse{}.FromResult, h, fiber.StatusCreated)
reg.From("partners").On("partnerOnboarded", requests.PartnerOnboardedRequest{}, h) // ...off the broker
gql.Register(fwgraphql.MutationWithBody[requests.InsertUserRequest](            // ...as GraphQL
    "createUser", requests.InsertUserResponse{}.FromResult, h))
grpcReg.Register(fwgrpc.CommandWithBody[usersv1.CreateUserRequest, usersv1.CreateUserResponse]( // ...as gRPC
    usersv1connect.UsersServiceCreateUserProcedure,
    requests.InsertUserRequest{}, requests.InsertUserResponse{}.FromResult, h))
```

The async (broker) path additionally gets transactional emission, at-least-once delivery, and an
operator retry surface — without the handler being aware of any of it.
→ [Handler invariance](https://claudioschirmer.github.io/omnicore/#handler-invariance)

---

## Install

```bash
go get github.com/ClaudioSchirmer/omnicore@latest
```

Requires Go 1.21+ (`log/slog` and generics).

## Quick start

```go
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

`bootstrap.Run` reads `microservice.${APP_PROFILE}.yaml`, wires the relational backend
(Postgres/MySQL/SQL Server/Oracle) + Mongo + the message transport (Kafka or NATS) + Fiber, applies migrations, registers the `GET /livez` + `GET /readyz` probes, mounts your Features, starts the `SyncEngine`
when views exist, and serves until `SIGINT`/`SIGTERM`.
→ [Bootstrap](https://claudioschirmer.github.io/omnicore/#bootstrap)

## A complete endpoint, end to end

A `POST /users` that persists a `User` aggregate with body validation, transactional outbox,
audit row, and Mongo projection — three small files:

```go
// domain/user.go — pure business rules, zero IO
type User struct {
    domain.AggregateRoot
    Name, Email string
    Phone       *string
}

func (u *User) BuildRules(_ string, _ domain.Service, r *domain.Rules) {
    r.IfInsertOrUpdate(func() {
        if u.Email == "" {
            r.AddNotification("Email", domain.RequiredFieldNotification{})
        }
    })
}
```

```go
// application/commands/insert_user.go — application boundary (Cmd owns input + output)
func (c InsertUserCommand) ToEntity(_ *configuration.AppContext) *User {
    return &User{Name: c.Name, Email: c.Email, Phone: c.Phone}
}
func (c InsertUserCommand) FromEntity(_ *configuration.AppContext, u *User) InsertUserResult {
    return InsertUserResult{ID: *u.GetID(), Name: u.Name, Email: u.Email, Phone: u.Phone}
}
```

```go
// web/user_routes.go — register the route
users.Post("/", fwweb.CommandWithBody(d.Pipeline,
    requests.InsertUserRequest{}, requests.InsertUserResponse{}.FromResult,
    &handlers.InsertCommandHandler[*User, *InsertUserCommand, commands.InsertUserResult]{Repo: userRepo},
    fiber.StatusCreated))
```

That single call gives you, for free:

- ✅ **Schema validation** at the wire → `400` + `SchemaViolationNotification`
- ✅ **Business validation** via `BuildRules` → `422` + typed notification
- ✅ **Unique-constraint mapping** (PG `23505`) → `409`
- ✅ **Aggregate persistence** — root + every child row in one `pgx.Tx`
- ✅ **Transactional outbox + audit** row in the same transaction
- ✅ **Async projection** — Debezium → the broker (Kafka/NATS) → `SyncEngine` upserts the Mongo view
- ✅ **Typed Result** projected to the wire (no JSON tags below `web/`)

Swap in `UpdateCommandHandler`, `PartialUpdateCommandHandler`, `ArchiveCommandHandler`,
`UnarchiveCommandHandler`, or `DeleteCommandHandler` — the wiring keeps the same shape.
Manual handlers are a **sibling** path, not a poorer one: same envelope, same notification
semantics, same audit guarantees. → [CommandHandler](https://claudioschirmer.github.io/omnicore/#command-handler)

---

## Documentation

- 📖 **[Documentation site](https://claudioschirmer.github.io/omnicore/)** — the public manual (published from [`docs/`](docs/) via GitHub Pages); the consumer's source of truth for every exported API.
- 📝 **[`CHANGELOG.md`](CHANGELOG.md)** — release notes (Keep a Changelog, SemVer; the API may evolve through `0.x.y`).
- 🧪 **[`omnicore-example-users`](https://github.com/ClaudioSchirmer/omnicore-example-users)** — reference service exercising every feature, plus end-to-end QA suites against real Postgres/MySQL/SQL Server/Oracle + Mongo + Kafka/NATS + Debezium + Keycloak + Redis.

## Stack

Fiber v3 (HTTP) · connectrpc.com/connect (gRPC) · pgx v5 (PostgreSQL) · go-sql-driver (MySQL) · go-mssqldb (SQL Server) ·
go-ora (Oracle) · modernc.org/sqlite (SQLite) · mongo-driver v2 (MongoDB 5.2+) · segmentio/kafka-go (Kafka) · nats.go (NATS) ·
golang-migrate v4 (SQL migrations) · golang-jwt v5 + MicahParks/keyfunc (JWT).

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
