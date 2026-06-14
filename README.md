# omnicore

[![Go Reference](https://pkg.go.dev/badge/github.com/ClaudioSchirmer/omnicore.svg)](https://pkg.go.dev/github.com/ClaudioSchirmer/omnicore)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

DDD + CQRS infrastructure for Go microservices.

omnicore is a framework that handles the wire-up of every layer of a DDD service — write side (sealed `ValidEntity` types, `AggregateRoot` with cascade, transactional outbox, audit), read side (Mongo-projected views with keyset pagination, sparse responses, sort allowlists, cross-service composition via Kafka), HTTP transport (Fiber-based with auto-generated OpenAPI/Swagger UI, JWT auth, three-layer authorization), declarative outbound HTTP (`httpclient` with retry/cache/breaker/HMAC signing), and operational concerns (migrations, Mongo schema evolution, declarative YAML config).

## Install

```bash
go get github.com/ClaudioSchirmer/omnicore@v0.4.0
```

Requires Go 1.21+ (uses `log/slog` and generics extensively).

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

Boot reads `microservice.${APP_PROFILE}.yaml` (e.g. `microservice.dev.yaml`), wires Postgres + Mongo + Kafka + Fiber, auto-registers `GET /health`, applies migrations, starts the SyncEngine when any feature declares views, and serves HTTP until SIGINT/SIGTERM.

A feature is a struct that implements `bootstrap.Feature` (or `bootstrap.ReadableFeature` if it has views). It owns one aggregate root: declares the entity, repository, command/query handlers, and HTTP routes — all wired in its `Mount(app *fiber.App, d bootstrap.Deps)` method. The framework's Auto Command Handlers (`InsertCommandHandler[T, *Cmd, TResult]`, `UpdateCommandHandler`, etc.) and Auto Query Handlers (`FindByIDQueryHandler`, `FindByParamsQueryHandler`) cover trivial CRUD in one line; manual handlers coexist for cross-service work.

## Documentation

The full manual is published as a single HTML file in this repo: [`DOCS.html`](DOCS.html). It is the consumer-facing source of truth for every public API — read it before writing handlers, repositories, or features.

A reference service consuming this framework lives at [`github.com/ClaudioSchirmer/omnicore-example-users`](https://github.com/ClaudioSchirmer/omnicore-example-users) — sandbox + canonical example.

## Stack

- Fiber v2 (HTTP)
- pgx v5 (PostgreSQL)
- mongo-driver v2 (MongoDB)
- segmentio/kafka-go (Kafka)
- golang-migrate/migrate v4 (SQL migrations)
- golang-jwt/jwt v5 + MicahParks/keyfunc (JWT)

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
