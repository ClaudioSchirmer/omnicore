# Persistence lifecycle hooks — gap analysis

## Status

Design complete; ready for implementation. All eleven topics (1-11) are closed in the "Closed decisions" section below. Each decision block is the contract for its area. The "Implementation prompt" section at the bottom carries the self-contained directive for the implementation round.

## Why this file exists

While designing cross-service async communication (`msintercomunication.md`), the conversation surfaced that the Orchestrator's `before/after` hooks do not behave the way the API's name and shape suggest. The `msintercomunication.md` work is paused until this gap is closed, because any in-TX integration-event emission (the cornerstone of producer-side async with atomicity) depends on a usable hook surface.

This file is scoped strictly to the hook lifecycle. It is not about cross-service events; it is about the underlying extension point. Once it closes, `msintercomunication.md` resumes with a defined surface to build on.

## The current state (verified in source)

### Orchestrator hooks exist but run OUTSIDE the TX

`application/persistence/orchestrator.go` exposes `before` and `after` callbacks on every write method (`Insert`, `Update`, `Archive`, `Unarchive`, `Delete`, `FindByID`). Concretely for `Insert`:

```go
func (o *Orchestrator[T]) Insert(insertable, beforeInsert, afterInsert) (ID, error) {
    if beforeInsert != nil { beforeInsert() }        // BEFORE the TX is opened
    id, err := o.Repo.Insert(o.Context, insertable)  // TX opens, writes, COMMITS inside this call
    if afterInsert != nil { afterInsert(id) }        // AFTER COMMIT
    return id, nil
}
```

The TX itself is owned by the persister (`infra/executor.go`), not by the Orchestrator. The simple-path `Postgres.Insert` (lines 28–65) does:

```go
tx, err := p.pool.Begin(ctx)
defer tx.Rollback(ctx)
// data write + outbox + audit_events (when database destination active)
tx.Commit(ctx)
// post-commit: slog echo
```

So the effective sequence today is:

```
beforeInsert()         ← outside TX
BEGIN
  data write
  outbox INSERT
  audit_events INSERT  (when database destination is active)
COMMIT
afterInsert(id)        ← outside TX, AFTER commit
slog echo              ← outside TX
```

Consequences:

- A hook that wanted to write to PG atomically with the aggregate cannot — `after` runs after COMMIT, so any write it does is a second TX (dual-write risk).
- A hook that wanted to inspect the persisted ValidEntity cannot — `after` receives only `domain.ID`. The entity is in the handler, the TX has closed, and any state the persister could have updated mid-TX (generated columns, server-side defaults beyond `id`) is invisible.
- Same shape for `Update`/`Archive`/`Unarchive`/`Delete`, except those callbacks take zero arguments (no `id` returned).

### Auto Command Handlers pass `nil, nil`

The canonical write path uses the Auto handlers in `application/handlers/`. They all build the Orchestrator and call its method with both hook slots nil. `application/handlers/insert.go` line 41:

```go
orch := persistence.NewOrchestrator(h.Repo, ctx)
id, err := orch.Insert(insertable, nil, nil)   // <— hooks are unreachable
```

Same for `update.go`, `partial_update.go`, `archive.go`, `unarchive.go`, `delete.go`. A developer using the canonical (Auto) path has no way to inject behaviour at any point of the lifecycle — neither pre-validation, nor pre-commit, nor post-commit.

### Manual handlers can pass non-nil callbacks but get nothing useful

A handwritten handler (the "manual path") can pass functions into `orch.Insert(insertable, before, after)`, but the callbacks still run outside the TX. The escape hatch is documentation-only — the hooks don't deliver atomicity even when the developer reaches for them.

## What "fixing the hooks" should deliver

Listed here as scope reminder; the concrete shape is the matter of the topics below. At minimum:

1. **Hooks invoked INSIDE the TX**, between data write + outbox + audit_events and the COMMIT. A hook error must roll the whole TX back.
2. **A useful payload reaches the in-TX hook** — ValidEntity, generated `id`, request `AppContext`, plus a TX handle the hook can use to write companion rows in the same transaction.
3. **The canonical (Auto) path exposes the slot.** A consumer using `InsertCommandHandler[T, *Cmd, TResult]` must be able to opt into the in-TX hook without abandoning the Auto handler. Failing this means every endpoint that needs an in-TX side effect (events, denormalizations, cross-table updates) regresses to a manual handler — which is exactly what `msintercomunication.md` would have to assume, defeating its premise.
4. **No leakage of `*pgx.Tx` into the domain layer.** The TX handle exposed to the hook is a thin framework type (e.g. `fwinfra.TxHandle`) with the minimal surface the hook needs.
5. **Backwards-compat for the pre-commit-irrelevant uses** of `before/after` already in the field, OR explicit removal with a migration story. The maintainer decides.

## Closed decisions

### 1. Hook positions inside the TX [CLOSED 2026-06-13]

**Decision.** Two slots, both fire inside the TX. No post-COMMIT slot.

```
BEGIN
  ⬇ afterBegin()                          ← position A
  data write
  outbox INSERT
  audit_events INSERT (when configured)
  ⬇ beforeCommit(id, …)                   ← position D
COMMIT
```

**Rationale.** The framework exposes exclusively what the consumer cannot simulate from outside the orchestrator call. Pre-persistence and post-COMMIT effects are achievable by running code before / after the `orch.Method()` call site; they don't justify framework surface. Atomic in-TX side effects ARE NOT achievable from outside — that's why these two positions exist. The names (`afterBegin` / `beforeCommit`) anchor to the TX verbs themselves so the consumer reads the trigger directly.

**Impact.**
- 5 write verbs receive both slots: Insert, Update (covers PUT + PATCH), Archive, Unarchive, Delete.
- FindByID is out: it's a read, no atomic write to pair with.
- No post-COMMIT framework slot. Consumers doing cache invalidation / websocket push / metrics run that code after `orch.Method()` returns (in the handler).

