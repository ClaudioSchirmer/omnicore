# Microservice Intercommunication — Problem statement

## Status

**Paused.** Resolution depends on `hookfixes.md` landing first. This file holds the problem statement so the context is not lost while the prerequisite is being worked on.

## What we have today

Two communication mechanisms already exist between services in the stack:

- **Sync HTTP via `infra/httpclient`** — typed `Call[Req, Resp]` over the per-service HTTP transport registry. Handler A calls a port declared in domain; the adapter in `infra/external/<svc>.go` performs the HTTP round-trip and maps vendor DTOs to domain types. Used when "A needs an answer from B in order to respond to its own caller". Temporal coupling; additive latency in chain; cascading failures.

- **Read-side cross-service composition via `UpstreamSubscription` + `FromMongo`** — service A publishes domain events to its own Kafka topic (out of the existing PG outbox + Debezium pipeline); service B declares an `UpstreamSubscription` that materializes A's events into a local Mongo collection in B's database; B's views embed that collection via `fwinfra.FromMongo("users").On("buyer_id")`. The framework's `UpstreamSubscriber` keeps the local projection consistent and ripples recompose to every B view that embeds the collection. Used when "B's queries need to show data owned by A". B never reads from A's DB and never calls A on the request path.

Both surfaces are documented in `CLAUDE.md` and `DOCS.html` and are the canonical paths today.

## The gap

There is **no canonical write-side async path**. Concretely:

- A handler in service A cannot today say "let B react to this event" without either (a) calling B over HTTP synchronously, which couples A's response latency to B's availability, or (b) reusing the read-side projection pattern as a back-channel, which is a misuse — `UpstreamSubscription` is for read-side state mirroring, not for triggering reactions.

- Without a canonical async path, developers will improvise async over `HttpClient` — POST + timeout + ignore-the-result, fire-and-forget calls with retries hidden in middleware, "background" goroutines launched mid-request. Every improvisation reinvents idempotency, ordering, retry, and observability — badly, and inconsistently across services.

The framework's value proposition (one canonical path per concern) demands that we close this gap before the improvisations start.

## Why this is paused

While designing the producer-side API shape, the conversation surfaced a structural gap in the Orchestrator's `before/after` hooks:

- Hooks exist on the Orchestrator's API but do NOT run inside the same `pgx.Tx` as the data write (TX is opened/committed inside `Postgres.Insert/Update/etc.`).
- Auto Command Handlers (`InsertCommandHandler`, etc.) pass `nil, nil` for both hooks — there is no way for the developer using the canonical path to inject code into the lifecycle.
- The `after` hook receives only the `domain.ID`; the ValidEntity is not threaded back. The TX has already committed by the time `after` runs.

Any in-TX integration-event emission (the cornerstone of cross-service async with atomicity) depends on the hook surface being usable. Fix the hooks first, then this file resumes.

See `hookfixes.md` for the prerequisite work.

## Open questions to address once the prerequisite lands

Listed here as memory aid. Numbering is purely indicative; resolution order TBD when this file resumes.

- Broker choice and DB-level mechanics for cross-service event flow.
- Coexistence of the existing `outbox` (CDC) with any new table dedicated to cross-service business facts; in-TX choreography vs the existing `audit_events` row.
- Producer-side API shape — who emits, where the event lives until it is persisted, how it gets onto the wire.
- Consumer-side reaction shape — port declaration, dispatch path, idempotency.
- Event DTO ownership (per-consumer copy vs shared contract).
- Topic naming convention.
- Idempotency, ordering, retry, DLQ.
- Schema versioning and evolution.
- Observability — how events surface in audit and in `slog`.
- Boot validation and declaration (yaml + scan guards).
- Public surface impact — `DOCS.html`, `CLAUDE.md`, examples in `omnicore-example-users`.

Discussion resumes after `hookfixes.md` closes.
