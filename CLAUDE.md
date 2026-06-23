# OmniCore

> **CRITICAL RULES — read before touching anything in `omnicore/`.**
>
> 1. **Every change to this module needs explicit maintainer approval first.** Describe the change + motivation + impact, wait for approval, then edit. No exception for "obvious", "small", "cosmetic", or "a consumer needs it". If a consumer is urgent, work around it in the consumer — never patch the framework "for now".
> 2. **Never remove functionality without explicit confirmation.** "Appears redundant" / "new API covers it" / "looks like dead code" is not authorization. When a new feature impacts an old one, stop, describe the overlap, and offer via `AskUserQuestion`: *Remove / Deprecate / Keep both / Adapt to delegate*. Applies to any public surface (functions, endpoints, yaml fields, flags, defaults, struct fields, options).
> 3. **Canonical and manual routes stay feature-equivalent.** Every feature must work the same through the Auto/convention path AND the hand-written `pipeline.Handler` + explicit wiring path — same envelope, pipeline, audit/outbox/notification semantics, schema enforcement. Manual is the escape hatch for *wiring control*, not a poorer tier. If a feature can only fit one side, stop and offer options before coding one-sided.
> 4. **English everywhere** — code, comments, docs, identifiers, tests, logs, error strings. The only non-English text allowed is the seven translation catalogs in `application/translation/` (`ptbr`/`eng`/`esp`/`fra`/`deu`/`ita`/`nld`); the surrounding Go stays English. Chat may be any language.
> 5. **Verify, never guess.** Every claim about the code (signatures, behavior, defaults, existence) must be backed by a `Read`/`grep`, including while planning. A plan built on a guessed contract has no value. Say "I'm guessing — let me verify" and verify, rather than present inference as fact.
> 6. **The AI never writes git history.** No `commit`/`push`/`tag`/PR/release. At task start, get onto a coherent branch (`feature|fix|docs|refactor/<kebab-outcome>`): off `main` via `git checkout -b`, or rename an in-flight unmerged branch via `git branch -m` (never re-stack). Apply edits, then deliver one English commit-message suggestion as chat text. `git checkout -b` / `git branch -m` are the only git-writes allowed.
> 7. **This file is a spec of what IS, not history.** No "Phase N" labels, no changelog/dated entries, no "was X now Y", no references to removed APIs, no absence/TODO statements ("not yet", "future X", "currently only"). When the spec contradicts the code, the spec is wrong — fix it in the same round as the code change.
> 8. **95% is the minimum test coverage.** No production changes to enable testability without maintainer approval. `_test.go` files may cross DDD layers only if production imports already allow it.
>
> **Every approved change ships in one round with:** the code edit + unit tests (green `go build ./... && go vet ./... && go test ./... -count=1` is a precondition, not proof of working — then ask via `AskUserQuestion` whether to run the `omnicore-example-users` E2E suites) + a `CHANGELOG.md` `[Unreleased]` entry (public-surface changes only) + a `docs/` site update (the consumer-facing manual at `docs/content/sections/<id>.html` + a `changelog.html` entry; the site and this file must tell the same story). Purely internal changes (private helper, refactor without API change, comment-only) may skip CHANGELOG/docs — record the rationale.

---

Go framework library providing **DDD + CQRS infrastructure** for microservices. Services import it as a Go module dependency; OmniCore itself contains no service code.

- **Module path**: `github.com/ClaudioSchirmer/omnicore`
- **Local path**: `/Volumes/Lynx/Development/omnicore-stack/omnicore`
- **Maintainer**: Claudio Schirmer (`claudioschirmer@icloud.com`)
- **Reference consumer**: [`../omnicore-example-users`](../omnicore-example-users/CLAUDE.md) — sandbox service that exercises every framework feature
- **End-user manual**: the [`docs/`](docs/) site (per-section pages under `docs/content/sections/`) — the consumer's view. This file is the agent/maintainer view; keep both in sync.

## Stack

- Go ≥ 1.21 (uses `log/slog` + generics); toolchain pinned to `go 1.26.3`
- Fiber v3 (HTTP), pgx v5 (Postgres), mongo-driver v2, segmentio/kafka-go, google/uuid

## Build and test commands

```
go build ./...
go vet ./...
go test ./... -count=1                       # unit suite (default)
go test -tags=integration ./... -count=1     # integration (needs docker compose up in ../omnicore-example-users/devops)
```

Tests sit next to the file under test (`foo.go` ↔ `foo_test.go`). Integration tests opt in via `//go:build integration` (real Postgres + Mongo + Kafka), excluded from the default run.

## Architecture — 4-layer DDD with strict boundaries

```
web/                  HTTP transport only; openapi/ = OpenAPI 3.1 + Swagger UI;
                      graphql/ = GraphQL endpoint (own surface); queryschema/ =
                      shared read-side DTO reflection (REST + OpenAPI + GraphQL)
application/
  configuration/      AppContext (UUID + language + Identity), Language, Identity
  translation/        Translator + Module; Core{PTBR,ENG,ES,FR,DE,IT,NL} built-ins
  notifications/      ContextDTO/MessageDTO (carries NotificationKey)
  pipeline/           Request/Command/Query, Handler[TReq,TRes], Result[T], Pipeline
  persistence/        ScopedRepository[T], RequestContext, TxHandle, hooks, WriteOption[T]
  queries/            QueryHandler + ViewReader port + ReadCriteria/Page DTOs
domain/               Pure business rules, ZERO IO
  aggregate_mapping.go  AggregateRootProvider (table/FK declared in infra via TableSchema)
  entity.go             ValidEntity sealed types (carry Entity directly)
  path_render.go        childCollectionSegment / PluralizeWord (camelCase child path segment)
infra/
  audit/              AuditEvent + Config + persister + echo + partitions
  events/             Publisher + SlogPublisher
  log/                Header/Data/Log/Export shapes
  (root)              postgres, executor, aggregate_persister, outbox, mongo,
                      mongo_view_reader, view, composer, sync, external, rebuild, exception
```

### Dependency rules — NEVER violate

| Layer | May import | Must NOT import |
|---|---|---|
| `domain` | stdlib + `google/uuid` only | everything else |
| `application/*` | `domain`, other `application/*` | `infra`, `web` |
| `infra` | `domain`, `application/persistence` | `web` |
| `web` | `domain`, `application/*` | `infra` directly |

Cross-layer errors travel via `domain.NotificationCarrier` (any error carrying `[]*NotificationContext`), so layers never type-import each other's error structs.

## Core concepts

### ValidEntity (`domain/entity.go`)

Sealed types producible ONLY by the domain package (private `entity()` method enforces at compile time): `Insertable` / `Updatable` / `Archivable` / `Deletable` / `Unarchivable` / `Batch`.

- Metadata accessors: `Signature() uuid.UUID`, `EntityName()`, `ActionName()`, `DateTime()`, `Events()`.
- Carries the validated `Entity` directly via `Source() Entity`; infra resolves table/columns via the Repository's `TableSchema`. Domain never pronounces table/column/FK.
- Optional `*aggregateMeta` for aggregate-aware persistence via `AggregateInfo() (root, isAggregate)`.

Constructed via the high-level `Get*` path (all take `actionName string`; all validate a `BaseEntity`-embedding struct and auto-attach aggregate metadata when the entity implements `AggregateRootProvider`). There are no low-level `NewInsertable(table, fields)` constructors.

| Function | Form |
|---|---|
| `GetInsertable(e Entity, svc Service, actionName string)` | no closure (no prior state; no snapshot) |
| `GetUpdatable[T](e T, apply func(T) error, svc Service, actionName string)` | closure; snapshots BEFORE `apply` |
| `GetPartialUpdatable[T](e T, apply func(T) error, svc Service, actionName string)` | closure; snapshots BEFORE `apply` |
| `GetArchivable(e Entity, svc Service, actionName string)` | no closure; snapshot at entry |
| `GetUnarchivable(e Entity, svc Service, actionName string)` | no closure; snapshot at entry |
| `GetDeletable(e Entity, svc Service, actionName string)` | no closure; snapshot at entry |

The framework runs `apply` (= `cmd.ApplyTo` / `cmd.ApplyPartiallyTo`, both `func(T) error`) inside the domain function, so `domain.Old[T](e)` in `BuildRules` returns the pre-mutation state.

### EntityMode + `Modes()`

`Modes()` declares the operation set the entity accepts. Missing mode → `*NotAllowedNotification` (`SemanticForbidden` → 403).

| Constant | Consulted by | Failure notification |
|---|---|---|
| `ModeDisplay` | (informative, no gate) | — |
| `ModeInsert` | `validateForInsert` | `InsertNotAllowedNotification` |
| `ModeUpdate` | `validateForUpdate` | `UpdateNotAllowedNotification` |
| `ModeDelete` | `validateForDelete` | `DeleteNotAllowedNotification` |
| `ModeArchive` | `getArchivable` | `ArchiveNotAllowedNotification` |
| `ModeUnarchive` | `getUnarchivable` | `UnarchiveNotAllowedNotification` |

`ModeArchive`/`ModeUnarchive` are independent of `ModeUpdate` — enabling "freeze-once" (`[ModeDisplay, ModeInsert, ModeArchive, ModeUnarchive]`) or append-only (`[ModeDisplay, ModeInsert]`). Archive/Unarchive run `BuildRules` in `ModeUpdate` with distinct `actionName` (`"GetArchivable"`/`"GetUnarchivable"`), so `IfUpdate` covers state-transition verbs; checks then feed the same `checkAllNotifications` gate.

### BaseEntity (`domain/entity_base.go`)

Embeddable base for user entities. Provides: ID management (`SetID`/`GetID`/`ClearID`); per-entity `NotificationContext` (auto-named from struct via reflect); `RegisterEvent(DomainEvent)`; VO registration (`AddValueObject`, `AddAggregateValueObject`); old-state accessor `Old() Entity` (typed: `domain.Old[T](e) T`); `AddFieldNameAlias`; private framework methods for the `Get*` family. `RequiresService() bool` defaults to `false` via embed — override returning `true` when `BuildRules` needs `domain.Service`.

The `Entity` interface has NO `TableName()`/`ToFields()` — infra maps Go fields to columns via a per-Repository `TableSchema`. Mandatory interface members: `Modes`, `BuildRules`.

```go
type Customer struct {
    domain.BaseEntity
    Name, Email string
}
func (c *Customer) Modes() []domain.EntityMode {
    return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
func (c *Customer) BuildRules(actionName string, service domain.Service, r *domain.Rules) {
    r.IfInsertOrUpdate(func() {
        if c.Name == "" {
            r.AddNotification("Name", domain.RequiredFieldNotification{})
        }
    })
}
```

### AggregateRoot (`domain/aggregate_root.go`)

Embeds `BaseEntity`. Manages typed `AggregateValueObject` collections with status tracking (Constructor/Added/Changed/Removed):
- `AggregateConstructor(items)` — init with CONSTRUCTOR status (trusted DB load, no type-guard).
- `ClearAggregateItemsOfType(name)` — bulk REMOVE a type.

Public mutation primitives (each type-guarded against `root.AggregateChildren()`; a miss emits `InvalidAggregateChildNotification`, 422, and skips the add):
- `domain.AddAggregateChild(root, item)`, `ChangeAggregateChild(root, original, replacement)`, `RemoveAggregateChild(root, item)`
- `domain.ReplaceAggregateChildrenOf[VO](root, items)` — Clear + re-add the whole VO collection.
- `domain.ValidateAggregateChild(root, item, actionName, svc) bool` — optional inline `BuildRules` run. Pitfall: if used inline AND the item enters the collection, the boundary runs `BuildRules` again → duplicate notification. Pick one path.

Convention (ruler): commands do NOT call the primitives directly. Expose business-named root methods (`u.AddAddress(addr)`, `u.ReplaceAddresses(addrs)`) that enforce cross-child invariants and delegate — the root is the consistency boundary ("tell, don't ask").

Root methods must call `domain.EnsureInitialized(u)` first: a freshly constructed entity has `notifCtx == nil` and `AddNotification` is a no-op. `resetEntity` (in `getInsertable/getUpdatable/getDeletable`) does NOT clear `notifCtx`, so construction-time notifications survive to `checkAllNotifications`. `IsValid` clears `notifCtx` itself before re-checking. Primitives call `ensureRootInit(root)` as defense in depth. Declared type-names cache per root `reflect.Type` in a `sync.Map`.

Read-only typed query helpers: `domain.GetAggregateItemsOf[VO]`, `GetAddedItemsOf[VO]`, `GetChangedItemsOf[VO]`, `GetRemovedItemsOf[VO]`, `GetCurrentItemsOf[VO]`.

### AggregateValueObject (`domain/aggregate_vo.go`)

```go
type AggregateValueObject interface {
    BuildRules(actionName string, service Service, r *Rules)
    GetID() string   // empty for new, set when loaded
}
```

Infra maps child columns via its `TableSchema` (`AddressSchema()`); only declared fields persist/scan/audit. `ID` is the PK (DB-gen, skipped on write list, used in WHERE). The root FK is declared via `.FK("user_id")` and injected by the persister (not a struct field). Undeclared exported fields are runtime-only.

### Rules DSL (`domain/rules.go`)

Mode-scoped closures, each running only when the current `EntityMode` matches: `r.IfInsert`, `r.IfUpdate`, `r.IfDelete`, `r.IfInsertOrUpdate`, `r.IfDisplay`. The framework picks the mode; the caller picks `actionName` (3rd param on every `Get*`). Auto handlers pass the canonical PascalCase string; manual handlers may pass any string.

| Trigger | Mode | actionName (Auto default) |
|---|---|---|
| `GetInsertable` | `ModeInsert` | `"GetInsertable"` |
| `GetUpdatable` / `GetPartialUpdatable` | `ModeUpdate` | `"GetUpdatable"` / `"GetPartialUpdatable"` |
| `GetArchivable` / `GetUnarchivable` | `ModeUpdate` | `"GetArchivable"` / `"GetUnarchivable"` |
| `GetDeletable` | `ModeDelete` | `"GetDeletable"` |
| `IsValid(e, mode, svc)` | matching mode | `"isValid"` (fixed; not invoked for Archive/Unarchive/Display) |

Key facts:
- Archive/Unarchive fire `IfUpdate` (not a dedicated `IfArchive`) — branch on `actionName` inside the closure.
- `actionName` is case-sensitive: `Get*` emits PascalCase, `IsValid` emits `"isValid"`. Custom strings must match exactly.
- An AVO's `BuildRules` receives the SAME `actionName` as its root, propagated verbatim.
- `IfDisplay` is inert — no framework path calls `NewRules(ModeDisplay, ...)`.

```go
r.IfUpdate(func() {
    if actionName == "GetArchivable" { /* e.g. cannot archive primary account */ }
})
```

### Old-state snapshot

Domain owns the pre-write snapshot. `Get*` captures it; `BuildRules` reads it via `domain.Old[T](e) T`; the auditor consumes it for `changes` (delta) / `snapshot` (on Delete).

```go
r.IfUpdate(func() {
    if old := domain.Old(u); old != nil && old.Email != u.Email && u.Activated {
        r.AddNotification("Email", EmailLockedAfterActivationNotification{})
    }
})
```

Mechanics:
- `BaseEntity` carries a private `old Entity`, exposed via `Old() Entity`.
- Capture timing: `GetUpdatable`/`GetPartialUpdatable` snapshot BEFORE `apply`; `GetDeletable`/`GetArchivable`/`GetUnarchivable` at entry; `GetInsertable` does NOT snapshot (`Old()` returns typed zero — guard with `if old := domain.Old(u); old != nil`).
- The snapshot is a JSON round-trip clone: exported fields only, private state (`notifCtx`, `events`, `signature`) ignored. A read-only ghost — mutator methods on it are silent no-ops (`notifCtx` nil).
- For aggregates, `captureOld` deep-copies the items map, so `domain.GetCurrentItemsOf[Address](&domain.Old(u).AggregateRoot)` exposes prior children.
- Custom infra bypassing `Get*` leaves `Old()` unpopulated → auditor degrades gracefully (Update emits every column with `from=null`; Delete snapshots the live source).

### Audit event shape

One `AuditEvent` per ValidEntity write (granularity B — aggregate is the event unit; mirrors the outbox row). Routing via `audit.destinations` in `microservice.<profile>.yaml`:
- `database` → `audit.InsertAuditEvent` writes to `audit_events` inside the SAME `pgx.Tx` as data + outbox. Authoritative.
- `slog` → `audit.EchoSlog` emits a flat slog line after COMMIT. Lossy observability echo.

Code: shape `infra/audit/event.go`; in-TX writer `infra/audit/persister.go`; echo `infra/audit/echo.go`; per-verb builders `infra/audit_builder.go`.

Top-level fields (every event): `threadId` (UUID, from `AppContext.ID()`), `entityType`, `entityId` (root ID), `verb` (`insert`|`update`|`archive`|`unarchive`|`delete` — SQL-grounded; PUT+PATCH share `update`), `actionName`, `kind` (`snapshot`|`delta`|`transition`), `actor` (JWT `sub` or `"anonymous"`), `actorIssuer` (omitempty), `actorClaims` (omitempty, filtered by `auth.auditClaims`), `dateTime` (RFC3339Nano).

Body block — one regime per event, selected by `kind`:

| `kind` | Verbs | Block |
|---|---|---|
| `snapshot` | `insert`, `delete` | `snapshot: map[goFieldName]value` (post-insert / pre-delete via Old()) |
| `delta` | `update` | `changes: []{field, fieldLabelKey, from, to}` sorted by `field`; `field` = raw Go name (never the column) |
| `transition` | `archive`, `unarchive` | none (verb is the recovery hint) |

`children` map appears when the source implements `AggregateRootProvider` and a child is observable. Keyed by Go type name; each entry `{id, op, snapshot|changes}`. Child ops are SQL-grounded — same 5-verb set as the root:

| Verb | Children | Per-child `op` | Block |
|---|---|---|---|
| `insert` | all current | `inserted` | snapshot |
| `update` — Added | `AddAggregateChild` | `inserted` | snapshot |
| `update` — Changed | `ChangeAggregateChild` | `updated` | changes (vs pre-mutation child) |
| `update` — Removed | `RemoveAggregateChild`/`ReplaceAggregateChildrenOf` | `archived` | snapshot (pre-archive, from Old()) |
| `update` — Constructor | untouched | — (skipped) | — |
| `archive` | every loaded active child | `archived` | snapshot |
| `unarchive` | every loaded archived child | `unarchived` | snapshot |
| `delete` | every loaded child (FK ON DELETE CASCADE) | `deleted` | snapshot |

UPDATED children pair pre/post by `GetID()` against the deep-copied prior aggregates map. Flat entities (no `AggregateRootProvider`) carry no `children` block.

### Notification system (`domain/notification*.go`)

- `Notification` is a marker interface (`isNotification()` + `Semantic() NotificationSemantic`). Concrete types embed `DomainNotificationBase` / `ApplicationNotificationBase` / `InfrastructureNotificationBase` (all default `SemanticValidation`; override for Conflict/NotFound/…). Translation key = struct type name via `reflect.TypeOf(n).Name()`.

```go
type UsernameAlreadyExistsNotification struct{ domain.DomainNotificationBase }
```