### 2. Hook signature and payload [CLOSED 2026-06-13]

**Decision.** Same generic shape across the 5 write verbs:

```go
type AfterBeginHook[T any]   func(ctx *fwconfig.AppContext, t T,               tx TxHandle) error
type BeforeCommitHook[T any] func(ctx *fwconfig.AppContext, t T, id domain.ID, tx TxHandle) error
```

**Rationale.**
- `ctx *fwconfig.AppContext` — the same request-scoped context the handler receives. Satisfies `context.Context` (cancellation), carries Identity (authz inside the hook), Language.
- `t T` — the consumer's domain entity type (`*User`, `*Order`), not the `Insertable`/`Updatable` wrapper. Aligns with the Auto handler's existing T parameter. Wrapper metadata (Signature, ActionName, Events) accessible via BaseEntity-promoted methods on the entity.
- `id domain.ID` — only on `beforeCommit`, populated for all verbs (Insert: generated by the write; others: known from path). Omitted from `afterBegin` because for Insert it doesn't exist yet and for the others it's already available via `t.GetID()`.
- `tx TxHandle` — application-layer interface (Topic 3). Hook uses it to write companion rows / read state in the same TX as the framework's own writes.
- `error` return — non-nil ⇒ ROLLBACK. Full error matrix (panic, typed constraint violation) is Topic 7.

**Impact.**
- PartialUpdate (PATCH) does not have its own slots — it uses the Update slot (same SQL fingerprint on the persister side).
- FindByID does not have slots.
- Same shape for all 5 verbs: framework declares 2 hook types, parameterized by T; each verb's Auto handler accepts both.

### 3. Application-layer `TxHandle` surface [CLOSED 2026-06-13]

**Decision.** `TxHandle` is an application-layer interface in `application/persistence/`. Concrete impl lives in `infra/` wrapping `*pgx.Tx`. Application code never imports pgx.

```go
package persistence

import "context"

type TxHandle interface {
    Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
    Query(ctx context.Context, sql string, args ...any) (Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) Row
}

type CommandTag struct {
    RowsAffected int64
}

type Rows interface {
    Next() bool
    Scan(dest ...any) error
    Err() error
    Close()
}

type Row interface {
    Scan(dest ...any) error
}
```

**Rationale.** Wrapping over native pgx types pays a one-time implementation cost in infra adapters (3 structs, ~10 trivial methods) to keep `application/` pgx-free. Future-proofs the abstraction if the framework ever grows another driver — consumer code under `application/commands/` survives intact. Layer rule preserved cleanly via the standard Go idiom "interface in the consumer layer, impl in the provider layer" (same pattern as `domain.Repository[T]` and `application/queries.ViewReader`).

**Impact.**
- `application/persistence/` gains 4 types (TxHandle, CommandTag, Rows, Row).
- `infra/` gains 3 adapter structs (`pgxTxHandle` / `pgxRows` / `pgxRow`); `CommandTag` is constructed inline as a plain struct.
- Deliberately excluded from the interface: `Begin`/`Commit`/`Rollback` (lifecycle is the framework's), `Conn` (full escape), `CopyFrom`/`SendBatch`/`Prepare`/`LargeObjects` (advanced features — graduates to manual handler with `WithTx(ctx, fn)`, Topic 5).
- Consumer owns the iterator: `defer rows.Close()` is the convention; documented in godoc.

### 6. Fate of the existing `before` / `after` slots [CLOSED 2026-06-13]

**Decision.** Remove the `before` / `after` callback parameters from every Orchestrator method. Maintainer explicitly authorized.

```go
// Before:
Orchestrator.Insert(insertable, beforeInsert func() error, afterInsert func(domain.ID) error) (ID, error)
Orchestrator.Update(updatable, beforeUpdate, afterUpdate func() error) error
Orchestrator.Delete(deletable, beforeDelete, afterDelete func() error) error
Orchestrator.Archive(archivable, beforeArchive, afterArchive func() error) error
Orchestrator.Unarchive(unarchivable, beforeUnarchive, afterUnarchive func() error) error
Orchestrator.FindByID(id, beforeFind func() error, afterFind func(TEntity) error) (TEntity, error)

// After:
Orchestrator.Insert(insertable) (ID, error)
Orchestrator.Update(updatable) error
Orchestrator.Delete(deletable) error
Orchestrator.Archive(archivable) error
Orchestrator.Unarchive(unarchivable) error
Orchestrator.FindByID(id) (TEntity, error)
```

**Rationale.** The semantic the old hooks were trying to express (pre / post persistence orchestration) is achievable without framework surface: code that needs to run before the orch call runs before, code that needs to run after runs after. The atomic in-TX work that the names misleadingly suggested is delivered by the new `afterBegin` / `beforeCommit` slots that fire from the persister side, where the TX actually lives.

**Impact.**
- 6 Auto Command Handlers in `application/handlers/*.go` lose the trailing `nil, nil` args on every `orch.Method()` call (trivial cascade).
- `Orchestrator` doc-comment lines 21-23 ("Pre/post hooks remain so callers can opt into custom orchestration...") get rewritten — the justification dies with the params.
- `Orchestrator` shrinks to `Repo + Context + Logger` — a thin typed pass-through. Whether it survives as a layer at all is Topic 9.
- New hooks (`afterBegin`/`beforeCommit`) do NOT live on the Orchestrator. They reach the persister via the Auto handler's path (Topic 4) or via the manual path (Topic 5).

### 4. Exposure on the canonical (Auto) path [CLOSED 2026-06-13]

**Decision.** Optional methods on the Cmd, detected via type assertion. Method names follow Go's "method-named-after-the-event" convention — `AfterBegin` / `BeforeCommit`, no prefix. (The original lock cravado an `On` prefix; revisited the same day when the asymmetry with the manual path's `WithAfterBegin` / `WithBeforeCommit` functional options surfaced visually in side-by-side example blocks. Each surface follows its own idiom: method on a struct named after the event it responds to, free function configured via `With*` per the functional-options idiom.)

**Shape.**

Provider interfaces (declared in `application/persistence/`, alongside `TxHandle` from Topic 3 and the function types from Topic 2):

```go
type AfterBeginHookProvider[T any] interface {
    AfterBegin(ctx *fwconfig.AppContext, t T, tx TxHandle) error
}

type BeforeCommitHookProvider[T any] interface {
    BeforeCommit(ctx *fwconfig.AppContext, t T, id domain.ID, tx TxHandle) error
}
```

Auto handler detects each independently at the top of `Handle`:

```go
// application/handlers/insert.go
func (h *InsertCommandHandler[T, Cmd, TResult]) Handle(ctx *fwconfig.AppContext, cmd Cmd) (TResult, error) {
    // ... build entity, validate, build insertable ...
    var afterBegin AfterBeginHook[T]
    if p, ok := any(cmd).(AfterBeginHookProvider[T]); ok {
        afterBegin = p.AfterBegin
    }
    var beforeCommit BeforeCommitHook[T]
    if p, ok := any(cmd).(BeforeCommitHookProvider[T]); ok {
        beforeCommit = p.BeforeCommit
    }
    // ... pass to persister (Topic 9 fixes the Orchestrator/persister API) ...
}
```

Consumer declares only the methods they need:

```go
// application/commands/insert_user.go
type InsertUserCommand struct {
    pipeline.CommandBase
    Name  string
    Email string
}

func (c *InsertUserCommand) ToEntity(_ *fwconfig.AppContext) *User { ... }
func (c *InsertUserCommand) FromEntity(_ *fwconfig.AppContext, u *User) InsertUserResult { ... }

// Optional — declared only when an in-TX side effect is needed.
func (c *InsertUserCommand) BeforeCommit(
    ctx *fwconfig.AppContext, u *User, id domain.ID, tx persistence.TxHandle,
) error {
    _, err := tx.Exec(ctx,
        `INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload) VALUES ($1,$2,$3,$4)`,
        "users", id.String(), "UserActivationRequired", `{"user_id":"`+id.String()+`"}`,
    )
    return err
}

// Recommended compile-time safety to prevent silent typos:
var _ persistence.BeforeCommitHookProvider[*User] = (*InsertUserCommand)(nil)
```

**Rationale.**
- Co-location: hooks live next to `ToEntity` / `FromEntity` in `application/commands/<cmd>.go`. Wire-up in `web/` stays untouched.
- Aligned with existing pattern: "Cmd owns the application boundary" already covers input (`ToEntity`/`ApplyTo`) and output (`FromEntity`); in-TX side effects are a natural extension of the same principle.
- Per-endpoint hooks naturally — each Cmd has its own optional methods.
- Limitation accepted: in-TX hooks should not depend on external IO (cache, Kafka publish, webhook are post-COMMIT concerns). Consumer that needs external deps inside a TX graduates to the manual path (Topic 5).
- Method names without prefix follow Go's idiom for struct methods named after the event (`Read`, `Close`, `Unwrap`, `ServeHTTP`). The functional-options counterparts in Topic 5 (`WithAfterBegin` / `WithBeforeCommit`) follow the orthogonal `With*` idiom for call-site configuration; each surface stays internally idiomatic without forcing a single naming on both.

**Impact.**
- Types added in `application/persistence/`: `AfterBeginHookProvider[T any]`, `BeforeCommitHookProvider[T any]`.
- 5 Auto Command Handlers (Insert / Update / Archive / Unarchive / Delete) gain two type-assertion checks each at the top of `Handle`. `PartialUpdateCommandHandler` shares the same provider interfaces as `UpdateCommandHandler` (both fire the same persister Update slot — single SQL fingerprint).
- Per-request runtime cost: 2 type assertions per Handle (~10ns each); negligible.
- Constraint on T in the provider interfaces is `T any` — intentionally loose; the consuming handler's constraint is what binds T to a real entity type.
- Compile-time safety pattern (`var _ persistence.BeforeCommitHookProvider[*T] = (*Cmd)(nil)`) documented in CLAUDE.md as recommended boilerplate at the bottom of any Cmd file declaring an `AfterBegin` / `BeforeCommit` method. Framework does not enforce; consumer opt-in to catch typos at `go build` time.

### 5. Exposure on the manual path [CLOSED 2026-06-13]

**Decision.** Manual handler uses the same mechanism as the Auto path, via explicit closures passed as functional options to the write methods. No lower-level `WithTx(ctx, fn)` escape hatch — manual = "Auto without the Cmd magic", same persister surface.

**Shape.**

Two option constructors in `application/persistence/`:

```go
package persistence

type WriteOption[T any] func(*writeOptions[T])

type writeOptions[T any] struct {
    afterBegin   AfterBeginHook[T]
    beforeCommit BeforeCommitHook[T]
}

func WithAfterBegin[T any](fn AfterBeginHook[T]) WriteOption[T] {
    return func(o *writeOptions[T]) { o.afterBegin = fn }
}

func WithBeforeCommit[T any](fn BeforeCommitHook[T]) WriteOption[T] {
    return func(o *writeOptions[T]) { o.beforeCommit = fn }
}
```

The 5 Repo write methods gain a variadic `opts ...WriteOption[T]` parameter. Manual handler passes closures directly:

```go
func (h *CustomHandler) Handle(ctx *fwconfig.AppContext, cmd *MyCmd) (Result, error) {
    user := buildUser(cmd)
    insertable, err := domain.GetInsertable(user, h.svc, "MyAdminCreate")
    if err != nil { return Result{}, err }

    id, err := h.repo.Insert(ctx, insertable,
        persistence.WithBeforeCommit[*User](func(
            ctx *fwconfig.AppContext, u *User, id domain.ID, tx persistence.TxHandle,
        ) error {
            _, err := tx.Exec(ctx,
                `INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload) VALUES ($1,$2,$3,$4)`,
                "users", id.String(), "ExtraIntegrationEvent", `{"...":"..."}`,
            )
            return err
        }),
    )
    if err != nil { return Result{}, err }
    return Result{ID: id}, nil
}
```