- `NotificationContext` groups messages by context name. Methods: `AddNotification(name, n, value...)`, `AddNotificationMessage(msg)`, `Scoped`, `HasErrors`, `Clear`, `ChangeFieldName`, `Copy`, `Messages`.
- `NotificationMessage` carries optional `Path []PathSegment`, `Override`, `FieldName`, `FieldValue`, `FuncName`, `Err`, `Vars`, plus the typed `Notification`. Wire field resolved by `ResolveFieldName()` with precedence **Override > rendered Path > FieldName**. Render vars come from `domain.MessageVars(msg)` (merges `tvar`-tag fields + per-emit `Vars`; per-emit wins).

Root `Entity.BuildRules` and `AggregateValueObject.BuildRules` share signature, DSL, emit shape, and propagated `actionName` — only the receiver differs. The wire `field` reflects the JSON path:

| Origin | Emit | Wire `field` |
|---|---|---|
| Root | `r.AddNotification("Name", n)` | `name` |
| Root + echo | `r.AddNotification("Email", n, u.Email)` | `email` + `value` populated |
| Aggregate child | `r.AddNotification("ZipCode", n)` | `addresses[0].zipCode` |

Path mechanics: `NewRules(mode, ctx, entityType)` packages the mode + destination ctx + owner `reflect.Type` (used to read the field's `labelKey` tag at emit). For an AVO, ctx is a `Scoped(NameSegment(collection), IndexSegment(i))` view. `runAggregateValidations` uses `childCollectionSegment(typeName)` (camelCase pluralized: `Address`→`addresses`, `OrderLine`→`orderLines`). `toLowerCamel` is acronym-aware (`URLPath`→`urlPath`); already-lowercase strings pass through.

Controlling the wire field name — three paths, all feeding `ResolveFieldName()`:
- Default — `toLowerCamel` render of the Go field name.
- `BaseEntity.AddFieldNameAlias(orig, new)` — declarative, stable entity rule; applied in `checkAllNotifications` via `applyFieldAliases`.
- `NotificationContext.ChangeFieldName(orig, new)` — imperative, per-endpoint; sets `Override` on matching messages.

Both override paths populate `NotificationMessage.Override` (top of precedence); choice is entity-definition vs request-handling time.

**Field labels — `labelKey:"<catalogKey>"` struct tag.** Adds a translated human label next to the technical `field`, for frontend-less channels (e-mail/SMS/push/audit). `Rules.AddNotification` reads the tag at emit and writes `NotificationMessage.LabelKey`; `application/notifications/convert.go::ToContextDTOs` renders via `Translator.Render(lang, key, nil)` onto `MessageDTO.FieldLabel`. Audit's `infra/audit_builder.go::computeChanges` writes the raw key to `FieldChange.FieldLabelKey`; `audit.RenderLabels` / `audit.RenderLabelsInJSON` render at read time.

```go
type Address struct {
    Street  string `labelKey:"AddressStreetField"`
    ZipCode string `labelKey:"AddressZipCodeField"`
}
// catalog: "AddressZipCodeField": "CEP"  → wire fieldLabel: "CEP"
```

Rules:
- No tag = no label (`FieldLabel` / `FieldLabelKey` are omitempty). `labelKey:"-"` (or empty value) opts out.
- A field not in the `TableSchema` is runtime-only → never surfaces, label included.
- Catalog miss → raw key fallback + `slog.Warn("translation.key.missing", ...)` once per `(lang, key)`.
- Audit stores the KEY, not the rendered string (immutable; readers render per-locale).
- Resolution is by the Go field name the rule passed; `AddFieldNameAlias` renames `field` only, `fieldLabel` stays.
- Manual `AddNotificationMessage` defaults `LabelKey == ""` — set `msg.LabelKey` explicitly. Reflection plan cached per `reflect.Type` (`domain/field_label.go` `sync.Map`).

`r.AddNotification(field, n, value)` covers the common case: variadic `value` → `FieldValue` (supports `string`, `*string` safe-deref, nil, else `fmt.Sprint`). For `Err`/`FuncName`/`Override`/multi-segment Path use `r.AddNotificationMessage(NotificationMessage{...})`.

`NotificationKey` (typed identity, e.g. `"RecordNotFoundNotification"`) is preserved through translation on `MessageDTO.NotificationKey` and `ErrorMessage.NotificationKey`; the HTTP layer maps it to status codes.

Per-layer error wrappers avoid `NewNotificationContext + Add + NewXxxError` boilerplate:
- `domain.SingleNotificationError(ctx, field, n)`, `domain.NotFoundError(ctx, field, value)` (uses `RecordNotFoundNotification`), `domain.FieldErrorWithCause(ctx, field, cause, n)` → `*DomainError`
- `infra.SingleNotificationError` / `infra.FieldErrorWithCause` → `*InfrastructureError`
- `application/exception.SingleNotificationError` / `.FieldErrorWithCause` → `*ApplicationError`

### Parameterized notifications

Notifications carry runtime variables into translated messages without one catalog entry per value (e.g. a per-tenant max length). Four mechanisms feed render-time vars; per-emit wins on key collision.

| Mechanism | Where | Use |
|---|---|---|
| `tvar:"<name>"` struct tag | exported notification field | default vars (≥95% of cases) |
| `TranslationVars() map[string]string` | method (`domain.TranslationVarsProvider`) | unexported/computed vars; REPLACES tag extraction |
| `Vars map[string]string` per emit | `r.AddNotificationWithVars(...)` | per-call-site additions/overrides |
| `NotificationContext.SetVars(...)` | context label | vars for the wrapping label (`"UserOf{tenantId}"`) |

```go
type NameMaxLengthExceededNotification struct {
    domain.DomainNotificationBase
    MaxLength int `tvar:"maxLength"`
}
// catalog: "...": "Name exceeds the maximum of {maxLength} characters."
r.AddNotificationWithVars("Name",
    NameMaxLengthExceededNotification{MaxLength: u.NameMaxLength},
    map[string]string{"context": "premium-plan"}, u.Name)
```

Pointer fields deref (nil → empty); unexported skipped; plan cached per `reflect.Type`.

Translation surface (`Render` = catalog lookup + interpolate; `Interpolate` = substitution only):
```go
func (t *Translator) Render(lang configuration.Language, key string, vars map[string]string) string
func Interpolate(s string, vars map[string]string) string
```
Placeholder `{<name>}` matches `[A-Za-z_][A-Za-z0-9_]*`; non-matches verbatim. Missing key → returns `key` + warn-once `translation.key.missing`. Missing var → leaves `{name}` literal + warn-once `translation.var.missing`. `Get`/`GetOr` are the no-interpolation lookups for var-less notifications. `ToContextDTOs` resolves vars via `domain.MessageVars(msg)` + `ctx.ContextVars()` then renders — Auto/manual/audit paths inherit it with no code change.

### Result[T] and Pipeline (`application/pipeline/`)

- `Result[T]` = single struct discriminated by `state` (StateSuccess/StateFailure/StateException). Factories: `Success(v)`, `Failure(notifications)`, `Exception(err)`. Fluent DSL: `.OnSuccess(fn).OnFailure(fn).OnException(fn)`, plus `ValueOr`, `MustValue`, `FirstSuccess`, `ForEach`.
- `Pipeline.Run[T]` / `Pipeline.Dispatch[TReq,TRes]` are generic top-level functions (Go forbids generic methods). They: catch `domain.NotificationCarrier` errors → `Failure[T]` (translated via `Translator` + request `Language`); catch panics via defer/recover → `Exception[T]`; log via `slog`.

```go
pipe := pipeline.New(translator)
result := pipeline.Dispatch(pipe, ctx, cmd, handler)
return web.RespondFromResult(c, result, fiber.StatusCreated)
```

### Persistence ports (`application/persistence/`)

Domain declares pure ports (NO ctx, actor, or hooks); application adds the request-scoped write binding.

| Port | Layer | Surface |
|---|---|---|
| `domain.Reader[T]` | domain | `FindByID(id) (T, error)` + `New() T` |
| `domain.Writer` | domain | `Insert(Insertable) (ID, error)` / `Update` / `Archive` / `Unarchive` / `Delete` (non-generic; ValidEntity carries the source) |
| `domain.Repository[T]` | domain | `Reader[T] + Writer` |
| `persistence.ScopedRepository[T]` | application | `Reader[T]` + `Scope(ctx *AppContext, opts ...WriteOption[T]) domain.Writer` |

Reads stay direct on the handle; writes go through `Scope`, which binds ctx (cancellation → pgx, actor → audit) + in-TX hooks and returns a pure `domain.Writer`. `infra.BaseRepository[T]` implements `Scope`; the consumer's repo (embed + a `FindByID` loader) satisfies `ScopedRepository[T]` with no extra code. The request ctx is `persistence.RequestContext` (embeds `context.Context` + `ID()`/`ActorSubject()`/`ActorIssuer()`/`ActorClaims()`), satisfied by `*configuration.AppContext`. There is no `domain.Context`.

Lifecycle-hook contract — one sealed marker, two hook types, two providers:

```go
type TxHandle interface { /* unexported txHandle() seal */ }
type AfterBeginHook[T any]   func(ctx *AppContext, t T, tx TxHandle) error
type BeforeCommitHook[T any] func(ctx *AppContext, t T, id domain.ID, tx TxHandle) error
type AfterBeginHookProvider[T any]   interface { AfterBegin(*AppContext, T, TxHandle) error }
type BeforeCommitHookProvider[T any] interface { BeforeCommit(*AppContext, T, domain.ID, TxHandle) error }
func WithAfterBegin[T any](fn AfterBeginHook[T])   WriteOption[T]
func WithBeforeCommit[T any](fn BeforeCommitHook[T]) WriteOption[T]
```

`TxHandle` is opaque to application code — the seal is unexported, so only the framework's `infra/pgxTxHandle` implements it. The canonical in-TX side effect: declare a port (in `application/` or `domain/`) taking a `persistence.TxHandle`, implement it in `infra/` where the adapter calls `fwinfra.UnwrapPgxTx(tx)` to get the live `pgx.Tx` and owns the SQL. The persister fires resolved closures at two fixed positions:

```
BEGIN
  ⬇ afterBegin()        ← position A
  data write (root + children when aggregate)
  outbox INSERT
  audit_events INSERT (when configured)
  ⬇ beforeCommit(id, …) ← position D
COMMIT
```

Flat path (`infra.Postgres`) and aggregate path (`infra.aggregate_persister`) fire at the same analogous positions; consumer code never knows which ran. The aggregate path fires one hook per `repo.Method()` call (granularity B), with all children reachable via `domain.GetCurrentItemsOf[VO]`.

## Auto Command Handlers

For trivial CRUD — logic fits entirely on the Entity via `BuildRules` — the framework ships ready-made generic handlers in `application/handlers/`. The service writes only the Command: input boundary (`ToEntity`/`ApplyTo`/`ApplyPartiallyTo`) AND output boundary (`FromEntity`) as methods on the Cmd struct. The wire `Response.FromResult` finishes the projection at the web layer. Wiring is one line — `&handlers.InsertCommandHandler[*User, *InsertUserCommand, results.InsertUserResult]{Repo: repo}` — no `Project` field, no `Auditor` field. Auto and manual handlers coexist (DDD preserved).

**In-TX side effects are opt-in via Cmd methods.** A Cmd declaring `AfterBegin(ctx, t, tx) error` and/or `BeforeCommit(ctx, t, id, tx) error` satisfies the matching provider interface; every Auto handler type-asserts both at the top of `Handle` and forwards them as `WriteOption[T]` closures to `Repo.Scope(ctx, opts...)`. Closures fire INSIDE the persister TX (positions A and D — see "Persistence ports"). Compile-time typo guard: `var _ persistence.BeforeCommitHookProvider[*T] = (*Cmd)(nil)`.

### Canonical vocabulary

Every Cmd implements input + output as methods on its own struct. `FromEntity(ctx, T) (TResult, error)` is required on all 6 verbs; bodyless verbs (Archive/Unarchive/Delete) typically set `TResult = fwresults.None` and return `fwresults.None{}, nil`.

| Operation | Command (base) | Input method | Framework handler | Strict body? | Verb |
|---|---|---|---|---|---|
| Insert | `InsertXxxCommand` (`CommandBase`) | `ToEntity(ctx) (T, error)` | `InsertCommandHandler[T,*Cmd,TResult]` | no | POST |
| Update (full) | `UpdateXxxCommand` (`CommandBaseWithID`) | `ApplyTo(ctx, T) error` | `UpdateCommandHandler` (embeds `pipeline.FullBody`) | **yes** | PUT |
| Partial Update | `PatchXxxCommand` (`CommandBaseWithID`, ptr fields) | `ApplyPartiallyTo(ctx, T) error` | `PartialUpdateCommandHandler` | no | PATCH |
| Archive | `ArchiveXxxCommand` (`CommandBaseWithID`) | `ApplyTo(ctx, T) error` | `ArchiveCommandHandler` | no | PATCH/DELETE |
| Unarchive | `UnarchiveXxxCommand` (`CommandBaseWithID`) | `ApplyTo(ctx, T) error` | `UnarchiveCommandHandler` | no | PATCH |
| Delete | `DeleteXxxCommand` (`CommandBaseWithID`) | `ApplyTo(ctx, T) error` | `DeleteCommandHandler` | no | DELETE |

The Cmd owns input AND output; the handler threads the request `*AppContext` to both boundaries (`ToEntity`/`ApplyTo` in, `FromEntity` out — same ctx, the hook for identity-aware translation/projection). State-transition verbs use `ApplyTo` to consume ctx + populate runtime authz fields. The handler exposes no `Project` field. `Request.ToCommand()` is a pure body mapper — **no ctx** — keeping web transport-only.

**Where Result and Response live.**
- `application/commands/xxx_user.go`: `XxxUserResult` (Go-pure, no JSON tags, no methods) + `Cmd.FromEntity(ctx, T) (Result, error)`.
- `web/requests/xxx_user_request.go`: `XxxUserResponse` (JSON tags) + `func (XxxUserResponse) FromResult(XxxUserResult) XxxUserResponse`.
- No projection: `TResult = fwresults.None`, `FromEntity` returns `fwresults.None{}, nil`, pair with `fwresponses.NoBody`. Runtime detects `responses.None` and emits the success envelope with no `data` field.

`Cmd.FromEntity` lives in application (app→domain ✓); `Response.FromResult` in web (web→application ✓). Domain never sees JSON tags; application never sees wire shape.

**Uniform pointer Cmd pattern**: handler's second type param is always `*Cmd` (`SetPathID` needs pointer receiver; value-receiver `ToEntity`/`ApplyTo` are promoted). **PUT vs PATCH is HTTP rule, enforced by type**: `UpdateCommandHandler` (all fields required) vs `PartialUpdateCommandHandler` (each optional) are distinct handlers on distinct Commands.

### Route wrappers

The `HandleCommand*` family; suffixes communicate what the endpoint accepts. No bare `HandleCommand`.

| Wrapper | Body? | Path ID? | Use |
|---|---|---|---|
| `fwweb.HandleCommandWithBody(pipe, sample, respProj, h, status)` | yes | no | POST (Insert) |
| `fwweb.HandleCommandWithBodyID(pipe, sample, respProj, h, status)` | yes | yes | PUT / PATCH |
| `fwweb.HandleCommandWithID(pipe, respProj, h, status)` | no | yes | Archive / Unarchive / Delete |

`HandleCommandWithBody{,ID}` flow: alloc `var req TReq` → strict-body check if handler embeds `pipeline.FullBody` → `c.Bind().Body(&req)` → `cmd := req.ToCommand()` → `cmd.SetPathID(c.Params("id"))` (WithID variant) → `Dispatch(pipe, AppContext(c), cmd, h)` → on Success `respProj(result.Value())` maps `TResult`→`TResp` (or `responses.None` envelope); on Failure `RespondFromResult` honors each notification's Semantic. `HandleCommandWithID` (no body): `new(T)` + `SetPathID` + `Dispatch` + projection.

Generic inference is anchored by the **sample `TReq`** (`requests.InsertUserRequest{}`) + the **`responseProjection` method value** (`requests.InsertUserResponse{}.FromResult`, signature `func(TResult) TResp`). No-projection routes pass `fwresponses.NoBody` (`func(fwresults.None) fwresponses.None`).

### HTTP error semantics

| Scenario | Status | Notification | Context |
|---|---|---|---|
| Malformed JSON body | **400** | `SchemaViolationNotification` (Schema) | `"Schema"` |
| Wrong-typed field | **400** | `SchemaViolationNotification` carrying `field` (JSON path) | `"Schema"` |
| Missing required field (FullBody) | **400** | `RequiredFieldNotification{}.WithSemantic(SemanticSchema)` | `"Schema"` |
| Domain rejects values (`BuildRules`) | **422** | business rules | varies |
| Resource absent | **404** | `RecordNotFoundNotification` | varies |

`RequiredFieldNotification` is one struct; the carried semantic distinguishes wire (Schema→400) from domain (default→422).

### Strict body via marker `pipeline.FullBody`

Handlers requiring all body fields embed `pipeline.FullBody`; wrappers type-assert `pipeline.FullBodyEnforcer` at construction. Reflection runs on the **Request** type (`TReq`), not the Command.

- **Strict** — reflect on `*TReq` listing exported fields (skips anonymous embedded, skips `json:"-"`); parse body into `map[string]json.RawMessage`; any missing key → **400** with one `RequiredFieldNotification` per field (Schema). Malformed JSON → 400 `SchemaViolationNotification`.
- **Lenient** (no marker) — missing body = `{}`, partial body OK, invalid JSON → 400.

ALL exported non-anonymous non-`json:"-"` fields are mandatory under the marker — **no per-field opt-out**; flexibility is PATCH (no marker). Expected set is reflected once at construction, cached by `reflect.Type` in a module-level `sync.Map`. Only `UpdateCommandHandler` implements the marker; Insert deliberately does not (POST accepts optional fields).

### End-to-end skeleton

```go
// application/commands/insert_user.go — Command + Result co-located
type InsertUserCommand struct {
    pipeline.CommandBase
    Name, Email string
    Phone       *string
}
// ToEntity gets *AppContext: only layer translating ctx → business fields.
func (c InsertUserCommand) ToEntity(_ *configuration.AppContext) (*User, error) {
    return &User{Name: c.Name, Email: c.Email, Phone: c.Phone}, nil
}
// FromEntity is symmetric output on the Cmd, same ctx boundary.
func (c InsertUserCommand) FromEntity(_ *configuration.AppContext, u *User) (InsertUserResult, error) {
    return InsertUserResult{ID: *u.GetID(), Name: u.Name, Email: u.Email, Phone: u.Phone}, nil
}
// Optional in-TX side effect via convention; detected by type assertion.
// TxHandle is sealed — a port in application/ receives it; its infra/ adapter
// calls fwinfra.UnwrapPgxTx(tx) and owns the SQL.
func (c InsertUserCommand) BeforeCommit(ctx *configuration.AppContext, u *User, id domain.ID, tx persistence.TxHandle) error {
    return c.NotificationOutbox.EnqueueActivationRequested(ctx, tx, id)
}
var _ persistence.BeforeCommitHookProvider[*User] = (*InsertUserCommand)(nil)

type InsertUserResult struct { ID domain.ID; Name, Email string; Phone *string } // pure data

// web/requests/insert_user_request.go — Request + Response co-located
type InsertUserRequest struct {
    Name  string  `json:"name"`
    Email string  `json:"email"`
    Phone *string `json:"phone,omitempty"`
}
func (r InsertUserRequest) ToCommand() *commands.InsertUserCommand { // body-only, no ctx
    return &commands.InsertUserCommand{Name: r.Name, Email: r.Email, Phone: r.Phone}
}
type InsertUserResponse struct { /* JSON-tagged mirror */ }
func (InsertUserResponse) FromResult(r commands.InsertUserResult) InsertUserResponse { /* 1:1 */ }

// route
users.Post("/", fwweb.HandleCommandWithBody(d.Pipeline,
    requests.InsertUserRequest{}, requests.InsertUserResponse{}.FromResult,
    &handlers.InsertCommandHandler[*User, *InsertUserCommand, commands.InsertUserResult]{Repo: userRepo},
    fiber.StatusCreated))
```

**Shape rulers**: Request ≡ Command (required → `string`, optional → `*string`; `ToCommand` is 1:1, no normalization). Result ≡ Response (each field 1:1; `FromResult` is pure assignment). Other verbs follow the same Request+Command+Result+Response quad — Result/Response optional (`fwresults.None`/`fwresponses.NoBody`); Update strict via `pipeline.FullBody`, PATCH lenient, bodyless verbs via `HandleCommandWithID`.

### UnarchiveCommandHandler asymmetry

`UnarchiveCommandHandler` does NOT call `FindByID` (record is archived; `FindByID` filters `WHERE deleted_at IS NULL`). It gets an empty sample via `Repo.New()`, sets the path ID, passes to `GetUnarchivable`; archived-children cascade is direct SQL in `aggregate_persister.unarchiveAggregate`. The asymmetry is internal — `routes.go` wiring is identical. `Repo.New()` is part of `domain.Reader[T]`; `BaseRepository[T]` users inject `NewEntity func() T`.

### Manual path (cross-service, side effects)

A handler with external IO, an injected domain service, or orchestration manually implements `pipeline.Handler[*Cmd, TResult]` and registers via the same `HandleCommand*` wrappers. In-TX side effects: pass `persistence.WithAfterBegin[T](fn)` / `persistence.WithBeforeCommit[T](fn)` to `repo.Scope(ctx, opts...).Method(valid)` — same positions A/D and same `ctx/t/id/tx` payload as the Auto path.

```go
func (h *CreateUserAdminHandler) Handle(ctx *configuration.AppContext, cmd *AdminCreateUserCommand) (Result, error) {
    insertable, err := domain.GetInsertable(buildUser(cmd), h.svc, "AdminCreate")
    if err != nil { return Result{}, err }
    id, err := h.repo.Scope(ctx,
        persistence.WithBeforeCommit[*User](func(ctx *configuration.AppContext, u *User, id domain.ID, tx persistence.TxHandle) error {
            return h.NotificationOutbox.EnqueueAdminUserActivated(ctx, tx, id) // adapter unwraps tx
        }),
    ).Insert(insertable)
    if err != nil { return Result{}, err }
    return Result{ID: id}, nil
}
```

## Read-side wrappers (Auto Query Handlers)

Symmetric to the write side. Every GET declares **input** via Request DTO with `query:"..."`/`filter:"..."` tags (allowlist) AND **output** via a mandatory projector `func(map[string]any) R`. The wrapper enforces both at the wire boundary; application stays Fiber-agnostic.

### Canonical wrappers

| Wrapper | Path ID? | Allowlist | Flow |
|---|---|---|---|
| `fwweb.HandleQueryWithParams[TReq,TQ,R]` | no | reflection on TReq `query:"X" filter:"ops"` tags (cached); unknown key/operator → 400 `SchemaViolationNotification` | parse query → `ReadCriteria` → `req.ToQuery(criteria)` (no ctx) → Dispatch → handler `q.ToCriteria(ctx)` (JWT overlays) → projects each `page.Items` + `Data:[]R` + `Pagination` |
| `fwweb.HandleQueryWithID[TReq,TQ,R]` | yes | only `?includeArchived`; any other key → 400 | `req.ToQuery()` → `SetPathID` → Dispatch → `Reader.ReadByID` → projects `result.Value()` |

```go
users.Get("/", fwweb.HandleQueryWithParams(d.Pipeline,
    requests.FindUsersByParamsRequest{}, fwresponses.AutoFromDoc[requests.FindUsersByParamsResponse],
    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery]{Reader: d.ViewReader, View: view.Name()}))
users.Get("/:id", fwweb.HandleQueryWithID(d.Pipeline,
    requests.FindUserByIDRequest{}, fwresponses.AutoFromDoc[requests.FindUserByIDResponse],
    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery]{Reader: d.ViewReader, View: view.Name()}))
```

**Nested embed groups.** A struct field tagged `query:"prefix"` (no `filter:`) is an embed group — the walker recurses; each leaf produces a wire key `prefix.<leaf>` (`?addresses.zipCode.startswith=...`). Each leaf resolves to a **Go field path** (`Addresses.ZipCode`); `MongoViewReader` translates it to the physical column via the view `TableSchema`. Pagination keys are top-level only.

### Filter operators

Field tagged `query:"X" filter:"ops"` accepts the listed operators; wire is `?X.<op>=value` (no suffix for `eq`); operator outside the set → 400. Constants in `web/handle_query.go` (`fwweb.OpEq`, …), shared by `applyFilterParam` and the OpenAPI generator. Regex-operator values are escaped via `regexp.QuoteMeta`.

| Operator | Semantic | Mongo emission |
|---|---|---|
| `eq` (default) | exact equality | `{name:"Bob"}` |
| `ne` | inequality | `{name:{$ne:"Bob"}}` |
| `in` / `nin` | (not) in list | `{email:{$in:[...]}}` / `$nin` |
| `gte`,`lte`,`gt`,`lt` | ordinal (no `i` variant) | `{age:{$gte:18}}` |
| `startswith` / `contains` | prefix / substring (case-sensitive) | `{name:{$regex:"^Bob"}}` / `{$regex:"ob"}` |
| `ieq` / `ine` | case-insensitive (in)equality | `{$regex:"^bob$",$options:"i"}` / `$not` |
| `iin` / `inin` | case-insensitive (not-)in-list | `RegexMatchList` sentinel → `$in`/`$nin` of `bson.Regex` |
| `istartswith` / `icontains` | case-insensitive prefix / substring | `{$regex:"^bob",$options:"i"}` / `{$regex:"OB",$options:"i"}` |

**Multiple operators on one field are AND-ed** — folded into a `queries.MultiClause` sentinel on `ReadCriteria.Filter[field]`; `MongoViewReader` expands it into a top-level `$and`. A single operator stays plain `{field:value}` so indexes remain usable.

### Reserved control keys

All declared on the Request DTO as a field with the reserved `query:` tag and **no** `filter:` tag. Full wire/edge-case surface: docs/ site.

- **`?fields=` sparse projection** (`Fields *string query:"fields"`). Boot guard: when R is a struct, every exported field at every depth must be `*T`/slice/map AND carry `,omitempty` — violations panic at construction naming each path. Tokens validated against R's `json:` wire paths, translated wire→Go→column; unknown → 400 `SchemaViolationNotification{field:"fields[<bad>]"}`. Auto-excludes `_id:0` unless `id` is requested.
- **`?sort=` ordering** (`Sort *string query:"sort"`). Allowlist over R's wire paths; optional `-` prefix = descending. Unknown → 400 `SchemaViolationNotification{field:"sort[<token>]"}`. Emits `SortField{Field:goPath, Desc}`; reader translates to column + `bson.D`. Multi-key applied in declaration order.
- **`?after=`/`?before=` keyset pagination** over `(sort_values..., _id)` — `_id` ASC is the final tiebreaker. Opaque cursor `base64url({"v":1,"k":[...,"<_id>"],"h":"<context_sha256>"})`; `h` = `queries.HashContext(filter, sort, search, includeArchived)`. Any context mismatch (sort/filter/search/archived changed, tuple-length mismatch, corrupt, `after`+`before` together) → 400 `SchemaViolationNotification` on the cursor key. Stateless — no session, no TTL, no store.
- **`?limit=` ceiling** — cascade `ViewDefinition.MaxLimit(N)` > yaml `query.maxLimit` > framework `100`. `N>resolvedMax` → 400 `LimitExceededNotification{value:"<resolvedMax>"}` (Schema). `MaxLimit` is operational state — does NOT participate in `RebuildHash`/`ArtifactHash`.
- **`?onlyTotal=true` count-only** (`OnlyTotal *bool query:"onlyTotal"`) — short-circuits to `CountDocuments(filter)`; envelope omits `Data`, `Pagination` is `*TotalOnlyPagination{Total}`. Incompatible with `fields`/`sort`/`limit`/`after`/`before` → 400; filters + `?search` + `?includeArchived` stay valid. `Response.Pagination` is typed `any` to carry both shapes.

```yaml
# microservice.<profile>.yaml — service-wide override (views without own MaxLimit)
query: { maxLimit: 200, maxExportRows: 5000 }
```

### Tabular export

`fwweb.HandleQueryExport[TReq,TQ]` (+ `HandleQueryAsCSV` / `HandleQueryAsXLSX`) stream the same view read as a flat file, reusing the SAME Request DTO + `pipeline.Handler[TQ, queries.Page]`. Layout is hierarchical (one column per nesting level), headers from each column's `labelKey` via the `Translator`. Columns default to the view's business columns; `?fields=` narrows via the **view schema** allowlist. Filters/`?search`/`?sort`/`?includeArchived` apply; user pagination is ignored (full filtered set capped at the resolved export ceiling: `ViewDefinition.MaxExportRows` > yaml `query.maxExportRows` > `infra.DefaultMaxExportRows` 10000, with `ReadCriteria.BypassMaxLimit=true`). Format is a pluggable `web/export.Encoder` (`export.CSV`, `export.XLSX`). OpenAPI-aware siblings `HandleQueryAs{CSV,XLSX}Spec` return `(handler, openapi.RouteSpec)` with `FileResponse` + `OmittedQueryParams`. Full surface: docs/ site.

### Projector contract

`func(map[string]any) R` — same signature in both wrappers. The doc is already **Go-field-keyed** (`MongoViewReader` translated column→Go via `TableSchema`); the Response carries only `json:"<wire>"` tags (renamed columns handled infra-side by `TableSchema.Field("Email","mail")`).

| Projector | Use when |
|---|---|
| `fwresponses.AutoFromDoc[R]` | **Default.** Tag-driven; keys by Go field name, `json:` governs wire shape. Recursive; normalizes `_id→id`, nil slices → empty. |
| `fwresponses.RawDoc` | `func(map[string]any) map[string]any` passthrough when the doc shape IS the wire contract. |
| `R{}.FromDoc(map[string]any) R` | Custom logic — derived/conditional/ctx-aware shaping. |

### Manual query handlers

Building blocks for bypassing the auto wrapper (custom path identifier, vendor lookup, bespoke envelope):

- `fwweb.NewQueryParser[Req,Resp]() *QueryParser` — typed Mount-time parser; construction runs the same boot scan (fields structural guard, sort `slog.Warn`, projection-schema build). `parser.Parse(c) (criteria, badField, ok)` with allowlist + translation. When `Resp` is `map[string]any`, degrades to `ParseCriteria`. Construct once, reuse per request.
- `fwweb.ParseCriteria(c, req) (criteria, badField, ok)` — un-typed escape hatch (RawDoc / no `?fields`/`?sort` opt-in); allowlist applies, projection schema nil so `?fields=`/`?sort=` are pass-through.
- `fwweb.BindPath(c, &req) (badField, ok)` — populates `path:"<name>"` fields from `c.Params`. Returns `("",true)` when no `path:` tags.
- `fwweb.RespondSchemaViolation(c, pipe, field)` — canonical 400 envelope.
- `fwweb.ProjectPage[R](page, fn)` / `fwweb.RespondPaged[R](c, status, page, fn)` — projection + envelope helpers.

Chain: `BindPath → NewQueryParser.Parse (or ParseCriteria) → ToQuery/ToCommand → Dispatch`.

```go
listParser := fwweb.NewQueryParser[requests.FindXxxCustomRequest, requests.FindXxxCustomResponse]() // boot scan
g.Get("/", func(c fiber.Ctx) error {
    appCtx := fwweb.AppContext(c); appCtx.SetParent(c)
    var req requests.FindXxxCustomRequest
    crit, badField, ok := listParser.Parse(c)
    if !ok { return fwweb.RespondSchemaViolation(c, pipe, badField) }
    result := pipeline.Dispatch(pipe, appCtx, req.ToQuery(crit), h)
    if !result.IsSuccess() { return fwweb.RespondFromResult(c, result, fiber.StatusOK) }
    return fwweb.RespondPaged(c, fiber.StatusOK, result.Value(), requests.FindXxxCustomResponse{}.FromDoc)
})
```

### `path:` struct tag — universal URL-segment binding

Every Request DTO field tagged `path:"<name>"` is populated from `c.Params("<name>")` (with type conversion) before `ToCommand`/`ToQuery`. Closes the gap between canonical wrappers (auto-bind only `:id`) and custom/compound segments (`:email`, `/tenants/:tenantId/users/:id`).

| Wrapper | Auto-binds `:id`? | `path:"<other>"`? | `path:"id"`? |
|---|---|---|---|
| `HandleCommandWithBody` | No | Yes | Yes (map `SetPathID` in `ToCommand`) |
| `HandleCommandWithBodyID` | Yes | Yes | **No** — boot panic |
| `HandleCommandWithID` | Yes | n/a | n/a |
| `HandleQueryWithParams` | No | Yes | Yes |
| `HandleQueryWithID` | Yes | Yes | **No** — boot panic |

**Supported types**: `string`, signed/unsigned ints, `float32/64`, `bool`, `uuid.UUID`, `domain.ID`; pointer/slice/struct rejected at boot; conversion failure → 400 `SchemaViolationNotification`. **FullBody interaction**: strict-body check skips `path:`-tagged fields; declaring both `path:"X"` and `json:"X"` on one field is a boot panic. **Runtime guard**: each ID-requiring auto handler calls `handlers.RequirePathID(...)` first in `Handle` — empty ID → panic caught by `pipeline.Run` → 500. Handlers embed `pipeline.PathIDRequired` so wrappers detect the requirement; pairing a no-`:id`-bind wrapper with an ID-requiring handler and no `path:` tag logs `slog.Warn` at construction.

## Aggregate persistence (transparent dispatch)

Entities opt into aggregate-aware persistence by implementing `AggregateRootProvider`:

```go
type AggregateRootProvider interface {
    GetAggregateRoot() *AggregateRoot
    AggregateChildren() []AggregateValueObject  // declared boundary
}
```

`AggregateChildren()` declares **which types** belong to the aggregate (domain concern). Child table/FK live in the child's `TableSchema` via `.Child(...)` on the root schema. Universal symmetric cascade: root archive → children archive; root delete → children delete (FK `ON DELETE CASCADE`); root unarchive → children unarchive.

The top-level primitives `AddAggregateChild`/`ChangeAggregateChild`/`RemoveAggregateChild`/`ReplaceAggregateChildrenOf` consult `AggregateChildren()` and reject VOs of undeclared types with `InvalidAggregateChildNotification` (422). `AggregateConstructor` (DB load) bypasses the type-guard — types come from the schema's `Child(...)`, already trusted.

`GetInsertable/GetUpdatable/GetArchivable/GetDeletable/GetUnarchivable` detect the interface and attach `*aggregateMeta` (root pointer) to the ValidEntity. `infra.Postgres.Insert/Update/Archive/Delete/Unarchive` check `entity.AggregateInfo()` and dispatch to the aggregate path. They take `*TableSchema` (3rd arg) + the resolved `writeHook` (4th arg). Both flat and aggregate paths fire lifecycle hooks at the same TX positions.

**Child validation is transparent.** `runAggregateValidations` (inside `validateForInsert/Update/Delete`) iterates `root.AllAggregateItems()`: for each typeName, fires `BuildRules(actionName, svc, r)` on each non-`Removed` item, with `r` pre-scoped at `[NameSegment(collection), IndexSegment(i)]` (segment via `toLowerCamel(typeName)`). The root's `BuildRules` need not register children. `AddAggregateValueObject` remains for typeNames **outside** `AggregateChildren()` (VOs without their own table, e.g. tags in a JSONB column); typeNames already in the map are skipped to avoid double validation.

### Guarantees of the aggregate path

| Guarantee | Where |
|---|---|
| Single `pgx.Tx` for root + all children | `infra/aggregate_persister.go` |
| Exactly one outbox row per call (the aggregate IS the event unit) | `insertAggregate` / `updateAggregate` |
| FK injected from root id before child INSERT (child struct must NOT include FK field) | `insertChild` |
| Status iteration: Added→INSERT, Changed→UPDATE, Removed→Archive, Constructor→no-op/INSERT | `applyChildChanges` / `insertChildren` |
| Root archive cascades archive of all active children | `archiveAggregate` |
| Root unarchive restores archived children (requires `ArchivedFinder` on Repository) | `unarchiveAggregate` |
| Hard delete relies on FK `ON DELETE CASCADE` | `deleteAggregate` |
| Lifecycle hooks fire ONCE per call at positions A and D | `fireAfterBegin` / `fireBeforeCommit` in `infra/hook_dispatch.go` |

### Example consumer

```go
type User struct {
    domain.AggregateRoot
    Name, Email, Username, Phone string
}

func (u *User) GetAggregateRoot() *domain.AggregateRoot { return &u.AggregateRoot }
func (u *User) AggregateChildren() []domain.AggregateValueObject {
    return []domain.AggregateValueObject{Address{}}   // aggregate boundary (domain)
}

// Domain methods — commands call these, not the primitives.
func (u *User) AddAddress(addr Address) {
    for _, existing := range domain.GetCurrentItemsOf[Address](&u.AggregateRoot) {
        if existing.sameBusinessIdentity(addr) {
            u.AddNotification("Address", DuplicateAddressNotification{})
            return
        }
    }
    domain.AddAggregateChild(u, addr)
}
func (u *User) ChangeAddress(o, r Address)    { domain.ChangeAggregateChild(u, o, r) }
func (u *User) RemoveAddress(a Address)       { domain.RemoveAggregateChild(u, a) }
func (u *User) ReplaceAddresses(as []Address) { domain.ReplaceAggregateChildrenOf(u, as) }

type UserRepository struct{ fwinfra.BaseAggregateRepository[*User] }

func NewUserRepository(pg *fwinfra.Postgres) *UserRepository {
    r := &UserRepository{
        BaseAggregateRepository: fwinfra.NewBaseAggregateRepository[*User](
            pg, func() *User { return &User{} }),
    }
    r.WithSchema(UserSchema())   // one schema → write + criteria + scan + children
    return r
}
// FindByID / FindArchivedByID promoted by BaseAggregateRepository[*User].
```

## Persistence

```go
// Setup (once)
pg, _ := infra.NewPostgres(ctx, dsn)
mongo, _ := infra.NewMongoDB(ctx, mongoURI, dbName)
pipe := pipeline.New(translation.Default())
// pg.WithAudit(...) configured ONCE at boot — every write routes through it.

type UserRepository struct {
    infra.BaseRepository[*User]
    loader *infra.AggregateLoader[*User]
}
func NewUserRepository(pg *infra.Postgres) *UserRepository {
    newUser := func() *User { return &User{} }   // single factory source of truth
    r := &UserRepository{
        BaseRepository: infra.BaseRepository[*User]{
            Postgres: pg, ContextName: "User", NewEntity: newUser,
            Constraints: map[string]infra.ConstraintBinding{
                "users_email_active_idx": {Notification: EmailAlreadyExistsNotification{}, Field: "email"},
            },
        },
    }
    schema := UserSchema()
    r.Schema = schema
    r.loader = infra.NewAggregateLoader[*User](pg, newUser).WithContextName("User").WithSchema(schema)
    return r
}
func (r *UserRepository) FindByID(id domain.ID) (*User, error) {
    return r.loader.FindOne(context.Background(), criteria.ByID(id))
}

// Handler — binds the write scope and calls the pure domain.Writer.
func (h *CreateUserHandler) Handle(ctx *configuration.AppContext, cmd CreateUserCommand) (domain.ID, error) {
    user := &User{Name: cmd.Name /* ... */}
    for _, addr := range cmd.Addresses { user.AddAddress(addr) }
    insertable, err := domain.GetInsertable(user, nil, "GetInsertable")
    if err != nil { return domain.ID{}, err }
    return h.repo.Scope(ctx).Insert(insertable)
}
```

**`infra.BaseRepository[T]`** implements `Scope(ctx, opts...) domain.Writer` (the `boundWriter` carries the 5 writes delegating to `infra.Postgres` with the bound ctx) and `New() T` via `NewEntity`. **`NewEntity` is mandatory** (`New()` panics if nil; typically shared with the loader). **`infra.ConstraintBinding`** translates unique violations (PG `23505`) into `*InfrastructureError` carrying the typed notification via `infra.FieldErrorWithCause`. Unregistered constraints / other codes / non-pgErr errors return raw.

**`infra.AggregateLoader[T]`** loads live aggregates (root + children) via the search engine — `FindOne(ctx, *criteria.Query)` / `FindAll(ctx, *criteria.Query)`. Two coexisting scan modes:

- **Auto-scan (default)** — columns for root and children come from the `TableSchema` threaded via `WithSchema`. SELECT list and scanner share the one `column ↔ Go field` map, so a renamed column round-trips. Absence of `WithRootScanner` activates root auto-scan; the schema's `Child(...)` activate child auto-scan.
- **Manual (`WithRootScanner`/`WithChildScanner`)** — service supplies the scan fn for non-trivial decoding (JOIN, CASE, COALESCE). Per-typeName, manual wins over auto. **A manual root scanner with `FindOne`/`FindAll` must populate the entity id** (scan + `SetID`).

Both modes: archived scope governs the `deleted_at` gate (root + children). Nonexistent root → `*DomainError` + `RecordNotFoundNotification` (404). T must satisfy `domain.Entity`.

### Entity search engine (`criteria`)

`infra/criteria` is the backend-neutral DSL for loading **live domain aggregates** from PostgreSQL — the dev-facing, compile-time counterpart to the end-user Mongo read side. It returns source-of-truth aggregates ready for a command (`GetUpdatable`); it does NOT replace `ViewReader.ReadByID`/`ReadPage` (eventually-consistent projection, returns documents).

| Layer | Posture |
|---|---|
| Pure DSL (`criteria`, stdlib only) | Sealed `Expr` tree + builder; field names are **Go field names**, resolved to columns by the loader |
| PG translator (unexported in `infra`) | `Visitor` walks tree → `WHERE` + `$n` args; identifiers via `validIdentifier`, values parameterized, `domain.ID` unwrapped via `.Value()` |
| Import boundary | `criteria` imported only by `infra`; `domain`/`application` stay in business vocabulary |

Operators: `Eq/Ne/In/Nin/Gt/Gte/Lt/Lte/Like/ILike/IsNull/NotNull`, `And/Or/Not` (nestable), sugar `Contains/StartsWith/EndsWith` (case-insensitive, escaped) + `Between`. Wrapped in a `Query` carrying `WHERE` + `OrderBy`/`OrderByDesc` + `Limit` + archived `Scope` (`Active` default / `IncludeArchived` / `OnlyArchived`, a `Query` method, NOT part of the boolean algebra). `criteria.ByID(id)` is the PK shortcut.

```go
func (r *UserRepository) FindByEmail(email string) (*User, error) {
    return r.FindOne(context.Background(), criteria.Where(criteria.Eq("Email", email)))
}
users, err := r.FindAll(ctx, criteria.Where(criteria.And(
    criteria.Eq("TenantID", t),
    criteria.Or(criteria.Eq("Status", "active"), criteria.ILike("Name", "bob%")),
)).OrderByDesc("CreatedAt").Limit(50))
```

`FindOne` returns one match or `RecordNotFoundNotification`; **>1 matches is an error**. `FindAll` returns a possibly-empty slice and batches children (`WHERE fk IN (...)` per child type, no N+1). Nullable PG fields map to **pointer types** in the domain (`Phone *string`); pgx writes NULL when nil, reads NULL as nil.

### Schema mapping (`TableSchema`)

Everything above infrastructure speaks the **Go field name** (PascalCase). The only place a physical column/table name appears is `TableSchema`: the **mandatory, explicit, complete** map between a Go type's fields and its physical columns. No convention, no name-inference — every persisted field is declared; an undeclared exported field is never persisted, scanned, or audited.

One `TableSchema` drives the write path, the criteria engine, and auto-scan read-back, and is reused by the read-side `ViewDefinition` — so a column rename round-trips everywhere automatically.

**Three-name model.** wire (`json:`/`query:` in `web/`) ↔ Go field (every layer above infra) ↔ physical column (`TableSchema.Field("Email","mail")` in `infra/`). The web membrane translates wire↔Go; the infra membrane translates Go↔column.

A type-anchored `NewTableSchema[T]` validates every `Field` against the Go type **at construction** (a missing/unexported field panics). A type-less `NewExternalSchema` describes an upstream service's columns for an external `FromSchema` embed source (no local struct).

```go
func UserSchema() *fwinfra.TableSchema {
    return fwinfra.NewTableSchema[*User]("users").
        PK("id").                       // single-column PK column (Go side fixed to ID by the Entity contract)
        Field("Name", "name").
        Field("Email", "mail").         // renamed column
        SoftDelete("deleted_at").       // managed: enables the predicate
        CreatedAt("created_at").        // managed: stamps NOW() on INSERT
        UpdatedAt("updated_at").        // managed: stamps NOW() on INSERT + UPDATE
        Child(AddressSchema())          // aggregate child, keyed by Go type name
}
func AddressSchema() *fwinfra.TableSchema {
    return fwinfra.NewTableSchema[Address]("addresses").
        PK("id").
        FK("user_id").                  // FK to root — injected by persister, NOT a struct field
        Field("Street", "street").Field("ZipCode", "zip_code").
        SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at")
}
// Type-less external source (no local struct):
fwinfra.NewExternalSchema("users").PK("id").Field("Email", "mail")
```

**Three managed columns — by presence, not a flag.** Calling `SoftDelete`/`CreatedAt`/`UpdatedAt` enables the behavior; omitting disables it. `created_at`/`updated_at` are actively stamped `NOW()` (never a DB `DEFAULT`). `SoftDelete` present → read gate `col IS NULL` + Archive/Unarchive write `col = NOW()`/`NULL`; omitted → Archive/Unarchive unavailable. On the read path the three are also readable under fixed logical names `CreatedAt`/`UpdatedAt`/`DeletedAt`.

**One declaration, every consumer.** `BaseAggregateRepository.WithSchema(schema)` (and flat `BaseRepository.WithSchema`) threads the schema into the write binding (`Schema`) AND the read loader (`Loader.WithSchema`). Children come from `schema.children` (`.Child(...)`). Boot checks run here, panicking at construction not first request. Assigning `r.Schema = schema` directly is the unchecked escape hatch.

**The same schema drives the Mongo view** — root via `.Schema(UserSchema())`, each embed via `fwinfra.FromSchema(...)`. From the schema the framework derives the embed's table, store kind (`NewTableSchema[T]` → local Postgres; `NewExternalSchema` → external/Mongo), and the join FK for an `EmbedMany`. A local embed's parent-side segment is **derived** from the schema's Go type (pluralized for `EmbedMany` via `domain.PluralizeWord`; type name for one-to-one `Embed`), so `.As(...)` is optional; an external embed has no Go type, so `.As(...)` is **required**.

```go
fwinfra.View("users").Version(1).Root("users").
    Schema(UserSchema()).
    EmbedMany("addresses", fwinfra.FromSchema(AddressSchema()))   // segment derived → "Addresses"
```

**Boot checks** (panic at construction): field-exists on the type; bijection over each source's full column set (mapped + PK + managed — no two map to one column); `Modes() ⟺ SoftDelete`; **PK mandatory + single-column** on every schema; **FK mandatory on aggregate children** (`Child(...)` without `.FK`; read side an `EmbedMany` without `.FK` / one-to-one `Embed` without `.On` are fatal `ValidateViewSchemas` errors); **aggregate depth = 1** — a grandchild `Child(...)` panics (model a separate aggregate; read-side depth uses nested `EmbedMany`/`Embed`); **aggregate boundary agreement** — `AggregateChildren()` and `.Child(...)` must name the same set. Width is unlimited.

**Audit stays map-blind** — `snapshot` keys and `changes[].field` are the Go field name, never the column. **Field labels** come from a `labelKey:"<catalogKey>"` struct tag (resolved by Go field name); a type-less `NewExternalSchema` declares them inline as an optional 3rd arg `Field("Name","name","PartnerNameField")` — **external-only** (passing a label on `NewTableSchema[T]` is a boot panic).

**Out of scope:** the schema maps *names*, not *shape*. A renamed column on an otherwise-conforming table is supported; non-conforming shapes (boolean/enum soft-delete, composite PK, exotic types) are rejected.

## Read side (CQRS)

- `infra/view.go` — `View(name).Root(table).Schema(ts).EmbedMany("field", FromSchema(childTs)).Embed("field", FromSchema(externalTs).On("fk").As("Seg"))` defines `ViewDefinition`s. `FromSchema` is the single embed source constructor; schema mandatory on root + every embed. `.DeleteOnArchive()` drops archived rows from the projection (default: archived rows survive — Mongo mirrors PostgreSQL symmetrically).
- `infra/composer.go` — composes documents from PostgreSQL. Omits `WHERE deleted_at IS NULL` by default; `.DeleteOnArchive()` applies the filter on root + every embed (cascade, no per-embed override). `EmbedMany` matches root `id` to child FK (`source.joinKey`); `Embed` uses `doc[source.joinKey]` as the source id.
- `infra/sync.go` — `SyncEngine` consumes Kafka and upserts Mongo views. Reads metadata from **Kafka headers + message key** (Debezium Outbox Event Router shape): `aggregate_id` ← Key; `aggregate_type`/`event_type` ← headers. `DELETED` → `mongo.Delete` (unconditional). `ARCHIVED` → compose+upsert by default; `mongo.Delete` when `.DeleteOnArchive()`. `UNARCHIVED` → re-compose+upsert (always).
- `infra/upstream_subscriber.go` — `UpstreamSubscriber` materializes upstream A's events into local Mongo and triggers downstream recompose. See "Cross-service composition".
- `infra/rebuild.go` — `RebuildView`/`RebuildViewSince`/`RebuildAllViews`.
- `application/queries/` — `ViewReader` port + `ReadCriteria`/`Page`; `QueryHandler.Read(ctx, view, criteria)`/`ReadByID(...)` (pure).
- `infra/mongo_view_reader.go` — `MongoViewReader` implements `queries.ViewReader`.
- `web/query_parse.go`, `web/query_routes.go` — `ParseReadCriteria` + `QueryRouter` Fiber adapter.

### Schema requirements for the read side

Every table read by the composer (root + embed sources) **must have a `deleted_at TIMESTAMP` column** — the soft-delete marker the SyncEngine + reader rely on. By default the composer omits the `deleted_at IS NULL` filter (consumers read archived rows only via `IncludeArchived=true` on the reader port). `.DeleteOnArchive()` applies the filter on root + every embed.

### Declarative Mongo surface

`ViewDefinition` carries every Mongo-side artifact. `bootstrap.Run` calls `infra.CheckServiceRegistry` (DB-per-service guard) then `infra.ApplyMongoSpecs` between `collectViews` and `SyncEngine.Start` — the cluster is brought to the declared shape before the first Kafka message lands.

**Index builders (one fluent builder):**

| Type | Builder |
|---|---|
| Ascending / Descending | `fwinfra.Index("email")` / `.Desc()` |
| Compound (ESR) | `fwinfra.Compound("email", "created_at")` |
| Unique / Sparse / Hashed | `.Unique()` / `fwinfra.Index("phone").Sparse()` / `.Hashed()` |
| Partial | `fwinfra.Index("deleted_at").Partial(fwinfra.Exists("deleted_at", false))` |
| TTL | `fwinfra.Index("expires_at").TTL(7 * 24 * time.Hour)` |
| Text (one per view) | `fwinfra.TextIndex("name","email").DefaultLanguage("portuguese").Weights(...)` |
| 2dsphere | `fwinfra.GeoIndex("location")` |

Each spec accepts `.Name("custom")` and `.Collation(...)`. Compose via variadic `.Indexes(...)` on `*ViewDefinition` (accumulates).

```go
fwinfra.View("users").Version(1).Root("users").
    Indexes(fwinfra.Index("email").Unique(), fwinfra.TextIndex("name","email").DefaultLanguage("portuguese")).
    JSONSchema(bson.M{"bsonType": "object", "required": []string{"_id", "email"}}).
    JSONSchemaValidationLevel(fwinfra.ValidationLevelStrict).
    JSONSchemaValidationAction(fwinfra.ValidationActionError).
    Collation(&fwinfra.CollationSpec{Locale: "pt", Strength: 1}).
    Capped(&fwinfra.CappedSpec{SizeBytes: 1 << 30, MaxDocs: 1_000_000}).   // mutually exclusive with TimeSeries
    TimeSeries(&fwinfra.TimeSeriesSpec{TimeField: "ts", MetaField: "sensor_id", Granularity: "seconds"})
```

`Version(N)` is **mandatory** (positive int). Bump on any rebuild-relevant change (root, embeds, DeleteOnArchive, $jsonSchema, collation, capped, time-series); it feeds `RebuildHash`, so a shape change without a bump aborts boot with `DriftForgotToBump`. Index-only changes need no bump.

`ValidateMongoSpec()` boot invariants: positive `Version`; `Capped` ⊕ `TimeSeries`; `CappedSpec.SizeBytes > 0`; `TimeSeriesSpec.TimeField` mandatory; `Granularity` ∈ `{seconds,minutes,hours}`; ≤1 `TextIndex`/view; every index declares ≥1 key; validation level ∈ `{strict,moderate,off}`; action ∈ `{error,warn}`; every index key and every `$jsonSchema.required` entry **names a column the composer emits** (root PK + mapped + 3 managed + FK, each embed subtree by doc field e.g. `addresses.zip_code`, plus `_id`) — keys are **physical column paths**, not Go names; a stray key aborts boot.

**Apply semantics (idempotent):** steady state → read-side round-trips only. New collection → `createCollection` carries collation/capped/time-series/validator in one round-trip. Validator update → `collMod` in place. Collation/capped/time-series divergence → **strict abort** (immutable; operator owns migration). Index divergence → strict abort with `IndexOptionsConflict`/`IndexKeySpecsConflict`; `OMNICORE_MONGO_FORCE_REBUILD=true` (operator opt-in) drops + recreates the conflicting index (indexes only, never collections).

**DB-per-service guard (`infra.CheckServiceRegistry`):** writes a per-boot marker under `omnicore_service_registry`; computes `foreign = observed − declared views − framework-owned − system.*`. `APP_PROFILE=dev` → `slog.Warn`, boot continues; any other profile → abort. Other service markers in the same DB → unconditional `slog.Warn`. Connection user needs `find/insert/update/remove`, `createIndex`, `collMod`, `createCollection`, `listCollections` (all in `dbOwner`).

### Cross-service composition

When B needs read-side data owned by A, the canonical path is **event-driven local projection in Mongo**: B subscribes to A's Kafka topic via a framework-managed consumer, materializes upstream state into a local Mongo collection in B's DB, and the framework recomposes every B view embedding that collection. B never reads A's database or HTTP surface on the request path; A is unaware of B.

**Declaration** — `microservice.<profile>.yaml` (canonical) or `Wiring.UpstreamSubscriptions` (manual/tests):

```yaml
upstreamSubscriptions:
  - topic: users.events
    collection: users                # local Mongo collection in B's DB
    workers: 2
    filter: [name, email]            # allowlist; nil/empty keeps full payload
    onUpstreamDelete: anonymize      # cascade (default) | anonymize | keep
    anonymizeFields: [name, email]
```

**Embed** — views consume the projected collection via an external `fwinfra.FromSchema` (type-less `NewExternalSchema`; `.On` = parent doc FK on one-to-one `Embed`; `.As` required, no Go type to derive the segment):

```go
fwinfra.View("orders").Version(1).Root("orders").Schema(OrderSchema()).
    Embed("buyer", fwinfra.FromSchema(
        fwinfra.NewExternalSchema("users").PK("id").Field("Name","name").Field("Email","mail")).
        On("buyer_id").As("Buyer")).
    EmbedMany("lines", fwinfra.FromSchema(OrderLineSchema())).
    Indexes(fwinfra.Index("buyer_id"))   // boot guard §8.1 requires it
```

**Runtime** — for every event on A's topic, `infra.UpstreamSubscriber`: (1) decodes payload as `map[string]any`, applies `Filter`; (2) dispatches by `event_type` — `INSERTED`/`UPDATED`/`UNARCHIVED` → `mongo.Upsert`; `ARCHIVED` → `Upsert` (default, `deleted_at` populated) or `Delete` (`DeleteOnArchive=true`); `DELETED` → by `OnUpstreamDelete` (`cascade` removes / `anonymize` upserts with `anonymizeFields` zeroed / `keep` no-op); (3) **recompose-ripple** — for every B view embedding the collection, finds local docs whose join field references the changed upstream id (`MongoDB.FindIDsByField`, index-only via §8.1) and re-composes + upserts each; (4) **failure isolation** — per-view recompose errors are logged + counted (`upstreamMetrics`) + persisted to `omnicore_upstream_failures` + skipped; the Kafka offset still advances (no poison pill).

**`omnicore_upstream_failures`** (PG): natural key `(subscription_topic, view_name, upstream_id, local_id, stage)` UNIQUE (`local_id=''` on `discover` stage); `error` overwritten per retry; `attempt` auto-incremented on conflict; `first_seen_at` frozen, `last_attempt_at` refreshed; `resolved_at` NULL while pending, set by `ResolveUpstreamFailures` when a clean recompose pass for `(subscription, view, upstream_id)` completes. Mirrors live state, not a growing log. All writes best-effort (PG error → `slog.Warn` + discard, never blocks the consumer).

**Retry — `UpstreamSubscriber.RetryPendingFailures(ctx) (int, error)`.** Lists pending by topic, dedups by `upstream_id`, re-runs `ripple` per id; idempotent. The framework owns the primitive; the consumer decides exposure (ticker / HTTP / RPC). **Inspection CLI — `omnicore-admin upstream-list-failures`** (read-only; `--topic`, `--view`, `--format text|json`, `--limit`).

**Boot guards** (`bootstrap.validateUpstreamSubscriptions`): §8.1 every external `FromSchema` embed declares a covering index on the join field (join field FIRST if compound); §8.2 collection names don't collide subscription↔subscription or subscription↔local view; §8.3 every external embed over X resolves to an `UpstreamSubscription.Collection=X` (view-on-view rejected — ripple is one-hop); §8.4 `onUpstreamDelete: anonymize` requires non-empty `anonymizeFields`.

**Schema mandatory on every view.** `infra.ValidateViewSchemas(views)` (called by `bootstrap.Run`) walks every view at any nesting depth and aborts when the root has no `.Schema(...)` or an external embed is missing `.As(...)`. No schema-less mode.

**Registry semantics.** The projected collection counts toward `omnicore_service_registry` (locally-managed) but NOT `omnicore_mongo_views` — an `UpstreamSubscription` has no `Version`/`JSONSchema`/declared indexes, so there's nothing to drift-check. `Filter` drift is operator-owned: change YAML + redeploy + `omnicore-admin replay-all-as-events` against A.

**Bootstrap path** for a new B against an A whose Kafka retention misses history: `omnicore-admin replay-all-as-events --aggregate <name>` (runs in A's process) reads every active row and inserts a synthetic `INSERTED` outbox event per row; Debezium picks them up; B subscribers consume them as real INSERTs.

## HTTP status mapping

Each `Notification` declares a **Semantic**; the framework maps it to an HTTP status automatically. The typed declaration on the notification IS the registration.

| Semantic | HTTP status |
|---|---|
| `SemanticValidation` (default) | 422 Unprocessable Entity |
| `SemanticSchema` | 400 Bad Request (wire contract violated) |
| `SemanticNotFound` | 404 Not Found |
| `SemanticMethodNotAllowed` | 405 Method Not Allowed |
| `SemanticConflict` | 409 Conflict |
| `SemanticForbidden` | 403 Forbidden |
| `SemanticUnauthorized` | 401 Unauthorized |
| `SemanticPayloadTooLarge` | 413 Content Too Large |
| `SemanticUnavailable` | 503 Service Unavailable |
| `SemanticInternal` | 500 Internal Server Error |

`statusFromNotifications` picks the first non-Validation Semantic; all-Validation falls back to 422. The enum is transport-agnostic. `MessageDTO.Semantic` + wire `ErrorMessage.Semantic` carry the typed identity so clients branch UI without parsing the HTTP code.

- **SemanticSchema** — emitted by `HandleCommandWithBody{,ID}` when the body violates the Request schema (malformed JSON, missing/wrong-typed field). Distinguishes "wire format violated" from "domain rejects values" (422).
- **SemanticInternal** — `fwweb.ErrorHandler` on recovered panic or any non-`NotificationCarrier` error escaping a handler/middleware (`InternalServerErrorNotification`, context `"Server"`). Panic value + stack stay on the server log only. `RespondWithInternalServerError` uses the English default instead of the Translator, so a handler bug cannot cascade into the error path.

`ErrorHandler` specializes three Fiber router codes; every other `*fiber.Error` is treated as an unknown escape → 500. Services needing custom HTTP semantics MUST emit a `NotificationCarrier`, never call `fiber.NewError`.

| Fiber code | Notification | Context | Semantic → HTTP |
|---|---|---|---|
| 404 (route not matched) | `RouteNotFoundNotification` | `"Route"` | NotFound → 404 |
| 405 (method not allowed) | `MethodNotAllowedNotification` | `"Route"` | MethodNotAllowed → 405 |
| 413 (body over `BodyLimit`) | `PayloadTooLargeNotification` | `"Request"` | PayloadTooLarge → 413 |

Kernel notifications already declare their Semantic; services override per-struct:

```go
func (EmailAlreadyExistsNotification) Semantic() domain.NotificationSemantic {
    return domain.SemanticConflict
}
```

## Naming conventions

| Item | Convention | Example |
|---|---|---|
| Notification struct | `<What>Notification` | `UsernameAlreadyExistsNotification` |
| Translation key | identical to struct name | `"RequiredFieldNotification": "Required field."` |
| Enum description key | `<Type>.<VALUE>` | `"EntityMode.INSERT": "Inserir"` |
| Entity files (services) | lowercase singular | `customer.go` |
| Generic type param | `T` for value, `TEntity` for entity | `Result[T]`, `Repository[TEntity]` |

## Quick reference — where to add things

| Need | File / surface |
|---|---|
| New domain notification | `domain/notification_core.go` |
| New application notification | `application/notifications/core.go` |
| New translation key | `application/translation/{ptbr,eng,esp,fra,deu,ita,nld}.go` (keep all 7 in sync) |
| New value object | new file in `domain/` |
| New respond helper | `web/response.go` |
| Custom HTTP status on a notification | service struct overrides `Semantic() domain.NotificationSemantic` |
| Notification with runtime vars | `tvar:"<name>"` tags + `{<name>}` placeholders; per-emit `r.AddNotificationWithVars(...)`; escape hatch `TranslationVars()` |
| New audit field | `infra/audit/event.go` |
| In-TX side effect (Auto path) | declare `BeforeCommit`/`AfterBegin` on the Cmd (detected via `persistence.BeforeCommitHookProvider[T]`) |
| In-TX side effect (manual path) | `persistence.WithBeforeCommit[T](fn)` / `WithAfterBegin[T](fn)` on `repo.Scope(...)` |
| Read/write state in a hook's TX | port whose method takes `persistence.TxHandle`; adapter calls `fwinfra.UnwrapPgxTx(tx)` |
| Declare aggregate child | root `AggregateChildren() []AggregateValueObject`; table/cols/FK in child `TableSchema` via `root.Child(...)` |
| Trivial CRUD without a handler | `handlers.{Insert,Update,PartialUpdate,Archive,Unarchive,Delete}CommandHandler[T,*Cmd,TResult]` |
| Route wrapper — with body | `fwweb.HandleCommandWithBody{,ID}(...)` |
| Route wrapper — no body | `fwweb.HandleCommandWithID(pipe, fwresponses.NoBody, h, status)` |
| Route wrapper — paged list | `fwweb.HandleQueryWithParams(pipe, req, fwresponses.AutoFromDoc[R], h)` |
| Route wrapper — by-id GET | `fwweb.HandleQueryWithID(...)` |
| Manual query allowlist | `fwweb.NewQueryParser[Req,Resp]()` / `fwweb.ParseCriteria(c, req)` |
| Repository | embed `fwinfra.BaseRepository[T]` + `NewEntity func() T` + optional `Constraints` |
| Aggregate load | `fwinfra.NewAggregateLoader[T](pg, factory).WithSchema(schema)` + `FindOne`/`FindAll(ctx, criteria...)` |
| Load config | `bootstrap.LoadConfig()` (`APP_PROFILE` env; `./microservice.${APP_PROFILE}.yaml`) |
| Migrations | `migration.New(pg.Pool(), dir)` |
| Mongo drift reconcile | `infra.DetectViewDrift` + `SyncEngine.ExecuteRebuild`; `mongo.rebuild` yaml |
| Service capability | implement `bootstrap.Feature` / `ReadableFeature`; bundle in `Wiring.Features` |
| OpenAPI + Swagger | `Wiring.OpenAPI = &openapi.Config{...}`; document via `*Spec` wrappers + `openapi.Mount`/`MountRaw` |
| GraphQL endpoint | `Wiring.GraphQL = fwgraphql.New(d.Pipeline).Register(fwgraphql.Query/Mutation[...])`; own surface, not in Swagger; `graphql:` yaml knobs |
| Emit integration event | declare `integration.publishes.events.<key>` + `fwintegration.Dispatch(ctx, key, payload, opts...)` |
| React to integration event | declare `integration.subscribes...` + implement `bootstrap.IntegrationFeature` |
| Retry pending failures | admin routes calling `Receiver.RetryPendingFailures` / `UpstreamSubscriber.RetryPendingFailures` |

## Integration events

Cross-service async messaging — the write-side counterpart to sync `httpclient` and read-side `UpstreamSubscription`. Producers emit typed events into `integration_events` (atomic with the data write when invoked from a `BeforeCommit` hook with `WithTx(tx)`); subscribers consume Kafka via the `Receiver` registry, route each payload through the SAME `pipeline.Handler[TCmd, TResult]` HTTP routes consume, and dedup per consumer group via `omnicore_integration_processed`.

### Producer

```go
fwintegration.Dispatch(ctx, "userActivated", UserActivatedPayload{Email: email},
    fwintegration.WithTx(tx),              // atomic with data row + outbox + audit
    fwintegration.WithAggregateID(userID), // required when YAML declares `aggregate:`
    fwintegration.WithCorrelation(corrID), // optional — defaults to ctx.CorrelationID()
    fwintegration.WithCausation(causID),   // optional — defaults to ctx.CausationID()
)
```

```yaml
integration:
  publishes:
    events:
      userActivated:
        eventType: UserActivated   # wire header value
        aggregate: User            # optional; if declared, Dispatch requires WithAggregateID
        version: 1                 # optional — defaults to 1
```

`WithTx` lands the row in the entity write's TX (any Dispatch error aborts everything); without it the row autocommits on the package PG pool. Unknown `eventKey` → `ErrIntegrationEventNotConfigured` (validate-at-call-time). Framework auto-fills `event_id`, `thread_id` (`ctx.ID()`), `actor`, `created_at`; `correlation_id`/`causation_id` default to the inbound trace chain when emitted inside a receiver handler.

### Subscriber

```go
reg.From("partners").
    On("partnerOnboarded", requests.PartnerOnboardedRequest{}, insertHandler)
```

```yaml
integration:
  defaults: { consumerGroup: "...", workers: 4, startFrom: latest }  # latest | earliest
  subscribes:
    partners:
      topic: partners.integration.events
      events:
        partnerOnboarded: { eventType: PartnerOnboarded }
```

- **Handler invariance** — the registered handler is the SAME `pipeline.Handler` an HTTP route uses; the sample DTO carries `ToCommand()`. Reflection runs once at MountReceivers.
- **Boot guards** — a receiver whose `(sourceKey, eventKey)` is undeclared aborts boot; `integration.ValidateSubscriptionsCovered` aborts when a declared subscribe has no receiver; `startFrom` is enum-validated.
- **Consumer-group topology** — one Kafka reader per distinct `(topic, consumerGroup)`, demultiplexed by `event_type`. Unmatched `event_type` is skipped (offset commits). Two receivers on the same `(topic, consumerGroup, event_type)` abort boot.
- **Per-message pipeline (no outer TX)** — read `event_id`; pre-check `omnicore_integration_processed` (hit → ack, skip); build fresh `*AppContext` from inbound headers; unmarshal → `ToCommand()` → dispatch; success → `INSERT ... ON CONFLICT DO NOTHING` → ack; error → `RecordIntegrationFailure(...)`, offset still advances.
- **At-least-once** — a ms race between handler COMMIT and dedup INSERT can double-invoke after a crash. Handlers MUST be idempotent (UPSERT / `ON CONFLICT` / external idempotency keys).
- **Failure registry** — every failure persists to `omnicore_integration_failures`; `Receiver.RetryPendingFailures(...)` re-dispatches pending rows. Operators drive retry; the framework never auto-retries.

### DTO ownership and storage

Per-consumer copy. Each service owns its Go payload types; the wire JSON is the contract. Additive producer changes are non-breaking (unmapped fields ignored). Breaking changes use sibling event keys (`userActivated` → `userActivatedV2`) with distinct `eventType` + `version`; old and new coexist during migration.

| Table | Owner | Purpose |
|---|---|---|
| `integration_events` | producer | authoritative store of every emitted event; in-TX with the data row under `WithTx`; forensic timeline on `(aggregate_type, aggregate_id, created_at)` |
| `omnicore_integration_failures` | consumer | one row per handler failure; natural key `(consumer_group, source_key, event_key, event_id)` |
| `omnicore_integration_processed` | consumer | per-`(event_id, consumer_group)` dedup; BRIN index on `processed_at` |

All three ship via framework migration `0002_integration_events.{up,down}.sql`. `integration_events` has its own (long) retention for replay/audit; operator-driven pruning, no auto-prune.

### Coordinated shutdown

`shutdown.drainTimeoutSeconds` (default 30s) caps the parallel drain on SIGINT/SIGTERM. The framework drains HTTP server, integration consumer pool, and upstream subscribers in parallel under a shared `shutdownCtx`. Per-stage timeouts surface as `slog.Warn` naming the stage.

## Critical invariants

1. `ValidEntity` instances are created only via `domain` package functions (sealed types, private `entity()` method).
2. **Outbox is atomic with the data write** — `infra/executor.go` and `infra/aggregate_persister.go` run write + outbox INSERT + COMMIT in one `pgx.Tx`. Custom repos must preserve this.
3. One outbox row per aggregate operation (granularity B). Children contribute only to the snapshot payload; SyncEngine re-reads from Postgres, so the payload is informational.
4. **Audit travels with the persister.** Each write builds an `AuditEvent` routed by `audit.destinations`: `database` → INSERT into `audit_events` in the same TX; `slog` → structured line after COMMIT. Empty `destinations: []` disables audit.
5. Notifications are typed structs; the string message comes from the translation layer at the boundary. `NotificationKey` flows to the wire.
6. Domain has zero IO — pure types, validation, rules.
7. `domain.NotificationCarrier` is the cross-layer error contract: `error` + `NotificationContexts() []*NotificationContext`.
8. Kernel notifications embed their layer's base (`Domain`/`Application`/`Infrastructure`NotificationBase) — never mix.
9. Every Archivable has a symmetric Unarchivable.
10. **Mongo mirrors PostgreSQL by default.** DELETED → unconditional `mongo.Delete`. ARCHIVED → compose+upsert (doc survives with `deleted_at`) unless `DeleteOnArchive()` (hot-tier → `mongo.Delete`). UNARCHIVED → re-compose+upsert. The composer omits the `deleted_at IS NULL` filter by default; `DeleteOnArchive` views apply it on root + embeds.
11. Lifecycle hooks fire inside the TX, once per aggregate operation: `afterBegin` before any framework write, `beforeCommit` after all writes and before COMMIT. Same positions on flat and aggregate paths.
12. Hook error rolls the TX back; type identity preserved. `NotificationCarrier` → `Result.Failure` at the carrier's status; non-carrier errors → `Result.Exception` (500). Persister emits a best-effort `slog.Warn("persistence.hook.error", ...)`.
13. Hook panic rolls back AND propagates; `pipeline.Run`'s `defer/recover` is the one canonical recover point.
14. The `docs/` site is the source of truth for the public surface — every approved surface change updates `docs/content/sections/` + a `changelog.html` entry in the same round.
15. Integration events are at-least-once; consumer handlers must be idempotent (dedup is best-effort).
16. `integration_events` IS the producer-side audit; `audit_events` is unchanged and never carries integration payload. Cross-reference via `thread_id` (on both tables).
17. No outer TX on the Receiver path — each `Repo.Method` opens its own short TX, identical to the HTTP path.

## Migrations

Framework manages numbered SQL files in `cfg.Migrations.Dir` (default `./migrations`). Wrapper over `golang-migrate/migrate v4` — lock/recovery/parsing not reimplemented.

`cfg.Migrations.AutoRun` is an `AutoRunMode` enum (`check | true | false`; bare YAML bool normalizes). Default: `dev → true`, else → `check`. Explicit value wins. When `true`, `bootstrap.Run` runs `ValidateDownExists` + `Up` before serving HTTP.

### Conventions

Filename `{version}_{name}.{up|down}.sql`; `version` is a monotonic integer.

**Versions 1 and 2 are reserved for the framework control plane** (injected via `embed.FS` from `infra/migration/embedded/`). Version 1 (`0001_outbox`) creates `outbox` (Debezium CDC source) + `omnicore_mongo_views` (Mongo view registry). Version 2 (`0002_integration_events`) creates the integration tables. The framework tracks these in `omnicore_framework_migrations`; service migrations start at `0002+` in `omnicore_migrations` (no collision). Never write the framework SQL manually.

`.down.sql` is mandatory (`Manager.ValidateDownExists`) — may be `-- intentionally empty`. The same check rejects any `*.{up,down}.sql` without a parseable `{version}_{name}` prefix with `MigrationFilenameInvalidNotification`.

### Tracking

| Table | Who writes | Contains |
|---|---|---|
| `omnicore_framework_migrations` | Framework (embedded) | versions 1–2 |
| `omnicore_migrations` | Service | versions 2+ |

Separate tables avoid version collisions. Each stores `(version BIGINT PRIMARY KEY, dirty BOOLEAN)`. A mid-way failure leaves `dirty=true`, blocking `Up` until `Force`. `.down.sql` is read from disk/embed at `Down(N)` time, never stored.

### API

```go
mgr := migration.New(pg.Pool(), "./migrations")
mgr.Up(ctx)              // applies pending (framework then service)
mgr.Down(ctx, 1)         // reverts N service migrations
mgr.Status(ctx)          // (version uint, dirty bool, err) — service only
mgr.Pending(ctx)         // ([]uint, err) — on disk but unapplied
mgr.Force(ctx, 5)        // recovery: version=5, dirty=false (service)
mgr.ValidateDownExists() // checks .down.sql counterparts
```

### Strict mode (autoRun=check)

Non-dev default. `bootstrap.Run` does NOT apply migrations: reads `Status` (abort on `dirty=true`, naming `Force`), reads `Pending` (abort listing pending + current + recovery options A/B/C), else proceeds. Operator picks A (framework applies next boot), B (manual SQL reconcile + `INSERT INTO omnicore_migrations`), or C (skip the check). Migrations run between `wire(deps)` and SyncEngine start — schema ready before the Kafka consumer composes views.

## Mongo schema evolution

Symmetric to the Postgres migration policy, on the Mongo read-side projections: drift detection between the code-declared `ViewDefinition` and the materialized collection, the rebuild trigger, and orphan cleanup. The control plane lives entirely in PostgreSQL (`omnicore_mongo_views`, framework migration 0001). Mongo collections carry only domain data.

### Three-mode control model

`migrations.autoRun` and `mongo.rebuild.autoRun` share the enum `check | true | false` (dev → `true`, else → `check`; explicit wins).

| Mode | Validates? | Acts when safe? | Aborts on doubt? |
|---|---|---|---|
| `check` | yes | no — boot aborts with diagnostic | yes |
| `true` | yes | yes (linear drift, fresh init, wipe-recover, opted-in downgrade) | yes (ambiguous intent) |
| `false` | no | n/a | no — operator owns it |

### View versioning + spec hash

Every `ViewDefinition` declares a mandatory `Version(N)` (positive int) — the developer-intent signal; bump it whenever rebuild-relevant state changes (root, embeds, `DeleteOnArchive`, `$jsonSchema`, collation, capped, time-series). Index-only changes need no bump. Three deterministic SHA-256 hashes (stable across runs — serializer sorts keys, normalizes numeric/granularity/index order):

| Hash | Covers |
|---|---|
| `RebuildHash()` | version + rootTable + embeds + DeleteOnArchive + $jsonSchema + collation + capped + time-series |
| `ArtifactHash()` | Indexes only — flow through `ApplyMongoSpecs`, no rebuild |
| `Hash()` | combined identity stamped on the registry row |

### PG control plane — `omnicore_mongo_views`

Migration 0001 creates it alongside `outbox`. One row per managed view, keyed by `view_name`:

```
view_name              TEXT PRIMARY KEY
version                INTEGER NOT NULL CHECK (version > 0)
rebuild_hash           VARCHAR(64) NOT NULL
artifact_hash          VARCHAR(64) NOT NULL
combined_hash          VARCHAR(64) NOT NULL
previous_version       INTEGER
previous_combined_hash VARCHAR(64)
previous_applied_at    TIMESTAMP
status                 TEXT NOT NULL DEFAULT 'done' CHECK (status IN ('done','processing'))
started_at             TIMESTAMP        -- NULL when status='done'
pid                    TEXT             -- holder pid during 'processing'
host                   TEXT             -- holder host during 'processing'
applied_at             TIMESTAMP NOT NULL
applied_by             TEXT NOT NULL    -- "<svc>@pid:<n>" or "manual-reconcile-*"
code_version           TEXT             -- OMNICORE_CODE_VERSION env
```

Partial index `..._status_idx ON status WHERE status <> 'done'` keeps mid-flight queries constant-cost.

### Hybrid concurrency primitive

Two PG-side primitives in tandem, driven exclusively by `ExecuteRebuild`:
- `pg_advisory_lock` — cluster-wide mutual exclusion, auto-release on disconnect, no TTL math. Acquired on a pinned `pgxpool.Conn` via `infra.TryAcquireViewLock`.
- Status column — `done | processing` with `started_at`/`pid`/`host`; survives crashes (forensic).

State machine: `done →(processing, started_at, pid, host)→ processing →(rebuild)→ done (hashes, applied_at, started_at=NULL)`. Crash recovery: lock auto-releases on TCP close; next boot acquires cleanly, sees `status='processing'`, emits `slog.Warn "...taking over"`, re-runs `BeginRebuild`, proceeds.

### Drift detection at boot

`bootstrap.Run` runs `infra.DetectViewDrift` between `ApplyMongoSpecs` and `SyncEngine.Start`. Eight branches over (registry × Mongo × code version):

| Decision | Condition | autoRun=true | autoRun=check |
|---|---|---|---|
| `DriftNone` | combined hash matches | No-op | No-op |
| `DriftFreshInit` | no row + Mongo empty | Write row (`status='done'`) | Abort |
| `DriftAlienData` | no row + Mongo populated | **Always abort** | Abort |
| `DriftMongoWiped` | row matches + Mongo empty | Rebuild | Abort |
| `DriftArtifactOnly` | same version + rebuild_hash matches + combined differs | UPDATE row (no doc rewrite) | Abort |
| `DriftForgotToBump` | same version + rebuild_hash differs | **Always abort** | Abort |
| `DriftRebuildRequired` | registry version < code version | Rebuild | Abort |
| `DriftDowngrade` | registry version > code version | Abort unless `allowDowngrade: true` → rebuild | Abort |

Under `autoRun=false` every branch is skipped — runtime errors are the operator's concern.

### Rebuild execution

`SyncEngine.ExecuteRebuild(ctx, plan, cfg)` runs on one view on a pinned `pgxpool.Conn`: acquire+pin → `TryAcquireViewLock` (else read holder via `pg_locks`+`pg_stat_activity`, abort) → defer releases → detect takeover → `BeginRebuild` (status='processing') → cleanup orphan fields (`$unset` the difference) → snapshot `_id` set → compose+upsert from Postgres in batches of 1000 → orphan reconciliation (`delete`/`warn` per `cfg.Orphan`) → `EndRebuild` (status='done' + new hashes + `applied_at`, captures `previous_*`; last data write) → `slog.Info "view.rebuild.end"`. Fast paths: `InitRegistryOnly` and `RefreshRegistryArtifactOnly`.

### YAML, API, privileges

```yaml
mongo:
  rebuild:
    autoRun: check          # check | true | false — dev=true / else=check
    orphan: delete          # delete | warn — default delete
    allowDowngrade: false   # opt-in for canary / blue-green rollback
```

- Strict decoding on `mongo.rebuild`: unknown keys abort boot.
- `OMNICORE_MONGO_FORCE_REBUILD=true` governs only index divergence in `ApplyMongoSpecs`; it never triggers this rebuild path and never drops collections.
- Registry helpers (any `pgExec`): `infra.{ReadViewRegistry, InitViewRegistry, BeginRebuild, EndRebuild, ListNonDone}`. Lock helpers (pinned conn): `infra.{ViewLockKey, TryAcquireViewLock, ReleaseViewLock, ReadViewLockHolder}`. Drift: `infra.DetectViewDrift` → `*DriftReport`. Orchestration: `(*SyncEngine).{ExecuteRebuild, InitRegistryOnly, RefreshRegistryArtifactOnly}`.
- **PG privileges**: `SELECT, INSERT, UPDATE` on `omnicore_mongo_views`; `pg_try_advisory_lock`/`pg_advisory_unlock`; read on `pg_locks`/`pg_stat_activity`.
- **Mongo privileges**: `find, insert, update, remove, aggregate, collMod, createCollection, listCollections`.

## Required PostgreSQL schema

The `outbox` table is created by embedded migration 1 — never write its SQL manually (Debezium Outbox Event Router depends on the exact columns; canonical signature in `infra/migration/embedded/0001_outbox.up.sql`).

Service domain tables (migrations 2+) follow these conventions so executor-generated SQL and the composer's queries work:

```sql
CREATE TABLE users (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    deleted_at  TIMESTAMP,                -- soft-delete marker (TableSchema.SoftDelete)
    created_at  TIMESTAMP    NOT NULL,    -- framework stamps NOW() on INSERT
    updated_at  TIMESTAMP    NOT NULL     -- framework stamps NOW() on INSERT + UPDATE
);
```

The framework actively stamps `created_at`/`updated_at`; declare names/presence via `TableSchema.{CreatedAt,UpdatedAt}(col)`. `deleted_at` is required only for archivable entities (`TableSchema.SoftDelete(col)`).

**Child tables** carry an FK to the root, declared in the child `TableSchema` via `.FK("user_id")` (persister injects the root id on child INSERT):

```sql
CREATE TABLE addresses (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    deleted_at  TIMESTAMP,                -- MANDATORY: symmetric cascade archive/unarchive
    created_at  TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMP NOT NULL DEFAULT NOW()
);
CREATE INDEX addresses_user_id_idx ON addresses (user_id);
```

Universal symmetric cascade: root archive → `UPDATE addresses SET deleted_at = NOW() WHERE user_id = $1` in the same TX; root unarchive restores archived children; root delete relies on FK `ON DELETE CASCADE`.

Cross-service data materializes into **B's own Mongo database** via `UpstreamSubscription` (no local PG cache table). The local collection is upsert-managed by `UpstreamSubscriber`; embeds reference it via `fwinfra.FromSchema(fwinfra.NewExternalSchema("users")…)`. See "Cross-service composition".

## Concurrency and lifecycle

| Type | Thread-safe? | Lifecycle |
|---|---|---|
| `translation.Translator` | Yes (`sync.RWMutex`) | Singleton or per-service |
| `configuration.AppContext` | Yes (`sync.RWMutex` for language/metadata) | Per HTTP request |
| `pipeline.Pipeline` | Yes (stateless after construction) | Singleton per service |
| `domain.BaseEntity` / user entities | **No** — mutable validation state | Per operation (don't share) |
| `domain.NotificationContext` | **No** — slice appended | Owned by its BaseEntity |
| `infra.Postgres` (pgx pool) | Yes | Singleton per database |
| `infra.MongoDB` | Yes (driver pools) | Singleton per database |
| `infra.SyncEngine` | Yes (single consumer goroutine) | Singleton per service (`Start(ctx)`) |

**Handlers** hold the `persistence.ScopedRepository[T]` singleton and call `repo.Scope(ctx, opts...).Method(valid)` — `ctx` is the request `*AppContext`, `opts` the `WriteOption[T]`s the Auto handler derived from the Cmd's optional `AfterBegin`/`BeforeCommit`. Audit emission is automatic (configured once at boot via `WithAudit`); handlers never thread an Auditor.

## Go pitfalls specific to this codebase

1. **Methods can't be generic** — hence `pipeline.Run[T]`, `pipeline.Dispatch[TReq,TRes]`, `web.RespondFromResult[T]` are top-level functions taking `*Pipeline`, not methods.
2. **`errors.As` with the carrier interface** catches all layers without importing each:
   ```go
   var carrier domain.NotificationCarrier
   if errors.As(err, &carrier) { /* DomainError, ApplicationError, InfrastructureError, ... */ }
   ```
3. **Named return + `defer/recover`** — required in `pipeline.Run` to convert panics to `Result.Exception`.
4. **`reflect.TypeOf(n).Name()` strips pointers** — `*Customer` and `Customer` both give `"Customer"`.
5. **Private-receiver base methods cross packages via promotion** — `DomainNotificationBase.isNotification()` works when user notifications in other packages embed it.
6. **`Fields` is `map[string]any`** — non-deterministic order; `infra/executor.go` uses `sortedKeys` for deterministic SQL.
7. **`infra.validIdentifier` panics on bad input** — intentional SQL-injection defense (identifiers come from domain code, never user input).
8. **`google/uuid` is canonical** (vendored via Fiber). Don't add `gofrs/uuid`.
9. **`slog` levels** — `Info` routine, `Warn` audit failures (non-blocking), `Error` unhandled.
10. **Aggregate value objects are value types, not pointers** — `AggregateRoot` change-tracks via `reflect.DeepEqual`.

## Full request flow (concrete)

`POST /users` with body `{"name":...,"addresses":[{...}]}`:

```
1. Fiber middleware: build AppContext (UUID + Language from headers)
2. Body parse → CreateUserCommand
3. pipeline.Dispatch(pipe, appCtx, cmd, &CreateUserHandler{repo})
   └─ pipeline.Run (defer/recover)
      └─ handler.Handle(ctx, cmd)
         └─ user := &User{...}; user.AddAddress(addr) per addr
         └─ domain.GetInsertable(user, nil, "GetInsertable")
            └─ ensureInit / resetEntity / validateForInsert
               └─ BuildRules (root) + runAggregateValidations (each AllAggregateItems
                  with CurrentStatus != Removed; path = toLowerCamel(typeName))
               └─ checkAllNotifications → *DomainError if any
            └─ extractAggregateMeta → Insertable with aggregate metadata
         └─ opts := WriteOption[*User] from Cmd AfterBegin/BeforeCommit
         └─ repo.Scope(ctx, opts...).Insert(insertable)
            └─ Postgres.Insert(ctx, insertable, &Config, AdaptWriteOptions(opts))
               └─ AggregateInfo ok → insertAggregate:
                  BEGIN TX
                  ⬇ afterBegin (position A; rolls back on error)
                  INSERT users RETURNING id
                  INSERT addresses per Added child (user_id injected)
                  INSERT outbox (root + children snapshot)
                  IF Audit.Includes(database): audit.InsertAuditEvent (atomic)
                  ⬇ beforeCommit (position D; rolls back on error)
                  tx.Commit
                  (post-commit best-effort) IF Audit.Includes(slog): audit.EchoSlog
4. web.RespondFromResult(c, result, 201) → Response{Success:true, Data:id}

Async (eventually consistent):
5. Debezium tails outbox via WAL logical replication
6. Outbox Event Router → Kafka "users.events": key=aggregate_id,
   headers aggregate_type/event_type, value=payload snapshot
7. SyncEngine consumes (consumer-group partition):
   extractEvent → composer.Compose(view, aggregate_id)
     fetchRow(users,"id",id) WHERE deleted_at IS NULL → root
     applyEmbeds: fetchWhere(addresses,"user_id",id) WHERE deleted_at IS NULL
   mongo.Upsert("users", id, doc)
```

**Error path:** `BuildRules` adds notifications → `*DomainError` propagates → `pipeline.Run` catches via `NotificationCarrier`, translates to DTOs (each carries `NotificationKey` + `Semantic`) → `Failure[id]`. `RespondFromResult` calls `statusFromNotifications`: `SemanticValidation` → **422**; `SemanticNotFound` (e.g. `RecordNotFoundNotification`) → **404**.

## microservice.&lt;profile&gt;.yaml — declarative config

One file per profile at module root; `microservice.dev.yaml` + `microservice.prd.yaml` are the canonical pair. `bootstrap.LoadConfig` reads `APP_PROFILE` (required) and loads `microservice.${APP_PROFILE}.yaml`. `OMNICORE_CONFIG_PATH` overrides the path. Extra profiles accepted; only `dev` unlocks `auth.mode=disabled`. The profile is an env var, never a YAML field.

Substitution forms inside `${...}` (single pass, no recursion):

| Form | Behavior |
|---|---|
| `${VAR}` / `${VAR:default}` | Env var; set+non-empty wins, else default, else empty. Misses silent. |
| `${file:/abs/path}` | File contents read once at boot; trailing newline trimmed (PEM round-trips). Missing file → boot aborts. |
| `${vault:store/path#field}` | Delegated to a registered `bootstrap.SecretResolver`. Unregistered → boot aborts. |

`file`/`vault` are reserved names (strict — boot aborts on failure). Default cannot contain literal `}`. YAML 1.2 — quote ambiguous values (DSNs, URIs).

```yaml
# microservice.prd.yaml
service: my-service
http:
  addr: ":8080"
postgres:
  dsn: "${DATABASE_URL:postgres://localhost:5432/mydb}"
mongo:
  uri: "${MONGO_URI:mongodb://localhost:27017}"
  database: "${MONGO_DB:my_views}"
  rebuild:
    autoRun: check          # check | true | false — dev=true / else=check
    orphan: delete          # delete | warn
    allowDowngrade: false
kafka:
  brokers: ["${KAFKA_BROKERS:localhost:9092}"]
  syncGroupId: "${SYNC_GROUP_ID:my-service-sync}"
  syncWorkers: 4            # default runtime.NumCPU(); 1 = serial
migrations:
  dir: ./migrations
  autoRun: check            # check | true | false — dev=true / else=check
query:
  maxLimit: 200             # ceiling on ?limit=; 0/absent → 100; per-view override fwinfra.View().MaxLimit(N)
  maxExportRows: 5000       # ceiling on CSV/XLSX export rows; 0/absent → 10000; per-view .MaxExportRows(N)
auth:
  mode: jwt                 # jwt | disabled (disabled rejected unless APP_PROFILE=dev)
  jwt:
    algorithms: [RS256, ES256, EdDSA]  # default all three asymmetric
    issuer: https://idp.example.com
    audience: my-service
    leewaySeconds: 30
    jwksUrl: https://idp.example.com/.well-known/jwks.json
    # publicKeyPem: |       # mutually exclusive with jwksUrl
  externalValidator:        # optional revocation check
    method: POST            # GET | POST
    url: https://idp.example.com/realms/x/protocol/openid-connect/token/introspect
    tokenPlacement: form_field   # bearer_header | form_field | json_body | query_param
    tokenField: token            # required unless bearer_header
    extraHeaders:
      Authorization: "Basic ${IDP_CLIENT_CREDS}"
    success:
      jsonPath: $.active
      expectedValue: true
    timeoutMs: 2000         # default 2000
    failMode: closed        # closed | open; default closed
    cacheTtlSeconds: 0      # 0 disables cache; >0 caches positive answers N s
  publicRoutes:
    - GET /health
    - GET /ready
  auditClaims:              # JWT claims surfaced in audit actorClaims; empty default
    - tenant_id
    - roles
audit:                      # omitted block = both destinations
  destinations:
    - slog                  # post-commit echo (observability)
    - database              # in-TX audit_events row (source of truth)
  # destinations: []        # explicit empty disables audit
```

Mandatory: `service`, `postgres.dsn`, `mongo.uri`, `mongo.database`, `kafka.brokers`, `kafka.syncGroupId` — `LoadConfig` errors listing the missing. `auth:` defaults to `{mode: disabled}` when absent (rejected unless `APP_PROFILE=dev`); `mode: jwt` requires `issuer` + `audience` + exactly one of `jwksUrl`/`publicKeyPem`.

`audit.destinations` routes each successful write's `AuditEvent`: absent/default → both `slog`+`database`; `[database]` → only the in-TX PG row (compliance); `[slog]` → only the post-commit echo; `[]` → disabled. Unknown tokens or duplicates abort boot.

## Authentication middleware

When `auth.mode: jwt`, `bootstrap.Run` registers `fwweb.AuthMiddleware` right after `AppContextMiddleware`; every request is validated before any Feature route, and on success `AppContext.Identity` is populated. `disabled` → middleware not registered, `Identity()` stays nil, zero per-request cost. Middleware lives in `omnicore/web/auth_middleware.go`, taking primitive `AuthOptions` + `*pipeline.Pipeline`; `bootstrap.authOptionsFromConfig` flattens `AuthConfig` → `AuthOptions` (dependency direction `bootstrap → web`).

### Per-request flow

1. **publicRoutes bypass** — exact `METHOD /path` match → `c.Next()`.
2. **Bearer extraction** — `Authorization: Bearer <token>` (scheme case-insensitive). Empty/malformed → `MissingAuthorizationNotification`, **401**.
3. **Local JWT validation** — signature via `jwksUrl` (`MicahParks/keyfunc` fetches + caches JWKS, refreshes on `kid` miss) OR `publicKeyPem` (RSA/ECDSA/Ed25519 via `x509.ParsePKIXPublicKey`). `alg` allowlisted (`WithValidMethods` rejects symmetric HS\*); `iss`/`aud` pinned; `exp` enforced (`WithExpirationRequired` + `WithLeeway`).
4. **Identity attachment** — `SetIdentity(&Identity{Subject, Issuer, ExpiresAt, Claims})`. Also `SetBearerToken(token)` — consumed only by the `forward-bearer` httpclient provider; handlers read `Identity()`.
5. **Tenant gate (Layer 3, opt-in)** — when `AuthOptions.TenantRequired`, reject `Identity.TenantID() == ""` with `TenantMissingNotification (403)` before any handler.

### Error responses

| Scenario | Notification | HTTP |
|---|---|---|
| No/malformed `Bearer` header | `MissingAuthorizationNotification` | 401 |
| Malformed / bad sig / wrong `iss`/`aud` / bad `alg` / `!Valid` | `InvalidTokenNotification` | 401 |
| `exp` past (after leeway) | `ExpiredTokenNotification` | 401 |
| `TenantRequired` and empty TenantID | `TenantMissingNotification` | 403 |

Expired is split from Invalid so clients branch refresh-vs-relogin. Notifications in `application/notifications/core.go`, translated in all seven catalogs; `respondAuthFailure` derives status from each notification's `Semantic()` — new failure modes are notification changes, not middleware changes.

### External validator (revocation check)

When `auth.externalValidator` is set, an outbound IdP call (RFC 7662 introspection or compatible) runs **after** local validation — catches tokens revoked at the IdP despite a still-valid local signature. Lives in `omnicore/web/external_validator.go`.

**Cache opt-in, default off.** `cacheTtlSeconds > 0` enables an in-memory positive-only cache keyed by SHA-256 of the token (`tokenCacheKey`; raw token never stored). Only successes memoized; negatives and transport errors bypass cache so revocation is honored next request.

| Placement | Outgoing shape |
|---|---|
| `bearer_header` | `Authorization: Bearer <token>` (tokenField ignored) |
| `form_field` | form-urlencoded `<tokenField>=<token>` (Keycloak) |
| `json_body` | `{"<tokenField>": "<token>"}` |
| `query_param` | `?<tokenField>=<token>` |

**Success check** — body parsed as JSON, `success.jsonPath` (dot notation) walked, value compared to `success.expectedValue` via Go `==` on `any` (`true` != `"true"`). Miss/mismatch → reject. **Fail mode** — on validator error (transport, timeout, non-2xx, bad JSON): `closed` (default) rejects; `open` accepts when local pre-validation passed. Constructed at boot via `newExternalValidator` (invalid config fails boot, not per request); `http.Client.Timeout` enforces `timeoutMs` (default 2000).

### Actor on audit and events

The audit pipeline and `infra/events.SlogPublisher` read the principal from `persistence.RequestContext` (satisfied by `*AppContext`) and surface it on every artifact with no handler change: `actor` (JWT `sub`, or `"anonymous"`), `actorIssuer` (omitted when empty), `actorClaims` (opt-in subset declared by `auth.auditClaims`, audit only). Works for both Auto and manual handlers because every write goes through `repo.Scope(ctx, ...).Method(valid)`.

```json
{ "msg":"audit","entityType":"User","entityId":"abc-123","verb":"insert",
  "actionName":"GetInsertable","kind":"snapshot","actor":"user-42",
  "actorIssuer":"https://idp.example","actorClaims":{"tenant_id":"acme","roles":["admin"]},
  "snapshot":{"name":"Jane Doe"},"children":{"Address":[{"op":"inserted","snapshot":{}}]} }
```

Token issuance is out of scope — the IdP (Keycloak, Auth0, in-house) mints tokens; the framework only validates. Libraries: `golang-jwt/jwt/v5` (parsing/validation, no symmetric HS\*) + `MicahParks/keyfunc/v3` (JWKS cache).

## Authorization

Three concentric layers, each consuming `AppContext.Identity()` and surfacing rejections through the canonical envelope (`SemanticForbidden → 403`). The framework does NOT enforce identity at infra: `Postgres.*` and `MongoViewReader.*` execute what application validated.

### Layer 1 — Coarse-grained declarative gate (transport)

Enforces "route requires permission X", static across all requests. `fwopenapi.RequirePermission(p)` is a `MountOption` for `openapi.Mount`/`MountRaw`:

```go
fwopenapi.Mount(d.OpenAPIRegistry, users, fiber.MethodPost, "/", insertH, insertSpec,
    fwopenapi.Doc{Summary: "Create a user", Tags: tags},
    fwopenapi.RequirePermission("users:write"))
```

On the option, Mount/MountRaw: (1) patch `spec.RequiredPermission` so the generator appends `**Required permission:** \`<p>\``; (2) wrap the handler with `web.PermissionGate`, short-circuiting 403 `MissingPermissionNotification` (field `permission`) when `Identity.HasPermission(p)` is false. The 403 entry is auto-emitted in `/openapi.json` on EVERY non-public route when `auth.mode: jwt`.

**Permission format `resource:action`.** IdP emits a `permissions` claim (`auth.authorization.permissionsClaim`):

| Claim entry | Matches |
|---|---|
| `users:read` | exactly `users:read` |
| `users:*` | any `users:<anything>` |
| `*:*` | any request (super-admin) |

Tolerated claim shapes: `[]string`, `[]any`, space/comma-separated strings. nil/absent/unsupported → empty set (deny, never crash). **Caller-side wildcards panic** (`HasPermission("users:*")` is a bug → caught by `pipeline.Run` → 500); compose with explicit OR over concrete actions.

**Master switch `auth.authorization.enabled`** (default false). When false the runtime gate no-ops AND the description suffix is suppressed; 403 auto-emission still applies whenever auth is on. **Boot validation** when `enabled: true`: bootstrap scans the Registry (after `Wiring.BeforeServe`, before `openapi.Register`) — every non-public route MUST declare `RequirePermission(...)` or boot panics. **Adjacent enforcement** (independent of authz.enabled): when `Wiring.OpenAPI != nil`, `scanRouteRegistration` panics on any Fiber route registered outside `Mount`/`MountRaw`.

### Layer 2 — Fine-grained programmatic rule (domain)

Enforces rules dependent on resource state, principal claims, or their relationship ("only the owner can archive"). `BuildRules` runs BEFORE the SQL on every write verb; branch on `actionName`:

```go
func (u *User) BuildRules(actionName string, svc domain.Service, r *domain.Rules) {
    r.IfUpdate(func() {  // fires for PUT, PATCH, Archive, Unarchive
        if actionName == "GetArchivable" && u.RequestingPrincipalEmail != "" {
            if u.Email != u.RequestingPrincipalEmail && !u.RequestingPrincipalIsAdmin {
                r.AddNotification("ID", domain.ArchiveNotAllowedNotification{})
            }
        }
    })
}
```

Identity-derived fields reach the entity via the Command mapper (`ToEntity(ctx)`/`ApplyTo(ctx, t)`):

```go
func (*ArchiveUserCommand) ApplyTo(ctx *configuration.AppContext, u *User) error {
    if id := ctx.Identity(); id != nil {
        if email, _ := id.Claims["email"].(string); email != "" { u.RequestingPrincipalEmail = email }
        u.RequestingPrincipalIsAdmin = id.HasPermission("users:admin")
    }
    return nil
}
```

The Command is the only place identity → business-field translation lives. These fields are runtime-only because they are NOT in the `TableSchema` — the persister never writes/scans/audits undeclared fields. The kernel `Update/Delete/Archive/UnarchiveNotAllowedNotification` all return `SemanticForbidden → 403`.

### Layer 3 — Tenant scoping (cross-cutting)

Multi-tenant isolation, wired by YAML:
- **Claim presence gate** (`tenant.required: true`) — `AuthMiddleware` rejects non-public requests with empty `Identity.TenantID()` (`TenantMissingNotification (403)`).
- **Reads** — `Query.ToCriteria(ctx)` injects `crit.Filter["tenant_id"] = ctx.Identity().TenantID()`.
- **Writes** — Command mapper sets `entity.TenantID`; `BuildRules` emits `TenantMismatchNotification (403)` when the resource's TenantID differs on UPDATE/ARCHIVE/DELETE.

No auto-injection at infra: tenant is a domain concept, and injecting it at `infra/` would force infra to pronounce domain vocabulary (`infra → domain only`).

### Identity helpers + YAML

```go
func (i *Identity) HasPermission(p string) bool   // nil-safe false; panics on caller-side wildcards
func (i *Identity) TenantID() string              // nil-safe ""; reads the configured claim
```

`TenantID` reads the configured claim (default `tenant_id`; override `auth.authorization.tenant.claim`) tolerating `string`, `[]string{one}`, `[]any{one}`. Bootstrap calls `configuration.SetPermissionsClaim(name)` / `SetTenantClaim(name)` from YAML.

```yaml
auth:
  mode: jwt
  authorization:                # default nil (off; identity helpers still work)
    enabled: true               # master switch — false → runtime gate no-ops
    permissionsClaim: permissions
    tenant:
      enabled: false
      claim: tenant_id
      required: false           # true → TenantMissingNotification on empty
```

Strict decoding on `authorization` + `tenant` — unknown keys abort boot. Cross-rules: `authorization.enabled: true` requires `mode: jwt`; `tenant.required: true` requires `tenant.enabled: true`.

| Notification | Emitter | Layer |
|---|---|---|
| `MissingPermissionNotification` | `web.PermissionGate` | 1 |
| `Update/Delete/Archive/UnarchiveNotAllowedNotification` | service `BuildRules` | 2 |
| `TenantMissingNotification` | `AuthMiddleware` (`tenant.required`) | 3 |
| `TenantMismatchNotification` | service `BuildRules` / `Query.ToCriteria(ctx)` | 3 |

**Defense-in-depth at infra is deliberately absent** — a `ScopeFilter` knob on `TableSchema` would force infra to pronounce domain concepts and create two sources of truth. Identity stays in `application/` + `domain/`.

## OpenAPI / Swagger UI

`omnicore/web/openapi` generates an OpenAPI 3.1.0 document by reflection over the same Go types the HTTP wrappers consume — Request DTOs (`json:`/`path:`/`query:`/`filter:` tags), Response DTOs, the FullBody marker, `HasPathID` assertions. No `swag init`, no hand-written YAML.

### Registration paths

| Path | Used by | Carries |
|---|---|---|
| `openapi.Mount(reg, group, method, path, handler, spec, doc)` | canonical `*Spec` siblings AND manual handlers parsing a typed Request DTO | `RouteSpec{RequestType, ResponseType, SuccessStatus, Strict, HasPathID, Paged, FileResponse, OmittedQueryParams}` + `Doc{Summary, Description, OperationID, Tags, Deprecated, Hidden, Public}` |
| `openapi.MountRaw(reg, group, method, path, handler, raw)` | routes without a typed Request DTO (auth demos, in-process upstreams, vendor-shaped showcase) | `RawSpec{Summary, Description, OperationID, Tags, Deprecated, Hidden, Public, Parameters, RequestBody, Responses}` |

Canonical `*Spec` siblings return `(fiber.Handler, openapi.RouteSpec)`: `HandleCommandWithBodySpec`, `HandleCommandWithBodyIDSpec`, `HandleCommandWithIDSpec`, `HandleQueryWithParamsSpec`, `HandleQueryWithIDSpec`. Each is identical to its non-`Spec` wrapper plus the returned `RouteSpec`. `Strict` is detected via `pipeline.FullBodyEnforcer`; `HasPathID` is true wherever the wrapper auto-binds `:id`; `Paged` only on `HandleQueryWithParamsSpec`.

### Paged success envelope (RouteSpec.Paged)

`fwweb.RespondPaged` routes carry `data` as an array of `ResponseType` plus a top-level `pagination` (`web.PaginationInfo`: `has_next`, `has_prev`, `next_cursor`/`prev_cursor` omitempty, `total`). `RespondWithSuccess` routes carry one `ResponseType` and no `pagination`. `RouteSpec.Paged` selects which the assembler renders; `HandleQueryWithParamsSpec` sets it automatically, manual mounts via `fwopenapi.RouteSpecOfPaged[TReq,TResp](status)`. `Paged:true` with `fwresponses.None`/nil `ResponseType` panics at `Mount`. `PaginationInfo` is a named `$ref` component. The `?onlyTotal=true` variant is documented in `Doc.Description` prose.

### Schema generator coverage

`openapi.Generator` walks a `reflect.Type` → in-memory `openapi.Schema`:

| Go type | Schema |
|---|---|
| `string`/`*string` | `{type: string, nullable?}` |
| `int*`/`uint*` | `{type: integer, format: int32\|int64}` |
| `float32/64` | `{type: number, format: float\|double}` |
| `bool`/`*bool` | `{type: boolean, nullable?}` |
| `time.Time`/`*time.Time` | `{type: string, format: date-time, nullable?}` |
| `uuid.UUID` / `domain.ID` | `{type: string, format: uuid}` |
| `[]T` / `[]byte` | `{type: array, items}` / `{type: string, format: byte}` |
| `map[string]T` / `map[string]any` | `{type: object, additionalProperties: <T>\|true}` |
| Named struct | `$ref` into `components/schemas/<Name>` (registered once) |
| Anonymous embed / anonymous struct field | flattened inline / inlined object |
| `json:"-"` | skipped |
| `path:"X"` | skipped from body → path parameter |
| `query:"X" filter:"ops"` | skipped from body → one query param per operator (`name`, `name.in`, `name.gte`, …) |
| `example:"value"` | sets the property `example`; composite/parse-failure → boot panic |

Required-field rule: **Strict** (handler embeds `pipeline.FullBody`) → every kept field required; **Lenient** → required when non-pointer AND json tag lacks `,omitempty`. Schemas cached by `(reflect.Type, strict)` in a `sync.Map`; named structs cached by name in `Components`.

### Response envelope + path params + auth

Successes wrap `ResponseType` in `web.Response` (`success`, `status`, `description`, `data`); on `responses.None` the envelope omits `data`. Standard error envelopes auto-added per route shape (all referencing the single `ErrorEnvelope` schema, typed `ErrorMessage`):

| Status | Added when |
|---|---|
| 400 | route carries a request body |
| 401 | auth enabled AND route not public |
| 404 | `HasPathID=true` OR `RawSpec` declares a path param |
| 422 / 500 | always |

Custom error status via `Doc.ResponseExamples[N]` auto-creates the entry; `default` auto-merge applies only on statuses with a `DefaultErrorExample` (400/401/403/404/422/500).

**Path parameters** — canonical routes: walk `RouteSpec.RequestType` for `path:"X"` → one `parameters[].in=path` each; then walk the Fiber path for `:name` segments → string stub for each not covered. So `/tenants/:tenantId/users/:id` works without special-casing.

**Auth declarations** — `openapi.WithAuth(AuthContext{PublicRoutes})` (passed by `bootstrap.Run` when `auth.mode: jwt`): materializes `components.securitySchemes.bearerAuth` (`http`/`bearer`/`JWT`); every non-public op gains `security: [{bearerAuth: []}]` + 401. "Public" = `Doc.Public`/`RawSpec.Public` OR exact `METHOD /path` match against `AuthContext.PublicRoutes`. Bootstrap auto-extends with `GET /openapi.json` + `GET /docs`.

### Bootstrap integration + extras

`Wiring.OpenAPI *openapi.Config` is opt-in: nil → nothing registered, every `Mount`/`MountRaw` a Fiber-only passthrough. Non-nil → bootstrap builds an `*openapi.Registry`, threads it through `Deps`, registers `GET /health` via `MountRaw`, and after all features mount calls `openapi.Register(...)` serving `GET /openapi.json` + `GET /docs` (`/docs` loads `swagger-ui-dist` from the unpkg CDN; override `/docs` after `Register` for offline operation).

- **Language selector** — `openapi.Config.LanguageSelector bool` (default false) adds a `<select>` + a `requestInterceptor` writing the choice into `Accept-Language` on every "Try it out". `Languages []LanguageOption` ({Label, Value}); when empty + selector on, bootstrap auto-populates from `Wiring.Translations` (dedup by `Language`, `LangENG` rotated to position 0). `Wiring.Translations` is mandatory regardless (notifications/errors/audit all render through the `Translator`).
- **Hidden** = excluded from spec, still routed on Fiber (internal upstreams). **Public** = appears in spec WITHOUT `security`/401 (health probes, OIDC discovery, identity demos).

## GraphQL

`omnicore/web/graphql` exposes the same application handlers REST consumes through a single `POST /graphql` endpoint whose schema is reflected from the same DTOs. **GraphQL is its own web surface — separate from REST/OpenAPI.** It never goes through `openapi.Mount`/`MountRaw`, never appears in the Swagger document, and is not policed by the REST route scans (`scanRouteRegistration`/`scanAuthorization`). The ONLY thing shared with REST is the application-layer handlers the resolvers dispatch to (`pipeline.Handler[TQ, queries.Page]` read, `pipeline.Handler[*Cmd, TResult]` write). Engine: `vektah/gqlparser/v2` (parse + validate) + a framework-owned executor + introspection.

### Registry — attach handlers

`fwgraphql.New(d.Pipeline)` → `.Register(...)` per root field. Canonical (Auto) and manual handlers attach identically (same `pipeline.Handler`).

| Constructor | Maps | Field |
|---|---|---|
| `Query[TReq, TQ, R]` | read `pipeline.Handler[TQ, queries.Page]` | `users(where, first, after, last, before, orderBy, search, includeArchived): UserConnection!` |
| `Mutation[TReq, TCmd, *TCmd, TResult, TResp]` | insert (body, no id) | `createUser(input: InsertUserInput!): InsertUserResponse!` |
| `MutationWithID[…]` | update/patch (body + id) | `updateUser(id: ID!, input: …!): …!` |
| `MutationByID[TCmd, *TCmd, TResult]` | archive/unarchive/delete (id, no body) | `archiveUser(id: ID!): MutationResult!` |

### Reflected schema + execution

- **Node object** named after the entity (not the Go Response type); fields by `json:` wire name, resolved from the Go-field-keyed view doc.
- **Relay connection** `{edges{node,cursor}, pageInfo{hasNextPage,hasPreviousPage,startCursor,endCursor}, totalCount}`; `edges[].cursor` = `Page.ItemCursors[i]` (the per-row keyset cursor the reader emits — it cannot be rebuilt above the reader once the physical keyset values are stripped).
- **`where` input** from the `filter:` allowlist; one input field per leaf exposing the declared operators (nested embed leaves flatten `addresses.zipCode`→`addresses_zipCode`). Folds through the SAME criteria emission as REST (`queryschema.ApplyFilterParam`), so `where:{name:{startswith:"Bo"}}` == REST `?name.startswith=Bo`.
- **Mutation input** from `json:` tags; NonNull under `pipeline.FullBody` (strict) else non-pointer-without-omitempty (same rule as REST/OpenAPI). Missing-required is enforced by gqlparser validation.
- **Args → criteria/command** reuse `web/queryschema`; selection set → projection (trim); `pipeline.Dispatch` with the request `*AppContext` (so `ToCriteria(ctx)` overlays, `BuildRules`, outbox, audit apply unchanged).
- **Errors**: always HTTP 200 `{data,errors}`; notifications → `errors[].extensions{notificationKey, semantic, field}` (GraphQL sibling of `RespondFromResult`); panic → opaque `{semantic:"Internal"}`.

### Shared reflection core (`web/queryschema`)

The filter operator vocabulary (`Op*`, `knownOps`) + emission, the Request filter allowlist (`ExtractRequestSchema`/`WalkRequest`), and the Response projection map + sparse-render guard live in one internal package consumed by the REST wrappers, the OpenAPI generator, AND the GraphQL builder — one traversal, three projections. `fwweb.Op*` are re-exports.

### Authorization

Layers 2 (`BuildRules`) and 3 (tenant via `ToCriteria`) ride inside `Dispatch` — inherited unchanged. Layer 1 (coarse `RequirePermission`) is route-shaped in REST; on the single GraphQL endpoint it moves **per-field into the resolver**. `AuthMiddleware` (matched by path) still authenticates `/graphql` when `auth.mode: jwt`.

### Bootstrap integration

`Wiring.GraphQL *graphql.Registry` opt-in (nil → nothing mounted). `bootstrap.GraphQLConfig` (yaml `graphql:`) carries the serving knobs. bootstrap mounts `POST <path>` as its own surface AFTER the REST scans (like the framework's own non-spec routes), enables introspection from config, and serves the GraphiQL playground at `uiPath` when `playground: true`.

```yaml
graphql:
  path: /graphql          # POST endpoint (default)
  uiPath: /graphql/ui     # GET GraphiQL playground (when playground: true)
  playground: false       # serve GraphiQL at uiPath
  introspection: false    # answer __schema / __type
  rootRedirect: false     # GET / → 302 path; both this + openapi.rootRedirect → boot panic
```

## Cache subsystem

`omnicore/infra/cache` is the framework's generic byte-level key-value cache. The `cache.Cache` port is the single contract for domain services, infra adapters, and the outbound httpclient response cache. Two implementations ship: `cache.NewMemory` (in-process LRU+TTL) and `cache.NewRedis` (go-redis). Other backends implement the interface and inject via `Wiring.Cache` / `Wiring.SharedCache`.

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
}
```

- `Get`: `(value, true, nil)` hit, `(nil, false, nil)` miss, `(nil, false, err)` transport/decode failure.
- `Set`: `ttl == 0` = no expiration; negative TTL rejected with `cache.ErrInvalidTTL`. `Delete` idempotent. No `Has`/`Clear`/batch.
- Typed helpers `cache.GetJSON[T]` / `cache.SetJSON[T]` sit outside the interface and tolerate a nil `Cache` (degrade to no-op), so YAML-toggled cache code needs no nil guard.

### Two instances — Private and Shared

| Field | Scope | Populated when | Use for |
|---|---|---|---|
| `Deps.Cache` | service-private | `cache:` block declared | domain memoization, httpclient response cache |
| `Deps.SharedCache` | cross-service | `cache.shared:` declared | feature flags, cluster rate limits, gateway sessions |

The split is DI-enforced. `SharedCache` is nil unless declared — guard explicitly. `cache.shared.store: memory` is rejected at boot (an in-process LRU cannot honor cross-service reads). Omitting the top-level `cache:` block leaves `Deps.Cache` nil; the httpclient cache layer bypasses.

Backend cascade (per `cache.store` / `cache.shared.store`): `memory`/unset → in-process LRU (boot panic if `Wiring.Cache` set — declare `store: custom`); `redis` → go-redis adapter from `redis:` sub-block (boot panic if `Wiring.Cache` set); `custom` → requires `Wiring.Cache` (boot panic if nil). Redis `failMode`: `open` (default) swallows transport errors as a miss + `slog.Warn`; `closed` propagates. Connection is lazy — Redis down at boot does not block `New()`. `Wiring.Cache`/`SharedCache` resolve AFTER `wire(deps)`; the httpclient picks up the late private cache via `HttpClient.SetCache` (atomic swap).

```go
v, ok, err := cache.GetJSON[Profile](ctx, deps.Cache, "user:42:profile")
_ = cache.SetJSON(ctx, deps.Cache, "user:42:profile", v, 5*time.Minute)
```

Full cache config: the docs/ site.

## httpclient package

`omnicore/infra/httpclient` is the outbound HTTP subsystem. Services declare upstreams in `microservice.<profile>.yaml` under `httpClient:`; the framework builds a singleton `*httpclient.HttpClient` on `Deps.HttpClient` (nil when no `httpClient:` block). Each declared service gets its own `http.Transport`/`http.Client` so one bad upstream cannot starve the others. `httpclient.New(cfg)` runs `applyDefaults` then `Validate`, accumulating all issues into one boot error.

### Tag binding

Request/response DTOs carry `http:"..."` tags; `binding/` parses each `reflect.Type` once (cached in `sync.Map`) and walks a pre-built plan on the hot path. Tag forms: `path,name` / `query,name[,csv|multi]` / `header,Name` / `headers` (`map[string]string`) / `body,json|xml|form|stream|multipart`. Exactly one body field; every `{placeholder}` in the path needs a matching `path` field and vice-versa. A response struct with no tagged fields decodes whole as the body; `struct{}` discards it. Codecs `json` (default), `xml`, `form-urlencoded` are package-private.

### Call surface

```go
func Call[Req any, Resp any](
    ctx context.Context,        // AppContext satisfies it
    c *HttpClient,
    service, endpoint string,   // YAML keys
    req Req,                    // typed struct with http:"..." tags
    opts ...InvokeOption,
) (Resp, error)
```

The single typed entry point; no untyped path. Options: `WithConfig(CallConfig)` (per-call YAML override — `BaseURL`, `Timeout`, `AuthProvider`, `Method`, `Path`, codecs, `AcceptableStatus`, `NoCache`, `CacheKey`, `IdempotencyKey`, `Retry`, `InlineAuth`), `WithExtraHeader`/`WithExtraQuery` (additive), `WithClientCert(tls.Certificate)` (ephemeral cloned transport). `InlineAuth` supplies per-call Bearer/APIKey/Basic (exactly one; wins over `AuthProvider`; no token cache). TLS minVersion / ciphers / CA / pool size are transport-bound — not per-call overridable.

Status branching: `2xx` → decoded `Resp`, nil; `acceptableStatus` → decoded `Resp` + `*HttpError{Acceptable:true}` (branch with `IsAcceptableStatus`); other non-2xx / transport / decode → zero `Resp` + `*HttpError`.

```go
resp, err := httpclient.Call[GetUserRequest, GetUserResponse](ctx, c, "keycloak", "getUser", GetUserRequest{ID: "42"})
```

### Error model + slog

`*HttpError{Service, Endpoint, Method, URL, Status, Headers, Body, Duration, Cause, Acceptable, Attempt}`. Helpers: `IsAcceptableStatus`, `IsRetriable`, `IsCircuitOpen`. Sentinels: `ErrRequestBuild`, `ErrResponseDecode`, `ErrTokenAcquire`, `ErrCircuitOpen`. One `http.outbound` slog record per call (`Warn` on error, else `Info`): `threadId`/`downstreamThreadId`, `service`/`endpoint`/`method`/`url`/`status`/`durationMs`/`*Bytes`, `attempt`, `cacheStatus`, `breakerState`, `authProvider`; headers+bodies only when `logBodies`. Redaction cascades `defaults → service` over always-applied framework defaults (`Authorization`/`Cookie`/`X-API-Key` headers; `token`/`api_key`/`access_token`/`signature`/`code` query keys; opt-in body JSONPath). The wire is never altered — redaction only affects slog.

### Middleware chain

Fixed, non-configurable order. Positions 1, 2, 6–9 always wired; 3, 4, 5 appended per endpoint policy. A short-circuit (e.g. cache hit) returns without calling `next`.

| Pos | Layer | When | Role |
|---|---|---|---|
| 1 | `correlationMiddleware` | always | inject thread/request id headers from `AppContext.ID()` |
| 2 | `loggingMiddleware` | always | buffer bodies, time, emit the single slog record |
| 3 | `authMiddleware` | provider resolved | attach credential; on `RevocableProvider`+401 invalidate+retry once |
| 4 | `idempotencyMiddleware` | `idempotency.enabled` | inject the key once (persists across retries) |
| 5 | `cacheMiddleware` | store wired + `cache.enabled` | GET/HEAD lookup; hit short-circuits |
| 6 | `retryMiddleware` | always | re-dispatch on retriable failure; breaker sits inside it |
| 7 | `breakerMiddleware` | always | per-`(service,endpoint)` circuit; `ErrCircuitOpen` when open |
| 8 | `signingMiddleware` | always | re-sign each attempt before transport |
| 9 | `transportMiddleware` | always | dial via the per-service `http.Client` |

### Capabilities (one line each — full config: docs/ site)

- **Retry** — declarative `retry:` (`maxAttempts`, `backoff` constant/linear/exponential/exponential-jitter, `retryOn`, `respectRetryAfter`); endpoint overrides defaults field by field; POST/PATCH clamped to 1 attempt unless `idempotency:` is declared; cancellation never retried.
- **Cache** — `cache:` policy knobs (`enabled`, `defaultTTL`, `honorCacheControl`, per-endpoint `ttl`/`varyOn`/`cacheAcceptable`) over the top-level cache backend; GET/HEAD only; SHA-256-hashed key; `obs.CacheStatus` hit/miss/bypass.
- **Circuit breaker** — `circuitBreaker:` (`failureThreshold`/`successThreshold`/`openFor`) per `(service,endpoint)`; 5xx/transport = failure, 4xx = success; sits inside retry.
- **Idempotency** — `idempotency:` (`header`, `source: ctx`(UUIDv7)|`explicit`); same key across retries unlocks POST/PATCH retry; mirrored onto `AppContext`.
- **TLS + pool** — per-service `tls:` (`minVersion`, `cipherSuites` modern/intermediate/legacy/explicit, mTLS `clientCertFile`/`clientKeyFile`, `caBundle` replaces system roots) and `pool:` built once at boot; rotated certs use `WithClientCert`.
- **Streaming** — `responseStream`(→`StreamResponse`), `http:"body,stream"`(`io.Reader`), `http:"body,multipart"`(`Multipart`), `responseSSE`(→`SSEResponse`); caller closes; streaming forbids cache and forces `maxAttempts:1` + no signing on uploads.
- **HMAC signing** — per-service `signing:` (`hmac-sha256`, AWS SigV4-lite canonical string, `signedHeaders`, re-signed per attempt); auto-adds signature/keyId headers to redaction.

Auth provider types (`authProviders.<name>`, selected per service via `auth.provider`):

| Type | Behavior |
|---|---|
| `none` | no-op (mTLS / anonymous) |
| `header-static` | raw value via `attach` |
| `bearer-static` | static `Authorization: Bearer {token}` |
| `basic` | RFC 7617 base64(user:pass) |
| `forward-bearer` | propagates inbound JWT from `AppContext.BearerToken()`; never cached |
| `oauth2-client-credentials` | RFC 6749 client_credentials; per-provider token cache + single-flight; optional revocation-on-401 |
| `credentials-exchange` | generic POST-credentials-get-token; arbitrary `requestFields` (+`requestFieldsFromCtx` for per-tenant), JSONPath token extraction |

### Composition, resolver, testing

Handlers never import `httpclient`. The pattern: `Deps.HttpClient` → `infra/external/<svc>.go` struct holding the typed `Call` surface and vendor→domain mapping → handler depends on that struct. Swapping HTTP for gRPC or a fake never touches `application/`. `BaseURLResolver` (`Resolve(ctx, service) (string, error)`, registered via `WithResolver`) handles dynamic routing — per-call `CallConfig.BaseURL` wins, empty return falls back to YAML, error aborts before dialing; `StaticBaseURLResolver` is the reference impl. `httpclient.NewFake()` returns a test harness whose `Client()` drops into any `*HttpClient` param; it short-circuits the middleware chain but keeps the real `binding/` layer (tag misuse still surfaces): `WhenCalled(...).Match*(...).Return(...)`, `Calls(...)`, `AssertExpectations()`. Full httpclient config + examples: the docs/ site.

## bootstrap package

`omnicore/bootstrap` orchestrates the boot from `microservice.<profile>.yaml` + a `Wire` callback returning `Wiring`.

### Environment variables

The framework reads exactly four process env vars; everything else in `${VAR:default}` is consumer YAML. Destructive/loose decisions stay out of versioned YAML, hence env-only.

| Variable | Required | Controls |
|---|---|---|
| `APP_PROFILE` | yes | selects `./microservice.${APP_PROFILE}.yaml`; `dev`/`prd` canonical, others accepted; `dev` is the only profile allowing `auth.mode=disabled` and defaulting `migrations`/`mongo.rebuild` autoRun true |
| `OMNICORE_CONFIG_PATH` | no | overrides the YAML path (still needs `APP_PROFILE`) |
| `OMNICORE_MONGO_FORCE_REBUILD` | no | exact `"true"` drops/recreates divergent Mongo indexes; does NOT drop collections or trigger doc rebuild |
| `OMNICORE_CODE_VERSION` | no | build id stamped on `code_version` of `omnicore_mongo_views`; never a boot blocker |

### Main functions + types

| Function | Use |
|---|---|
| `bootstrap.Run(wire) error` | load + build singletons + wire + serve until SIGINT/SIGTERM |
| `bootstrap.Build() (Deps, *Config, error)` | build singletons without serving |
| `bootstrap.Serve(ctx, deps, wiring) error` | serve with deps already built (manual path owns translations + SyncEngine) |

- `Deps` — built singletons: `Config`, `Logger`, `Postgres` (audit pre-wired via `WithAudit`), `Mongo`, `Translator`, `Pipeline`, `ViewReader`, `Export` (tabular export ambient inputs), `Cache`, `SharedCache`, `HttpClient` (nil w/o `httpClient:`), `OpenAPIRegistry` (nil w/o `Wiring.OpenAPI`), `IntegrationRegistry`, `UpstreamSubscribers` (nil w/o declared upstream subscriptions).
- `Wiring` — `Translations`, `Features`, optional `BeforeServe`, `OnShutdown`, `OpenAPI *openapi.Config`, `GraphQL *graphql.Registry`, `Cache`/`SharedCache`, `UpstreamSubscriptions`.
- `Feature` — `Mount(app *fiber.App, deps Deps)`. `ReadableFeature` — `Feature + Views() []*infra.ViewDefinition` (collected for the SyncEngine).

### `Run` behavior

JSON slog on stdout; `signal.NotifyContext(SIGINT, SIGTERM)`; connect Postgres+Mongo; `validateWiring` (needs Features or BeforeServe); migrations `Up` if `Migrations.AutoRun` (before SyncEngine); `collectViews` rejecting name collisions; Fiber app with `ErrorHandler: fwweb.ErrorHandler` (404/405/413 specialized, else 500; stack stays on the log); middlewares `Recover` → request logger → `AppContextMiddleware` → `AuthMiddleware` (jwt only); auto `GET /health`; mount each feature in order; OpenAPI registered before auth/health when `Wiring.OpenAPI != nil`; SyncEngine starts only if views collected; 10s HTTP drain then `OnShutdown`.

### Canonical main.go

```go
func main() {
    if err := bootstrap.Run(Wire); err != nil {
        log.Fatal(err)
    }
}

func Wire(d bootstrap.Deps) bootstrap.Wiring {
    return bootstrap.Wiring{
        Translations: []translation.Module{apptrans.ENG()},
        Features:     []bootstrap.Feature{NewUsersFeature(d)},
    }
}
```

## Bootstrap checklist for a new microservice

1. `go mod init …` + `go get github.com/ClaudioSchirmer/omnicore`.
2. DDD layers `domain/`, `application/{commands,handlers,translations}/`, `infra/`, `web/`; composition + migration location are the service's choice.
3. `microservice.dev.yaml` + `microservice.prd.yaml` at module root (selected by `APP_PROFILE`).
4. SQL migrations start at `0002_*.{up,down}.sql` (outbox is v1, framework-injected — do not write); each `.up.sql` needs a `.down.sql`; path from `migrations.dir`.
5. Entities embed `BaseEntity`/`AggregateRoot` and implement `Entity` (+ `AggregateRootProvider` if aggregate).
6. Repository embeds `fwinfra.BaseRepository[T]` (inject `NewEntity func() T`) + delegates `FindByID` to `fwinfra.AggregateLoader[T]`.
7. Commands embed `pipeline.CommandBase`/`CommandBaseWithID`, implement `ToEntity()`/`ApplyTo()`; no JSON tags (wire lives in `web/requests/`).
8. Manual handlers only for cross-service / domain-service logic (Auto handlers cover trivial CRUD).
9. Views in `infra/views.go` via `fwinfra.View(...).Root(...).EmbedMany(...)`.
10. `web/` exposes `MountXxx(app, repo, view, deps)`; body endpoints via `fwweb.HandleCommandWithBody{,ID}`, bodyless via `fwweb.HandleCommandWithID`. `/health` comes from the framework.
11. Feature struct per aggregate implements `Feature`/`ReadableFeature`, `Mount` delegating to `web.MountXxx`.
12. `Wire(d Deps) Wiring` aggregates Translations + Features; optional `Wiring.OpenAPI` publishes `/openapi.json`+`/docs`.
13. `func main()` ≤10 lines calling `bootstrap.Run`.

Layouts (all supported): consolidated `bootstrap/` package main (canonical `omnicore-example-users`), Go `cmd/<binary>/`, or flat root `main.go`. Non-Go artifacts live in `migrations/` and `devops/` at module root, not in `infra/`. Exotic boot uses `Build()` + `Serve()`.

## Glossary

- **AfterBeginHook[T] / BeforeCommitHook[T]** — function types for the TX lifecycle hook signatures (positions A and D).
- **AfterBeginHookProvider[T] / BeforeCommitHookProvider[T]** — `application/persistence/` interfaces Auto handlers detect by assertion to forward hooks as `WriteOption[T]`.
- **Aggregate root** — entity owning a collection of `AggregateValueObject` items with state transitions.
- **AggregateRootProvider** — opt-in (`GetAggregateRoot` + `AggregateChildren`) activating aggregate-aware persistence + child validation.
- **Audit** — `AuditEvent` emitted per write, routed by `audit.destinations`; body `kind`: snapshot/delta/transition.
- **Old** — pre-mutation entity snapshot from `Get*`; via `Entity.Old()` / `domain.Old[T]`; consumed by `BuildRules` + auditor.
- **Cache** — `cache.Cache` byte-level port in `infra/cache` (Get/Set/Delete); impls `NewMemory`/`NewRedis`, custom via `Wiring.Cache`/`SharedCache` + `store: custom`.
- **Carrier** — `domain.NotificationCarrier`: error with `NotificationContexts()`. Cross-layer error contract.
- **RequestContext** — request-scoped `persistence.RequestContext` (context + `ID()` + actor accessors) satisfied by `*AppContext`; domain has no context type.
- **Domain event** — `DomainEvent` accumulated via `RegisterEvent`, published by `events.Publisher` after persistence.
- **Granularity B** — one outbox row per aggregate operation regardless of child count.
- **Notification / NotificationKey** — typed marker the domain emits (translation key = Go type name); `NotificationKey` is that struct-name identity preserved through translation to the wire.
- **Outbox** — domain row + event row in one TX; Debezium tails it to Kafka.
- **Pipeline / Result[T]** — application wrapper translating `NotificationCarrier` errors into `Result[T]` (Success/Failure/Exception).
- **Service** — `domain.Service` marker for domain services injectable into `BuildRules`.
- **UpstreamSubscription** — declarative tie of an upstream Kafka topic to a local Mongo collection materialized by `UpstreamSubscriber`; the cross-service composition path.
- **FromSchema** — `fwinfra.FromSchema(*TableSchema)`, the single embed-source constructor; store kind derived from schema type (local PG vs external Mongo).
- **TxHandle / UnwrapPgxTx** — sealed marker handed to in-TX hooks; `fwinfra.UnwrapPgxTx(TxHandle) pgx.Tx` (in `infra/`) is the only bridge to the live tx.
- **TableSchema** — mandatory explicit Go-field↔column map; one declaration drives write + criteria + scan + Mongo view.
- **ValidEntity** — sealed `Insertable`/`Updatable`/`Archivable`/`Unarchivable`/`Deletable`/`Batch` produced only by domain.
- **WriteOption[T]** — functional option on `Scope(ctx, opts...)` carrying the lifecycle hooks.
- **Reader[T] / Writer / Repository[T]** — pure domain repository ports; **ScopedRepository[T]** — application write binding (`Reader[T]` + `Scope`), implemented by `infra.BaseRepository[T]`.
- **fwintegration.Dispatch** — canonical cross-service producer entry; resolves `eventKey` against YAML, `WithTx` lands atomically with the write.
- **Registry / Receiver** — `*fwintegration.Registry` mounted by `IntegrationFeature.MountReceivers`; each `Receiver` is one `(sourceKey, eventKey)` with `RetryPendingFailures`.
- **IntegrationFeature** — opt-in interface registering receivers via `MountReceivers`; mirror of `ReadableFeature`.
- **eventKey / sourceKey vs wire event_type** — Go-side identifiers stable across migrations; the literal Kafka `event_type` header lives ONLY in YAML.
- **omnicore_integration_failures** — consumer failure registry, natural key `(consumer_group, source_key, event_key, event_id)`.
- **omnicore_integration_processed** — consumer dedup table, PK `(event_id, consumer_group)`; `ON CONFLICT DO NOTHING`.