**Convergence.** Auto handler from Topic 4, after detecting the Cmd's `AfterBegin`/`BeforeCommit` methods via type assertion, builds the same `WriteOption[T]` values and calls the same `Repo.Insert`. There is ONE surface in the persister; Auto and manual differ only in how the closures originate (Cmd method vs explicit closure).

**Rationale.**
- Parity rule satisfied at both outcome and shape — consumer of the manual path recognizes the slot names (`WithAfterBegin`/`WithBeforeCommit`) immediately. Same mental model as Auto's `AfterBegin`/`BeforeCommit`.
- Single internal surface in the persister. No "Auto flow" vs "Manual flow" branching — same option mechanism, two ways to populate it.
- Surface added is minimal: two public option constructors + variadic param on the 5 Repo write methods.
- Multi-aggregate atomic composition stays outside the canonical path — aligned with the aggregate-as-consistency-boundary + outbox granularity B design. A consumer that genuinely needs multi-aggregate atomicity writes a custom Repository method that opens its own TX internally (infra-side code, outside framework primitives) — rare case, manual handling acceptable.
- If a real use case ever justifies a `WithTx(ctx, fn)` surface, that's a separate Topic under explicit approval. Pre-emitting the surface now would pay maintenance cost for a feature that may never come.

**Impact.**
- `application/persistence/` gains `WriteOption[T any]` and the two constructors `WithAfterBegin[T]` / `WithBeforeCommit[T]`.
- 5 Repo write methods (`Insert`, `Update`, `Archive`, `Unarchive`, `Delete`) gain `opts ...WriteOption[T]` parameter.
- Auto path's call site (inside each Auto Command Handler) changes from "no options" to "options derived from Cmd type assertion."
- Manual path writes hooks as plain closures at the call site — co-located with the handler's logic, no Cmd boilerplate required.
- No `WithTx` exposed. Consumer needing TX control beyond what `Repo.Method(..., opts...)` provides writes a custom Repository method.

### 7. Error semantics [CLOSED 2026-06-13]

**Decision.** Linear first-failure semantics. Hook errors propagate verbatim, preserving `NotificationCarrier` identity. Panic in hook caught by `pipeline.Run`'s existing recover — no new recover layer at persister level. No auto-binding of constraint violations inside the hook's `tx.Exec`. Persister emits a `slog.Warn` observability line whenever a hook returns non-nil error.

**Matrix.**

| Scenario | Action |
|---|---|
| `afterBegin` returns nil | continue to data write |
| `afterBegin` returns non-nil error | ROLLBACK + error propagates verbatim |
| `afterBegin` panics | `defer tx.Rollback()` fires, panic bubbles to `pipeline.Run` → `Result.Exception` |
| `beforeCommit` returns nil | proceed to COMMIT |
| `beforeCommit` returns non-nil error | ROLLBACK + error propagates verbatim |
| `beforeCommit` panics | same as `afterBegin` panic |
| Hook calls `tx.Exec` and it succeeds | hook continues normally |
| Hook calls `tx.Exec` and it fails | `tx.Exec` returns raw pgx error; hook decides (return raw OR translate to typed notification via `infra.SingleNotificationError(...)`) |

**Detailed semantics.**

1. **Type identity preserved end-to-end.** Hook returning a `NotificationCarrier` (e.g., `domain.SingleNotificationError(..., EmailLockedNotification{}, ...)`) propagates verbatim through the persister. `pipeline.Run`'s existing `errors.As(err, &carrier)` check picks it up and produces `Result.Failure` with the typed notification translated. Wire response carries the canonical envelope at the notification's `Semantic()` HTTP status (422 / 404 / 409 / etc.). Hook returning a raw (non-carrier) error follows the framework's existing non-carrier path → `Result.Exception` (500).

2. **No auto-binding of constraint violations inside `tx.Exec`.** The `infra.ConstraintBinding` mechanism (PG SQLSTATE `23505` → typed notification, configured via `Constraints` map on the Repository) lives at the Repository level. `TxHandle` is escape-hatch low-level — returns raw pgx errors unchanged. Consumer that wants "constraint X → MyNotification" inspects the pgError and returns the typed notification explicitly. Rationale: hook's `tx.Exec` typically writes to custom tables the framework does not know about; their constraints are the consumer's. Auto-binding would be confusing magic limited to the parent Repository's declared constraints.

3. **No new recover at persister level.** `pipeline.Run` is the single recover point. When a hook panics, `defer tx.Rollback(ctx)` fires (panic correctly rolls back), the panic propagates up through the persister, and `pipeline.Run`'s existing `defer/recover` catches it → `Result.Exception`. Persister does NOT recover.

4. **Linear first-failure semantics.** Sequence inside the TX: `afterBegin → data write → outbox INSERT → audit_events INSERT → beforeCommit → COMMIT`. The first error stops the sequence; subsequent steps don't run. A hook never observes "data write failed" because if data write fails, `beforeCommit` (which is after it) never fires. Compensation logic for the "tried but failed" case belongs outside the `orch.Method()` call site, not in a hook.

5. **COMMIT failure is post-hook.** If `beforeCommit` succeeds but COMMIT itself fails (rare: connection drop between last write and COMMIT), persister returns the COMMIT error to the caller. Hook already exited; its work rolls back with everything else. Caller receives the COMMIT error (typically raw → Exception).

6. **Cancellation propagates via context.** The `ctx` the hook receives is the handler's request `*AppContext`. Client disconnect / shutdown / timeout cancels the context; `tx.Exec(ctx, ...)` returns `context.Canceled`; hook returns; ROLLBACK; persister returns the canceled error. `fwweb.ErrorHandler` handles client disconnect gracefully on the wire side.

7. **Observability via `slog.Warn` on hook error (non-blocking).** When a hook returns non-nil error, the persister emits one `slog.Warn` with fields: `verb` (Insert/Update/Archive/Unarchive/Delete), `hookSlot` (afterBegin/beforeCommit), `entityType`, `threadId` (from `AppContext.ID()`), `error` (string). Best-effort — failure to emit slog never blocks the rollback path. Helps forensics ("why did this Insert fail with EmailLockedNotification — which hook fired?") without inspecting code paths. Does not replace audit (audit does not run on ROLLBACK).

**Impact.**
- No new code on the application side — semantics are infra-persister concern.
- Persister gains a `slog.Warn` emission in the hook-error branches of both Insert/Update/Archive/Unarchive/Delete code paths (after the hook returns error, before the explicit `tx.Rollback(ctx)` cleanup).
- `TxHandle.Exec` godoc explicitly documents "returns raw pgx errors; no constraint mapping at this surface."
- CLAUDE.md "HTTP status mapping" section unchanged — new hooks feed the existing carrier-vs-non-carrier dispatch via `pipeline.Run`.

### 8. Aggregate-path coexistence [CLOSED 2026-06-13]

**Decision.** Same two slots, same positions, same generic signature, same firing cardinality. The aggregate path (`infra/aggregate_persister.go`) and the flat path (`infra/postgres.go`) integrate the hooks symmetrically — single internal surface, transparent dispatch.

**Positions in the aggregate sequence (uniform across verbs).**

```
BEGIN
  ⬇ afterBegin()                              ← position A — before any framework write
  
  // Verb-specific sequence (root + children, single TX):
  Insert    : root INSERT → child INSERTs (Added/Constructor)
  Update    : root UPDATE → child INSERT/UPDATE/UPDATE-deleted_at per status
              (Added/Changed/Removed/Constructor)
  Archive   : root UPDATE deleted_at=NOW() → cascade UPDATE children SET deleted_at=NOW()
  Unarchive : root UPDATE deleted_at=NULL → cascade UPDATE children SET deleted_at=NULL
              (via ArchivedFinder)
  Delete    : root DELETE → FK ON DELETE CASCADE handles children at PG level
  
  outbox INSERT                               (single row — granularity B)
  audit_events INSERT                         (when "database" in audit.destinations)
  ⬇ beforeCommit(id, …)                       ← position D — after all framework writes
COMMIT
```

**Contract.**

1. **`afterBegin` fires BEFORE the root write.** Never interleaved between root and children. Before any modifying SQL the framework emits in this TX.

2. **`beforeCommit` fires AFTER the last framework write**, which includes: root write + every child write (per status, or cascade) + outbox INSERT + audit_events INSERT (when active). Before COMMIT.

3. **Hook fires ONCE per `orch.Method()` call.** Not per child. Same cardinality as outbox and audit (granularity B). For an Insert of a User with 3 Addresses, `beforeCommit` runs once, receiving `*User` whose `AggregateRoot` carries all three children. Consumer reads children via existing helpers:

```go
func (c *InsertUserCommand) BeforeCommit(
    ctx *fwconfig.AppContext, u *User, id domain.ID, tx persistence.TxHandle,
) error {
    addresses := domain.GetCurrentItemsOf[Address](&u.AggregateRoot)
    // ... compose extra outbox row(s), denormalization writes, etc.
    return nil
}
```

4. **Same generic signature regardless of aggregate vs flat.** `func(ctx, t T, ... tx TxHandle) error` — T is the root entity type. No `AfterBeginAggregate[T]` separate from `AfterBegin[T]`. **The consumer does not know which dispatch path the persister took** — `entity.AggregateInfo()` is internal routing.

5. **Error semantics from Topic 7 apply identically.** First-failure means: `afterBegin` failure aborts before root write; `beforeCommit` failure rolls back root + N children + outbox + audit_events. `NotificationCarrier` identity preserved; `slog.Warn` on error carries the same fields (`verb`, `hookSlot`, `entityType` = root type, `threadId`, `error`) — no aggregate-specific field, because the consumer-visible contract does not distinguish.

6. **Single `WriteOption[T]` mechanism from Topic 5 feeds both paths.** Persister's internal dispatch between flat-path code and aggregate-path code is invisible to Auto handler (Topic 4) and manual handler (Topic 5); both produce the same `writeOptions[T]` value and the chosen path fires the closures at the analogous positions.

**Impact.**
- `infra/aggregate_persister.go` gains the hook firing logic at the two analogous positions (right after BEGIN, right before COMMIT) — mirroring `infra/postgres.go`'s flat-path firing logic.
- Shared helper recommended: extract the "resolve `writeOptions[T]` + fire `afterBegin` / fire `beforeCommit` with `slog.Warn` on error" pattern into a single internal function consumed by both paths. Prevents drift between the two implementations.
- No new types, no new public surface beyond what Topics 2/3/5 already declared.
- Doc / godoc on the persister explicitly states the firing positions for both paths so a future contributor does not invent a third interpretation.

### 9. Orchestrator API impact [CLOSED 2026-06-13]

**Decision.** Remove the `application/persistence/Orchestrator[T]` type entirely. Maintainer explicitly authorized. Auto handlers and manual handlers both call `Repository[T]` directly, threading `*AppContext` as the first argument and `WriteOption[T]` variadics as the trailing arguments.

**Shape.**

```go
// Before (post-Topic-6, after callback removal):
orch := persistence.NewOrchestrator(h.Repo, ctx)
id, err := orch.Insert(insertable)

// After:
id, err := h.Repo.Insert(ctx, insertable, opts...)
```

`Repository[T]` interface (in `domain/`) accepts the same variadic `opts ...persistence.WriteOption[T]` on every write method, mirroring what Topic 5 already cravado:

```go
package domain

type Repository[TEntity any] interface {
    Insert(ctx Context, t Insertable, opts ...persistence.WriteOption[TEntity]) (ID, error)
    Update(ctx Context, t Updatable, opts ...persistence.WriteOption[TEntity]) error
    Archive(ctx Context, t Archivable, opts ...persistence.WriteOption[TEntity]) error
    Unarchive(ctx Context, t Unarchivable, opts ...persistence.WriteOption[TEntity]) error
    Delete(ctx Context, t Deletable, opts ...persistence.WriteOption[TEntity]) error
    FindByID(id ID) (TEntity, error)
    New() TEntity
}
```

(Caveat on layering: `domain.Repository[T]` now references `persistence.WriteOption[T]`, which lives in `application/persistence/`. This is a domain → application import — currently forbidden by the layer rule. Topic 10/11 must address: either move `WriteOption` and the hook types to `domain/` (since the framework imports them at the domain interface level), or restructure the interface signature so the variadic is consumed at the `BaseRepository` impl layer instead of the `domain.Repository` interface. Implementation-round consideration; doesn't reopen Topic 9.)

**Rationale.**
- The `Orchestrator` post-Topic-6 was pure pass-through — six methods forwarding to the Repository, plus a `WithLogger` setter and a context-bundling convenience. No semantic value remained.
- "One canonical path" rule — keeping the `Orchestrator` alongside direct Repo access creates two valid ways to write the same handler. Removal collapses to one.
- Audit emission already lives in the persister (`Postgres.WithAudit`); hook firing already lives in the persister (Topics 1-8). Orchestrator carried no behavior — only a `Repo` field, a `Context` field, and a `Logger`. Removal sheds 200+ lines of doc-comment fiction.
- Aligned with `feedback_find_canonical_convergence_first.md` — the `Repository[T]` is the canonical convergence point; both Auto and manual handlers reach it; no need for a wrapper layer that adds zero semantics.

**Impact.**
- Files removed: `application/persistence/orchestrator.go` entirely. Public surface removed: `Orchestrator[T]`, `NewOrchestrator`, `WithLogger`, and the 6 forwarding methods (`Insert`/`Update`/`Archive`/`Unarchive`/`Delete`/`FindByID`).
- `application/persistence/` retains the new persistence-layer types from Topics 2/3/5: `TxHandle`, `CommandTag`, `Rows`, `Row`, `AfterBeginHook[T]`, `BeforeCommitHook[T]`, `AfterBeginHookProvider[T]`, `BeforeCommitHookProvider[T]`, `WriteOption[T]`, `WithAfterBegin[T]`, `WithBeforeCommit[T]`.
- 6 Auto Command Handlers in `application/handlers/*.go` change from `orch := persistence.NewOrchestrator(h.Repo, ctx); id, err := orch.Insert(insertable, nil, nil)` to `id, err := h.Repo.Insert(ctx, insertable, opts...)` where `opts` is the slice derived from type assertion on the Cmd.
- Manual handlers in `omnicore-example-users` (if any) migrate from `orch.Method(...)` to `h.Repo.Method(ctx, ...)` — mechanical refactor, sed-friendly within a round.
- CLAUDE.md sections "Orchestrator (`application/persistence/orchestrator.go`)", "Critical invariants" item about orchestrator+audit, "Quick reference — where to add things" entry, "Full request flow (concrete)", and several glossary references all rewrite or drop. Detailed surface impact is Topic 11.

### 10. Test coverage and migration [CLOSED 2026-06-13]

**Decision.** Three-tier test coverage with deep matrix across all 5 verbs and both persister paths. QA E2E cases added to `omnicore-example-users/qa/e2e.sh` in the same round as the framework change. Single-round migration — framework + example service together, no deprecation cycle.

**Test surface.**

**Tier 1 — Persister-level (canonical mechanism).** `infra/postgres_test.go` (flat path) and `infra/aggregate_persister_test.go` (aggregate path).

Per verb (Insert / Update / Archive / Unarchive / Delete) × per slot (afterBegin / beforeCommit), the deep matrix runs:

- A. Hook fires when option provided
- B. Hook receives correct payload — `ctx` is request ctx, `t` is the entity, `id` is set (beforeCommit only), `tx` is functional (smoke `tx.Exec` works)
- C. Hook error rolls back — no row in domain table, no row in `outbox`, no row in `audit_events`
- D. Hook panic rolls back AND propagates panic — recover at test level, verify TX undone
- E. `slog.Warn` emitted on hook error — capture slog output, verify fields (`verb`, `hookSlot`, `entityType`, `threadId`, `error`)
- F. Hook absent leaves behavior unchanged — no opts, observable behavior identical to pre-hook implementation

5 verbs × 2 slots × 6 scenarios × 2 paths (flat + aggregate) = **120 tests at Tier 1**.

**Tier 2 — Auto handler dispatch.** `application/handlers/{insert,update,partial_update,archive,unarchive,delete}_test.go`.

Per Auto handler:
- Cmd implementing `AfterBeginHookProvider` only → afterBegin option passed to Repo
- Cmd implementing `BeforeCommitHookProvider` only → beforeCommit option passed
- Cmd implementing both → both options passed
- Cmd implementing neither → no options passed

Mock Repo captures the `[]WriteOption[T]` slice; test verifies content. 4 scenarios × 6 handlers = **24 tests at Tier 2**.

**Tier 3 — Application-layer types.** `application/persistence/tx_test.go`. Most of the surface is passive (interfaces, function types). One composition test verifying `WithAfterBegin` + `WithBeforeCommit` apply correctly when both passed. **~2 tests at Tier 3**.

**Total: ~146 unit tests added in the framework round.**

**QA E2E cases (in `omnicore-example-users/qa/e2e.sh`)** — added same round as the framework change:

1. **Happy-path with hook.** Add `BeforeCommit` to an existing command (or dedicated endpoint) that writes an extra outbox row carrying a different event type. Verify both rows reach Mongo via SyncEngine — proves the hook fired in the same TX as the data write.
2. **Rollback via hook.** Hook returns a typed notification (simulating "duplicate detected later in TX"). Verify user was NOT created (subsequent GET returns 404) and wire response carries the typed notification at the correct HTTP status.

**Migration plan — single round, no deprecation cycle.**

Framework branch (`feature/persistence-lifecycle-hooks` in `omnicore/`):
1. Add `application/persistence/` — TxHandle, CommandTag, Rows, Row, AfterBeginHook, BeforeCommitHook, AfterBeginHookProvider, BeforeCommitHookProvider, WriteOption, WithAfterBegin, WithBeforeCommit, Writer[T] (resolves the Topic 9 layer caveat — Writer[T] is the typed write port carrying the variadic; `infra.BaseRepository[T]` implements it).
2. Add infra adapters — `pgxTxHandle`, `pgxRows`, `pgxRow`.
3. Update `infra.Postgres` + `infra.aggregate_persister` to accept `WriteOption` and fire hooks at positions A and D (Topics 1, 7, 8).
4. Update `infra.BaseRepository[T]` impl.
5. Update 6 Auto Command Handlers — type assertion + opts forward.
6. Remove `application/persistence/orchestrator.go` (Topic 9 lock).
7. Add unit tests (Tiers 1+2+3 = ~146 tests).
8. Update `CLAUDE.md` / `DOCS.html` (Topic 11 fixes the exact surface).
9. Update `CHANGELOG.md` — `[Unreleased]` block with `Added` / `Changed` / `Removed`.
10. `go build ./... && go vet ./... && go test ./... -count=1` green before PR.

Consumer branch (`feature/persistence-lifecycle-hooks` in `omnicore-example-users/`):
- Pull updated `omnicore` via `go.mod`.
- Migrate handlers — `orch.Method(...)` → `repo.Method(ctx, ...)` (sed-friendly cascade).
- Add the 2 QA E2E cases to `qa/e2e.sh`.
- Run `qa/e2e.sh` + `qa/audit.sh` (hooks touch persistence path; audit also lives there).

**Single round means:** both PRs are scoped to land together. No "framework lands first, consumer catches up later". Discard whatever needs to be discarded; no backwards-compat shims.

**Impact.**
- ~146 framework unit tests added.
- 2 QA E2E cases added to the consumer service.
- One PR pair under one round (framework + example).
- Pre-existing tests in `omnicore/` and `omnicore-example-users/` may need adjustments where they assumed `Orchestrator` or the old callbacks — sed-friendly cascade.
- CHANGELOG covers: `Added` (new types/options), `Changed` (Repository signature path, Auto handler dispatch), `Removed` (Orchestrator entirely).
- No backwards-compat shims; no parallel API; one canonical path.

### 11. Public surface impact (CLAUDE.md + DOCS.html) [CLOSED 2026-06-13]

**Decision.** The implementation round rewrites the listed `CLAUDE.md` sections inline and performs a DEEP, end-to-end review of `DOCS.html`. Both docs land green (matching the new public surface) as a precondition of "done". `CHANGELOG.md` captures the historical record; `CLAUDE.md` describes current state only — no Phase N labels, no "was X, now is Y" framing.

**Mandatory DEEP-REVIEW directive for `DOCS.html`.** When the implementation reaches the `DOCS.html` step, the contributor MUST review the entire file end-to-end — not just surface-level find-and-replace of `orch.Method(...)` examples. `DOCS.html` is the consumer-facing manual; every section referencing persistence, write lifecycle, manual handlers, Auto handlers, examples involving `Orchestrator`, or vocabulary tied to the old before/after model must be revisited holistically to ensure the documented system reflects the new model coherently. Surface-level swaps risk leaving stale prose, broken cross-references, inconsistent terminology, and orphan examples. Procedure: read `DOCS.html` cold first; build a list of affected sections; rewrite each with the new model in mind; verify no dangling references to the removed surface remain.

**CLAUDE.md — sections affected.**

Delete entirely:
- Subsection "Orchestrator (`application/persistence/orchestrator.go`)" (under "Core concepts").

Rewrite — replace Orchestrator references with direct Repo calls + hook documentation:
- "Auto Command Handlers" — end-to-end example, route wrappers, the asymmetry note on `UnarchiveCommandHandler`.
- "Manual path (cross-service, side effects)" — example switches to `repo.Method(ctx, ..., opts...)`.
- "Aggregate persistence (transparent dispatch)" — end-to-end example.
- "Persistence" (top-level) — setup example.
- "Full request flow (concrete)" — replace `orch.Insert` with `repo.Insert(ctx, ..., opts...)`; document positions A and D of the hooks inside the sequence.

Update tables:
- "Concurrency and lifecycle" — drop the `Orchestrator[T]` row.
- "Architecture — 4-layer DDD with strict boundaries" — `application/persistence/` directory listing loses Orchestrator, gains hook types.
- "Quick reference — where to add things" — drop Orchestrator entries; add:
  - "Want in-TX side effect on Auto path" → "declare `BeforeCommit` on the Cmd"
  - "Want in-TX side effect on manual path" → "pass `persistence.WithBeforeCommit[T](fn)` to `repo.Method(...)`"
  - "Want to read state inside the hook's TX" → "use `tx.Query(ctx, sql)` inside the hook closure"
  - "Want compile-time safety against typo in hook method name" → "`var _ persistence.BeforeCommitHookProvider[*T] = (*Cmd)(nil)` at the bottom of the Cmd file"

Critical invariants — add three:
- "`afterBegin` fires inside the TX before any framework write; `beforeCommit` fires after all framework writes (data + outbox + audit) and before COMMIT. Single fire per aggregate operation."
- "Hook returns non-nil error → ROLLBACK; type identity preserved end-to-end (NotificationCarrier → Failure with translated notifications, non-carrier → Exception)."
- "Hook panic → `defer tx.Rollback()` fires, panic propagates to `pipeline.Run` → Exception. Persister has no own recover."

Glossary — drop "Orchestrator"; add:
- `TxHandle` — application-layer interface exposed to in-TX hooks. Methods `Exec` / `Query` / `QueryRow`. Implemented in `infra/` wrapping `*pgx.Tx`.
- `AfterBeginHook[T]` / `BeforeCommitHook[T]` — function types declaring the hook shape. T is the user's domain entity.
- `WriteOption[T]` — functional option consumed by `Repo` write methods; carries `AfterBeginHook[T]` and/or `BeforeCommitHook[T]`.
- `AfterBeginHookProvider[T]` / `BeforeCommitHookProvider[T]` — interfaces detected by Auto handlers via type assertion. Cmd declaring `AfterBegin` / `BeforeCommit` satisfies the matching provider automatically.

**DOCS.html — surface affected (deep-review-required).**
- Drop any HTML example showing `persistence.NewOrchestrator(...)`.
- Add a section "In-TX side effects via hooks" covering:
  - The two slot positions and what each is for
  - Auto path example: Cmd declaring `BeforeCommit` + recommended boilerplate `var _ persistence.BeforeCommitHookProvider[*User] = (*Cmd)(nil)`
  - Manual path example: `repo.Insert(ctx, insertable, persistence.WithBeforeCommit[*User](fn))`
  - TxHandle surface (`Exec` / `Query` / `QueryRow`) and the `defer rows.Close()` ownership note
- Update existing write-path examples to use `repo.Method(ctx, ...)` instead of `orch.Method(...)`.
- Update related "Where to add things" / "How to do X" sections.
- The DEEP-REVIEW directive above governs scope — assume more sections are affected than this list captures; the implementation pass discovers them by reading the file end-to-end.

**Impact.**
- ~10 CLAUDE.md sections touched in the implementation round.
- DOCS.html scope determined by end-to-end review during implementation; assume at least 6 sections but expect more.
- All updates land in the same PR as the code (precondition of "done" per the framework's top-of-CLAUDE.md CRITICAL RULE).
- CHANGELOG entries (`Added` / `Changed` / `Removed`) capture the history; CLAUDE.md describes the current state only; DOCS.html mirrors consumer-visible API.

## Implementation prompt

The design above is locked in full. To begin the implementation round, open a Claude Code session at the omnicore-stack root and use the prompt below verbatim:

> Implement the persistence lifecycle hooks redesign described in `omnicore/tasks/hookfixes.md`'s "Closed decisions" section (Topics 1-11). All eleven topics are locked; the design IS the contract. Treat each closed-decision block as the spec for that area.
>
> Implementation order (single round, no deprecation cycle, discard whatever needs discarding):
>
> Framework branch `feature/persistence-lifecycle-hooks` in `omnicore/`:
> 1. Add `application/persistence/` types — `TxHandle`, `CommandTag`, `Rows`, `Row` (Topic 3); `AfterBeginHook[T]`, `BeforeCommitHook[T]` (Topic 2); `AfterBeginHookProvider[T]`, `BeforeCommitHookProvider[T]` (Topic 4); `WriteOption[T]`, `WithAfterBegin[T]`, `WithBeforeCommit[T]` (Topic 5); `Writer[T]` port carrying the variadic to resolve the Topic 9 layer caveat.
> 2. Add `infra/` adapters — `pgxTxHandle`, `pgxRows`, `pgxRow`.
> 3. Update `infra.Postgres` (flat path) + `infra.aggregate_persister` to accept `WriteOption[T]` and fire hooks at positions A and D per Topics 1, 7, 8. Extract a shared helper so flat and aggregate paths cannot drift on the firing contract.
> 4. Update `infra.BaseRepository[T]` impl to accept and forward the variadic.
> 5. Update the 6 Auto Command Handlers (`application/handlers/{insert,update,partial_update,archive,unarchive,delete}.go`) — type assertion on the Cmd against the two provider interfaces; pass derived options to the Repo call.
> 6. Remove `application/persistence/orchestrator.go` entirely (Topics 6, 9). Drop `NewOrchestrator`, `WithLogger`, all 6 forwarding methods.
> 7. Add unit tests per Topic 10: Tier 1 (~120 tests across persister paths and verbs), Tier 2 (~24 Auto handler dispatch tests), Tier 3 (~2 composition tests). Target ~146 new tests total.
> 8. Update `CLAUDE.md` per Topic 11's section list — inline rewrite, current state only, no Phase N labels.
> 9. **DEEP-REVIEW** `DOCS.html` per Topic 11's mandatory directive — read the entire file cold first, list affected sections, rewrite each holistically. Do not perform surface-level find-and-replace only.
> 10. Update `CHANGELOG.md` `[Unreleased]` block — `Added` (new types/options), `Changed` (Auto handler dispatch path, Repository signature shape), `Removed` (Orchestrator entirely).
> 11. `cd omnicore && go build ./... && go vet ./... && go test ./... -count=1` must be green before PR.
>
> Consumer branch `feature/persistence-lifecycle-hooks` in `omnicore-example-users/`:
> 12. Pull updated `omnicore` via `go.mod`.
> 13. Migrate handler call sites — `orch.Method(...)` → `repo.Method(ctx, ...)`. Sed-friendly mechanical refactor.
> 14. Add the 2 QA E2E cases per Topic 10: (a) happy-path hook writing extra outbox row, verified via Mongo; (b) rollback hook returning typed notification, verified via subsequent 404 + correct wire status.
> 15. Run `qa/e2e.sh` + `qa/audit.sh` (hooks touch persistence path and audit also lives there).
>
> Both PRs are scoped to land together as a single round. After framework build/vet/test green and before opening the PR, ask the maintainer whether to also run the full E2E QA suites (`qa/e2e.sh`, `qa/auth.sh`, `qa/audit.sh`, `qa/httpclient.sh`) per the framework's top-of-CLAUDE.md CRITICAL RULE.
