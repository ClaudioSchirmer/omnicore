# OmniCore

> **CRITICAL RULE — CHANGES TO THE FRAMEWORK REQUIRE EXPLICIT APPROVAL**
>
> **ANY AND EVERY change to any file in this module (`omnicore/`) must be explicitly approved by the maintainer BEFORE being applied.** No exceptions — not for "obvious fix", not for "small improvement", not to support a feature requested by a consumer service, not for cosmetic refactor.
>
> Mandatory flow:
> 1. Identify the finding/need (usually while using a consumer service)
> 2. Describe to the maintainer the proposed change, motivation, and impact
> 3. Wait for explicit approval
> 4. **Only then** edit framework files
> 5. **Every approved change must update the [`docs/`](docs/) documentation site in the same round** — the `docs/` site (published via GitHub Pages at <https://claudioschirmer.github.io/omnicore/>) is the **end-user manual** of the framework (consumer's view, not maintainer's). `CLAUDE.md` here is the agent/maintainer view. The two must tell the same story about features, public APIs, conventions, and examples. The site is a set of per-section pages under `docs/content/sections/<id>.html` (navigation + order in `docs/content/nav.json`): update the relevant section page(s) and add a release entry to `docs/content/sections/changelog.html`. Before marking a change as "done", verify that the `docs/` site reflects the new public surface. If the change is purely internal (refactor without API change, private helper, comment-only), the `docs/` update may be skipped — record the rationale in the PR/commit.
> 6. **Every approved change comes with unit tests** covering the new/changed behavior. A green build/vet is **not** proof of working. When wrapping up the round: `cd omnicore && go build ./... && go vet ./... && go test ./... -count=1` — a green suite is a precondition of "done". After it passes, **ask the maintainer whether to also run the E2E QA suites** in the sibling `omnicore-example-users` repo (`qa/e2e.sh`, `qa/auth.sh`, `qa/audit.sh`, `qa/httpclient.sh`); via `AskUserQuestion`, recommend the subset relevant to what was changed in the framework and always include a "run all" option. Opt-in because they require docker compose up + register-connector (auth/audit also need Keycloak).
> 7. **Every approved change updates [`CHANGELOG.md`](CHANGELOG.md) in the same round.** Public-surface changes (new exported types/functions, contract changes, deprecations, removals, bug fixes that change observable behavior, security fixes) land under the appropriate section (**Added** / **Changed** / **Deprecated** / **Removed** / **Fixed** / **Security**) of the topmost `## [Unreleased]` block — create the block when absent. When cutting a release, rename the `[Unreleased]` block to `## [<x.y.z>] - YYYY-MM-DD` and add the matching tag link at the bottom of the file. Internal-only changes (refactor without API change, private helper, comment-only) may skip `CHANGELOG.md` — record the rationale in the PR/commit.
>
> If urgency is high in the consumer service, consider a temporary local workaround in the service while the framework change is being discussed — never apply the change to the framework "for now".

> **CRITICAL RULE — NEVER REMOVE FUNCTIONALITY WITHOUT EXPLICIT CONFIRMATION**
>
> **Removing existing functionality requires explicit maintainer confirmation BEFORE the removal — no exceptions.** This applies whenever a new feature impacts, supersedes, or appears to make an old feature redundant: the decision to remove, deprecate, adapt, or keep both coexisting belongs to the maintainer, never to the AI.
>
> "Appears redundant" is not authorization. "The new API covers the old case" is not authorization. "It looks like dead code" is not authorization. Approval to add the new feature is NOT approval to delete the old one.
>
> Applies to (non-exhaustive): public functions/methods/types, handlers, endpoints, config/yaml fields, flags, default behaviors, API options, contracts, exported constants, struct fields, interface methods, wrapper modes, validator modes, builder options.
>
> Mandatory flow when a new feature impacts an old one:
> 1. Implement / propose the new feature
> 2. **Stop before touching the old code path.** Describe the impact in plain text: what the old feature does, what the new one does, where they overlap, what would break if the old one is removed
> 3. Present the options via `AskUserQuestion` — typical set: `Remove old / Deprecate old (keep working, mark for future removal) / Keep both coexisting / Adapt old to delegate to new`. Recommend the option that fits best, but never pre-execute it
> 4. Wait for the explicit answer
> 5. Only then apply the chosen path
>
> "Dead field/code cleanup in chain" (the rule that says when a dead artifact is confirmed dead, the cleanup must sweep yaml + code + tests + the `docs/` site + CLAUDE.md in the same round) applies **after** the maintainer confirms the removal — it is not autonomous authorization to delete.
>
> Reason: the AI does not see all consumers, future plans, historical reasons, or migration constraints. Silent removal is destructive and irreversible from a contract/API standpoint; the cost of asking is one short turn, the cost of a wrong removal is broken consumers + lost trust + rework.

> **CRITICAL RULE — CANONICAL AND MANUAL ROUTES MUST STAY FEATURE-EQUIVALENT**
>
> Every framework feature must be reachable AND behave the same way through BOTH the **canonical** route (Auto handlers / generic wrappers / convention-driven wire-up — `InsertCommandHandler`, `HandleQueryWithParams`, auto-scan, Cmd `ToCommand`/`FromEntity`, etc.) AND the **manual** route (hand-written `pipeline.Handler` + explicit `fwweb.BindPath`/`ParseCriteria`/`RespondPaged` + `WithRootScanner`/`WithChildScanner`, etc.). The two are alternative ways of using the framework, **not different tiers of capability** — a consumer that picks one path must never lose access to a feature the other path offers.
>
> Equivalence applies to the **outcome**, not the exact API shape — the canonical route hides ceremony behind convention while the manual route exposes the seams, but the wire envelope, the validation pipeline, the audit/outbox/notification semantics, the schema enforcement, and the read/write contract must be the same. "Manual is the escape hatch" does NOT mean "manual is poorer" — it means the consumer takes on the wiring in exchange for control, and the framework still guarantees feature parity on the seams it owns (envelope, pipeline, persistence, notifications, audit).
>
> Mandatory flow when implementing a new feature:
> 1. Design it so that it lands on both routes in the same round (canonical + manual)
> 2. If a feature can naturally only fit one of the two (technical limitation, conceptual mismatch, layer boundary), **STOP before implementing on only one side**. Describe the gap to the maintainer in plain text: what works on side A, why side B cannot host it as-is, what the consumer would lose on side B
> 3. Present options via `AskUserQuestion` — typical set: `Reshape feature to fit both / Ship on canonical only and document the limitation / Ship on manual only and document the limitation / Introduce a bridging helper on the missing side / Defer until both fit`. Recommend the option that fits best, never pre-execute
> 4. Wait for the explicit decision before coding
>
> The same rule applies in reverse: when a feature already exists on one route and a request comes to add it on the other, confirm the parity story by describing the behavior on each side — not just on the new side.
>
> Reason: feature asymmetry creates silent traps. A consumer adopts the manual route for one endpoint (because the canonical wrapper does not fit a custom path/identifier/shape) and later discovers a canonical-only feature it needs is unreachable — or vice versa. The cost of the up-front conversation is one short turn; the cost of "Phase N+2 retrofits the missing route" is months of churn plus a documentation graveyard of "available on Auto only" / "manual handlers must hand-roll this" footnotes. The framework's value proposition rests on the promise that **the choice between canonical and manual is about wiring style, not about what features the consumer can use**.

> **CRITICAL RULE — ENGLISH IS THE FRAMEWORK LANGUAGE**
>
> All code, comments, documentation (including this `CLAUDE.md` and the `docs/` site), identifiers, file names, test fixtures, log messages, and error strings in `omnicore/` are written in **English** — no exceptions.
>
> **The only exception** is translation strings inside `application/translation/` — the framework's built-in i18n catalog. By design it ships with **seven languages**: PT-BR (`ptbr.go`), English (`eng.go`), Spanish (`esp.go`), French (`fra.go`), German (`deu.go`), Italian (`ita.go`), and Dutch (`nld.go`). Those seven modules are the *only* place in this module where non-English text is allowed; the surrounding Go code (struct names, function names, identifiers, comments) stays English even inside the translation package.
>
> Maintainer ↔ Claude conversations can happen in any language. The language of the chat does not affect what gets written to disk: framework artifacts are always English (except the four translation modules above).

> **CRITICAL RULE — DO NOT GUESS, VERIFY BEFORE ASSERTING OR PLANNING**
>
> Every claim about the code (return types, function signatures, layer behavior, what a handler does, what a wrapper emits, whether a function exists, what an env var defaults to, where a setting is read) MUST be backed by reading the actual source — never plausible-sounding inference from names, surrounding context, or pattern-matching against similar codebases. The truth is one `Read`/`grep` away; skipping that step to sound fast is failure.
>
> **The same applies to planning.** A proposed change that assumes a function works a certain way without checking is the same failure mode in a different dress — guessing dressed up as design. Verify the assumption before writing the plan, not after the maintainer points out the mistake. A plan built on a guessed contract has no value; redo the verification step, then redo the plan.
>
> When uncertain: either run the lookup before answering, or say explicitly "I'm guessing — let me verify" and then verify. **Never present a guess as a fact.** A wrong answer is worse than "I need to check first" because it gets quoted back, propagated into other files, and only gets corrected when it visibly breaks something — and by then it has cost trust + rework.
>
> The source code is the ground truth. The maintainer's words are the ground truth for intent. The AI's pattern-matched inference is not.

> **CRITICAL RULE — AI DOES NOT COMMIT, PUSH, OR OPEN PRs**
>
> The maintainer keeps absolute control of the git tree and the GitHub remote. **The AI is strictly forbidden from running any command that records a commit or writes to the remote:**
> - `git commit` in any form (including `--amend`, with or without `-m`).
> - `git push` in any form (fast-forward, force, force-with-lease, tags, releases).
> - `git tag` (local creation or pushed).
> - `gh pr create` / `gh release create` / any `gh api` invocation that modifies state.
> - Any other command that records a commit or modifies remote git state.
>
> Read-only git inspection (`git status`, `git log`, `git diff`, `git branch --list`) and file edits via the `Edit` / `Read` / `Write` tools remain allowed.
>
> **The closed loop the AI follows on every task:**
>
> 1. **At task start, create a feature branch with a coherent descriptor.** Prefix by intent (`feature/<slug>` for new behavior, `fix/<slug>` for bug fixes, `docs/<slug>` for doc-only edits, `refactor/<slug>` for internal cleanups). The slug is lowercase-kebab-case and names the *outcome*, not the file edited: `feature/audit-claim-allowlist`, not `feature/edit-auditor`. `git checkout -b <branch>` is the only git-write the AI runs — it is structural setup (local, reversible) so the maintainer's main tree stays clean from in-flight work.
>
> 2. **Apply the file changes for the task on that branch** via the `Edit` / `Read` / `Write` tools.
>
> 3. **At task end, deliver one commit-message suggestion in English** as plain chat text for the maintainer to copy/use:
>    - Title in the imperative mood (~72 chars max).
>    - Optional body in short paragraphs explaining the *why* of the change.
>    - No `Co-Authored-By` trailer — the suggestion is clean for the maintainer to use verbatim.
>
> The maintainer is the sole actor who runs `git commit`, `git push`, creates tags/releases, opens PRs, and merges. The AI's job ends when the file changes are applied to the feature branch and the commit-message suggestion is delivered in chat.

> **CRITICAL RULE — THIS DOCUMENT DESCRIBES THE CURRENT STATE, NOT HISTORY**
>
> `CLAUDE.md` is a **spec of what IS**, not a changelog of what changed. When an approved change ships, edit the relevant sections to describe the new behavior directly — do not append "Phase N" entries, do not preserve old wording with "(was X, now Y)" framing, do not annotate features with "(Phase 21)" tags.
>
> Forbidden:
> - "Phase N" labels in section headings, inline parentheticals, or code comments
> - A "Project history" / "Changelog" / dated-entry section
> - "X used to be Y, now is Z", "the old M was demoted", "Phase K removed N" framing — describe the current behavior directly
> - References to APIs, types, or fields that no longer exist in the code (`AggregateMapping`, `ChildMapping`, `HardDelete`, etc. — if it's gone from the source, it's gone from here)
>
> **Absence statements are tripwires.** Any phrase that says "this hasn't shipped yet" — "no X yet", "TODO: future Y", "not yet supported", "Currently only Z is accepted", "(future) X", "the W phase ships", "arrives in the dedicated phase" — ages silently the moment X/Y/Z/W lands, even when that round edits an unrelated section of this file. Before declaring a round done, run `grep -n -iE "yet|todo|not yet|\(future\)|future (code|phase|phases)|phase (ships|will introduce|introduces)|dedicated phase|currently only|no .* (suite|test|coverage)" CLAUDE.md` and confirm each hit still holds. Stale hit → fix in the same round.
>
> Project history lives in git (`git log`, commit messages, PR descriptions). The maintainer remembers his own decisions. When the spec contradicts the code, the spec is wrong — fix it in the same round as the code change, not in a later cleanup.

---

Go framework library providing **DDD + CQRS infrastructure** for microservices. Services import it as a Go module dependency; OmniCore itself contains no service code.

- **Module path**: `github.com/ClaudioSchirmer/omnicore`
- **Local path**: `/Volumes/Lynx/Development/omnicore-stack/omnicore`
- **Maintainer**: Claudio Schirmer (`claudioschirmer@icloud.com`)
- **Reference consumer**: [`../omnicore-example-users`](../omnicore-example-users/CLAUDE.md) — sandbox service that exercises every framework feature

## Stack

- Go ≥ 1.21 required (uses `log/slog` and generics extensively); toolchain currently pinned to `go 1.26.3`
- Fiber v3 (HTTP layer)
- pgx v5 (PostgreSQL driver)
- mongo-driver v2 (MongoDB)
- segmentio/kafka-go (Kafka consumer)
- google/uuid

## Build and test commands

```
go build ./...
go vet ./...
go test ./... -count=1                       # unit suite (default)
go test -tags=integration ./... -count=1     # integration suite (requires docker compose up in ../omnicore-example-users/devops)
```

Tests live next to the file under test (`foo.go` ↔ `foo_test.go`). Integration tests opt in via `//go:build integration` and exercise real Postgres + Mongo + Kafka — they are excluded from the default `go test ./...` run.

## Architecture — 4-layer DDD with strict boundaries

```
web/                          HTTP transport only
  openapi/                    OpenAPI 3.1 spec generator + Swagger UI route
application/
  configuration/              AppContext (UUID + language + Identity), Language enum, Identity
  translation/                Translator + Module interface, CorePTBR/CoreENG/CoreES/CoreFR/CoreDE/CoreIT/CoreNL built-ins
  notifications/              ContextDTO/MessageDTO (carries NotificationKey)
  pipeline/                   Request/Command/Query, Handler[TReq,TRes], Result[T], Pipeline
  persistence/                ScopedRepository[T] (Reader[T] + Scope) write
                              binding + RequestContext + TxHandle (sealed marker) +
                              AfterBeginHook[T]/BeforeCommitHook[T] + provider
                              interfaces + WriteOption[T]/WithAfterBegin/WithBeforeCommit
  queries/                    QueryHandler + ViewReader port + ReadCriteria/Page DTOs
domain/                       Pure business rules, ZERO IO
  aggregate_mapping.go        AggregateRootProvider (GetAggregateRoot +
                              AggregateChildren — domain declarations; table/FK
                              declared in infra via an explicit TableSchema)
  entity.go                   ValidEntity sealed types (Insertable/Updatable/Archivable/
                              Deletable/Unarchivable/Batch) — carry Entity directly;
                              infra maps table/columns via the Repository's TableSchema
  path_render.go              childCollectionSegment(typeName) — camelCase pluralizer
                              for the aggregate-child notification path segment
                              (Address→addresses, OrderLine→orderLines) + exported
                              PluralizeWord (used by infra to derive the local embed segment)
infra/
  audit/                      AuditEvent + Config (destinations) + persister + echo + partitions
  events/                     Publisher + SlogPublisher
  log/                        Header/Data/Log/Export shapes
  (root files)                postgres, executor, aggregate_persister, outbox, mongo,
                              mongo_view_reader, view, composer, sync, external, rebuild,
                              exception
```

### Dependency rules — NEVER violate

| Layer | May import | Must NOT import |
|---|---|---|
| `domain` | stdlib + `google/uuid` only | everything else |
| `application/*` | `domain`, other `application/*` | `infra`, `web` |
| `infra` | `domain`, `application/persistence` (to implement interfaces) | `web` |
| `web` | `domain`, `application/*` | `infra` directly |

Cross-layer error handling uses the **`domain.NotificationCarrier`** interface (any error carrying `[]*NotificationContext`) so layers never need to type-import each other's error structs.

## Core concepts

### ValidEntity (`domain/entity.go`)

Sealed-style types that can ONLY be produced by the domain package. The private `entity()` method enforces this at compile time.

- `Insertable` / `Updatable` / `Archivable` / `Deletable` / **`Unarchivable`** / `Batch`
- Each carries metadata: `Signature() uuid.UUID`, `EntityName()`, `ActionName()`, `DateTime()`, `Events()`
- Each carries the validated `Entity` directly via `Source() Entity` — validation attestation. Infra resolves table/columns/values via reflection. DDD-pure domain does not pronounce table/column/FK.
- Each ALSO carries optional `*aggregateMeta` consumed by `infra.Postgres` for aggregate-aware persistence. Accessor: `AggregateInfo() (root, isAggregate)`.
- Constructed via the high-level path:
  - `domain.GetInsertable(entity, service)` — no closure (Insert has no prior state).
  - `domain.GetUpdatable[T](loaded, apply, service)` and `domain.GetPartialUpdatable[T](loaded, apply, service)` — **closure form**. The framework snapshots the entity BEFORE running `apply`, so `domain.Old[T](e)` inside BuildRules returns the pre-mutation state. Pass `cmd.ApplyTo` or `cmd.ApplyPartiallyTo` directly — both are `func(T)`.
  - `domain.GetArchivable(entity, service)` / `GetDeletable` / `GetUnarchivable` — no closure (state transitions, no mutation step). Snapshot captured at function entry, exposed via `domain.Old[T](e)` for transition-aware invariants ("cannot archive when balance > 0", etc.) and for audit forensics.
- All Get* validate a `BaseEntity`-embedding struct AND auto-attach aggregate metadata if the entity implements `AggregateRootProvider`. There are no low-level constructors `NewInsertable(table, fields)` etc — the domain does not emit raw table/fields.

### EntityMode + `Modes()`

`Modes()` declares the **set of operations the entity accepts**. The framework consults it before each primary operation — missing mode → `*NotAllowedNotification` (`SemanticForbidden` → 403).

| Constant | Who consults | Failure notification |
|---|---|---|
| `ModeDisplay` | (informative, no active gate) | — |
| `ModeInsert` | `validateForInsert` | `InsertNotAllowedNotification` |
| `ModeUpdate` | `validateForUpdate` | `UpdateNotAllowedNotification` |
| `ModeDelete` | `validateForDelete` | `DeleteNotAllowedNotification` |
| `ModeArchive` | `getArchivable` | `ArchiveNotAllowedNotification` |
| `ModeUnarchive` | `getUnarchivable` | `UnarchiveNotAllowedNotification` |

`ModeArchive` and `ModeUnarchive` are **independent** of `ModeUpdate` — declaring Update does not enable Archive. This allows modeling "freeze-once" entities (set fields once, archive when obsolete) with `[ModeDisplay, ModeInsert, ModeArchive, ModeUnarchive]`. True append-only: `[ModeDisplay, ModeInsert]`.

Archive/Unarchive **run `BuildRules` in ModeUpdate with a distinct `actionName`** (`"GetArchivable"` / `"GetUnarchivable"`), so the existing `IfUpdate` DSL fires for state-transition verbs too — symmetric with PUT (`"GetUpdatable"`) and PATCH (`"GetPartialUpdatable"`). The service branches on `actionName` inside `IfUpdate` when it needs archive-specific logic. After BuildRules, the state-transition checks (Modes() declaring Archive/Unarchive + ID validity) still run and feed into the same `checkAllNotifications` gate.

### BaseEntity (`domain/entity_base.go`)

Embeddable struct for user entities. Provides:
- ID management (`SetID`, `GetID`, `ClearID`)
- Per-entity `NotificationContext` (auto-created from struct name via reflect)
- Event registration (`RegisterEvent(DomainEvent)`)
- Value object validation registration (`AddValueObject`, `AddAggregateValueObject`)
- Old-state accessor `Old() Entity` (typed wrapper: `domain.Old[T](e) T`) — populated by the Get* domain functions during write flows. See [Old-state snapshot](#old-state-snapshot) below.
- Field name aliasing (`AddFieldNameAlias`)
- Private framework methods used by `GetInsertable/Updatable/Archivable/Deletable/Unarchivable`
- **Default `RequiresService() bool { return false }`** promoted via embed — an entity that needs `domain.Service` in `BuildRules` overrides by declaring its own method returning `true`. The other entries of the `Entity` interface (`Modes`, `BuildRules`) remain mandatory.

The `Entity` interface has no `TableName()` or `ToFields()`. Infra maps Go fields to physical columns via an explicit `TableSchema` declared per Repository — no reflection-by-convention.

User entities embed `BaseEntity` and implement the `Entity` interface:

```go
type Customer struct {
    domain.BaseEntity
    Name  string
    Email string
}

func (c *Customer) Modes() []domain.EntityMode {
    return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete, domain.ModeArchive, domain.ModeUnarchive}
}
// RequiresService omitted — *BaseEntity promotes the default `false` via embed.
// NO TableName/ToFields — the Repository declares a TableSchema mapping
// {ID,Name,Email} → {id,name,mail} for the "customers" table.
func (c *Customer) BuildRules(actionName string, service domain.Service, r *domain.Rules) {
    r.IfInsertOrUpdate(func() {
        if c.Name == "" {
            r.AddNotification("Name", domain.RequiredFieldNotification{})
        }
    })
}
```

### AggregateRoot (`domain/aggregate_root.go`)

Embeds `BaseEntity`. Manages typed collections of `AggregateValueObject` with status tracking (Constructor/Added/Changed/Removed):
- `AggregateConstructor(items)` — initialize with status CONSTRUCTOR (loaded from DB; trusted, no type-guard)
- `ClearAggregateItemsOfType(name)` — bulk REMOVE all of a type

**Root authority over the aggregate boundary.** The internal methods `addAggregateItem`/`changeAggregateItem`/`removeAggregateItem` are only used by `AggregateConstructor` (trusted DB load) and the primitives below. The public API is via top-level primitives with type-guard:

- `domain.AddAggregateChild(root, item)` — checks whether `classNameOf(item)` is in `root.AggregateChildren()`. If not, emits `InvalidAggregateChildNotification` (422 Validation) in the root's `NotificationContext` and the item is NOT added to the collection.
- `domain.ChangeAggregateChild(root, original, replacement)` — type-guard on both VOs.
- `domain.RemoveAggregateChild(root, item)` — type-guard on the item.
- `domain.ReplaceAggregateChildrenOf[VO](root, items)` — replaces the entire collection of type VO (Clear + loop of `AddAggregateChild`). Type-safe via generics + runtime type-guard.
- `domain.ValidateAggregateChild(root, item, actionName, svc) bool` — **optional**. Runs `item.BuildRules` inline with `NotificationContext` scoped exactly like the boundary would; returns `true` when nothing was emitted. Use it when the root's method wants immediate feedback (rare case). Pitfall: if you use it inline AND the item enters the collection, the boundary runs `BuildRules` again and generates a duplicate notification — pick one path per item.

**Orthodox DDD convention (ruler):** commands do NOT call `AddAggregateChild`/etc. directly. The ruler is to expose domain methods on the root, named in business vocabulary (`u.AddAddress(addr, svc)`, `u.RemoveAddress(addr)`, `u.ReplaceAddresses(addrs)`) that run invariants spanning children (duplicates, cross rules, lookups via service) and delegate to the primitives. This is what makes the root the *consistency boundary*: the command "tells, doesn't ask".

**Construction-time notifications survive to the boundary.** Methods on the root call `domain.EnsureInitialized(u)` on the first line to ensure the `NotificationContext` exists before any `AddNotification` (a freshly constructed entity has `notifCtx == nil` and `BaseEntity.AddNotification` is a no-op in that case). The `resetEntity` called by `getInsertable/getUpdatable/getDeletable` **no longer clears** the `NotificationContext` — notifications accumulated during construction (e.g., `User.AddAddress` detecting a duplicate) survive until `checkAllNotifications` runs. `IsValid` (explicit re-check path) calls `notifCtx.Clear()` itself before validating, preserving its "fresh check" semantic. The primitives `AddAggregateChild`/`ChangeAggregateChild`/`RemoveAggregateChild`/`ReplaceAggregateChildrenOf`/`ValidateAggregateChild` call `ensureRootInit(root)` internally as defense in depth — the type-guard rejection (`InvalidAggregateChildNotification`) reaches the response even if some caller forgets `EnsureInitialized`.

Declared-typeNames cache is per-`reflect.Type` of the root in a module-level `sync.Map` — the first call pays `root.AggregateChildren()`, the rest are direct lookups.

Typed query helpers (Go generics, read-only):
- `domain.GetAggregateItemsOf[VO](ar)` — all items with their status entries
- `domain.GetAddedItemsOf[VO]`, `GetChangedItemsOf[VO]`, `GetRemovedItemsOf[VO]`, `GetCurrentItemsOf[VO]`

### AggregateValueObject (`domain/aggregate_vo.go`)

Interface that aggregate child items implement:

```go
type AggregateValueObject interface {
    BuildRules(actionName string, service Service, r *Rules)
    GetID() string       // empty for new items, set when loaded from DB
}
```

**Mapping:** infra maps the child's columns via its `TableSchema` (`AddressSchema()`); only declared fields are persisted/scanned/audited. `ID` is the PK — skipped on the INSERT/UPDATE write list (DB-gen) and used in the WHERE clause. FK to the root is declared via `.FK("user_id")` and injected by the persister (it is not a struct field). An exported field NOT declared in the schema is runtime-only — never persisted, scanned, or audited.

### Rules DSL (`domain/rules.go`)

Mode-scoped validation closures:
```go
r.IfInsert(func() { ... })
r.IfUpdate(func() { ... })
r.IfDelete(func() { ... })
r.IfInsertOrUpdate(func() { ... })
r.IfDisplay(func() { ... })
```

Each closure runs only when the current `EntityMode` matches. The framework picks the mode; the caller picks the `actionName` (the third parameter on every `Get*` entry point). Auto handlers pass the canonical PascalCase string (`"GetInsertable"`, etc.); manual handlers may pass any string to branch rules per endpoint:

| Trigger | Mode | `actionName` reaching `BuildRules` |
|---|---|---|
| `domain.GetInsertable(e, svc, an)` | `ModeInsert` | `an` (Auto handler default: `"GetInsertable"`) |
| `domain.GetUpdatable[T](e, apply, svc, an)` | `ModeUpdate` | `an` (Auto handler default: `"GetUpdatable"`) |
| `domain.GetPartialUpdatable[T](e, apply, svc, an)` | `ModeUpdate` | `an` (Auto handler default: `"GetPartialUpdatable"`) |
| `domain.GetArchivable(e, svc, an)` | `ModeUpdate` | `an` (Auto handler default: `"GetArchivable"`) |
| `domain.GetUnarchivable(e, svc, an)` | `ModeUpdate` | `an` (Auto handler default: `"GetUnarchivable"`) |
| `domain.GetDeletable(e, svc, an)` | `ModeDelete` | `an` (Auto handler default: `"GetDeletable"`) |
| `domain.IsValid(e, ModeInsert, svc)` | `ModeInsert` | `"isValid"` (fixed) |
| `domain.IsValid(e, ModeUpdate, svc)` | `ModeUpdate` | `"isValid"` (fixed) |
| `domain.IsValid(e, ModeDelete, svc)` | `ModeDelete` | `"isValid"` (fixed) |
| `domain.IsValid(e, ModeArchive/Unarchive/Display, svc)` | — | (BuildRules not invoked; IsValid just re-collects existing notifications) |

Reverse view — for each `IfXxx`, the actionNames the closure may observe when it fires:

| Closure | Modes that trigger it | actionNames it may receive |
|---|---|---|
| `IfInsert` | `ModeInsert` | `"GetInsertable"`, `"isValid"` |
| `IfUpdate` | `ModeUpdate` | `"GetUpdatable"`, `"GetPartialUpdatable"`, `"GetArchivable"`, `"GetUnarchivable"`, `"isValid"` |
| `IfDelete` | `ModeDelete` | `"GetDeletable"`, `"isValid"` |
| `IfInsertOrUpdate` | `ModeInsert` OR `ModeUpdate` | union of the `IfInsert` + `IfUpdate` rows |
| `IfDisplay` | `ModeDisplay` | (no framework path invokes `BuildRules` in `ModeDisplay` today — reserved for future informative check paths) |

**Key facts:**
- **Archive and Unarchive fire `IfUpdate`, not a dedicated `IfArchive`.** They use `ModeUpdate` with a distinct `actionName` so the existing DSL covers state-transition verbs. Branch on `actionName` inside the closure when archive-specific logic is needed.
- **`actionName` is case-sensitive.** The `Get*` family emits PascalCase (`"GetInsertable"`); `IsValid` emits lowercase `"isValid"`. Custom callers (e.g. a manual handler that passes `"AdminCreate"` to differentiate stricter validation) must match the string exactly inside `BuildRules`.
- **AggregateValueObject `BuildRules` receives the SAME `actionName` as the root.** When `runAggregateValidations` fires the AVO `BuildRules` per child, it propagates the root's `actionName` verbatim — so an AVO branching on `actionName == "GetArchivable"` matches the verb archiving the aggregate.
- **`IfDisplay` is inert in the canonical dispatch.** `ModeDisplay` is the default mode of a freshly constructed `BaseEntity`, but no framework path calls `NewRules(ModeDisplay, ctx)`. The closure exists in the DSL for symmetry; using it today produces dead code unless an external caller wires its own dispatch.

```go
r.IfUpdate(func() {
    if actionName == "GetArchivable" {
        // archive-specific: e.g., "cannot archive primary account"
    }
})
```

### Old-state snapshot

Domain owns the "what did the entity look like before this write?" snapshot. The Get* functions of the write path capture it automatically; `BuildRules` reads it via the typed helper `domain.Old[T](e) T`, and the auditor consumes it to compute `changes` (kind=delta) or to emit `snapshot` (kind=snapshot on Delete). Used both for **transition-aware invariants** in the domain and for **timeline reconstruction** in audit.

```go
func (u *User) BuildRules(actionName string, svc domain.Service, r *domain.Rules) {
    r.IfUpdate(func() {
        if old := domain.Old(u); old != nil {
            if old.Email != u.Email && u.Activated {
                r.AddNotification("Email", EmailLockedAfterActivationNotification{})
            }
        }
    })
}
```

**Mechanics**:

- `BaseEntity` carries a private `old Entity` field, exposed via `Old() Entity` on the `Entity` interface.
- `Get*` functions call an internal `captureOld(self)` at the right moment in the lifecycle:
  - `GetUpdatable[T](e, apply, svc)` and `GetPartialUpdatable[T](e, apply, svc)` snapshot **before** running the `apply` closure — so `Old()` reflects the loaded state, not the mutated one. Critical ordering: the apply runs inside the domain function, not in the handler.
  - `GetDeletable / GetArchivable / GetUnarchivable` snapshot at function entry — there is no mutation step. `Old()` returns the loaded state (= "the entity right before being deleted / archived / unarchived"). Useful in BuildRules for transition guards and in audit for forensics.
  - `GetInsertable` does NOT snapshot. `Old()` returns the typed zero value (`var zero T`). Defensive code: `if old := domain.Old(u); old != nil { ... }`.
- The snapshot is a JSON round-trip clone of the entity (`encoding/json`). Captures exported fields; ignores private `BaseEntity` / `AggregateRoot` state (`notifCtx`, `events`, `signature`). The clone is a **read-only ghost** — calling mutator domain methods on it is a silent no-op because its `notifCtx` is nil.
- For aggregates (entities implementing `AggregateRootProvider`), `captureOld` also deep-copies the current aggregate items map so `Old()` exposes the prior children via the usual helpers:

```go
oldAddrs := domain.GetCurrentItemsOf[Address](&domain.Old(u).AggregateRoot)
```

**Custom infra escape hatch**: an implementation that bypasses the framework's Get* functions (e.g., a Repository that builds `Updatable` from scratch — not supported by the public API today, but conceptually) would not have `Old()` populated. The auditor's Update path emits `changes` computed as `cur` vs nil (every column appears with from=null); on Delete it falls back to a snapshot of the live source. Graceful degrade, not error.

### Audit event shape

The framework writes one `AuditEvent` per ValidEntity write (granularity B — aggregate is the event unit; mirrors the outbox row that ships to Kafka). Routing is governed by `audit.destinations` in `microservice.<profile>.yaml`:

- **`database`** → `audit.InsertAuditEvent` writes a row to the `audit_events` table **inside the same `pgx.Tx`** as the data row + outbox row. The row IS the audit — atomic with the write by construction. Source of truth for forensics and compliance: if COMMIT succeeded, the audit exists in PG.
- **`slog`** → `audit.EchoSlog` emits a flat top-level slog line after COMMIT. Read-side echo for observability; lossy if the shipper drops events, but the PG row stays the authority. Format unchanged from the previous `SlogAuditor` shape — ELK pipelines and consumer dashboards continue working without changes.

Shape lives in [`infra/audit/event.go`](infra/audit/event.go); the in-TX writer in [`infra/audit/persister.go`](infra/audit/persister.go); the slog echo in [`infra/audit/echo.go`](infra/audit/echo.go); the per-verb event builders in [`infra/audit_builder.go`](infra/audit_builder.go). ELK consumers query the wire vocabulary directly: `entityType:User AND entityId:<uuid>` sorted by `dateTime` reconstructs the timeline of one aggregate. SQL consumers query `audit_events` directly: `WHERE entity_type='User' AND aggregate_id='<uuid>' ORDER BY occurred_at` returns the same timeline from the authoritative side, joinable to other PG state.

Top-level fields (every event carries these):

| Field | Type | Note |
|---|---|---|
| `threadId` | UUID string | per-request identifier from `AppContext.ID()` |
| `entityType` | string | Go type name of the root entity |
| `entityId` | string | aggregate root ID |
| `verb` | string | `insert` \| `update` \| `archive` \| `unarchive` \| `delete` — **SQL-grounded**: same SQL fingerprint → same verb. PUT and PATCH share `update` (SQL is identical: `UPDATE col=val, updated_at=NOW()`); the distinction lives in `actionName` |
| `actionName` | string | the `actionName` passed to the `Get*` call (canonical or custom) |
| `kind` | string | `snapshot` \| `delta` \| `transition` — discriminates the body block |
| `actor` | string | JWT `sub` or `"anonymous"` |
| `actorIssuer` | string (omitempty) | JWT `iss` when present |
| `actorClaims` | object (omitempty) | filtered by the `auth.auditClaims` allowlist |
| `dateTime` | RFC3339Nano | operation timestamp captured at ValidEntity construction |

Body block — exactly one of the three regimes applies per event, selected by `kind`:

| `kind` | Verbs | Carries | What's in the block |
|---|---|---|---|
| `snapshot` | `insert`, `delete` | `snapshot: map[goFieldName]value` | Full state (post-insert for Insert; pre-delete via Old() for Delete) |
| `delta` | `update` | `changes: []{field,fieldLabelKey,from,to}` sorted by `field` | Only mutated fields; unchanged ones absent. `field` is the faithful domain name (the raw Go field name, e.g. `Email`/`ZipCode`) — audit is map-blind and never carries the physical column. `fieldLabelKey` (omitempty) carries the catalog key declared on the source field's `label:"..."` struct tag — see "Field labels" under [Notification system](#notification-system-domainnotificationgo) |
| `transition` | `archive`, `unarchive` | (neither block) | The verb itself is the recovery hint (symmetric inverse) |

`PartialUpdate` (PATCH) shares `verb=update` with PUT (SQL is identical). The PUT vs PATCH distinction lives in `actionName` (`GetUpdatable` vs `GetPartialUpdatable`). Same unification the domain already applies — `IfUpdate` fires for both verbs. `Updatable.IsPartial()` is preserved on the type for non-audit callers but no longer routes the audit verb.

Children cascade — `children` map appears when the source implements `AggregateRootProvider` and at least one child is observable for the verb. Keyed by Go type name (`Address`, `OrderLine`); each entry carries `{id, op, snapshot}` or `{id, op, changes}` following the same kind discipline. **Child ops are SQL-grounded** — identical vocabulary to the root verb, every op echoes the SQL fingerprint of the row-level change:

| Verb | Children included | Per-child `op` | SQL on the child row | Body block |
|---|---|---|---|---|
| `insert` | all current (Constructor/Added) | `inserted` | `INSERT INTO addresses (...)` | `snapshot` |
| `update` — Added | appended via `AddAggregateChild` | `inserted` | `INSERT INTO addresses (...)` | `snapshot` |
| `update` — Changed | replaced via `ChangeAggregateChild` | `updated` | `UPDATE addresses SET col=val, updated_at=NOW()` | `changes` (diff against pre-mutation child) |
| `update` — Removed | marked via `RemoveAggregateChild`/`ReplaceAggregateChildrenOf` | `archived` | `UPDATE addresses SET deleted_at=NOW()` (the row stays in the DB, recoverable via unarchive) | `snapshot` (pre-archive state, from Old()) |
| `update` — Constructor | (untouched — **skipped**) | — | (no SQL) | — |
| `archive` | every loaded active child (cascade) | `archived` | `UPDATE addresses SET deleted_at=NOW() WHERE user_id=$1 AND deleted_at IS NULL` | `snapshot` |
| `unarchive` | every loaded archived child (cascade) | `unarchived` | `UPDATE addresses SET deleted_at=NULL WHERE user_id=$1 AND deleted_at IS NOT NULL` | `snapshot` |
| `delete` | every loaded child | `deleted` | (Postgres `FK ON DELETE CASCADE` on root delete) | `snapshot` |

The child op vocabulary is the same 5-verb set as the root verb (`inserted`/`updated`/`archived`/`unarchived`/`deleted`) — every op anchors in a SQL fingerprint, no exceptions. The `update` verb dispatches into 3 distinct child ops because the same outer verb produces 3 distinct SQLs at child level depending on the child's `CurrentStatus`. UPDATED children pair pre/post by `GetID()` against the deep-copied prior root's aggregates map (consequence of `captureOld` cloning the items map for aggregates — see above). For flat entities (no `AggregateRootProvider`), the `children` block is absent.

### Notification system (`domain/notification*.go`)

- `Notification` is a marker interface enforced via unexported `isNotification()` + `Semantic() NotificationSemantic`
- Concrete notification types embed one of: `DomainNotificationBase` / `ApplicationNotificationBase` / `InfrastructureNotificationBase` — all default to `SemanticValidation`. Override with `Semantic() NotificationSemantic { return Semantic… }` whenever the natural HTTP/transport semantic differs (Conflict, NotFound, …)
- Translation key is the struct's type name via `reflect.TypeOf(n).Name()` — same pattern as Kotlin's `::class.simpleName`

```go
type UsernameAlreadyExistsNotification struct{ domain.DomainNotificationBase }
```

`NotificationContext` groups messages by context name (entity/aggregate). Methods: `AddNotification(name, n, value...)`, `AddNotificationMessage(msg)`, `Scoped`, `HasErrors`, `Clear`, `ChangeFieldName`, `Copy`, `Messages`. (Previously called `AddField`/`Add` — renamed to make the intent clear; it is a notification, not a field.)

`NotificationMessage` carries optional `Path []PathSegment`, `Override`, `FieldName`, `FieldValue`, `FuncName`, `Err`, `Vars map[string]string`, plus the typed `Notification`. The wire field is resolved via `ResolveFieldName()` with precedence **Override > rendered Path > FieldName**. The translation variables resolved for rendering come from `domain.MessageVars(msg)`, which merges (a) the notification's `tvar`-tagged exported fields via `domain.ExtractVarsFromTags(n)` with (b) the per-emit `Vars` override (per-emit wins on key collision). See [Parameterized notifications](#parameterized-notifications) below.

**JSON path matching (the wire `field` matches the client's JSON exactly).** Validation messages emit Path-aware field names so the response carries the same path the client sent.

**Uniformity root vs AggregateValueObject — principle.** `Entity.BuildRules` and `AggregateValueObject.BuildRules` have the **same signature** and same body shape. Same DSL (`r.IfInsertOrUpdate`), same emit (`r.AddNotification(...)`, `r.AddNotificationMessage(...)`), same `actionName` propagated through the same validation chain. The only difference is the receiver (root is a struct with state and embedded ctx; AVO is a value type passed by the framework):

```go
// Root — User
func (u *User) BuildRules(actionName string, svc domain.Service, r *domain.Rules) {
    r.IfInsertOrUpdate(func() {
        if u.Name == "" {
            r.AddNotification("Name", domain.RequiredFieldNotification{})
        } else if !valid(u.Name) {
            // optional value becomes FieldValue — echo of the rejected input in the response
            r.AddNotification("Name", InvalidNameNotification{}, u.Name)
        }
    })
}

// AVO — Address — identical signature
func (a Address) BuildRules(actionName string, svc domain.Service, r *domain.Rules) {
    r.IfInsertOrUpdate(func() {
        if a.Street == "" {
            r.AddNotification("Street", domain.RequiredFieldNotification{})
        }
    })
}
```

`actionName` is the same string the root receives — the canonical (`"GetInsertable"`/`"GetUpdatable"`/`"GetDeletable"` etc.) when the Auto handler dispatches, or a custom string when a manual handler calls `domain.GetInsertable(e, svc, "AdminCreate")` directly. Allows the AVO to branch rules by flavor of action — useful when two endpoints fire the same `ModeInsert` but demand different rigor (e.g., `"AdminCreate"` forces ZipCode required in any country; default accepts empty).

Wire format of each one:

| Origin | What is written | Wire `field` |
|---|---|---|
| Root entity (`User.BuildRules`) | `r.AddNotification("Name", n)` | `"name"` |
| Root entity with input echo | `r.AddNotification("Email", n, u.Email)` | `"email"` + `value` populated |
| Aggregate child (`Address.BuildRules`) | `r.AddNotification("ZipCode", n)` | `"addresses[0].zipCode"` |
| Nested aggregate (future) | `r.AddNotification("Quantity", n)` | `"orders[0].lines[1].quantity"` |

Mechanics:

1. **Rules carries ctx + entityType.** `NewRules(mode, ctx, entityType)` packages the EntityMode + destination NotificationContext + the Go `reflect.Type` of the entity / value object that owns this Rules. `r.AddNotification`/`r.AddNotificationMessage` delegate to the internal ctx; the entityType lets `AddNotification` read the field's `label:"..."` struct tag at emit time and stamp `LabelKey` on the emitted message (see "Field labels" below). For the root, ctx is the entity's own `e.NotificationContext()` and entityType is `reflect.TypeOf(self)`; for an AVO, ctx is a `Scoped(NameSegment(collection), IndexSegment(i))` view of the root's ctx and entityType is `reflect.TypeOf(self)` of the AVO struct.
2. **Notification path segment is camelCase.** `runAggregateValidations` iterates `root.AllAggregateItems()` and uses `childCollectionSegment(typeName)` — `toLowerCamel(typeName)` pluralized in camelCase (`Address`→`"addresses"`, `OrderLine`→`"orderLines"`) — as the path segment name. It matches the JSON wire segment of the child collection, not the physical table name.
3. **Render `toLowerCamel` acronym-aware.** A Go identifier becomes camelCase: `Name`→`name`, `URL`→`url`, `ZipCode`→`zipCode`, `URLPath`→`urlPath`. Strings already in lowercase pass through — so the legacy `FieldName: "id"` in mode validators remains intact.
4. **Wire layer reads `ResolveFieldName()`** (`web/from_notifications.go`, `application/notifications/convert.go`) with precedence **Override > rendered Path > FieldName**.

**Controlling the wire field name — three paths.** All three feed `ResolveFieldName()`; pick by lifetime of the rule:

- **Default emission** — `r.AddNotification("Name", n)` inside `BuildRules`. The acronym-aware `toLowerCamel` render produces the wire name (`"name"`, `"zipCode"`, `"urlPath"`). The common case — no override needed.
- **`BaseEntity.AddFieldNameAlias(orig, new)`** — declarative on the entity. Every emission about `orig` surfaces as `new` on the wire, regardless of which handler triggered it. Applied automatically inside `checkAllNotifications` via `applyFieldAliases` before contexts are collected — consumers never call `ChangeFieldName` by hand for this case. Use when the renaming is a stable rule of the entity (legacy-compat shim, public-vs-internal naming).
- **`NotificationContext.ChangeFieldName(orig, new)`** — imperative in a manual handler. Walks messages, compares each against `ResolveFieldName()`, sets `Override = new` on the ones that match. Path/FieldName originals stay preserved for diagnosis; only the wire changes. Use when the renaming is conditional / per-endpoint.

Both override paths populate the same slot (`NotificationMessage.Override`) which sits at the top of the `ResolveFieldName()` precedence. The choice is purely about *when* the decision is taken — entity-definition time vs request-handling time.

**Field labels — `label:"<catalogKey>"` struct tag.** The wire `field` (`addresses[0].zipCode`) and the audit `field` (the raw Go field name, e.g. `ZipCode`) are technical identifiers. They are stable but not human-readable. Channels without a frontend (e-mail / SMS / push / audit read by compliance) consume the envelope directly and benefit from a translated label next to the technical identifier — "CEP é inválido" reads naturally; "addresses[0].zipCode é inválido" does not.

The mechanism extends the existing translation surface (no new translator, no boot phase): a `label:"<catalogKey>"` struct tag on the field declares which catalog entry renders the human label. `Rules.AddNotification` reads the tag at emit time via `reflect.Type` and writes the catalog key on `NotificationMessage.LabelKey`; the translation layer (`application/notifications/convert.go::ToContextDTOs`) renders the key via `Translator.Render(lang, key, nil)` alongside the existing `Message` render and surfaces the result on `MessageDTO.FieldLabel`. The audit pipeline (`infra/audit_builder.go::computeChanges`) walks the same tag and writes the raw key on `FieldChange.FieldLabelKey`; `audit.RenderLabels` (typed) and `audit.RenderLabelsInJSON` (map-form) consume the key at read time and replace it with `FieldLabel` rendered in the chosen locale.

```go
// consumer side — domain/user.go
type User struct {
    domain.AggregateRoot
    Name  string  `label:"UserNameField"`
    Email string  `label:"UserEmailField"`
    Phone *string                            // no label tag — nothing emitted
}

type Address struct {
    Street  string `label:"AddressStreetField"`
    ZipCode string `label:"AddressZipCodeField"`
}
```

```go
// consumer side — application/translations/ptbr.go (one entry per declared key)
"UserNameField":       "Nome",
"UserEmailField":      "E-mail",
"AddressStreetField":  "Rua",
"AddressZipCodeField": "CEP",
```

Wire envelope on a notification rendered in PT-BR with `label:"AddressZipCodeField"` on `Address.ZipCode`:

```json
{
  "context": "User",
  "messages": [{
    "notificationKey": "InvalidZipCodeNotification",
    "field": "addresses[0].zipCode",
    "fieldLabel": "CEP",
    "value": "ABC",
    "message": "O CEP é inválido."
  }]
}
```

Audit row (root delta on `Name`) with `label:"UserNameField"` on `User.Name`:

```json
{
  "verb": "update",
  "kind": "delta",
  "changes": [{
    "field": "Name",
    "fieldLabelKey": "UserNameField",
    "from": "Jane Doe",
    "to": "Jane Smith"
  }]
}
```

Rules:

- **Tag value = catalog key.** Lookup goes through the standard `Translator.Render(lang, key, nil)` — same primitive `MessageDTO.Message` uses today.
- **No tag = no label.** The wire `MessageDTO.FieldLabel` and the audit `FieldChange.FieldLabelKey` are `omitempty` — services without `label` tags see byte-identical envelopes.
- **`label:"-"`** opts a field out explicitly (mirror of `json:"-"`); empty tag value is treated the same.
- **A field not declared in the `TableSchema` carries no label.** Such a field is runtime-only — the framework never persists, scans, or audits it, so it never surfaces on the wire or in audit, label included.
- **Catalog miss → raw key fallback.** Same posture `Translator.Render` already applies to `MessageDTO.Message`: returns the catalog key as the rendered string AND emits `slog.Warn("translation.key.missing", "lang", lang, "key", key)` once per `(lang, key)` tuple. No boot-time validator.
- **Audit stores the key, not the rendered string.** Immutable artifact — the catalog evolves; the key remains. Readers render in the locale they want via `audit.RenderLabels(ev, t, lang)` (typed `*AuditEvent`) or `audit.RenderLabelsInJSON(doc, t, lang)` (parsed `map[string]any` from the `audit_events.jsonb` payload). Both helpers walk top-level `changes` + `children.<typeName>[].changes`, pop `fieldLabelKey`, and write `fieldLabel` rendered via `Translator.Render`. Snapshot blocks (Insert / Delete) are intentionally not touched — they carry `map[column]value` with no schema for labels.
- **Resolution is by the Go field name the rule passed.** `r.AddNotification("Email", n)` reads the `label` tag on `Email`. `AddFieldNameAlias("Email", "primaryEmail")` renames the wire `field` only — `fieldLabel` stays the translation of the tag on `Email`. The two surfaces are independent.
- **Naming convention (recommendation).** `<Entity><Field>Field` (e.g. `UserNameField`, `AddressZipCodeField`) mirrors the `<What>Notification` convention. Not enforced — any catalog key string works.
- **Manual emission via `AddNotificationMessage`** carries `LabelKey == ""` by default. Set `msg.LabelKey = "<catalogKey>"` explicitly on the message when the manual path needs a label.

Reflection plan cached per `reflect.Type` (`sync.Map` in `domain/field_label.go`) — first emission on a given type pays one walk; subsequent emissions are direct map reads. Same shape as the `tvar` extraction cache.

**`r.AddNotification(field, n, value)` covers the common case.** The variadic `value` becomes `FieldValue` and supports `string`, `*string` (safe deref), nil, and any other type (via `fmt.Sprint`). For messages that need `Err`, `FuncName`, `Override`, or multi-segment Path, use `r.AddNotificationMessage(NotificationMessage{...})` — rare escape hatch.

**Nested aggregates.** Scoped contexts compose recursively.

**`NotificationKey` propagation:** the typed identity (e.g. `"RecordNotFoundNotification"`) is preserved through translation. `MessageDTO.NotificationKey` and the wire-format `ErrorMessage.NotificationKey` both carry it. Clients can branch UI on the key; the HTTP layer maps it to status codes (see [HTTP status mapping](#http-status-mapping)).

**Error wrappers per layer** — each layer has helpers that return the correct typed error, avoiding the `NewNotificationContext + Add + NewXxxError` boilerplate:

- `domain.SingleNotificationError(contextName, fieldName, n) → *DomainError`
- `domain.NotFoundError(contextName, fieldName, fieldValue) → *DomainError` (uses `RecordNotFoundNotification`)
- `domain.FieldErrorWithCause(contextName, fieldName, cause, n) → *DomainError`
- `infra.SingleNotificationError(...)` / `infra.FieldErrorWithCause(...) → *InfrastructureError`
- `application/exception.SingleNotificationError(...)` / `exception.FieldErrorWithCause(...) → *ApplicationError`

`ApplicationError` lives in `omnicore/application/exception/` (moved from `pipeline/exception.go`).

### Parameterized notifications

Notifications carry **runtime-driven variables** into their translated messages without exploding the catalog into one entry per value. Canonical use case: a length / count / amount that varies per tenant or per request configuration, surfaced verbatim inside the user-facing message ("excede o tamanho máximo de **100** caracteres", where 100 is the per-tenant limit).

**Declaration — `tvar` struct tag.** The framework reflects over the notification struct's exported fields tagged `tvar:"<name>"` and exposes them as the variables `{<name>}` substitutes against in the catalog string. The struct field name is irrelevant; only the tag value matters.

```go
type NameMaxLengthExceededNotification struct {
    domain.DomainNotificationBase
    MaxLength int `tvar:"maxLength"`
}
```

```go
// ENG catalog
"NameMaxLengthExceededNotification": "Name exceeds the maximum allowed length of {maxLength} characters.",
// PTBR catalog
"NameMaxLengthExceededNotification": "O nome excede o tamanho máximo permitido de {maxLength} caracteres.",
```

Multiple fields produce multiple placeholders. Pointer fields are dereferenced (nil renders as empty string). Unexported fields are skipped — the framework can only walk the exported surface. The reflection plan is cached per `reflect.Type` in a module-level `sync.Map`, so repeated emissions of the same notification type pay one type lookup + field reads.

**Escape hatch — `TranslationVars()` method.** When the variables cannot be expressed as `tvar` tags (unexported state, computed values, ctx-aware shaping), declare an optional method on the notification:

```go
type SensitiveLimitNotification struct {
    domain.DomainNotificationBase
    threshold int  // unexported — invisible to tag reflection
}
func (n SensitiveLimitNotification) TranslationVars() map[string]string {
    return map[string]string{"threshold": strconv.Itoa(n.threshold)}
}
```

The method **replaces** (does not merge with) the tag-based extraction. The expected ratio is ≥95% pure `tvar`, ≤5% method-based. The method is detected via type assertion (`domain.TranslationVarsProvider`), so notifications without it pay nothing.

**Per-emit override — `Vars` on `NotificationMessage`.** When the same notification type ships with default vars from its tags but a specific call site needs to inject additional or overriding values, pass them via the per-emit `Vars` map. Rules DSL exposes the helper:

```go
r.AddNotificationWithVars(
    "Name",
    NameMaxLengthExceededNotification{MaxLength: u.NameMaxLength},
    map[string]string{"context": "premium-plan"},
    u.Name,                                  // optional value variadic — same as AddNotification
)
```

Resolution order at render time (per-emit wins on key collision): tag-derived vars (or `TranslationVars()` output) ← per-message `Vars`.

**Context label vars — `SetVars` on `NotificationContext`.** The wrapping context label (`"User"`, `"Order"`) goes through the same Translator as messages. Carry runtime variables for the label itself via:

```go
ctx := domain.NewNotificationContext("UserOf{tenantId}")
ctx.SetVars(map[string]string{"tenantId": "acme"})
// catalog: "UserOf{tenantId}": "Usuário de {tenantId}"
// resolved label on the wire: "Usuário de acme"
```

Empty map and nil both clear the slot. Scoped views forward to the root (`ContextVars()` reads from the root of the chain), mirroring how the message Path prefix composes.

**Translation surface — `Render` and `Interpolate`.**

```go
func (t *Translator) Render(lang configuration.Language, key string, vars map[string]string) string
func Interpolate(s string, vars map[string]string) string
```

- `Render(lang, key, vars)` resolves the catalog message + interpolates. Scanner accepts the literal placeholder `{<name>}` where `<name>` matches `[A-Za-z_][A-Za-z0-9_]*`; sequences that do not match (`{1bad}`, `{not closed`, isolated `{`) are written verbatim. Missing catalog key → returns `key` as fallback AND emits `slog.Warn("translation.key.missing", ...)` once per `(lang, key)` tuple. Missing placeholder in vars → leaves the literal `{name}` in the output AND emits `slog.Warn("translation.var.missing", ...)` once per `(lang, key, placeholder)` tuple. Both warns dedupe via a process-level `sync.Map`. `vars == nil` and `vars == {}` behave identically: no substitution, no missing-var warn.
- `Interpolate(s, vars)` is the substitution-only step (no catalog lookup). Missing placeholders are left literal without firing warn-once — `Interpolate` has no `(lang, key)` context.
- `Get(lang, key)` and `GetOr(lang, key, fallback)` remain the canonical no-interpolation lookups, for synthetic notifications without vars (e.g., `MissingPermissionNotification` in `PermissionGate`, `InternalServerErrorNotification` in `RespondWithInternalServerError`).

**Pipeline integration.** `application/notifications/convert.go::ToContextDTOs` resolves vars automatically through `domain.MessageVars(msg)` and `ctx.ContextVars()`, then renders via `Translator.Render` instead of `Get`. Auto Command Handlers, manual handlers, and audit/events paths inherit the new behavior without code changes — the substitution lives at the boundary that already translates.

**Backwards compatibility.** Notifications without `tvar` tags AND without `TranslationVars()` produce nil from `ExtractVarsFromTags` → `Render` behaves identically to `Get`. Catalog entries without placeholders pass through the scanner unchanged. The new `Vars` field on `NotificationMessage` and `contextVars` on `NotificationContext` default to nil. No existing call site needs editing.

### Result[T] and Pipeline (`application/pipeline/`)

- `Result[T]` is a single struct discriminated by `state` (StateSuccess/StateFailure/StateException)
- Factories: `Success(v)`, `Failure(notifications)`, `Exception(err)`
- Fluent DSL: `.OnSuccess(fn).OnFailure(fn).OnException(fn)`, plus `ValueOr`, `MustValue`, `FirstSuccess`, `ForEach`

`Pipeline.Run[T]` and `Pipeline.Dispatch[TReq,TRes]` are **generic top-level functions** (Go doesn't allow generic methods). They wrap handler execution and:
- Catch any error implementing `domain.NotificationCarrier` → `Failure[T]` (translated via configured `Translator` and the request's `Language`)
- Catch panics via `defer/recover` → `Exception[T]`
- Log structured events via `log/slog`

```go
pipe := pipeline.New(translator)
result := pipeline.Dispatch(pipe, ctx, cmd, handler)
return web.RespondFromResult(c, result, fiber.StatusCreated)
```

### Persistence ports (`application/persistence/`)

The **domain** layer declares the pure repository ports; the **application**
layer adds the request-scoped write binding. The domain ports carry NO
context, NO actor, NO hooks — those are infrastructure and bind below the port:

- **`domain.Reader[T]`** (domain) — the read port: `FindByID(id) (T, error)` + `New() T`. Pure, ctx-less; reads carry no request-scoped concern.
- **`domain.Writer`** (domain) — the write port: `Insert(Insertable) (ID, error)` / `Update` / `Archive` / `Unarchive` / `Delete`. Pure — each method takes only a ValidEntity (no ctx, no hooks). Non-generic because the ValidEntity flavors already carry the source entity.
- **`domain.Repository[T]`** (domain) — the full port, `Reader[T] + Writer`. What a consumer names when declaring a read+write repository (`type UserRepository interface { domain.Repository[*User]; FindByEmail(...) }`). Pure: stdlib + google/uuid only, zero application import.
- **`persistence.ScopedRepository[T]`** (application) — the binding the Auto Command Handlers hold: `domain.Reader[T]` + `Scope(ctx *AppContext, opts ...WriteOption[T]) domain.Writer`. Reads stay direct on the handle; writes go through `Scope`, which binds the ctx (cancellation → pgx, actor → audit) and the in-TX hooks and returns a pure `domain.Writer`. `infra.BaseRepository[T]` implements `Scope`; the consumer's repository (embedding it + a loader that provides `FindByID`) satisfies `ScopedRepository[T]` with no extra code.

The request-scoped ctx the persister consumes is **`persistence.RequestContext`** — an application interface embedding `context.Context` + `ID()`/`ActorSubject()`/`ActorIssuer()`/`ActorClaims()`, satisfied by `*configuration.AppContext`. The domain layer never pronounces it; that is the whole point of the Scope split. There is no `domain.Context` — request scope is an application concern.

The lifecycle-hook contract carries one sealed marker, two hook function types, and two provider interfaces:

```go
// Sealed marker — no public methods. The framework's pgxTxHandle (in infra/)
// is the only implementation. Application code never executes SQL through
// it; the hook threads the handle to a port whose infra adapter calls
// fwinfra.UnwrapPgxTx(tx) to recover the underlying pgx.Tx.
type TxHandle interface {
    // txHandle is the unexported sealing method.
}

type AfterBeginHook[T any]   func(ctx *AppContext, t T, tx TxHandle) error
type BeforeCommitHook[T any] func(ctx *AppContext, t T, id domain.ID, tx TxHandle) error

type AfterBeginHookProvider[T any]   interface { AfterBegin(*AppContext, T, TxHandle) error }
type BeforeCommitHookProvider[T any] interface { BeforeCommit(*AppContext, T, domain.ID, TxHandle) error }

func WithAfterBegin[T any](fn AfterBeginHook[T])   WriteOption[T]
func WithBeforeCommit[T any](fn BeforeCommitHook[T]) WriteOption[T]
```

`TxHandle` is opaque to application code — by construction, a hook cannot pronounce SQL through it. The canonical (and only) shape for an in-TX side effect is: declare a port in `application/` or `domain/` whose method receives a `persistence.TxHandle`, implement the port in `infra/` where the adapter calls `fwinfra.UnwrapPgxTx(tx)` to obtain the live `pgx.Tx` and owns the SQL + table name. The sealing method on `TxHandle` is unexported, so no implementation outside the framework's own `infra/pgxTxHandle` can satisfy the interface — any test fake or alternative transport plugs in by writing its own adapter in `infra/`. `WriteOption[T]` is the functional-option type the variadic carries; the persister fires the resolved closures at two fixed TX positions:

```
BEGIN
  ⬇ afterBegin()                          ← position A
  data write (root + children when aggregate)
  outbox INSERT
  audit_events INSERT (when configured)
  ⬇ beforeCommit(id, …)                   ← position D
COMMIT
```

Flat-path (`infra.Postgres` for non-aggregate entities) and aggregate-path (`infra.aggregate_persister` for entities implementing `AggregateRootProvider`) fire the hooks at the same analogous positions; consumer code never knows which path the persister took. The aggregate path emits a single hook firing per `repo.Method()` call — granularity B, matching the outbox and audit row cardinality. The hook receives the root entity with all aggregate children reachable via the usual `domain.GetCurrentItemsOf[VO]` helpers.

## Auto Command Handlers

For trivial CRUD — the logic fits entirely on the Entity via `BuildRules` — the framework provides **ready-made generic handlers**. The service writes only the Command — `ToEntity`/`ApplyTo`/`ApplyPartiallyTo` on the way in AND `FromEntity` on the way out, both as methods on the Cmd struct. The wire `Response.FromResult` finishes the projection at the web layer. The handler becomes `&handlers.InsertCommandHandler[*User, *InsertUserCommand, results.InsertUserResult]{Repo: repo}` in a single line — no `Project` field, no `Auditor` field. They coexist with manual handlers (DDD preserved).

**In-TX side effects are opt-in via Cmd methods.** A Cmd that declares `AfterBegin(ctx *AppContext, t T, tx persistence.TxHandle) error` and/or `BeforeCommit(ctx *AppContext, t T, id domain.ID, tx persistence.TxHandle) error` satisfies the matching provider interface; every Auto handler detects both at the top of `Handle` via type assertion and forwards them as `WriteOption[T]` closures to the `Repo.Scope(ctx, opts...)` write binding. The closures fire INSIDE the persister's TX (positions A and D — see "Persistence ports" above). Compile-time safety to catch typos: declare `var _ persistence.BeforeCommitHookProvider[*T] = (*Cmd)(nil)` at the bottom of the Cmd file.

### Canonical vocabulary

Every Cmd implements **input boundary + output boundary** as methods on its own struct. The output method `FromEntity(ctx, T) TResult` is required on all 6 verbs — bodyless verbs (Archive/Unarchive/Delete) typically declare `TResult = fwresults.None` and have FromEntity return `fwresults.None{}`.

| Operation | Command | Input method | Output method | Framework handler | Strict body? | HTTP verb |
|---|---|---|---|---|---|---|
| Insert | `InsertXxxCommand` (`pipeline.CommandBase`) | `ToEntity(ctx) T` | `FromEntity(ctx, T) TResult` | `handlers.InsertCommandHandler[T, *Cmd, TResult]` | no | POST |
| Update (full) | `UpdateXxxCommand` (`pipeline.CommandBaseWithID`) | `ApplyTo(ctx, T)` | `FromEntity(ctx, T) TResult` | `handlers.UpdateCommandHandler[T, *Cmd, TResult]` (embeds `pipeline.FullBody`) | **yes** | **PUT** |
| Partial Update | `PatchXxxCommand` (`pipeline.CommandBaseWithID`) — fields typically as pointers | `ApplyPartiallyTo(ctx, T)` | `FromEntity(ctx, T) TResult` | `handlers.PartialUpdateCommandHandler[T, *Cmd, TResult]` | no | **PATCH** |
| Archive | `ArchiveXxxCommand` (`pipeline.CommandBaseWithID`) | `ApplyTo(ctx, T)` (runtime authz fields) | `FromEntity(ctx, T) TResult` (typ. `fwresults.None`) | `handlers.ArchiveCommandHandler[T, *Cmd, TResult]` | no | PATCH/DELETE |
| Unarchive | `UnarchiveXxxCommand` (`pipeline.CommandBaseWithID`) | `ApplyTo(ctx, T)` (runtime authz fields) | `FromEntity(ctx, T) TResult` (typ. `fwresults.None`) | `handlers.UnarchiveCommandHandler[T, *Cmd, TResult]` | no | PATCH |
| Delete | `DeleteXxxCommand` (`pipeline.CommandBaseWithID`) | `ApplyTo(ctx, T)` (runtime authz fields) | `FromEntity(ctx, T) TResult` (typ. `fwresults.None`) | `handlers.DeleteCommandHandler[T, *Cmd, TResult]` | no | DELETE |

**Cmd owns the application boundary — input AND output.** Every Auto handler receives the request `*AppContext` and threads it to the Cmd at TWO symmetric points:

- **Input** — `Cmd.ToEntity(ctx)` (Insert), `Cmd.ApplyTo(ctx, t)` (Update/Archive/Unarchive/Delete), `Cmd.ApplyPartiallyTo(ctx, t)` (PartialUpdate) — translates ctx into business-named entity fields before validation/persistence.
- **Output** — `Cmd.FromEntity(ctx, t) TResult` on every verb — translates the post-persistence entity into the application-layer Result the wire will render. Same ctx as the input boundary, so a future authz/identity-aware projection (e.g. "show only fields the principal is allowed to see") consumes the same Identity.

The handler does NOT expose a `Project` field — the projection method lives on the Cmd, just like every other application-layer boundary on the use case. The **Request DTO does NOT receive ctx** — `Request.ToCommand()` is a pure body mapper. This keeps web ctx-free (transport-only) and concentrates identity-derived translation in application code (the Cmd) where the layer rules allow consuming AppContext.

The same `ApplyTo(ctx, t)` + `FromEntity(ctx, t)` pair exists on all 6 Auto Command interfaces — including Archive/Unarchive/Delete (which have no body). On the state-transition verbs ApplyTo is the hook for "consume ctx + populate a runtime authz field"; FromEntity typically returns `fwresults.None{}` for the "no data on the wire" shape. A Command that doesn't need ctx just ignores both parameters.

**Where Result and Response live.**

- Co-located in `application/commands/xxx_user.go`: `XxxUserResult` struct (Go-pure, no JSON tags, **pure data — no methods**) + the same file declares `Cmd.FromEntity(ctx, T) Result` as method on the Cmd.
- Co-located in `web/requests/xxx_user_request.go`: `XxxUserResponse` struct (with JSON tags) + `func (XxxUserResponse) FromResult(XxxUserResult) XxxUserResponse` — symmetric inverse of `ToCommand`.
- Endpoint with no projection (typical Archive/Unarchive/Delete): declare `TResult = fwresults.None` and write a one-line `FromEntity` returning `fwresults.None{}`; pair with `fwresponses.NoBody` at the wire wrapper. The runtime detects `responses.None` and emits the success envelope WITHOUT a "data" field (matches "204 No Content"-style shape).

The contract for Result and Response lives entirely under DDD layer rules: `Cmd.FromEntity` lives in application (app → domain ✓); `Response.FromResult` lives in web (web → application ✓). The domain never sees JSON tags; the application never sees the wire shape.

**Uniform pointer Cmd pattern**: the handler's second type param is always `*Cmd` (not `Cmd` by value). Reason: `SetPathID` needs a pointer receiver to mutate; keeping uniformity between Insert and the rest simplifies the mental model. `ToEntity`/`ApplyTo`/`ApplyPartiallyTo` may have value OR pointer receiver — Go promotes value-receiver to `*Cmd`.

**PUT vs PATCH is HTTP rule, not convention**. PUT replaces the entire resource → all fields required; PATCH partially updates → each field optional. The framework enforces via type: `UpdateCommandHandler` (PUT) vs `PartialUpdateCommandHandler` (PATCH) are distinct handlers consuming distinct Commands (`UpdateCommand[T, TResult]` vs `PartialUpdateCommand[T, TResult]`). Common footgun in the old "Cmd decides" design: lenient PUT silently overwrote a missing field with zero-value.

### Route wrappers

Endpoints with body use `HandleCommandWithBody{,ID}` (consume `RequestDTO`); endpoints without body use `HandleCommandWithID`. The whole family starts with `HandleCommand` — suffixes communicate what the endpoint accepts (body, ID, both). There is no bare `HandleCommand` (base "no body, no ID") because there is no use case today.

| Wrapper | Body? | Path ID? | Who implements |
|---|---|---|---|
| `fwweb.HandleCommandWithBody(pipe, sample, responseProjection, h, status)` | yes | no | POST with body (Insert) |
| `fwweb.HandleCommandWithBodyID(pipe, sample, responseProjection, h, status)` | yes | yes | PUT / PATCH with body (Update / Partial) |
| `fwweb.HandleCommandWithID(pipe, responseProjection, h, status)` | **no** | yes | Archive / Unarchive / Delete |

**HandleCommandWithBody / HandleCommandWithBodyID** allocate the `TReq` (wire DTO in `web/requests/`), validate the payload schema, call `req.ToCommand()` on the web→application boundary, and dispatch the Command via Pipeline:

1. Allocate `var req TReq`
2. **Strict path** (handler embeds `pipeline.FullBody`): empty body, missing required field OR malformed JSON → **400** (not 422) with `RequiredFieldNotification`/`SchemaViolationNotification` carrying `SemanticSchema`, `context: "Schema"`.
3. **`c.Bind().Body(&req)`** populates `req`. Type mismatch (wrong-typed field) → 400 with `SchemaViolationNotification` carrying the field's JSON path (e.g. `"addresses[0].zipCode"`).
4. `cmd := req.ToCommand()` produces the Command (in `application/commands/`).
5. `cmd.SetPathID(c.Params("id"))` on the WithID variant.
6. `Dispatch(pipe, AppContext(c), cmd, h)` → domain validates → 422 with notifications when applicable.
7. On Success: `responseProjection(result.Value())` maps `TResult` → wire `TResp`; if the resulting wire shape is `responses.None`, the envelope is emitted with no `data` field; otherwise the success envelope carries `data: wire`. On Failure/Exception: `RespondFromResult` honors each notification's Semantic.

**HandleCommandWithID** (reduced to no-body): just `new(T)` + `SetPathID` + `Dispatch` + response projection (same `respondWithProjection` helper as the body wrappers). Does not call `Bind().Body()`. Does not inspect FullBody. Used by Archive/Unarchive/Delete.

**Sample TReq + `responseProjection` function value** anchor the generic inference — the consumer passes `requests.InsertUserRequest{}` and `requests.InsertUserResponse{}.FromResult` (a method value with signature `func(TResult) TResp`); Go infers TReq + TCmd + TCmdPtr + TResult + TResp from the sample, the projection function, and the handler. For endpoints with no projection, pass the framework's `fwresponses.NoBody` (a `func(fwresults.None) fwresponses.None`).

### HTTP error semantics

| Scenario | Status | Notification | Context |
|---|---|---|---|
| Malformed JSON body | **400** | `SchemaViolationNotification` (semantic Schema) | `"Schema"` |
| Wrong-typed field (`"age": "twenty"` when int) | **400** | `SchemaViolationNotification` carrying `field` | `"Schema"` |
| Missing required field (FullBody marker) | **400** | `RequiredFieldNotification{}.WithSemantic(SemanticSchema)` | `"Schema"` |
| Domain rejects values (`BuildRules` notification Validation) | **422** | business rules | varies (e.g. `"User"`) |
| Resource does not exist (`RecordNotFoundNotification`) | **404** | `RecordNotFoundNotification` | varies |

`RequiredFieldNotification` is the same struct in both worlds — the distinction is the carried semantic. Wire emits with `WithSemantic(SemanticSchema)` → 400; domain emits default → 422.

### Strict body check via marker `pipeline.FullBody`

Handlers that need to require all fields present in the JSON body embed `pipeline.FullBody`. The wrappers `HandleCommandWithBody{,ID}` do a type-assertion to `pipeline.FullBodyEnforcer` at construction (closure capture). Reflection to list expected fields runs on the **Request** type (`TReq`), not on the Command.

- **Handler implements FullBodyEnforcer (strict)** — reflect on `*TReq` lists exported fields (skips anonymous embedded; skips `json:"-"`). Per request: parse the body into `map[string]json.RawMessage` and check that ALL expected keys are present. Missing body OR missing field → **400** with 1 `RequiredFieldNotification` per field (semantic Schema). Malformed JSON → 400 with `SchemaViolationNotification`.
- **Handler does NOT implement (lenient)** — missing body is treated as `{}` (Request zero-value, normal dispatch); partial body is OK; invalid JSON body → 400 with `SchemaViolationNotification`.

Closed rule: ALL exported fields of the Request (non-anonymous, non `json:"-"`) are mandatory when the marker is present. **No per-field opt-out.** Flexibility is via PATCH (handler without marker).

Reflection of the expected set runs once on wrapper construction and is cached by `reflect.Type` in a module-level `sync.Map` — per-request overhead is just `json.Unmarshal` of the body into a map + diff of sets.

Today implementing the marker: `UpdateCommandHandler`. Not implementing: `PartialUpdateCommandHandler`, `ArchiveCommandHandler`, `UnarchiveCommandHandler`, `DeleteCommandHandler`, `InsertCommandHandler`. Insert was left without the marker because POST accepts optional fields by design.

### End-to-end example

```go
// 1. Command + Result co-located — application/commands/insert_user.go
package commands

// Input
type InsertUserCommand struct {
    pipeline.CommandBase
    Name  string
    Email string
    Phone *string
}
// ToEntity receives *AppContext — Command is the only layer that translates
// ctx into business-named entity fields (e.g., OwnerUserID from JWT subject).
// Domain entity sees only business fields.
func (c InsertUserCommand) ToEntity(_ *configuration.AppContext) *User {
    return &User{Name: c.Name, Email: c.Email, Phone: c.Phone}
}

// Output method ON THE CMD (symmetric with ToEntity on the input side).
// Receives ctx — same boundary, same translation point for identity-aware
// projections. Result struct lives in the same file as pure data.
func (c InsertUserCommand) FromEntity(_ *configuration.AppContext, u *User) InsertUserResult {
    return InsertUserResult{ID: *u.GetID(), Name: u.Name, Email: u.Email, Phone: u.Phone}
}

// Optional in-TX side effect — declared on the Cmd via the
// persistence.BeforeCommitHookProvider[*User] convention. The Auto handler
// detects the BeforeCommit method via type assertion at the top of Handle
// and threads it as a WriteOption[*User] to the Repo.Insert call. Fires
// INSIDE the framework's TX between the data writes and COMMIT; a non-nil
// error rolls everything back.
//
// TxHandle is a sealed marker — the Cmd cannot pronounce SQL through it.
// The canonical shape is a port declared in application/ (or domain/)
// whose method receives a persistence.TxHandle; the port's adapter in
// infra/ calls fwinfra.UnwrapPgxTx(tx) to recover the pgx.Tx and owns
// the SQL + table name. The hook is just the composition point.
func (c InsertUserCommand) BeforeCommit(
    ctx *configuration.AppContext, u *User, id domain.ID, tx persistence.TxHandle,
) error {
    // Example: c.NotificationOutbox is a port injected on the Cmd; its
    // adapter in infra/ owns the SQL emitting the companion integration
    // event atomically with the framework's data + outbox + audit rows.
    return c.NotificationOutbox.EnqueueActivationRequested(ctx, tx, id)
}

// Recommended compile-time safety to catch typos in the method name.
var _ persistence.BeforeCommitHookProvider[*User] = (*InsertUserCommand)(nil)

// Output shape — pure data, no methods.
type InsertUserResult struct {
    ID    domain.ID
    Name  string
    Email string
    Phone *string
}

// 2. Request + Response co-located — web/requests/insert_user_request.go
package requests

// Input (wire format — JSON tags)
type InsertUserRequest struct {
    Name  string  `json:"name"`
    Email string  `json:"email"`
    Phone *string `json:"phone,omitempty"`
}
// ToCommand is body-only — no ctx parameter. Web layer stays free of
// identity interpretation; that belongs to the application layer.
func (r InsertUserRequest) ToCommand() *commands.InsertUserCommand {
    return &commands.InsertUserCommand{Name: r.Name, Email: r.Email, Phone: r.Phone}
}

// Output (wire format — JSON tags)
type InsertUserResponse struct {
    ID    domain.ID `json:"id"`
    Name  string    `json:"name"`
    Email string    `json:"email"`
    Phone *string   `json:"phone,omitempty"`
}
func (InsertUserResponse) FromResult(r commands.InsertUserResult) InsertUserResponse {
    return InsertUserResponse{ID: r.ID, Name: r.Name, Email: r.Email, Phone: r.Phone}
}

// 3. Route — sample Request + responseProjection function value anchor generic inference
users.Post("/", fwweb.HandleCommandWithBody(d.Pipeline,
    requests.InsertUserRequest{},
    requests.InsertUserResponse{}.FromResult,
    &handlers.InsertCommandHandler[*User, *InsertUserCommand, commands.InsertUserResult]{
        Repo: userRepo,
    },
    fiber.StatusCreated))
```

**Request ≡ Command shape ruler:** required field → `string` on both. Optional field → `*string` on both. `ToCommand()` is 1:1 assignment with no normalization. Domain rejects invalid values via `BuildRules` (notification semantic Validation → 422); wrapper rejects schema violations before dispatch (semantic Schema → 400).

**Result ≡ Response shape ruler:** mirror of the input ruler — each field on the Response maps 1:1 to a field on the Result. `FromResult()` is pure assignment with no JSON-aware decision. Result is what the use case wants to expose; Response is the wire-format projection of the same data.

For Update (PUT) / Partial Update (PATCH) / Archive / Delete / Unarchive: each one with its Request + Command + Result + Response (the last two optional — use `fwresults.None`/`fwresponses.NoBody` as defaults when nothing needs to be exposed). Update strict via `pipeline.FullBody` on the handler; PATCH lenient. Archive/Unarchive/Delete without body → `HandleCommandWithID`.

### UnarchiveCommandHandler asymmetry

`UnarchiveCommandHandler` does NOT call `FindByID` (record is archived, `Repository.FindByID` filters `WHERE deleted_at IS NULL`). Instead it obtains an empty sample via `Repo.New()`, sets the path ID, and passes it to `GetUnarchivable`. Cascade of archived children is via direct SQL in `aggregate_persister.unarchiveAggregate`.

The asymmetry is **internal to the handler** — the wire-up in `routes.go` is identical to the other Auto handlers. `Repo.New()` is a method of the `domain.Reader[T]` contract (carried by `persistence.ScopedRepository[T]`) that every implementation exposes; anyone using `BaseRepository[T]` injects a single `NewEntity func() T` in the constructor and `New()` comes promoted via embed.

```go
&handlers.UnarchiveCommandHandler[*User, *UnarchiveUserCommand, fwresults.None]{
    Repo: userRepo,
}
// UnarchiveUserCommand declares the bodyless verb shape:
//   func (*UnarchiveUserCommand) ApplyTo(_, _) {}
//   func (*UnarchiveUserCommand) FromEntity(_, _) fwresults.None { return fwresults.None{} }
```

### Manual path (cross-service, side effects)

A handler with external IO, an injected domain service, or complex orchestration: continues to be a struct that manually implements `pipeline.Handler[*Cmd, TResult]`. `TResult` is whatever application-layer value the handler returns; the route's `responseProjection` then maps it to the wire `TResp`. It registers on the route with the same `HandleCommand`/`HandleCommandWithID`. Auto and manual coexist in the same API.

**In-TX side effects on the manual path** — the handler reaches the same persister surface as the Auto path. Pass `persistence.WithAfterBegin[T](fn)` / `persistence.WithBeforeCommit[T](fn)` as options on the `repo.Scope(ctx, opts...).Method(valid)` write binding. The closures fire at positions A and D of the TX, with the same `ctx / t / id / tx` payload the Auto path's `AfterBegin` / `BeforeCommit` receive.

**Application stays SQL-free on the manual path too** — `persistence.TxHandle` is a sealed marker with no public methods, so the closure cannot pronounce SQL through it by construction. The canonical shape is a port declared in `application/` (or `domain/`) whose method receives a `persistence.TxHandle`; the port's adapter in `infra/` calls `fwinfra.UnwrapPgxTx(tx)` to recover the live `pgx.Tx` and owns the SQL + table name. The closure threads `tx` through the port so the side effect remains atomic with the framework's writes.

```go
func (h *CreateUserAdminHandler) Handle(ctx *configuration.AppContext, cmd *AdminCreateUserCommand) (Result, error) {
    user := buildUser(cmd)
    insertable, err := domain.GetInsertable(user, h.svc, "AdminCreate")
    if err != nil { return Result{}, err }

    id, err := h.repo.Scope(ctx,
        persistence.WithBeforeCommit[*User](func(
            ctx *configuration.AppContext, u *User, id domain.ID, tx persistence.TxHandle,
        ) error {
            // h.NotificationOutbox is a port injected on the handler; its
            // adapter in infra/ owns the SQL emitting the companion
            // integration event atomically with the framework's data +
            // outbox + audit rows. The adapter calls fwinfra.UnwrapPgxTx(tx)
            // to recover the pgx.Tx; the handler never imports pgx.
            return h.NotificationOutbox.EnqueueAdminUserActivated(ctx, tx, id)
        }),
    ).Insert(insertable)
    if err != nil { return Result{}, err }
    return Result{ID: id}, nil
}
```

The closure runs INSIDE the framework's TX between the data writes and COMMIT; a non-nil error rolls everything back. NotificationCarrier identity is preserved end-to-end. The `TxHandle` is an opaque token at the application layer — the only way to obtain the underlying `pgx.Tx` is via `fwinfra.UnwrapPgxTx`, an infra-layer helper not reachable from `application/`.

## Read-side wrappers (Auto Query Handlers)

Symmetric to `HandleCommandWith{Body,BodyID,ID}` on the write side. Every GET route declares **input** via Request DTO with `query:"..."`/`filter:"..."` tags (allowlist) AND **output** via a mandatory projector `func(map[string]any) R`. The wrapper enforces both at the wire boundary — application layer stays Fiber-agnostic.

### Canonical wrappers

| Wrapper | Path ID? | Allowlist | What it does |
|---|---|---|---|
| `fwweb.HandleQueryWithParams[TReq, TQ, R]` | no | reflection on TReq's `query:"X" filter:"ops"` tags (cached by `reflect.Type`); unknown key or operator outside the declared list → 400 with `SchemaViolationNotification` | parses query → `ReadCriteria` → `req.ToQuery(criteria)` (no ctx — web boundary is dumb mapping) → Dispatch → handler calls `q.ToCriteria(ctx)` (where JWT overlays land) → on success projects each `page.Items` doc via the projector + emits envelope with `Data: []R` + top-level `Pagination` |
| `fwweb.HandleQueryWithID[TReq, TQ, R]` | yes | only `?includeArchived` recognized; any other query key → 400 | parses optional `?includeArchived` → `req.ToQuery()` (no ctx) → `SetPathID(c.Params("id"))` → Dispatch → handler calls `q.ToCriteria(ctx)` and forwards to `Reader.ReadByID` → on success projects `result.Value()` via the projector + emits success envelope |

```go
users.Get("/", fwweb.HandleQueryWithParams(d.Pipeline,
    requests.FindUsersByParamsRequest{},
    fwresponses.AutoFromDoc[requests.FindUsersByParamsResponse],
    &handlers.FindByParamsQueryHandler[*queries.FindUserByParamsQuery]{
        Reader: d.ViewReader, View: view.Name(),
    }))

users.Get("/:id", fwweb.HandleQueryWithID(d.Pipeline,
    requests.FindUserByIDRequest{},
    fwresponses.AutoFromDoc[requests.FindUserByIDResponse],
    &handlers.FindByIDQueryHandler[*queries.FindUserByIDQuery]{
        Reader: d.ViewReader, View: view.Name(),
    }))
```

**Nested embed groups.** A struct-typed field on the Request DTO carrying `query:"prefix"` (and no `filter:` tag) is an embed group — the walker recurses, and every leaf below it produces a wire key prefixed by `prefix.`. Mirrors the Response side, where nested structs project nested doc paths via `AutoFromDoc`:

```go
type FindUsersByParamsRequest struct {
    Name      *string             `query:"name"  filter:"eq,startswith"`
    Email     *string             `query:"email" filter:"eq,in"`
    Addresses AddressFilterParams `query:"addresses"`

    Limit *int64 `query:"limit"`
}

type AddressFilterParams struct {
    City    *string `query:"city"    filter:"eq,istartswith"`
    ZipCode *string `query:"zipCode" filter:"eq,startswith"` // Go path Addresses.ZipCode → column zip_code via the view TableSchema
}
```

Wire: `?addresses.city=Berlin`, `?addresses.zipCode.startswith=10001`. Each leaf's wire name resolves to a **Go field path** (`Addresses.ZipCode`) via the Request DTO's `query:` tags; the `MongoViewReader` then translates that Go path to the physical Mongo column path using the view's `TableSchema`, so `Addresses.ZipCode` becomes the doc path `addresses.zip_code` because `AddressSchema().Field("ZipCode","zip_code")` says so — no `view:` tag, no `PascalToSnake` at this layer. Reserved pagination keys (`limit`, `after`, etc.) are honored only at the top level — embed groups carry filter leaves, not pagination controls. Pointer-to-struct (`*AddressFilterParams`) recurses identically.

**Filter operators.** A field declared `query:"X" filter:"ops"` accepts the operators listed (comma-separated). The wire shape is `?X.<op>=value` (no suffix for `eq`); operators outside the declared set are rejected with 400 `SchemaViolationNotification`. Exhaustive list:

| Operator | Semantic | Wire example | Mongo emission |
|---|---|---|---|
| `eq` | exact equality (default — no suffix) | `?name=Bob` | `{name: "Bob"}` |
| `ne` | inequality | `?name.ne=Bob` | `{name: {$ne: "Bob"}}` |
| `in` | value in list | `?email.in=a@x,b@y` | `{email: {$in: ["a@x","b@y"]}}` |
| `nin` | value not in list | `?email.nin=a@x,b@y` | `{email: {$nin: [...]}}` |
| `gte`, `lte`, `gt`, `lt` | ordinal comparison | `?age.gte=18` | `{age: {$gte: 18}}` |
| `startswith` | prefix match (case-sensitive) | `?name.startswith=Bob` matches "Bob Diego" | `{name: {$regex: "^Bob"}}` — value is regexp.QuoteMeta-escaped |
| `contains` | substring match (case-sensitive) | `?name.contains=ob` matches "Bob", "Roberto" | `{name: {$regex: "ob"}}` |
| `ieq` | case-insensitive equality | `?name.ieq=bob` matches "Bob" | `{name: {$regex: "^bob$", $options: "i"}}` |
| `ine` | case-insensitive inequality | `?name.ine=bob` rejects "Bob" | `{name: {$not: {$regex: "^bob$", $options: "i"}}}` |
| `iin` | case-insensitive in-list | `?name.iin=bob,alice` matches "Bob", "ALICE" | `queries.RegexMatchList{...}` sentinel → MongoViewReader expands into `{$in: [bson.Regex, ...]}` |
| `inin` | case-insensitive not-in-list | `?name.inin=bob,alice` rejects "Bob", "ALICE" | sentinel → `{$nin: [bson.Regex, ...]}` |
| `istartswith` | case-insensitive prefix | `?name.istartswith=bob` matches "Bob Diego" | `{name: {$regex: "^bob", $options: "i"}}` |
| `icontains` | case-insensitive substring | `?name.icontains=OB` matches "Bob", "Roberto" | `{name: {$regex: "OB", $options: "i"}}` |

The constants live in `web/handle_query.go` (`fwweb.OpEq`, `OpStartsWith`, `OpIContains`, …) and are referenced by both `applyFilterParam` (runtime) and the OpenAPI generator (each operator surfaces as its own `name.<op>` query parameter). User-supplied values feeding the regex operators are escaped via `regexp.QuoteMeta` so wire metacharacters are treated as literals — a query `?name.contains=a.b*c` matches the literal string `a.b*c`, never the regex pattern.

The numeric operators (`gte`/`lte`/`gt`/`lt`) have no `i` variant — case-folding has no meaning on ordinal comparisons.

**Multiple operators on the same field are AND-ed.** When the wire delivers more than one operator targeting the same field (e.g. `?age.gte=18&age.lte=65` or `?name.startswith=Bob&name.icontains=ob`), the wrapper folds the clauses into a `queries.MultiClause` sentinel on `ReadCriteria.Filter[field]` instead of overwriting the previous entry on the map. The canonical MongoViewReader expands the sentinel into a top-level `$and` array — each clause becomes a `{field: translatedValue}` entry so every declared operator is honored simultaneously. A single operator on a field stays as the plain `{field: value}` shape so Mongo indexes remain usable without an outer `$and`.

**Sparse responses via `?fields=`.** Opt-in field projection — the consumer picks a subset of the Response shape and the framework strips everything else end to end. The Request DTO opts in by declaring `Fields *string query:"fields"` (no `filter:` tag — it is a reserved control key, not a filter leaf). When the parameter is declared AND the wrapper's Response type R is a struct, three pieces fire automatically:

1. **Boot guard.** `HandleQueryWithParams` (and its `Spec` sibling) walks R recursively at construction. Every exported field — at every depth, including struct fields inside slices — must be either `*T` (pointer to scalar/struct) or a slice/map AND must carry `,omitempty` in its `json` tag. Violations are accumulated and surfaced as a boot panic naming every offending path (`FindXxxResponse.addresses.zipCode: missing ,omitempty in json tag`). Fail loud at construction — the contract cannot ship.

2. **Allowlist + wire→Go→column translation.** Each comma-separated token in `?fields=` is validated against R's declared wire paths (the Response's `json:` tags) and translated to the **Go field path**; the `MongoViewReader` then maps the Go path to the physical Mongo column path via the view's `TableSchema`. Unknown tokens emit 400 with `SchemaViolationNotification` on field `fields[<bad>]`. Nested paths walk segment-by-segment, so `?fields=addresses.zipCode` projects to Mongo as `{"addresses.zip_code":1}` — the `zip_code` comes from `AddressSchema().Field("ZipCode","zip_code")`, not from `PascalToSnake` — and `?fields=addresses` projects the whole nested subtree.

3. **Auto-exclusion of `_id`.** Mongo always returns `_id` unless explicitly excluded (the only mixed-mode projection it permits — exclusion of `_id` alongside inclusion of others). When the wire `fields` list does not include `id`, the framework adds `_id: 0` to the projection so the typed Response stays clean. When `id` is among the tokens, no `_id: 0` is added — Mongo returns both `id` and `_id`, and `AutoFromDoc`'s existing `id ← _id` fallback resolves the wire output identically across both cases.

| Wire form | Mongo projection emitted | Notes |
|---|---|---|
| `?fields=name,email` | `{name:1, email:1, _id:0}` | `id` not requested → `_id:0` added |
| `?fields=id,name` | `{id:1, name:1}` | `id` requested → no `_id:0`; Mongo auto-includes `_id` |
| `?fields=addresses` | `{addresses:1, _id:0}` | Whole subtree |
| `?fields=addresses.zipCode` | `{"addresses.zip_code":1, _id:0}` | `zip_code` from the view `TableSchema` (`Field("ZipCode","zip_code")`) |
| `?fields=bogus` | (rejected) | 400 `SchemaViolationNotification{field:"fields[bogus]"}` |

**Why pointer + omitempty.** Mongo strips the unwanted columns, the projector populates only what it finds, the remaining Go fields stay at their zero value. Pointer + `omitempty` lets `encoding/json` elide absent fields; without it, the zero value still renders (`"name":""`, `"addresses":[]`), defeating the point of `?fields=`. The boot guard is the cheapest way to make the wire shape coherent with what the consumer asked for. Empty slices are elided too (`omitempty` honors `len==0`) — a `?fields=name` request returns `{"name":"…"}` without an empty `"addresses":[]`.

**When the guard is skipped.** A Request DTO without `query:"fields"` imposes no constraint on its Response. `HandleQueryWithParams` paired with a projector returning `map[string]any` (e.g. `fwresponses.RawDoc`) bypasses the guard — there is no typed Response shape to enforce. Manual handlers via `fwweb.ParseCriteria` operate in pass-through mode: each `?fields=` token becomes an inclusion entry verbatim, no allowlist, no translation, no `_id: 0` auto-exclusion. The canonical surface is `HandleQueryWithParams` with a typed Response.

**Sortable response paths via `?sort=`.** Symmetric to `?fields=`: opt-in allowlist over the Response DTO's wire paths, applied to the ordering of results. The Request DTO opts in by declaring `Sort *string query:"sort"` (no `filter:` tag — reserved control key). When the parameter is declared AND the wrapper's Response type R is a struct, two pieces fire automatically:

1. **Allowlist + wire→Go translation.** Each comma-separated token is stripped of an optional `-` prefix (descending; bare token = ascending), validated against R's declared wire paths (the same `projectionSchema` `?fields=` consumes), and resolved to the **Go field path**. Unknown tokens emit 400 with `SchemaViolationNotification` on field `sort[<token>]` — the rejected token is surfaced verbatim including any `-` prefix. The resulting `SortField{Field: goPath, Desc: bool}` entries land on `ReadCriteria.Sort` carrying the Go path; `MongoViewReader` translates each Go path to its physical column via the view's `TableSchema` and emits the matching `bson.D` on `findOpts.SetSort`.

2. **Boot warning.** The wrapper has no way to verify at construction time that the Mongo view declares indexes covering the sortable paths — the `ViewDefinition` lives separately in `ReadableFeature.Views()`. When sort opt-in is detected and a typed Response is in scope, the framework emits a single `slog.Warn("query.sort.opt-in: …", "request", "<TReq>", "sortable_wire_paths", [...])` listing every sortable path so the operator can compare it against the view's `.Indexes(…)` (`fwinfra.Index` / `fwinfra.Compound`) during the same boot. No enforcement, no boot panic — pure operator-facing advisory, because the framework cannot know which index shape (single, compound ESR, partial) covers a given workload.

| Wire form | SortField emitted | Notes |
|---|---|---|
| `?sort=name` | `{Field:"name", Desc:false}` | Bare token → ascending |
| `?sort=-name` | `{Field:"name", Desc:true}` | `-` prefix → descending |
| `?sort=addresses.zipCode` | `{Field:"Addresses.ZipCode", Desc:false}` | reader → column `addresses.zip_code` via the view `TableSchema` |
| `?sort=addresses.state` | `{Field:"Addresses.State", Desc:false}` | reader → column `addresses.st` via `AddressSchema().Field("State","st")` |
| `?sort=name,-email` | 2 entries, independent directions | Multi-key sort applied in declaration order |
| `?sort=bogus` | (rejected) | 400 `SchemaViolationNotification{field:"sort[bogus]"}` |
| `?sort=-bogus` | (rejected) | 400 `SchemaViolationNotification{field:"sort[-bogus]"}` — `-` preserved |

**No boot guard for sort.** Unlike `?fields=`, there is no structural constraint on the Response: `?sort=` consumes only the wire→doc path map. When `?fields=` is also opt-in on the same Request DTO, the existing `pointer + omitempty` guard still fires — it is the `fields`-side rule, untouched by `sort`. RawDoc Responses and `ParseCriteria` callers get pass-through: tokens land verbatim, no allowlist, no warning.

**Keyset pagination via `?after=` / `?before=` + per-view `?limit=` ceiling.** The read side is **keyset-paginated over `(sort_value..., _id)`** — the cursor encodes the value tuple of the last (or first) returned doc and the reader walks forward / backward from that boundary. The reader **always** appends `_id` (ASC) as the last sort tiebreaker so a stray natural-storage-order Mongo query cannot drift the result set across pages; consumer-declared `?sort=` fields layer in front of the `_id` slot.

| Wire | Cursor tuple | Reader behavior |
|---|---|---|
| `?limit=N` only | n/a | First page; `_id` ASC; emits `next_cursor` when `+1 trick` proves there's more |
| `?after=<cursor>` | `(_id,)` (no `?sort=`) OR `(sort_values..., _id)` | Forward keyset: `$or` cascade with `$gt` (asc) / `$lt` (desc) per slot |
| `?before=<cursor>` | same shape as the cursor it was emitted as | Backward keyset: same cascade with inverted operators; query runs Mongo in inverted sort order; reader reverses the slice in Go so the caller observes canonical sort |
| `?sort=name&after=<cursor with 1-elem tuple>` | mismatched length | **400** `SchemaViolationNotification{field:"after"}` — consumer changed `?sort=` mid-navigation; request page 1 of the new sort first |
| `?name.startswith=B&after=<cursor issued without filter>` | context hash differs | **400** `SchemaViolationNotification{field:"after"}` — consumer changed filter mid-navigation |
| `?sort=-name&after=<cursor with asc sort>` | context hash differs | **400** — consumer flipped sort direction mid-navigation |
| `?search=newterm&after=<cursor without search>` | context hash differs | **400** — consumer changed `?search=` mid-navigation |
| `?includeArchived=true&after=<cursor with archived excluded>` | context hash differs | **400** — consumer flipped archived gate mid-navigation |
| `?after=<c>&before=<c>` | n/a | **400** `SchemaViolationNotification{field:"after,before"}` — mutually exclusive |
| `?after=not-base64` / corrupt JSON / `v != 1` | n/a | **400** `SchemaViolationNotification{field:"after"}` (`before` mirrors) |
| `?limit=abc` / `0` / `-5` | n/a | **400** `SchemaViolationNotification{field:"limit"}` |
| `?limit=N` where `N > resolvedMax` | n/a | **400** `LimitExceededNotification{field:"limit", value:"<resolvedMax>"}` — `Semantic=Schema → 400`, translatable per language |

**Cursor format.** Opaque to consumers — `base64(URLEncoding)` of `{"v":1, "k":[<values...>, "<_id>"], "h":"<context_sha256>"}`. URL-safe so `+` / `/` are never produced. The `v` field carries the schema version (currently `1`); a future bump rejects older cursors with the same `SchemaViolationNotification` envelope. The `h` field is the canonical SHA-256 of the issuing call's **full listing context** (`queries.HashContext`) — every axis that shapes the result set the cursor walks: `Filter` + `Sort` (field + Desc per entry, declaration order matters) + `Search` + `IncludeArchived`. Empty (omitted on the wire) when the issuing call carried the canonical default context (no filter, no sort, no search, archived excluded). The reader emits `NextCursor` and `PrevCursor` on every non-terminal page in both forward and backward navigation, so a UI can drive both "next" and "previous" buttons symmetrically.

**Cursors are scoped to the full listing context.** A cursor only navigates correctly inside the result set that issued it. The wrapper validates `len(cursor.K)-1 == len(criteria.Sort)` (structural pre-check protecting the reader's keyset builder) AND `cursor.H == HashContext(filter, sort, search, includeArchived)` (covers every listing axis) after parsing the query string. Any mismatch — sort field added/removed/reordered, sort direction flipped, filter value changed, `?search=` value changed, `?includeArchived=` flipped — is rejected with 400 `SchemaViolationNotification` on the cursor's wire key (`after` or `before`). Stateless by design: there is no server-side session, no per-cursor TTL, no token store. The whole "is this cursor still valid?" check runs in the wrapper against the wire request alone. Cursors persist arbitrarily — across requests, across browser tabs, across user devices — as long as the deploy's schema version and the full listing context match.

The strict check exists to convert silent consumer-side bugs (frontend changes one knob, server returns wrong/empty results, user blames the data) into loud, explicit failures with a clear remediation: "request page 1 of the new context".

**Stable sort discipline.** Every Mongo `Find` consulting the reader carries `SetSort` with `_id ASC` appended as the last key (`_id DESC` under the inverted backward path). This guarantees that:

- The `+1` trick (`SetLimit(limit+1)` and detect overflow) is meaningful — the result set is deterministic across calls even when the underlying collection is being written to concurrently.
- Consumers declaring a custom `?sort=` field that contains ties (e.g. two docs with the same `name`) see them paginated correctly: the `_id` tiebreaker disambiguates them under the same cascade.
- Cursors emitted from one page apply correctly on the next.

**Per-view `?limit=` ceiling — single cascade, three levels.** Every paged GET is bounded by a resolved-at-read-time ceiling computed in this order:

1. `ViewDefinition.MaxLimit(N)` — declarative per-view override (`fwinfra.View("users").MaxLimit(500)`). Applies uniformly to every endpoint consulting the view, regardless of how many handlers point at it — the cap describes the cost of reading this specific dataset, not of any single endpoint.
2. `cfg.Query.MaxLimit` — yaml-supplied service-wide default (`microservice.<profile>.yaml: query: { maxLimit: 200 }`).
3. Framework constant `100` — conservative fallback when neither is declared.

The resolved ceiling is **always > 0**. When the consumer sends no `?limit=`, the reader returns up to that ceiling (no separate hardcoded page-size default). When the consumer sends `?limit=N` and `N <= resolvedMax`, that value wins. When `N > resolvedMax`, the reader returns 400 with `LimitExceededNotification` carrying the effective ceiling as `FieldValue` so the consumer can show "max is X" without parsing the translated message.

`MaxLimit(N)` does NOT participate in `RebuildHash` / `ArtifactHash` — the cap is operational state, not projection shape. Bumping it neither triggers a Mongo rebuild nor requires a `Version(N)` bump.

```go
// Per-view override — applies to every endpoint reading "users":
fwinfra.View("users").
    Version(1).
    Root("users").
    Schema(UserSchema()).
    EmbedMany("addresses", fwinfra.FromSchema(AddressSchema())).
    MaxLimit(500)
```

```yaml
# microservice.<profile>.yaml — service-wide override (consumed by every view
# that does NOT declare its own MaxLimit):
query:
  maxLimit: 200
```

**Compound cursor + `?fields=` interaction.** When the consumer requests `?fields=name` AND `?sort=created_at`, the doc would otherwise not carry `created_at` (projected away) and the reader could not assemble the cursor. The reader transparently re-includes every active sort field path in the Mongo projection, then strips them from the returned doc after the cursor is built. The wire shape stays exactly as the consumer requested via `?fields=`.

**Count-only mode via `?onlyTotal=true`.** Boolean reserved control key opt-in by `OnlyTotal *bool query:"onlyTotal"` on the Request DTO. When `true`, the reader short-circuits to `CountDocuments(filter)` against the assembled `Filter` (identity overlays + archived gate + `?search=` still applied) and `RespondPaged` flips the envelope to the count-only shape: `Data` is omitted entirely and `Pagination` is a `*TotalOnlyPagination` carrying solely `Total` — no `has_next`/`has_prev`/cursors zero-value noise. The use case is a frontend computing pagination totals or "matches" badges without paying for the document fetch.

`onlyTotal=true` is incompatible with the listing-only reserved controls (`fields`, `sort`, `limit`, `after`, `before`); combining any of them is a consumer-side bug and rejects at the schema layer with 400 `SchemaViolationNotification{field: "onlyTotal[<conflict>]"}` — strict rejection rather than silent ignore so the bug surfaces immediately. Filter leaves (declared via `filter:"ops"`) plus `?search=` and `?includeArchived=` stay valid — counting a filtered subset is the canonical use case. Manual handlers via `fwweb.ParseCriteria` get the same treatment: the flag flows into `ReadCriteria.OnlyTotal`, conflicts are rejected with the same field shape, and the downstream branch on `queries.Page.OnlyTotal` is what `RespondPaged` consumes to pick the envelope shape.

`Response.Pagination` is typed `any` precisely because the slot carries two legitimate Go shapes: `*PaginationInfo` on listing requests and `*TotalOnlyPagination` on count-only requests. Both are typed structs — no untyped map on the wire.

```go
// Request DTO opts in.
type FindUsersByParamsRequest struct {
    Name   *string `query:"name"  filter:"eq"`
    Email  *string `query:"email" filter:"eq,in"`

    Limit     *int64  `query:"limit"`
    Fields    *string `query:"fields"`     // declares the parameter
    Sort      *string `query:"sort"`       // opts into the sort allowlist + boot warning
    OnlyTotal *bool   `query:"onlyTotal"`  // opts into count-only mode + conflict matrix
}

// Response satisfies the contract — every field at every depth is *T (or a slice)
// and carries ,omitempty.
type FindUsersByParamsResponse struct {
    ID        *string                          `json:"id,omitempty"`
    Name      *string                          `json:"name,omitempty"`
    Email     *string                          `json:"email,omitempty"`
    Phone     *string                          `json:"phone,omitempty"`
    Addresses []FindUsersByParamsAddressOutput `json:"addresses,omitempty"`
}

type FindUsersByParamsAddressOutput struct {
    ID      *string `json:"id,omitempty"`
    City    *string `json:"city,omitempty"`
    ZipCode *string `json:"zipCode,omitempty"` // Go field ZipCode; reader already translated column zip_code → ZipCode via the view TableSchema
}
```

**Projector contract.** `func(map[string]any) R` — same signature in both wrappers. `R` is whatever wire shape the consumer wants. Three canonical projectors ship with the framework:

| Projector | Use when |
|---|---|
| `fwresponses.AutoFromDoc[R]` | Tag-driven projection. R is a typed struct with `json:"<wire>"` tags only. It keys into the doc by the **Go field name** (the `MongoViewReader` already returned a Go-keyed doc, having translated column→Go via the view `TableSchema`); the `json:` tag governs solely the outgoing wire shape. Recursive — works through nested structs, slices of structs, and pointer-to-struct fields. Normalizes top-level `_id → id` (mongo-ism) and nil slices → empty typed slices at every depth. **This is the default**; reach for it whenever the projection is mechanical. |
| `fwresponses.RawDoc` | Identity passthrough — `func(map[string]any) map[string]any`. Use when the view doc shape IS the wire contract and a typed Response would just mirror it. |
| Consumer-declared `R{}.FromDoc(map[string]any) R` method | Custom logic — derived fields, conditional projection, ctx-aware shaping, or anything beyond tag-driven. The wrapper signature accepts any `func(map[string]any) R`. |

**No source-key override on the Response.** The projector consumes a single tag on the Response struct — `json:"<wire>"`, the standard encoding/json contract that names the field on the outgoing JSON. There is no `view:` tag: the doc the projector reads is already keyed by **Go field name**, because the `MongoViewReader` translated every physical column back to its Go field via the view's `TableSchema` before projection. A renamed column (`mail`, `cep`, `st`) is handled entirely on the infra side by `TableSchema.Field("Email","mail")` / `Field("PostalArea","cep")` — the Response never pronounces a physical name:

```go
type AddressOutput struct {
    Street     string `json:"street"`      // doc key "Street" → wire "street"
    ZipCode    string `json:"zipCode"`     // doc key "ZipCode" (reader already mapped column zip_code) → wire "zipCode"
    PostalArea string `json:"postalArea"`  // doc key "PostalArea" (reader mapped column cep via AddressSchema().Field("PostalArea","cep")) → wire "postalArea"
}
```

Co-located co-location convention still applies: `FindXxxResponse` struct and any `FromDoc` method live in the same `web/requests/find_xxx_request.go` file as the Request DTO.

### Manual query handlers

Anyone who wants to bypass the auto wrapper (custom identifier from the path, vendor-specific lookup, bespoke envelope) uses the building blocks. Manual routes have two helpers for query-string allowlist depending on whether the Response is typed:

- `fwweb.NewQueryParser[Req, Resp]() *QueryParser[Req, Resp]` — typed Mount-time parser for routes whose Request DTO opts into `?fields=` / `?sort=` AND that declare a typed Response. Construction runs the same boot scan `HandleQueryWithParams` runs internally: fields-side structural guard (panic when the Response violates the sparse-render contract — every field at every depth must be `*T` or a slice/map with `,omitempty`), `slog.Warn` advisory listing every sortable wire path so the operator can compare against the view's index declaration, and `extractProjectionSchema` for wire→doc translation. Per-request `parser.Parse(c)` returns the same `(criteria, badField, ok)` shape as `ParseCriteria` but with allowlist + translation enabled — e.g. `?fields=addresses.zipCode` becomes `{addresses.zip_code: 1}` and `_id:0` is added when `id` is not in the requested list. Construct once at `Mount`, reuse per request; the same schema caches the canonical wrapper memoizes are shared (zero per-route memory penalty). When `Resp` is `map[string]any` (RawDoc-style) the construction is still safe — `projSchema` stays nil and `Parse` degrades to `ParseCriteria` behavior.
- `fwweb.ParseCriteria(c, requestDTO) (criteria, badField, ok)` — un-typed escape hatch for handlers that have no typed Response (RawDoc projections, vendor-shaped envelopes) OR whose Request DTO does not opt into `?fields=` / `?sort=`. Same reflection-based allowlist applies; the projection schema is nil so `?fields=` is pass-through (tokens land verbatim, no allowlist, no `_id:0` auto-exclusion) and `?sort=` is pass-through (no snake_case translation). When the consumer has a typed Response and any reserved-key opt-in, prefer `NewQueryParser` — it closes the asymmetry against the canonical wrapper.
- `fwweb.BindPath(c, &req) (badField, ok)` — populates fields of `req` carrying a `path:"<name>"` struct tag from `c.Params("<name>")` with type conversion. Same cached `pathSchema` the canonical wrappers use internally. On the first conversion failure returns `(badField, false)` — caller forwards to `RespondSchemaViolation` to emit the canonical 400 envelope. Returns `("", true)` when the struct declares no `path:` tags. Manual handlers chain `BindPath → NewQueryParser.Parse (or ParseCriteria) → ToCommand/ToQuery → Dispatch`.
- `fwweb.RespondSchemaViolation(c, pipe, field)` — emits the canonical 400 envelope (`SchemaViolationNotification`, context `"Schema"`). Pair with the parser / `BindPath` so manual paths get the same wire shape as the canonical.
- `fwweb.ProjectPage[R](page, fn) ([]R, *PaginationInfo)` — walks `page.Items`, applies `fn` per doc, returns projected items + populated pagination envelope. Used when the handler builds the envelope by hand (sidecar fields, custom envelope shape).
- `fwweb.RespondPaged[R](c, status, page, fn)` — convenience: combines `ProjectPage` + `Respond` for the standard success envelope.

Canonical manual list shape (typed Response opting into `?fields=` / `?sort=`):

```go
// At Mount time — once per route. Boot scan fires here:
// fields-side guard + sort-side slog.Warn + projection-schema build.
listParser := fwweb.NewQueryParser[requests.FindXxxCustomRequest, requests.FindXxxCustomResponse]()

g.Get("/", func(c fiber.Ctx) error {
    appCtx := fwweb.AppContext(c)
    appCtx.SetParent(c)

    var req requests.FindXxxCustomRequest
    crit, badField, ok := listParser.Parse(c)
    if !ok {
        return fwweb.RespondSchemaViolation(c, pipe, badField)
    }
    q := req.ToQuery(crit)
    result := pipeline.Dispatch(pipe, appCtx, q, h)
    if !result.IsSuccess() {
        return fwweb.RespondFromResult(c, result, fiber.StatusOK)
    }
    return fwweb.RespondPaged(c, fiber.StatusOK, result.Value(),
        requests.FindXxxCustomResponse{}.FromDoc)
})
```

By-email lookup shape (`BindPath` chained before the parser):

```go
byEmailParser := fwweb.NewQueryParser[requests.FindXxxByEmailRequest, requests.FindXxxByEmailResponse]()

g.Get("/:email", func(c fiber.Ctx) error {
    appCtx := fwweb.AppContext(c)
    appCtx.SetParent(c)

    var req requests.FindXxxByEmailRequest
    if badField, ok := fwweb.BindPath(c, &req); !ok {
        return fwweb.RespondSchemaViolation(c, pipe, badField)
    }
    crit, badField, ok := byEmailParser.Parse(c)
    if !ok {
        return fwweb.RespondSchemaViolation(c, pipe, badField)
    }
    q := req.ToQuery(crit)
    result := pipeline.Dispatch(pipe, appCtx, q, h)
    if result.IsSuccess() {
        return fwweb.RespondWithSuccess(c, fiber.StatusOK,
            requests.FindXxxByEmailResponse{}.FromDoc(result.Value()))
    }
    return fwweb.RespondFromResult(c, result, fiber.StatusOK)
})
```

Same allowlist enforcement as the auto path; same envelope shape; same sparse-render guard fires at boot. What the handler hand-rolls is the dispatch + the `ToQuery` arguments. Auto and manual coexist — the choice is per-endpoint and based on whether the framework's `SetPathID` semantic fits the lookup.

### `path:` struct tag — universal URL-segment binding

Every Request DTO field tagged `path:"<name>"` is populated from the matching Fiber URL segment (`c.Params("<name>")`) before `ToCommand` / `ToQuery` runs. Closes the asymmetry between the canonical wrappers (which only auto-bind `:id` via the `pipeline.CommandWithID` / `queries.FindByIDQuery` interface) and routes with custom identifier segments (`:email`, `:tenantId`, …) or compound paths (`/tenants/:tenantId/users/:id`).

```go
// Read by-email — single segment route
type FindUserByEmailRequest struct {
    Email           string `path:"email"`
    IncludeArchived *bool  `query:"includeArchived"`
}
func (r FindUserByEmailRequest) ToQuery(crit fwqueries.ReadCriteria) *queries.FindUserByEmailQuery { ... }

// Compound route — both segments via tag
type UpdateOnTenantRequest struct {
    TenantID string `path:"tenantId"`     // /tenants/:tenantId/users/:id — :id still via interface on WithBodyID
    Name     string `json:"name"`
}
```

Per-wrapper behavior:

| Wrapper | Auto-binds `:id` via interface? | `path:"<other>"` allowed? | `path:"id"` allowed? |
|---|---|---|---|
| `HandleCommandWithBody` | No | Yes | Yes (dev maps `cmd.SetPathID(r.ID)` inside `ToCommand`) |
| `HandleCommandWithBodyID` | Yes | Yes | **No** — boot panic |
| `HandleCommandWithID` | Yes | n/a (wrapper has no Request DTO) | n/a |
| `HandleQueryWithParams` | No | Yes | Yes |
| `HandleQueryWithID` | Yes | Yes | **No** — boot panic |

**Manual routes:** call `fwweb.BindPath(c, &req)` explicitly (same helper as `ParseCriteria`) — `omnicore-example-users/web/user_custom_routes.go` is the canonical example.

**Supported field types:** `string`, signed/unsigned ints of any width, `float32`/`float64`, `bool`, `uuid.UUID`, `domain.ID`. Pointer/slice/struct types are rejected at boot. Conversion failure returns 400 `SchemaViolationNotification` with the segment name.

**`FullBody` interaction:** the strict-body check in `HandleCommandWithBody{,ID}` skips fields carrying a `path:` tag — their value comes from the URL, not the body. Declaring both `path:"X"` and `json:"X"` on the same field is a boot panic; the same value cannot come from two sources.

**Group A wrapper + ID-requiring handler warning:** when `HandleCommandWithBody` / `HandleQueryWithParams` (group A — no `:id` auto-bind) is paired with an ID-requiring auto handler (Update/PartialUpdate/Archive/Unarchive/Delete; FindByID is excluded because its group A wrapper does not compile with the handler's constraint) AND the Request DTO declares no `path:` tag at all, the wrapper logs a single `slog.Warn` at construction time. Catches the misconfiguration of "I expected the wrapper to populate the ID" — the runtime guard `handlers.RequirePathID` still catches the actual failure if the warning is ignored.

**Runtime guard.** Each ID-requiring auto handler calls `handlers.RequirePathID(<pathID>, "<HandlerName>")` as the first line of `Handle`. Empty path ID → developer-focused panic, caught by `pipeline.Run`'s `defer/recover`, surfaced to the wire as 500 `InternalServerErrorNotification`; full diagnostic on slog. Each handler also embeds `pipeline.PathIDRequired` so the wrappers can detect the requirement via type assertion at construction time (zero per-request cost).

## Aggregate persistence (transparent dispatch)

Entities opt into aggregate-aware persistence by implementing `AggregateRootProvider`:

```go
type AggregateRootProvider interface {
    GetAggregateRoot() *AggregateRoot
    AggregateChildren() []AggregateValueObject  // declared boundary
}
```

Child table/FK are declared in the child's `TableSchema` (`.Child(fwinfra.NewTableSchema[Child]("table").FK("col")...)` on the root schema). Universal symmetric cascade: root archive → children archive; root delete → children delete (via FK ON DELETE CASCADE); root unarchive → children unarchive.

`AggregateChildren()` declares **which types** belong to the aggregate — a domain definition, separated from infra. The top-level primitives (`AddAggregateChild`/`ChangeAggregateChild`/`RemoveAggregateChild`/`ReplaceAggregateChildrenOf`) consult this list and reject VOs of undeclared types with `InvalidAggregateChildNotification` (422). `AggregateConstructor` (DB load) bypasses the type-guard — types come from the schema's `Child(...)` declarations, already trusted.

`GetInsertable/GetUpdatable/GetArchivable/GetDeletable/GetUnarchivable` detect the interface and attach `*aggregateMeta` (carries only the root pointer) to the ValidEntity. `infra.Postgres.Insert/Update/Archive/Delete/Unarchive` check `entity.AggregateInfo()` at the start and dispatch to the aggregate path. They receive `*TableSchema` (third argument) for the Go↔column map + the resolved `writeHook` (fourth argument) the BaseRepository builds from the typed `WriteOption[T]` variadic. Both paths fire lifecycle hooks at the same TX positions — see "Persistence ports" above.

**Validation of children is also transparent.** `runAggregateValidations` (called inside `validateForInsert/Update/Delete`) detects `AggregateRootProvider` and iterates `root.AllAggregateItems()` automatically: for each typeName present, it fires `BuildRules(actionName, svc, r)` on each item of the `AggregateRoot` with `CurrentStatus != Removed`, with `r` carrying a `NotificationContext` already scoped at `[NameSegment(collection), IndexSegment(i)]` (path segment via camelCase `toLowerCamel(typeName)` — `Address`→`addresses`, `OrderLine`→`orderLines`). **The root's `BuildRules` does not need to manually register children** for theirs to run. The type-guard on the primitives ensures only types declared in `AggregateChildren()` reach the map, so iterating the map is equivalent to iterating the declared list. The AVO uses `r.IfInsert/IfUpdate/IfInsertOrUpdate` to decide what to validate — e.g., `Address.BuildRules` in the example only runs rules on Insert/Update, but the framework doesn't enforce it (entities may have Delete-specific rules, like "cannot delete primary address"). `AddAggregateValueObject` is still available for typeNames **outside** `AggregateChildren()` (VOs without their own table, e.g. tags in a JSONB column): typeNames present in the map are ignored in the manual slice to avoid double validation.

### Guarantees of the aggregate path

| Guarantee | Where |
|---|---|
| Single `pgx.Tx` for root + all children | `infra/aggregate_persister.go` |
| Exactly one outbox row per call (granularity B — the aggregate IS the event unit) | `insertAggregate` / `updateAggregate` etc. |
| FK injected from root id before child INSERT (struct of child must NOT include FK field) | `insertChild` |
| Status iteration: Added→INSERT, Changed→UPDATE, Removed→Archive (symmetric cascade), Constructor→no-op (update) or INSERT (insert) | `applyChildChanges` / `insertChildren` |
| Archive of root cascades archive of all currently-active children | `archiveAggregate` |
| Unarchive of root restores all archived children of that root (requires `ArchivedFinder` on the Repository) | `unarchiveAggregate` |
| Hard Delete of root relies on FK `ON DELETE CASCADE` at the schema level | `deleteAggregate` |
| Lifecycle hooks fire ONCE per call at positions A and D — same shape and same payload as the flat path | `fireAfterBegin` / `fireBeforeCommit` in `infra/hook_dispatch.go` |

### Example consumer

```go
type User struct {
    domain.AggregateRoot
    Name, Email, Username, Phone string
}

func (u *User) Modes() []domain.EntityMode { return []domain.EntityMode{...} }
func (u *User) GetAggregateRoot() *domain.AggregateRoot { return &u.AggregateRoot }
func (u *User) BuildRules(...) { ... }

// Declares the aggregate boundary (domain concern).
func (u *User) AggregateChildren() []domain.AggregateValueObject {
    return []domain.AggregateValueObject{Address{}}
}

// Domain methods — commands call these, not the primitives.
func (u *User) AddAddress(addr Address, svc domain.Service) {
    for _, existing := range domain.GetCurrentItemsOf[Address](&u.AggregateRoot) {
        if existing.sameBusinessIdentity(addr) {
            u.AddNotification("Address", DuplicateAddressNotification{})
            return
        }
    }
    domain.AddAggregateChild(u, addr)
}
func (u *User) ChangeAddress(o, r Address)         { domain.ChangeAggregateChild(u, o, r) }
func (u *User) RemoveAddress(a Address)            { domain.RemoveAggregateChild(u, a) }
func (u *User) ReplaceAddresses(as []Address)      { domain.ReplaceAggregateChildrenOf(u, as) }

// Schema — the single, explicit Go↔column map (lives in infra).
func UserSchema() *fwinfra.TableSchema {
    return fwinfra.NewTableSchema[*User]("users").
        PK("ID", "id").
        Field("Name", "name").
        Field("Email", "email").
        Field("Username", "username").
        Field("Phone", "phone").
        SoftDelete("deleted_at").
        CreatedAt("created_at").
        UpdatedAt("updated_at").
        Child(fwinfra.NewTableSchema[Address]("addresses").
            PK("ID", "id").
            FK("user_id").
            Field("Street", "street").
            Field("ZipCode", "zip_code").
            SoftDelete("deleted_at").
            CreatedAt("created_at").
            UpdatedAt("updated_at"))
}

// Repository — declares the mapping once via WithSchema; children come from
// the schema's Child(...) declarations.
type UserRepository struct {
    fwinfra.BaseAggregateRepository[*User]
}

func NewUserRepository(pg *fwinfra.Postgres) *UserRepository {
    r := &UserRepository{
        BaseAggregateRepository: fwinfra.NewBaseAggregateRepository[*User](
            pg, func() *User { return &User{} },
        ),
    }
    r.WithSchema(UserSchema())   // one schema → write + criteria + scan + children
    return r
}
// FindByID / FindArchivedByID are promoted by BaseAggregateRepository[*User].
```

> The common case skips this boilerplate by embedding `BaseAggregateRepository[T]`, which promotes `FindByID`/`FindArchivedByID` (and `FindOne`/`FindAll`) routed through the engine — see [Entity search engine](#entity-search-engine-criteria).

## Persistence

```go
// Setup (once)
pg, _ := infra.NewPostgres(ctx, dsn)
mongo, _ := infra.NewMongoDB(ctx, mongoURI, dbName)
translator := translation.Default()
pipe := pipeline.New(translator)
// Audit is configured ONCE on the Postgres adapter at boot — every write
// routes through it without an Auditor singleton.
// pg.WithAudit(&cfg.Audit, slog.Default(), cfg.Auth.AuditClaims)

// Per-domain Repository — embed fwinfra.BaseRepository[T] for the write
// binding (Scope → 5 writes) + AggregateLoader[T] for FindByID
type UserRepository struct {
    infra.BaseRepository[*User]
    loader *infra.AggregateLoader[*User]
}
func NewUserRepository(pg *infra.Postgres) *UserRepository {
    newUser := func() *User { return &User{} } // single source of truth for the factory
    r := &UserRepository{
        BaseRepository: infra.BaseRepository[*User]{
            Postgres:    pg,
            ContextName: "User",
            NewEntity:   newUser, // feeds Repo.New() (consumed by UnarchiveCommandHandler)
            Constraints: map[string]infra.ConstraintBinding{
                "users_email_active_idx": {Notification: EmailAlreadyExistsNotification{}, Field: "email"},
                "users_username_active_idx": {Notification: UsernameAlreadyExistsNotification{}, Field: "username"},
            },
        },
    }
    schema := UserSchema()        // the explicit Go↔column map (see "Schema mapping")
    r.Schema = schema             // feeds the write binding
    r.loader = infra.NewAggregateLoader[*User](pg, newUser).
        WithContextName("User").
        WithSchema(schema)        // same schema drives root + child auto-scan
    return r
}
func (r *UserRepository) FindByID(id domain.ID) (*User, error) {
    return r.loader.FindOne(context.Background(), criteria.ByID(id))
}
// ↑ Insert/Update/Archive/Unarchive/Delete come embedded — do not write by hand.
//   Aggregate-aware dispatch happens inside Postgres.Insert/etc.
//   FindByID comes from the AggregateLoader in auto-scan mode:
//     - Without calling WithRootScanner → framework generates the SELECT from
//       the columns the TableSchema declares for *User and populates via
//       row.Scan directly.
//     - The schema's Child(...) declarations drive child auto-scan for the
//       child collection.
//     - For non-trivial queries (JOIN, CASE, COALESCE) use WithRootScanner
//       and WithChildScanner — they coexist with the schema-driven auto-scan.

// Handler — binds the write scope via Scope(ctx, opts...) and calls the
// pure domain.Writer; any in-TX hook closures land as Scope opts.
func (h *CreateUserHandler) Handle(ctx *configuration.AppContext, cmd CreateUserCommand) (domain.ID, error) {
    user := &User{Name: cmd.Name, /* ... */}
    for _, addr := range cmd.Addresses { user.AddAddress(addr, nil) }

    insertable, err := domain.GetInsertable(user, nil, "GetInsertable")
    if err != nil { return domain.ID{}, err }

    return h.repo.Scope(ctx).Insert(insertable)
}

// HTTP route
app.Post("/users", func(c fiber.Ctx) error {
    var cmd CreateUserCommand
    c.Bind().Body(&cmd)
    result := pipeline.Dispatch(pipe, appCtx(c), cmd, &CreateUserHandler{repo, auditor})
    return web.RespondFromResult(c, result, fiber.StatusCreated)
})
```

**`infra.BaseRepository[T]`** implements `Scope(ctx, opts...) domain.Writer` — the returned `boundWriter` carries the 5 writes (Insert/Update/Archive/Unarchive/Delete) as one-liners delegating to `infra.Postgres` with the bound ctx — and provides `New() T` via the injected `NewEntity` factory — just embed it. **`NewEntity` is mandatory** (`New()` panics if nil): every framework Repository needs to construct T somehow, and centralizing this eliminates duplication with `AggregateLoader` or handlers (the same factory is typically shared with the `AggregateLoader`). **`infra.ConstraintBinding`** translates unique violations (PG SQLSTATE `23505`) into `*InfrastructureError` carrying the typed notification via `infra.FieldErrorWithCause` — replaces the manual `mapPGError`. Unregistered constraints, codes other than `23505`, and non-pgErr errors are returned raw.

**`infra.AggregateLoader[T]`** loads live aggregates (root + children) via the entity search engine — `FindOne(ctx, *criteria.Query)` / `FindAll(ctx, *criteria.Query)` (see [Entity search engine](#entity-search-engine-criteria)). It scans the matched rows in one of two coexisting modes:

- **Auto-scan (default)** — the columns for root and children come from the `TableSchema` (`Field("ZipCode","zip_code")`, …) threaded via `WithSchema`. The SELECT list and the scanner share the one schema's `column ↔ Go field` map, so a renamed column round-trips (write → criteria → read-back) — see "Schema mapping". The loader assembles `SELECT <pk>, col1, col2, ... FROM table WHERE <criteria> AND <scope gate>` and reads the PK back (the criteria path does not know it a priori). Zero scanner in the service. API: absence of `WithRootScanner` activates root auto-scan; the schema's `Child(...)` declarations activate child auto-scan per child type.

- **Manual (`WithRootScanner`/`WithChildScanner`)** — service provides the scan function. Escape hatch for non-trivial decoding (JOIN, CASE, COALESCE, computed denormalizations). Coexists with auto-scan (per typeName, manual scanner wins over auto when both are registered). **A manual root scanner used with `FindOne`/`FindAll` must populate the entity id (scan it + `SetID`)** — the engine recovers it via `GetID()` because, unlike the removed by-id `Load`, there is no input id on the criteria path.

In both modes: the archived scope governs the `deleted_at` gate (root and children). Nonexistent root → `*DomainError` with `RecordNotFoundNotification` (→ 404 HTTP). Aggregate without children: declare no children on the schema — only the root SELECT runs. T must satisfy `domain.Entity`. A lookup by any non-id field (email, tenant, …) is now a first-class `FindOne(criteria.Where(...))` — no hand-rolled SQL needed.

### Entity search engine (`criteria`)

`omnicore/infra/criteria` is the framework's backend-neutral query DSL for loading **live domain aggregates** from the authoritative store (PostgreSQL). It is the **dev-facing, compile-time** counterpart to the end-user-facing Mongo read side: the developer composes a criterion in Go inside an `infra` repository implementation; the engine returns the source-of-truth aggregate ready for a command (`GetUpdatable`). It does NOT replace the read side (`ViewReader.ReadByID`/`ReadPage`) — that serves the eventually-consistent projection and returns documents, not entities.

- **Pure DSL (`criteria` package, stdlib only).** Sealed `Expr` tree + fluent builder: `Eq/Ne/In/Nin/Gt/Gte/Lt/Lte/Like/ILike/IsNull/NotNull`, `And/Or/Not` (nestable), sugar `Contains/StartsWith/EndsWith` (case-insensitive, LIKE-metachar escaped) + `Between`. Field names are **Go field names** ("Email"), resolved to columns by the loader. Wrapped in a `Query` carrying `WHERE` + `OrderBy`/`OrderByDesc` + `Limit` + an archived `Scope`; `criteria.ByID(id)` is the PK shortcut (assumes the `ID`↔`id` convention). The archived scope (`Active` default / `IncludeArchived` / `OnlyArchived`) is a `Query` method, NOT part of the boolean algebra — the `Expr` tree stays soft-delete-agnostic.
- **Encapsulated PG translator (unexported in `infra`).** A `Visitor` walks the tree → `WHERE` fragment + `$n` args; identifiers pass through `validIdentifier`, values are parameterized, `domain.ID` args unwrapped via `.Value()`. The `Visitor` interface is the seam a future backend (e.g. Mongo → bson) implements; the IR is the neutral contract.
- **Layer posture.** `criteria` is imported only by `infra` (framework + the consumer's own infra repository impls). `domain` and `application` never import it — repository interfaces there stay in business vocabulary (`FindByID`, `FindByEmail`, …) and the infra impl is the only place that consumes the engine.

```go
// infra repository implementation — alternate-key lookup, no hand-rolled SQL:
func (r *UserRepository) FindByEmail(email string) (*User, error) {
    return r.FindOne(context.Background(), criteria.Where(criteria.Eq("Email", email)))
}

// richer: boolean composition + order + limit, returns []*User (children batched):
users, err := r.FindAll(ctx, criteria.Where(criteria.And(
    criteria.Eq("TenantID", t),
    criteria.Or(criteria.Eq("Status", "active"), criteria.ILike("Name", "bob%")),
)).OrderByDesc("CreatedAt").Limit(50))
```

`FindOne` returns one match or `RecordNotFoundNotification`; **>1 matches is an error** (the contract is "expected one"). `FindAll` returns a possibly-empty slice and loads children in one batched `WHERE fk IN (...)` per child type (no N+1).

Convention of nullable fields: nullable PG types map to **pointer types** in the domain (`Phone *string`, `Label *string`). pgx writes NULL when nil; reads NULL as nil. No wrapping mechanism in the framework — pointer is the canonical form.

### Schema mapping (`TableSchema`)

Everything above infrastructure speaks the **Go field name** (PascalCase) — domain, application, web. The criteria is `criteria.Eq("Email", v)`; the audit timeline says `Email`; repository signatures speak the domain. The only place a physical column/table name appears is the persistence boundary, in exactly one artifact: `TableSchema`. A schema is the **mandatory, explicit, complete** map between a Go type's fields and its physical columns. There is no convention, no name-inference, no `transient` tag: every persisted field is declared, and an undeclared exported field is simply never persisted, scanned, or audited.

One `TableSchema` drives the write path (INSERT/UPDATE/archive SQL), the criteria engine (the `WHERE` a Go-named criterion compiles to), and the auto-scan read-back (column → Go field). The **same** schema is reused by the read-side `ViewDefinition`, so the Mongo projection speaks the same names — a column rename round-trips everywhere automatically.

**Why it is mandatory + manual (design rationale).** The hand-declared map is deliberate, buying four things a convention cannot: (1) **a pure DDD domain** — domain/application/web speak only business vocabulary; the sole place a physical name lives is the `TableSchema` in `infra/`; (2) **transparent mapping flexibility** — any Go field → any column, transparent to every line of implemented code; a rename lives in one place and round-trips everywhere; (3) **adoption of existing/external tables** — point the framework at a schema you don't control (or an upstream collection via `NewExternalSchema`), field by field, with the one structural requirement of a **single, non-composite primary key**; (4) **no failed, tiring conventions** — name-inference is lossy/acronym-hostile (`UserID`/`UserId` → `user_id`, `URLPath`, `IPv4`) and silently wrong on divergence, whereas an explicit map makes a wrong name a boot panic. The map is the single lossless, unambiguous source of truth the persistence + read membrane depends on, so the framework refuses to fabricate it.

**Three-name model.** A field carries up to three names, resolved at two membranes: wire (`json:`/`query:` tags, in `web/`) ↔ Go field (`Email`, the single name every layer above infra uses) ↔ physical column (`mail`, `TableSchema.Field("Email","mail")` in `infra/`). The web membrane translates JSON↔Go; the infra membrane (`TableSchema`) translates Go↔column. Wire and physical names are invisible to the developer manipulating data.

**Declaring a schema.** A type-anchored schema validates every field against the Go type **at construction** — a `Field` naming a missing/unexported field panics immediately (the enforcement that replaces convention). A type-less external schema describes an upstream service's columns for an external `FromSchema` embed source.

```go
func UserSchema() *fwinfra.TableSchema {
    return fwinfra.NewTableSchema[*User]("users").
        PK("ID", "id").                 // Go "ID" ↔ column "id" (single-column PK)
        Field("Name", "name").
        Field("Email", "mail").         // renamed column
        SoftDelete("deleted_at").       // managed: presence enables the predicate
        CreatedAt("created_at").        // managed: framework stamps NOW() on INSERT
        UpdatedAt("updated_at").        // managed: framework stamps NOW() on INSERT + UPDATE
        Child(AddressSchema())          // aggregate child, keyed by Go type name
}

func AddressSchema() *fwinfra.TableSchema {
    return fwinfra.NewTableSchema[Address]("addresses").
        PK("ID", "id").
        FK("user_id").                  // FK to the root — injected by the persister, NOT a struct field
        Field("Street", "street").
        Field("ZipCode", "zip_code").
        SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at")
}

// Type-less — for an upstream-projected external FromSchema source (no local struct).
fwinfra.NewExternalSchema("users").PK("ID", "id").Field("Email", "mail")
```

The Go-side PK field is conventionally `ID` (the `BaseEntity` contract); `PK("ID","col")` declares both sides. `FK` names the child's foreign-key column referencing the root — injected by the persister, not a struct field. Every other persisted field is a `Field("Go","column")` pair.

**Three managed columns — by presence, not a flag.** Calling `SoftDelete(col)` / `CreatedAt(col)` / `UpdatedAt(col)` enables the behavior; omitting the call disables it (no boolean knob). `created_at`/`updated_at` are **actively stamped** `NOW()` on write — the framework never relies on a DB `DEFAULT NOW()` it does not own. `SoftDelete` present → read scope-gate `col IS NULL` + Archive/Unarchive write `col = NOW()` / `NULL`; omitted → no gate, Archive/Unarchive unavailable (boot check + runtime guard). On the read path these three are also readable under fixed logical Go names `CreatedAt`/`UpdatedAt`/`DeletedAt` so a view can project them without a domain field.

**One declaration, every consumer — `WithSchema`.** `BaseAggregateRepository.WithSchema(schema)` threads the schema into BOTH the write binding (`BaseRepository.Schema`) and the read loader (`Loader.WithSchema`). Children come from `schema.children` (`.Child(...)`), NOT a separate `WithChild` call. The `Modes() ⟺ SoftDelete` boot check runs here; the field-existence and bijection checks already ran while the schema was built. A misconfiguration panics at construction, not on the first request.

**The same schema drives the Mongo view.** The `ViewDefinition` reuses the schemas: the root via `.Schema(UserSchema())`, each embed via `fwinfra.FromSchema(...)` — the single embed source constructor. From the schema the framework derives the embed's table/collection, the store kind (type-anchored `NewTableSchema[T]` → local Postgres; type-less `NewExternalSchema` → external/Mongo), and — for an `EmbedMany` — the join FK. The composer writes physical columns to Mongo; the reader translates each column back to its Go field name using these schemas (`mail`→`Email`, `zip_code`→`ZipCode`) before the typed Response projects to the wire — so the wire speaks Go names with no per-Response source-key override (`ViewOf` is gone). For a local embed the parent-side Go segment is **derived** from the schema's Go type (pluralized for `EmbedMany` — `Address`→`Addresses`; the type name for a one-to-one `Embed`), so `.As(...)` is an optional override; an external embed has no Go type to derive from, so `.As(...)` is **required** there (uses the now-exported `domain.PluralizeWord` to pluralize the local segment).

```go
fwinfra.View("users").Version(1).Root("users").
    Schema(UserSchema()).
    EmbedMany("addresses", fwinfra.FromSchema(AddressSchema()))   // segment derived → "Addresses"
```

**Boot checks** (panic at construction): field-exists on the type (a typo is a boot panic, not a silent miss); bijection over each source's full column set (mapped fields + PK + every declared managed column — no two map to the same physical column); `Modes() ⟺ SoftDelete`; **PK mandatory + single-column, no default** (`PK(go,col)` must be declared on every schema — root, child, embed source — and rejects empty names; there is no `"ID"`/`"id"` guessing); **FK mandatory on aggregate children** (`Child(...)` without `.FK(col)` panics; on the read side an `EmbedMany` source without `.FK(col)` and a one-to-one `Embed` without `.On(col)` are fatal `ValidateViewSchemas` errors); **aggregate depth = 1** — a child schema that declares its own `Child(...)` (a grandchild) panics at `WithSchema` (write side: grandchildren are unsupported — model the sub-collection as a separate aggregate), and an embed source whose schema carries `Child(...)` is a fatal `ValidateViewSchemas` error (read side: depth IS supported, but via nested `EmbedMany`/`Embed`, never the schema's `Child(...)`). Width (number of child types + instances) is unlimited.

**Audit stays map-blind.** Audit does NOT consume the schema — the `snapshot` keys and `changes[].field` are the faithful Go field name (`Email`, `ZipCode`), never the physical column. A column rename never disturbs the timeline; `label:"…"` field labels resolve by Go field name too.

**Manual scanners bypass the schema by design.** `WithRootScanner`/`WithChildScanner` (the latter takes the child Go type name) hand the SELECT + scan to the developer, who owns the column names directly. The schema governs the auto-scan path; the manual path stays a full escape hatch.

**Out of scope:** third-party/legacy database adoption (predicate-shaped soft-delete, composite PK, exotic types — rejected). The scope is "an explicit map over the schema the framework manages".

## Read side (CQRS)

- `infra/view.go` — `View(name).Root(table).Schema(ts).EmbedMany("field", FromSchema(childTs)).Embed("field", FromSchema(externalTs).On("fk").As("Seg"))` defines `ViewDefinition`s. `FromSchema` is the single embed source constructor; schema is mandatory on the root and every embed. Opt in to dropping archived rows from the projection with `.DeleteOnArchive()` (default: archived rows survive in the projection — Mongo mirrors PostgreSQL symmetrically).
- `infra/composer.go` — composes documents from PostgreSQL using ViewDefinitions
  - **Omits the `WHERE deleted_at IS NULL` filter** in all three fetch helpers (fetchRow, fetchWhere, fetchAll) by default — Mongo views mirror PostgreSQL symmetrically (archived rows survive with `deleted_at` populated). When the `ViewDefinition` opted in via `.DeleteOnArchive()`, the filter is applied on the root SELECT and on every embed source (cascade: the flag governs the whole aggregate projection — there is no per-embed override).
  - `EmbedMany` uses the root's `id` to match the child's FK column (`source.joinKey`)
  - `Embed` (one-to-one) uses `doc[source.joinKey]` as the source's id
- `infra/sync.go` — `SyncEngine` consumes Kafka events and upserts MongoDB views
  - Reads metadata from **Kafka headers + message key** (Debezium Outbox Event Router native shape), not message body
  - `aggregate_id` ← message Key; `aggregate_type` and `event_type` ← headers
  - `DELETED` → `mongo.Delete` (hard delete is unconditional, regardless of `DeleteOnArchive`)
  - `ARCHIVED` → **compose + upsert** by default (document survives with `deleted_at` populated); `mongo.Delete` when the view opted in via `.DeleteOnArchive()`
  - `UNARCHIVED` → re-compose + upsert (always — covers both modes; for default views it just clears `deleted_at` on the existing document)
- `infra/upstream_subscriber.go` — `UpstreamSubscriber` materializes upstream service A's events into local Mongo collection B owns, and triggers downstream recompose on every view embedding the collection via an external `fwinfra.FromSchema` embed. One instance per `bootstrap.UpstreamSubscription` declared in yaml or `Wiring.UpstreamSubscriptions`. See "Cross-service composition" below.
- `infra/rebuild.go` — `RebuildView`, `RebuildViewSince`, `RebuildAllViews` for offline reconstruction
- `application/queries/view_reader.go` — `ViewReader` port + `ReadCriteria` / `Page` transport-agnostic types
- `application/queries/query_handler.go` — `QueryHandler.Read(ctx, view, criteria)` / `ReadByID(...)` — pure, returns `Page` / `map[string]any`
- `infra/mongo_view_reader.go` — `MongoViewReader` implements `queries.ViewReader` against MongoDB
- `web/query_parse.go` — `ParseReadCriteria(fiber.Ctx) queries.ReadCriteria` translates HTTP query string into application criteria
- `web/query_routes.go` — `QueryRouter` Fiber adapter

### Schema requirements for the read side

Every table read by the composer (root and embedded sources) **must have a `deleted_at TIMESTAMP` column** — the column is the soft-delete marker the SyncEngine + reader pipeline rely on. By default the composer omits the `deleted_at IS NULL` filter so archived rows reach the projection (consumers still read them only when they pass `IncludeArchived=true` via the reader port — same semantic as the existing `?includeArchived=true`). A view that calls `.DeleteOnArchive()` applies the filter on root + every embed so the projection mirrors only active data.

### Declarative Mongo surface

`ViewDefinition` carries every Mongo-side artifact the projection needs. `bootstrap.Run` calls `infra.CheckServiceRegistry` (DB-per-service guard) and then `infra.ApplyMongoSpecs` between `collectViews` and `SyncEngine.Start` — the cluster is brought to the declared shape before the first Kafka message lands.

**Index types — all declared via the same fluent builder:**

| Type | Builder | Mongo equivalent |
|---|---|---|
| Single-field ascending | `fwinfra.Index("email")` | `{email: 1}` |
| Descending | `fwinfra.Index("created_at").Desc()` | `{created_at: -1}` |
| Compound (ESR ordering) | `fwinfra.Compound("email", "created_at")` | `{email: 1, created_at: 1}` |
| Unique | `fwinfra.Index("email").Unique()` | `unique: true` |
| Partial | `fwinfra.Index("deleted_at").Partial(fwinfra.Exists("deleted_at", false))` | `partialFilterExpression: {...}` |
| Sparse | `fwinfra.Index("phone").Sparse()` | `sparse: true` |
| TTL | `fwinfra.Index("expires_at").TTL(7 * 24 * time.Hour)` | `expireAfterSeconds: 604800` |
| Text (one per view) | `fwinfra.TextIndex("name", "email").DefaultLanguage("portuguese").Weights(map[string]int{"name": 10})` | `{name: "text", email: "text"}` |
| 2dsphere | `fwinfra.GeoIndex("location")` | `{location: "2dsphere"}` |
| Hashed | `fwinfra.Index("user_id").Hashed()` | `{user_id: "hashed"}` |

Each spec accepts `.Name("custom")` to override Mongo's auto-derived name and `.Collation(&fwinfra.CollationSpec{...})` for per-index collation. Compose the slice via the variadic `.Indexes(spec1, spec2, ...)` on `*ViewDefinition`; multiple calls accumulate.

**Collection-level features (same builder):**

```go
fwinfra.View("users").
    Version(1).                                                              // mandatory; see "Mongo schema evolution"
    Root("users").
    Indexes(
        fwinfra.Index("email").Unique(),
        fwinfra.TextIndex("name", "email").DefaultLanguage("portuguese"),
    ).
    JSONSchema(bson.M{"bsonType": "object", "required": []string{"_id", "email"}}).
    JSONSchemaValidationLevel(fwinfra.ValidationLevelStrict).      // default
    JSONSchemaValidationAction(fwinfra.ValidationActionError).     // default
    Collation(&fwinfra.CollationSpec{Locale: "pt", Strength: 1}).
    Capped(&fwinfra.CappedSpec{SizeBytes: 1 << 30, MaxDocs: 1_000_000}).  // mutually exclusive with TimeSeries
    TimeSeries(&fwinfra.TimeSeriesSpec{TimeField: "ts", MetaField: "sensor_id", Granularity: "seconds"})
```

`Version(N)` is **mandatory** — every `ViewDefinition` must declare a positive integer version. Bump on every change to the rebuild-relevant declarative state (root table, embeds, DeleteOnArchive, $jsonSchema, collation, capped, time-series). The integer participates in `RebuildHash`, so the framework detects shape changes that landed without a version bump and aborts boot with a `DriftForgotToBump` diagnostic. Index-only changes do NOT require a bump — they flow through `ApplyMongoSpecs` without document recomposition. Full semantics in "Mongo schema evolution".

Boot invariants enforced by `ValidateMongoSpec()`:

1. `Version(N)` must be declared with a positive integer (`> 0`).
2. `Capped` and `TimeSeries` are mutually exclusive — Mongo rejects the combination.
3. `CappedSpec.SizeBytes` must be > 0.
4. `TimeSeriesSpec.TimeField` is mandatory.
5. `TimeSeriesSpec.Granularity` (when set) ∈ `{"seconds", "minutes", "hours"}`.
6. At most one `TextIndex` per view — Mongo allows only one per collection.
7. Every `IndexSpec` must declare at least one key.
8. `JSONSchemaSpec.ValidationLevel` ∈ `{"strict", "moderate", "off"}`.
9. `JSONSchemaSpec.ValidationAction` ∈ `{"error", "warn"}`.

**Apply semantics (idempotent):**

- Steady state: every artifact already matches — `ApplyMongoSpecs` only performs read-side round-trips (`listCollections` + `CreateOne`-with-identical-spec absorbed by the driver) and returns nil.
- New collection: `createCollection` carries the declared collation / capped / time-series / validator in one round-trip (no follow-up `collMod`).
- Existing collection, validator updated: `collMod` rewrites the validator in place.
- Existing collection, collation / capped / time-series divergence: **strict abort**. Mongo treats these as immutable; the framework never auto-drops the collection. Operator owns the migration.
- Existing index, divergent spec: default strict abort with the driver's `IndexOptionsConflict` / `IndexKeySpecsConflict` diagnostic naming the index.
- `OMNICORE_MONGO_FORCE_REBUILD=true` in the process env: index divergence is recovered by dropping the conflicting index and recreating it with the declared spec. **Operator opt-in only — never auto-enabled by any profile.** Scope is intentionally narrow to indexes; the env var does NOT authorize dropping collections.

**DB-per-service guard (`infra.CheckServiceRegistry`):**

- Writes a per-boot marker `{_id: cfg.Service, process_id, boot_at, pid, host}` under `omnicore_service_registry`.
- Lists every collection in the database and computes `foreign = observed − declared views − framework-owned − system.*`.
- `APP_PROFILE=dev` → `slog.Warn` naming the foreign collections; boot continues (lets hot-reload + ad-hoc mongosh work locally).
- Any other profile → boot aborts with the foreign collections listed in the error.
- Other service markers found in the same database trigger an unconditional `slog.Warn` (useful for inspection on deliberately shared clusters; never blocks).

**Privileges the connection user needs** on the service's database for the apply step to succeed: `find` / `insert` / `update` / `remove` (for the registry upsert and existing Composer path), `createIndex`, `collMod`, `createCollection`, `listCollections`. All present in Mongo's built-in `dbOwner` role; production deployments typically scope a least-privilege role that bundles these five.

### Cross-service composition

When service B needs read-side data owned by service A (orders embedding buyer details, invoices showing the product line, etc.), the framework's canonical path is **event-driven local projection in Mongo**: B subscribes to A's Kafka topic via a framework-managed consumer, materializes the upstream state into a local Mongo collection in B's database, and the framework triggers downstream recompose of every B view whose `ViewDefinition` embeds that collection. B never reads from A's database and never calls A's HTTP surface on the request path. A is unaware that B exists.

**Declaration** — `microservice.<profile>.yaml` (canonical) or `Wiring.UpstreamSubscriptions` (manual lifecycle / tests):

```yaml
upstreamSubscriptions:
  - topic: users.events
    collection: users                # local Mongo collection in B's database
    workers: 2
    filter: [name, email]            # allowlist; nil/empty keeps full payload
    onUpstreamDelete: anonymize      # cascade (default) | anonymize | keep
    anonymizeFields: [name, email]
```

**Embed** — views consume the upstream-projected collection via an external `fwinfra.FromSchema` (a type-less `NewExternalSchema` marks the source external; `.On` is the parent doc FK on a one-to-one `Embed`; `.As` is required because an external source has no Go type to derive the segment from):

```go
fwinfra.View("orders").
    Version(1).
    Root("orders").
    Schema(OrderSchema()).
    Embed("buyer", fwinfra.FromSchema(
        fwinfra.NewExternalSchema("users").PK("ID","id").Field("Name","name").Field("Email","mail")).
        On("buyer_id").As("Buyer")).
    EmbedMany("lines", fwinfra.FromSchema(OrderLineSchema())).
    Indexes(
        fwinfra.Index("buyer_id"), // boot guard §8.1 requires it
    )
```

**Runtime** — for every event on A's topic, `infra.UpstreamSubscriber`:

1. Decodes the payload as `map[string]any`; applies `Filter` allowlist.
2. Dispatches by `event_type`:
   - `INSERTED` / `UPDATED` / `UNARCHIVED` → `mongo.Upsert(collection, id, filtered)`
   - `ARCHIVED` + `DeleteOnArchive=false` → `mongo.Upsert` (doc survives with `deleted_at` populated)
   - `ARCHIVED` + `DeleteOnArchive=true`  → `mongo.Delete`
   - `DELETED` → dispatch by `OnUpstreamDelete` (`cascade` removes the local doc; `anonymize` upserts with the `anonymizeFields` allowlist zeroed; `keep` is a no-op for hot-tier mirrors)
3. On success, triggers **downstream recompose-ripple**: for every B view embedding the collection, finds the local docs whose join field references the changed upstream id (via `MongoDB.FindIDsByField` — index-only thanks to boot guard §8.1) and re-composes + upserts each one.
4. Failure isolation: per-view recompose errors are logged + counted on the in-memory `upstreamMetrics` counter + **persisted to `omnicore_upstream_failures`** + skipped. The Kafka offset still advances — a deterministic compose error becomes a recoverable stale doc, never a poison pill blocking the consumer group.

**Persistent failure registry — `omnicore_upstream_failures`.** Every ripple failure (discover / compose / upsert stage) is upserted into a PG table alongside the slog line and the in-memory counter:

| Column | Note |
|---|---|
| `subscription_topic`, `view_name`, `upstream_id`, `local_id`, `stage` | Natural key (UNIQUE). `local_id` is `''` on `discover` stage (the find itself failed before any local id was known). |
| `error` | Last error message — overwritten on each retry |
| `attempt` | Auto-incremented on conflict (1st sighting = 1, every reoccurrence +1) |
| `first_seen_at`, `last_attempt_at` | first_seen frozen; last_attempt refreshed on conflict |
| `resolved_at` | NULL while pending. Set to NOW() by `ResolveUpstreamFailures` when a recompose pass for the same (subscription, view, upstream_id) completes without errors |

A recompose round for `(view v, upstream_id u)` that finishes without any per-doc failure calls `ResolveUpstreamFailures(s.cfg.Topic, v.Name(), u)` — pending rows under that coordinate get marked resolved automatically. The table mirrors live state, not a monotonically-growing log.

Best-effort writes: any PG error on `RecordUpstreamFailure` / `ResolveUpstreamFailures` is logged at `slog.Warn` and discarded — never blocks the consumer. Same posture as the audit emission. Failure isolation contract preserved.

Operational queries (SQL on B's framework PG):

```sql
-- noinspection SqlNoDataSourceInspectionForFile
-- Currently stale entities (waiting on a fresh upstream event or operator reconcile):
SELECT subscription_topic, view_name, upstream_id, local_id, stage, attempt,
       last_attempt_at, error
FROM omnicore_upstream_failures
WHERE resolved_at IS NULL
ORDER BY last_attempt_at ASC;

-- Top offending views (recurring failures may indicate a deterministic Composer bug):
SELECT view_name, count(*) AS pending
FROM omnicore_upstream_failures
WHERE resolved_at IS NULL
GROUP BY view_name
ORDER BY pending DESC;
```

The in-memory `upstreamMetrics` counter remains (process-local snapshot for Prometheus / OTel polling); the PG table covers the gap the counter has — survivability across restarts + queryable list of which docs are actually stale right now.

**Retry surface — `UpstreamSubscriber.RetryPendingFailures(ctx) (retried int, err error)`.** Public runtime API: lists pending failures by `s.cfg.Topic`, deduplicates by `upstream_id`, re-runs `ripple(ctx, upstreamID)` for each unique id. Idempotent — a successful re-ripple calls `ResolveUpstreamFailures`, so the next call against the same drained slate is a no-op. The framework owns the primitive; the consumer service decides how to expose it (in-process ticker, authenticated HTTP endpoint, or admin RPC). Today the `*UpstreamSubscriber` slice is constructed inside `bootstrap.Run` and not surfaced on `Deps` — the consumer wiring path that lifts it out is expected to land alongside the second microservice in the stack, the first concrete consumer with multiple subscriptions.

**Inspection CLI — `omnicore-admin upstream-list-failures`.** Read-only triage tool. Runs in B's process (loads `microservice.${APP_PROFILE}.yaml` like `replay-all-as-events`), reads the table, prints text or JSON. Flags: `--topic`, `--view`, `--format text|json`, `--limit N`. Pure inspection: the CLI binary has no Wiring of B and cannot call `RetryPendingFailures` — actual retry stays on the runtime API above.

**Four boot guards** (`bootstrap.validateUpstreamSubscriptions`) enforce structural invariants before any subscriber goroutine spins. All four execute deterministically; every violation surfaces in a single diagnostic:

- §8.1 — every view's external `FromSchema` embed (the upstream-collection sources) declares a covering index on the join field (single-field or compound where the join field is FIRST).
- §8.2 — collection names do not collide subscription↔subscription, nor subscription↔local view (two writers on the same Mongo collection would race).
- §8.3 — every external `FromSchema` embed over collection X resolves to an `UpstreamSubscription.Collection=X` (no silently-empty embeds). View-on-view — an external `FromSchema` targeting another local `ViewDefinition.Name()` — is rejected at boot: the recompose ripple is one-hop (the subscriber consults `viewIndex.byMongoColl` populated from subscription collections only), so a change upstream of view Y would recompose Y but never re-ripple to view X that embeds Y. Drift would silently accumulate. The diagnostic suggests the supported alternatives (embed the upstream collection directly, or model the JOIN at the Postgres root via a local `FromSchema` embed).
- §8.4 — `onUpstreamDelete: anonymize` requires non-empty `anonymizeFields`.

**Schema is mandatory on every view (fatal boot enforcement).** `infra.ValidateViewSchemas(views)` (called by `bootstrap.Run`) walks every collected view at any embed nesting depth and aborts boot when the root declares no `.Schema(...)`, or when an external (type-less) embed is missing `.As(...)` — an external source has no Go type to derive the parent-side segment from. There is no optional / pass-through / schema-less mode and no `slog.Warn` advisory: every view root and every embed (`FromSchema` always carries a schema) must declare one. Local (type-anchored) embeds derive their Go segment from the schema's type; external embeds must declare it via `.As(...)`.

**Registry semantics.** The upstream-projected collection counts toward `omnicore_service_registry`'s DB-per-service marker (treated as locally-managed) but does NOT enter `omnicore_mongo_views` — an `UpstreamSubscription` has no `Version`, no `JSONSchema`, no consumer-declared indexes (only Mongo's built-in `_id` is used), so the drift-detection path has nothing to compare against. `Filter` drift is operator-owned: change the YAML + redeploy + run `omnicore-admin replay-all-as-events` against A.

**Bootstrap path** for a new B against an existing A whose Kafka retention does not cover full history: `omnicore-admin replay-all-as-events --aggregate <name>` runs in A's process (via A's `APP_PROFILE` + yaml), reads every active row, and inserts a synthetic `INSERTED` outbox event per row. Debezium picks them up; every B subscriber consumes them as if they were real INSERTs.

## HTTP status mapping

Each `Notification` declares its **Semantic** (`SemanticValidation` default; `Schema`/`NotFound`/`Conflict`/`Forbidden`/`Unauthorized`/`Unavailable`/`Internal`/`MethodNotAllowed`/`PayloadTooLarge` optional). The framework maps `Semantic → HTTP status` automatically — the typed declaration on the notification IS the registration; there is no separate setup step.

| Semantic | HTTP status |
|---|---|
| `SemanticValidation` (default) | 422 Unprocessable Entity |
| `SemanticSchema` | **400 Bad Request** (wire contract violated) |
| `SemanticNotFound` | 404 Not Found |
| `SemanticMethodNotAllowed` | **405 Method Not Allowed** (path matches, HTTP method does not) |
| `SemanticConflict` | 409 Conflict |
| `SemanticForbidden` | 403 Forbidden |
| `SemanticUnauthorized` | 401 Unauthorized |
| `SemanticPayloadTooLarge` | **413 Content Too Large** (request body exceeds the configured limit) |
| `SemanticUnavailable` | 503 Service Unavailable |
| `SemanticInternal` | **500 Internal Server Error** (recovered panic, escaped error) |

**SemanticSchema** is emitted by the `HandleCommandWithBody{,ID}` wrappers when the body does not match the Request schema (malformed JSON, missing required field, wrong type). Distinguishes "wire format violated" (consumer did not honor the HTTP contract) from "domain rejects values" (semantic Validation → 422). Wire notifications: `SchemaViolationNotification` (always Schema) + `RequiredFieldNotification{}.WithSemantic(SemanticSchema)` (same domain struct, semantic overridden per-instance).

**SemanticInternal** is emitted by `fwweb.ErrorHandler` when a panic is recovered or any non-`NotificationCarrier` error escapes a handler / middleware. Notification: `InternalServerErrorNotification` carried in context `"Server"`. The panic value and stack trace stay only on the server log (slog `LevelError`); the wire envelope carries solely the typed notification key and the translated message — never the underlying cause.

The same `InternalServerErrorNotification` envelope is also emitted by `RespondWithInternalServerError` (the helper `RespondFromResult` falls back to on `Result.Exception` — a panic caught by `pipeline.Run`'s own `defer/recover` before it reaches Fiber). On that branch the message uses the **English default** instead of going through the Translator — `RespondWithInternalServerError` stays standalone so a programming bug in the handler cannot cascade into the error-response path itself. The shape (`success`, `status`, `errors[{context:"Server", messages:[{notificationKey, semantic:"Internal", message}]}]`) is identical to the ErrorHandler path; only the language of `message` differs.

The same `ErrorHandler` also specializes three Fiber router codes — every other `*fiber.Error` code (403/418/etc.) is treated as an unknown escape and emitted as the 500 envelope by design. Services that need custom HTTP semantics MUST emit a `NotificationCarrier` with the appropriate `Semantic()` rather than calling `fiber.NewError` — the "one canonical path" rule still applies.

| Fiber code | Notification | Context | Field | Semantic → HTTP |
|---|---|---|---|---|
| 404 (route not matched) | `RouteNotFoundNotification` | `"Route"` | `METHOD /path` | `SemanticNotFound → 404` |
| 405 (method not allowed for the path) | `MethodNotAllowedNotification` | `"Route"` | `METHOD /path` | `SemanticMethodNotAllowed → 405` |
| 413 (body exceeds `BodyLimit`) | `PayloadTooLargeNotification` | `"Request"` | `METHOD /path` | `SemanticPayloadTooLarge → 413` |

Wired into `fiber.Config.ErrorHandler` automatically by `bootstrap.Run`.

Kernel notifications already declare theirs (`RecordNotFound → NotFound`, `EntityIsNotActive`/`EntityAlreadyAdded → Conflict`, `Insert/Update/DeleteNotAllowed → Forbidden`, `ServiceUnavailable → Unavailable`, `SchemaViolation → Schema`, `InternalServerError → Internal`, `RouteNotFound → NotFound`, `MethodNotAllowed → MethodNotAllowed`, `PayloadTooLarge → PayloadTooLarge`). Services declare Semantic on their notifications via method override:

```go
type EmailAlreadyExistsNotification struct{ domain.DomainNotificationBase }
func (EmailAlreadyExistsNotification) Semantic() domain.NotificationSemantic {
    return domain.SemanticConflict
}
```

`statusFromNotifications` picks the first non-Validation Semantic; if all messages are Validation, falls back to 422. `MessageDTO.Semantic` and the wire-format `ErrorMessage.Semantic` (string `"Conflict"`/`"NotFound"`/…) carry the typed identity so clients can branch UI without parsing the HTTP status code.

The Semantic enum is **transport-agnostic** — a future gRPC layer would map the same enum to gRPC codes without touching notification definitions.

## Naming conventions

| Item | Convention | Example |
|---|---|---|
| Notification struct | `<What>Notification` | `RequiredFieldNotification`, `UsernameAlreadyExistsNotification` |
| Translation key | identical to notification struct name | `"RequiredFieldNotification": "Required field."` |
| Enum description key | `<Type>.<VALUE>` | `"EntityMode.INSERT": "Inserir"` |
| Entity files (in services) | lowercase singular | `customer.go`, `order.go` |
| Generic type parameter | `T` for value, `TEntity` for entity types | `Result[T]`, `Repository[TEntity]` |

## Quick reference — where to add things

| Need | File |
|---|---|
| New domain notification | `domain/notification_core.go` (kernel: `RequiredField`, `RecordNotFound`, `EntityIsNotActive`, `EntityAlreadyAdded`, `Insert/Update/Delete/Archive/UnarchiveNotAllowed`, …) |
| New application notification | `application/notifications/core.go` |
| New translation key | `application/translation/ptbr.go` + `eng.go` + `esp.go` + `fra.go` + `deu.go` + `ita.go` + `nld.go` (keep all 7 built-ins in sync) |
| New value object | new file in `domain/` |
| New respond helper | `web/response.go` |
| New notification with custom HTTP status | service notification overrides `Semantic() domain.NotificationSemantic` (returns one of `SemanticConflict` / `SemanticNotFound` / `SemanticForbidden` / `SemanticUnauthorized` / `SemanticUnavailable` / `SemanticInternal` / `SemanticMethodNotAllowed` / `SemanticPayloadTooLarge`) |
| New notification carrying runtime variables (length, count, amount per tenant) | declare the struct with `tvar:"<name>"` tags on the exported fields the message should substitute. Catalog entries use the matching `{<name>}` placeholders. Pipeline auto-renders via `MessageVars(msg)` → `Translator.Render`. Escape hatch for unexported/computed values: declare a `TranslationVars() map[string]string` method (replaces tag extraction). See "Parameterized notifications" |
| Per-emit override on translation variables (same notif type, different vars per call site) | use `r.AddNotificationWithVars(field, n, vars, value...)` instead of `r.AddNotification`. The per-emit map merges on top of any tag-derived vars; per-emit wins on key collision |
| Parameterize the context label itself (`"UserOf{tenantId}"`) | declare the parameterized name on `domain.NewNotificationContext(name)` and call `ctx.SetVars(map[string]string{"tenantId": "..."})`. Catalog entry uses `{tenantId}` placeholder. Scoped contexts inherit from the root automatically |
| New audit field | `infra/audit/event.go` (`AuditEvent`/`FieldChange`/`ChildEvent`) — see "Audit event shape" |
| Want in-TX side effect on Auto path | declare `BeforeCommit(ctx *AppContext, t T, id domain.ID, tx persistence.TxHandle) error` (or `AfterBegin(ctx, t, tx)`) on the Cmd. The Auto handler detects it via type assertion against `persistence.BeforeCommitHookProvider[T]` / `AfterBeginHookProvider[T]` and threads it as a `WriteOption[T]` to the Repo write call. Fires INSIDE the framework's TX — non-nil error rolls everything back. |
| Want in-TX side effect on manual path | pass `persistence.WithBeforeCommit[T](fn)` (or `persistence.WithAfterBegin[T](fn)`) as an option on `repo.Scope(ctx, opts...).Method(valid)`. Same persister slot the Auto path fires; closure shape and TX semantics are identical. |
| Want to read or write state inside the hook's TX | declare a port in `application/` (or `domain/`) whose method takes a `persistence.TxHandle` parameter, e.g. `QuotaPort.AssertTenantQuota(ctx, tx, tenantID) error`. Implement the port in `infra/` — the adapter calls `fwinfra.UnwrapPgxTx(tx)` to recover the live `pgx.Tx`, executes SQL, owns the table name. Inject the port on the Cmd / handler and call it from the hook closure. `TxHandle` is a sealed marker with no public methods, so the hook cannot pronounce SQL directly — the port is the single authorized path. |
| Want compile-time safety against typo in hook method name | declare `var _ persistence.BeforeCommitHookProvider[*T] = (*Cmd)(nil)` (or the AfterBegin variant) at the bottom of the Cmd file. Catches misspelled / mistyped method signatures at `go build` time; the framework does not enforce it. |
| Declare aggregate child type | root entity implements `AggregateChildren() []AggregateValueObject` returning sample instances (the domain boundary). Table/columns/FK are declared in the child `TableSchema` via `root.Child(fwinfra.NewTableSchema[Child]("table").PK("ID","id").FK("col").Field(...)...)` — see "Schema mapping" |
| Drop archived rows from the Mongo projection (hot-tier read side) | `fwinfra.View("users").DeleteOnArchive().Root("users").EmbedMany(...)` — opt-in per view. Default keeps archived rows so Mongo mirrors PostgreSQL symmetrically. Cascade: the flag governs root + all embeds (no per-embed override). Reader still defaults to hiding archived; consumer opts in via the existing `IncludeArchived` path (`?includeArchived=true`) — and that path returns 404 on a `DeleteOnArchive` view because the document is absent. |
| Materialize an upstream service's events into a local Mongo collection (cross-service composition) | declare an `UpstreamSubscription` in `microservice.<profile>.yaml` (canonical) or `Wiring.UpstreamSubscriptions` (manual lifecycle / tests) with `topic`, `collection`, `filter`, `onUpstreamDelete`, optional `anonymizeFields`. Embed in a view via an external `fwinfra.FromSchema(fwinfra.NewExternalSchema("collection")…).On("join_field").As("Seg")`. Boot guard §8.1 requires a covering index on the join field. The framework runs one `UpstreamSubscriber` per declared entry and ripples recompose to every embedding view on each upstream event. See "Cross-service composition" under [Read side (CQRS)](#read-side-cqrs). |
| Declare MongoDB indexes on a view | `fwinfra.View(...).Indexes(fwinfra.Index("email").Unique(), fwinfra.Compound("email","created_at"), fwinfra.Index("deleted_at").Partial(fwinfra.Exists("deleted_at", false)), fwinfra.TextIndex("name","email").DefaultLanguage("portuguese"))` — single-field, compound, partial, sparse, TTL, text, 2dsphere, hashed. Materialized by `bootstrap.Run` via `fwinfra.ApplyMongoSpecs` between `collectViews` and `SyncEngine.Start`. Idempotent steady state; strict-on-divergence default; `OMNICORE_MONGO_FORCE_REBUILD=true` env var as operator escape (drops divergent indexes only; never collections). See "Declarative Mongo surface". |
| Declare a `$jsonSchema` validator on a view's collection | `fwinfra.View("users").JSONSchema(bson.M{"bsonType":"object","required":[]string{"_id","email"}}).JSONSchemaValidationLevel(fwinfra.ValidationLevelStrict).JSONSchemaValidationAction(fwinfra.ValidationActionError)` — defaults to `strict` / `error`. Framework uses `createCollection` for fresh collections and `collMod` for existing ones (idempotent). |
| Declare collection-level features (collation / capped / time-series) | `fwinfra.View(...).Collation(&fwinfra.CollationSpec{Locale:"pt",Strength:1}).Capped(&fwinfra.CappedSpec{SizeBytes: 1<<30}).TimeSeries(&fwinfra.TimeSeriesSpec{TimeField:"ts",Granularity:"seconds"})`. `Capped` ⊕ `TimeSeries` (boot rejects both). Collation is immutable on existing collections — divergence aborts boot; operator owns the migration. |
| Unlock `?search=` on a view (text index required by Mongo $text) | declare `fwinfra.TextIndex("field1", "field2", ...)` on the view's `.Indexes(...)`. Without it, the view's `?search=` requests crash at runtime with Mongo error 27 ("text index required for $text query"). At most one `TextIndex` per view — Mongo's limit. |
| Emit validation msg (root OR AVO) | inside `BuildRules`: `r.AddNotification("GoIdentifier", n)` — same shape on both. To echo the rejected input: `r.AddNotification("Email", n, u.Email)`. For messages with Err/FuncName/Override: `r.AddNotificationMessage(NotificationMessage{...})` |
| Implement AVO validation | `func (a Address) BuildRules(svc Service, r *Rules)` — same shape as `Entity.BuildRules`, only loses `actionName` |
| Rename a wire field for the whole entity (stable rule) | `u.AddFieldNameAlias("Email", "primaryEmail")` on `*BaseEntity` (typically in the constructor). Applied automatically inside `checkAllNotifications` via `applyFieldAliases` — every message about `Email` surfaces as `primaryEmail` on the wire, regardless of which handler triggered it |
| Rename a wire field for one request (conditional) | `ctx.ChangeFieldName(resolvedOldName, newName)` — imperative in a manual handler; sets `Override` on matching messages without losing Path/FieldName |
| Expose a translated human label for a field (notifications + audit) | declare `label:"<catalogKey>"` on the struct field (e.g. `ZipCode string \`label:"AddressZipCodeField"\``) and add the catalog entry in each `translation.Module`. `Rules.AddNotification` reads the tag at emit; `MessageDTO.FieldLabel` carries the rendered string in the actor's locale; `FieldChange.FieldLabelKey` carries the raw key for audit (render-at-read). Missing catalog entry → raw key on wire + `slog.Warn` once per `(lang, key)`. Independent of `AddFieldNameAlias`/`ChangeFieldName` — alias renames `FieldName`, label translates `FieldLabel` |
| Render an audit row's field labels at read time | `audit.RenderLabels(ev, deps.Translator, lang)` for a typed `*audit.AuditEvent` held in-process; `audit.RenderLabelsInJSON(doc, deps.Translator, lang)` for a `map[string]any` parsed off the `audit_events.jsonb` payload (BI / SQL consumers). Both mutate in place: walk `changes` + `children.<typeName>[].changes`, pop `fieldLabelKey`, and write `fieldLabel` rendered via `Translator.Render`. Snapshot blocks untouched. Catalog miss → raw key + `slog.Warn` once per `(lang, key)` |
| Read an audit row by id or timeline by aggregate | `audit.FindByID(ctx, exec, uuid)` returns `(*AuditEvent, error)`; miss surfaces as `audit.ErrAuditNotFound`. `audit.FindByAggregate(ctx, exec, entityType, aggregateID)` returns `[]*AuditEvent` newest-first (index-served by `audit_events_entity_timeline_idx`). Both consume the minimal `pgExec` interface (`*pgxpool.Pool` / `*pgxpool.Conn` / `*pgx.Conn` / `pgx.Tx`). Compose with `audit.RenderLabels` for translated read in three lines |
| 1-message error in domain | `domain.SingleNotificationError(ctx, field, n)` / `domain.NotFoundError(ctx, field, value)` / `domain.FieldErrorWithCause(ctx, field, cause, n)` |
| 1-message error in infra / application | `infra.SingleNotificationError(...)` / `infra.FieldErrorWithCause(...)` / `exception.SingleNotificationError(...)` / `exception.FieldErrorWithCause(...)` |
| Get AppContext in a Fiber handler | `fwweb.AppContext(c)` (already populated by `bootstrap.Run`'s default middleware) |
| Get authenticated principal | `ctx.Identity()` returns `*configuration.Identity` (or nil for public routes / `auth.mode=disabled` / pre-middleware). Fields: `Subject`, `Issuer`, `ExpiresAt`, `Claims map[string]any` (raw JWT claims) |
| Get raw verified bearer of the request | `ctx.BearerToken()` returns the raw JWT the `AuthMiddleware` verified, or `""` when no bearer is attached (public route, `auth.mode=disabled`, pre-middleware, failed authentication). Consumed exclusively by the `forward-bearer` httpclient auth provider so it can propagate the inbound user's credential downstream. Services should keep reading `Identity()` for principal data — the raw token carries no parsed information |
| Read actor in audit/event sinks | `ctx.ActorSubject()` → `sub` or `"anonymous"`; `ctx.ActorIssuer()` → `iss` or `""`; `ctx.ActorClaims()` → fresh copy of raw claim map or nil. These three methods are on `persistence.RequestContext` (satisfied by `*AppContext`) so the audit builders and `SlogPublisher` read them through the request-scoped interface |
| Configure which claims appear in audit | `auth.auditClaims: [tenant_id, roles, …]` in `microservice.<profile>.yaml`. Empty list → `actorClaims` block omitted. Audit-only — events never widen the claim surface |
| Mark a route as public (bypass auth) | declare `METHOD /path` under `auth.publicRoutes` in `microservice.<profile>.yaml`. Exact match — no globs |
| Auth failure notifications | `application/notifications/core.go`: `MissingAuthorizationNotification`, `InvalidTokenNotification`, `ExpiredTokenNotification` — all `SemanticUnauthorized → 401`. Translated by the catalogs |
| Authz failure notifications | `application/notifications/core.go`: `MissingPermissionNotification` (Layer 1 gate), `TenantMissingNotification` (Layer 3 middleware), `TenantMismatchNotification` (Layer 3 service code) — all `SemanticForbidden → 403` |
| Gate a route on a permission (Layer 1) | `fwopenapi.RequirePermission("users:write")` as a variadic `MountOption` on `openapi.Mount` / `openapi.MountRaw`. Same syntax across canonical, manual-with-pipeline, and raw paths. Boot panic on `Public:true + RequirePermission`, on duplicate `RequirePermission` in the same call, on caller-side wildcards (`users:*`), and on empty/no-colon strings |
| Test if the principal has a permission (Layer 2 / inline branching) | `ctx.Identity().HasPermission("users:write")`. Returns false on nil Identity; panics on caller-side wildcards. Layer-2 rules typically populate runtime authz fields from inside `Command.ApplyTo(ctx, t)` and consult them in `BuildRules` (`u.RequestingPrincipalIsAdmin = id.HasPermission("users:admin")`) |
| Read the tenant claim (Layer 3) | `ctx.Identity().TenantID()`. Reads the configured claim (default `tenant_id`; configurable via `auth.authorization.tenant.claim`). Returns "" on nil/missing/multi-valued shapes |
| Enable authorization in yaml | `auth.authorization.enabled: true` (master switch — when false, runtime gate no-ops; identity helpers still work). Requires `auth.mode: jwt`. Optional `permissionsClaim:` to override the claim name; optional `tenant: {enabled, claim, required}` sub-block for Layer 3. Unknown keys under `authorization` / `tenant` abort the boot |
| Boot-time authz enforcement | When `auth.authorization.enabled: true`, bootstrap scans the Registry AFTER `Wiring.BeforeServe` and BEFORE `openapi.Register` — panics with the offender list when a non-public route lacks `RequirePermission(...)` |
| Boot-time route-registration enforcement | When `Wiring.OpenAPI != nil`, bootstrap compares `app.GetRoutes(true)` vs `Registry.Operations()` and panics on any Fiber route registered outside `Mount`/`MountRaw`. Active regardless of `authorization.enabled` — the canonical channel is structural to the framework |
| Extra request-scoped state beyond Identity | service middleware AFTER `fwweb.AppContextMiddleware`; uses `ctx.Set(key, val)` |
| Trivial CRUD without writing a handler | import `omnicore/application/handlers`: `Insert/Update/PartialUpdate/Archive/Unarchive/DeleteCommandHandler[T, *Cmd, TResult]` — the Cmd declares `ToEntity`/`ApplyTo` AND `FromEntity(ctx, T) TResult` as methods; no `Project` field on the handler |
| Fiber route wrapper — endpoint WITH body | `fwweb.HandleCommandWithBody(pipe, requests.XxxRequest{}, requests.XxxResponse{}.FromResult, h, status)` (POST) or `fwweb.HandleCommandWithBodyID(...)` (PUT/PATCH). Consumes `RequestDTO` (in `web/requests/`) that produces Command via `ToCommand()`. |
| Fiber route wrapper — endpoint WITHOUT body (Archive/Unarchive/Delete) | `fwweb.HandleCommandWithID(pipe, fwresponses.NoBody, h, status)` (or your own `responseProjection` for endpoints that want to expose data) |
| Endpoint with no wire body (typical Archive/Unarchive/Delete) | Cmd declares `FromEntity` returning `fwresults.None{}` + wrapper takes `fwresponses.NoBody`. Wrapper detects `responses.None` at runtime and emits the success envelope WITHOUT a "data" field |
| Fiber route wrapper — paged list GET | `fwweb.HandleQueryWithParams(pipe, requests.FindXxxRequest{}, fwresponses.AutoFromDoc[requests.FindXxxResponse], handler)`. The projector (third arg) is mandatory — `fwresponses.AutoFromDoc[R]` is the tag-driven default; pass `fwresponses.RawDoc` for raw view-doc passthrough, or a consumer-declared `R{}.FromDoc` method for custom projection logic. The Request DTO carries `query:"..." filter:"..."` tags that the wrapper consumes via reflection (cached by `reflect.Type`) for allowlist enforcement. |
| Fiber route wrapper — by-id GET | `fwweb.HandleQueryWithID(pipe, requests.FindXxxByIDRequest{}, fwresponses.AutoFromDoc[requests.FindXxxByIDResponse], handler)`. Only `?includeArchived` recognized; any other query key produces 400. Same projector rules as the params wrapper. |
| Manual query handler — validate query string against an allowlist DTO | Construct once at Mount: `parser := fwweb.NewQueryParser[ReqDTO, RespDTO]()` — runs the same boot scan `HandleQueryWithParams` runs (sparse-render guard when `?fields=` is opted in, slog.Warn for `?sort=` opt-in, wire→doc projection schema). Per request: `crit, badField, ok := parser.Parse(c); if !ok { return fwweb.RespondSchemaViolation(c, pipe, badField) }`. Use `fwweb.ParseCriteria(c, requestDTO)` instead when the Response is RawDoc (`map[string]any`) OR the Request does not opt into `?fields=` / `?sort=` — same reflection-based schema, but the projection schema is nil so reserved keys land in pass-through mode (no allowlist, no wire→doc translation, no `_id:0` auto-exclusion). |
| Bind a URL segment to a Request field declaratively | Declare `path:"name"` on the field (any wrapper with a Request DTO populates it before `ToCommand`/`ToQuery`). On hand-rolled `fiber.Handler` closures, call `if badField, ok := fwweb.BindPath(c, &req); !ok { return fwweb.RespondSchemaViolation(c, pipe, badField) }` before `Bind().Body()` / `ParseCriteria`. Supported types: string, ints, uints, floats, bool, uuid.UUID, domain.ID. |
| Ensure an auto handler's path ID is populated | Either use a `:id`-canonical wrapper, set `cmd.SetPathID(r.SomeField)` in `ToCommand` (where `r.SomeField` is a `path:`-tagged field), or pull it from a non-path source (JWT subject, header). `handlers.RequirePathID` panics with diagnostics if empty at handler entry. |
| Manual query handler — emit a paged envelope | `fwweb.RespondPaged(c, fiber.StatusOK, page, fwresponses.AutoFromDoc[ResponseDTO])` (one-liner with the tag-driven default) OR `items, pagination := fwweb.ProjectPage(page, fn); fwweb.Respond(c, fwweb.Response{Success: true, Data: items, Pagination: pagination, ...})` (build the envelope by hand when adding sidecar fields). |
| Sparse responses on a paged GET endpoint (consumer picks a subset via `?fields=`) | declare `Fields *string \`query:"fields"\`` on the Request DTO + every field on the Response (recursively, including nested struct + slice element types) must be `*T` (or a slice/map) with `,omitempty`. `HandleQueryWithParams` boot guard panics with a path-by-path diagnostic on violation; runtime validates tokens against the Response's wire paths, translates the wire path to the Go field path (the reader maps Go→column via the view `TableSchema`), auto-adds `_id:0` when `id` is not in the requested list. Unknown tokens emit 400 `SchemaViolationNotification{field:"fields[<bad>]"}`. See "Sparse responses via `?fields=`" |
| Restrict sortable fields on a paged GET endpoint via `?sort=` | declare `Sort *string \`query:"sort"\`` on the Request DTO. When the Response is a struct, `HandleQueryWithParams` validates each comma-separated token (optional `-` prefix → descending) against the Response's declared wire paths and translates the wire path to the Go field path (the reader maps Go→column via the view `TableSchema`). Unknown tokens emit 400 `SchemaViolationNotification{field:"sort[<token>]"}` — the `-` prefix is preserved in the rejected token. A single `slog.Warn` fires at boot per endpoint listing every sortable wire path, so the operator can compare it against the Mongo view's `.Indexes(…)` declaration (`fwinfra.Index` / `fwinfra.Compound`). No boot guard on the Response shape — `?sort=` only consumes the path map. RawDoc Responses and `fwweb.ParseCriteria` callers get pass-through (tokens verbatim, no allowlist). See "Sortable response paths via `?sort=`" |
| Count-only response on a paged GET endpoint via `?onlyTotal=true` | declare `OnlyTotal *bool \`query:"onlyTotal"\`` on the Request DTO. The reader short-circuits to `CountDocuments(filter)` and `RespondPaged` emits an envelope with no `data` and `pagination = &TotalOnlyPagination{Total: N}` — no `has_next`/`has_prev`/cursors zero-value noise. Conflict matrix `{fields, sort, limit, after, before}` rejects at the schema layer with 400 `SchemaViolationNotification{field:"onlyTotal[<conflict>]"}`. Filter leaves + `?search=` + `?includeArchived=` stay valid (counting a filtered subset is the canonical use case). Manual handlers via `fwweb.ParseCriteria` get the same treatment; the downstream branch lives on `queries.Page.OnlyTotal`. See "Count-only mode via `?onlyTotal=true`" |
| Keyset pagination via `?after=` / `?before=` | declare `After *string \`query:"after"\`` and `Before *string \`query:"before"\`` on the Request DTO. Cursors are opaque `base64(URLEncoding)` of `{"v":1,"k":[<sort_values..>,"<_id>"]}`. The reader always appends `_id` ASC as the stable tiebreaker; `?sort=` fields layer in front. `?before=` queries Mongo in inverted sort and reverses the slice in Go so the caller sees canonical order. Strict 400 `SchemaViolationNotification` on: malformed cursor (bad base64/JSON, `v != 1`), cursor↔sort tuple-length mismatch, `?after=` + `?before=` together. See "Keyset pagination via `?after=` / `?before=`" |
| Cap `?limit=` per view (read-side ceiling) | declare `fwinfra.View("users").MaxLimit(500)` for a per-view override OR set `query.maxLimit: 200` in the yaml for a service-wide default. Framework fallback when neither is declared: 100. Resolution cascade at read time: per-view > yaml > framework constant. `MaxLimit(N)` does NOT participate in `RebuildHash` / `ArtifactHash` — operational state, not projection shape. `?limit=N` above the resolved ceiling rejects with 400 `LimitExceededNotification{field:"limit", value:"<ceiling>"}` (translatable per language). Consumer omitting `?limit=` receives up to the resolved ceiling — no separate hardcoded default page size. |
| Reject malformed pagination params strictly | the wrapper rejects with 400 `SchemaViolationNotification`: `?limit=abc`/`0`/`-5`, `?after=<corrupt>`, `?before=<corrupt>`, `?after=<c>&before=<c>` (mutually exclusive), `?sort=name&after=<no-sort-cursor>` (tuple/sort mismatch), context-change mid-navigation (cursor's `h` hash differs from current `HashContext(filter, sort, search, includeArchived)`). Cursor schema versioning (`v:1`) — a future bump rejects older cursors with the same envelope. |
| Tag-driven projection of a Mongo view doc | `fwresponses.AutoFromDoc[R]` — generic projector that maps the view doc onto a typed Response struct using `json:"<wire>"` tags only, keying into the doc by the **Go field name** (the reader already returned a Go-keyed doc). Recursive through nested structs/slices/pointers; normalizes top-level `_id → id` and nil slices → empty typed slices. Default projector for typed Responses; drop in place of a manually-written `FromDoc` whenever the projection is mechanical. |
| Raw view-doc passthrough on a GET | `fwresponses.RawDoc` — identity projector `func(map[string]any) map[string]any`. Use when the view doc shape IS the wire contract and a typed Response would just mirror it. Same role on read side that `fwresponses.NoBody` plays on the no-body command path. |
| Request DTO (JSON wire format) | `web/requests/xxx_user_request.go` in the service. Struct with `json:"..."` tags + method `ToCommand() *XxxCommand` (1:1 assignment). Shape mirrors the Command — pointer types for optionals. Co-located in the same file: `XxxUserResponse` (also wire format) + `FromResult(XxxUserResult) XxxUserResponse` |
| Result DTO (application-layer projection of the persisted entity) | `application/commands/xxx_user.go` in the service, **co-located with the Command**. Plain Go struct, **no methods** — projection logic lives on the Cmd via `func (c *XxxUserCommand) FromEntity(ctx *AppContext, t T) XxxUserResult`. Wrapped by the wire `XxxUserResponse` |
| Command with ID via path (Update/Patch/Delete/Archive/Unarchive) | embed `pipeline.CommandBaseWithID` (vs `CommandBase` for Insert) |
| PUT endpoint (full replace, strict body) | `&handlers.UpdateCommandHandler[T, *Cmd, TResult]{...}` + `Cmd impl pipeline.UpdateCommand[T, TResult]` (`ApplyTo(ctx, T)` + `FromEntity(ctx, T) TResult`) |
| PATCH endpoint (partial body, optional fields) | `&handlers.PartialUpdateCommandHandler[T, *Cmd, TResult]{...}` + `Cmd impl pipeline.PartialUpdateCommand[T, TResult]` (`ApplyPartiallyTo(ctx, T)` + `FromEntity(ctx, T) TResult`) |
| Custom strict handler (imposing full body) | embed `pipeline.FullBody` in the handler struct — wrapper enforces presence of all exported fields |
| Repository with 5 writes + constraint mapping | embed `fwinfra.BaseRepository[T]` + declare `NewEntity func() T` (mandatory) + optional `Constraints map[string]ConstraintBinding` |
| FindByID of aggregate (root + children) — auto-scan default | `fwinfra.NewAggregateLoader[T](pg, factory).WithSchema(schema)` (children come from the schema's `Child(...)` declarations) + `loader.FindOne(ctx, criteria.ByID(id))` (or embed `BaseAggregateRepository[T]` + `WithSchema(schema)`, which promotes `FindByID`) |
| Load an aggregate by any non-id field / a list by criteria | `loader.FindOne(ctx, criteria.Where(criteria.Eq("Email", v)))` (one or `RecordNotFound`; >1 errors) · `loader.FindAll(ctx, criteria.Where(expr).OrderByDesc("CreatedAt").Limit(50))` (children batched via `fk IN`). Archived scope via `.IncludeArchived()` / `.OnlyArchived()` on the `Query`. See "Entity search engine (`criteria`)" |
| FindByID with manual scanner (non-trivial queries) | same fluent API + `WithRootScanner(fn)` and/or `WithChildScanner(typeName, fn)`. Coexists with auto per typeName. A manual root scanner used with `FindOne`/`FindAll` must populate the id (scan it + `SetID`). |
| Load microservice config from YAML | `bootstrap.LoadConfig()` reads `APP_PROFILE` env (required, non-empty; `dev` and `prd` are canonical, extra variants like `prd-pem` accepted) and loads `./microservice.${APP_PROFILE}.yaml` (override via `OMNICORE_CONFIG_PATH`); interpolates `${VAR:default}`; validates required; rejects boot when `auth.mode=disabled` under non-`dev` profile |
| Manage migrations (auto-run, status, force, down) | `migration.New(pg.Pool(), dir)` in `omnicore/infra/migration`. `bootstrap.Run` calls `Up` automatically when `cfg.Migrations.AutoRun=true`; the default is profile-aware (dev=true, else=false). Strict mode in non-dev profiles aborts boot with a rich diagnostic naming pending versions when drift is detected — `mgr.Pending(ctx)` exposes the same list for tooling |
| Reconcile a Mongo view shape change (drift detection + rebuild) | `bootstrap.Run` calls `infra.DetectViewDrift` between `ApplyMongoSpecs` and `SyncEngine.Start`; dispatch over the 8-case matrix. `SyncEngine.ExecuteRebuild` runs the rebuild sequence on a pinned `pgxpool.Conn` (advisory lock + status column transitions + aggregation cleanup + compose+upsert + orphan reconciliation + EndRebuild). Knobs in `mongo.rebuild` yaml block: `autoRun: check|true|false`, `orphan: delete|warn`, `allowDowngrade: false`. Default in non-dev: `autoRun=check` — every non-None decision aborts boot with the matching §14 diagnostic naming the manual SQL reconcile. See "Mongo schema evolution" for the full matrix |
| Declare service capability (routes + read side) | implement `bootstrap.Feature` (only `Mount`) or `bootstrap.ReadableFeature` (`Mount` + `Views() []*ViewDefinition`). Bundle in `Wiring.Features`. Bootstrap calls `Mount` in order; aggregates Views from `ReadableFeature`s into the SyncEngine; rejects boot if 2 features declare the same view name |
| Health/liveness probe | **automatic** — framework registers `GET /health` (`{"status":"ok"}`) in every service. For a custom check (DB ping, dependencies), expose another route (`/healthz`, `/ready`) via feature or `BeforeServe` |
| Call an external HTTP service from a handler | declare `httpClient:` block in `microservice.<profile>.yaml`; receive `d.HttpClient` (`*httpclient.HttpClient`) on the feature; forward to an `infra/external/<svc>.go` service struct that hides the call surface. Application handlers depend on the service struct, never on `httpclient`. (Skeleton today; full Call surface arrives with the runtime phase.) |
| Enable OpenAPI + Swagger UI | set `Wiring.OpenAPI = &openapi.Config{Title, Version, Description}`. Bootstrap constructs `*openapi.Registry`, threads it through `Deps.OpenAPIRegistry`, auto-registers `GET /openapi.json` + `GET /docs`, and adds the docs routes to the AuthMiddleware `publicRoutes` when `auth.mode=jwt`. nil = OpenAPI disabled (no spec routes, `Deps.OpenAPIRegistry = nil`, `Mount` / `MountRaw` short-circuit to Fiber-only) |
| Add a language selector dropdown to Swagger UI | set `LanguageSelector: true` on `openapi.Config`. Bootstrap auto-fills `Languages` from `Wiring.Translations` (dedup by `configuration.Language`; `LangENG` rotated to position 0 when present so English is the dropdown's default; declaration order otherwise preserved). Explicit `Languages: []LanguageOption{...}` on `openapi.Config` overrides auto-discovery — no rotation, no dedup |
| Mandatory wiring guards (boot-time `validateWiring`) | (1) at least one Feature OR a BeforeServe hook (`/health` alone does not count); (2) at least one `translation.Module` on `Wiring.Translations` — the entire stack consumes the Translator (notifications, error envelopes, audit fields). Both checks fire under `bootstrap.Run`; custom `Serve` flows call `validateWiring` themselves if they want the same guards |
| Document a canonical (Auto handler) route | replace `fwweb.HandleCommandWithBody(...)` with `fwweb.HandleCommandWithBodySpec(...)` (same signature; returns `(fiber.Handler, openapi.RouteSpec)`). Pass through `openapi.Mount(d.OpenAPIRegistry, group, method, path, handler, spec, openapi.Doc{Summary, Tags, ...})`. Same pattern for `WithBodyID`/`WithID`/`QueryWithParams`/`QueryWithID` |
| Document a manual handler that still parses a typed Request DTO | call `openapi.Mount(d.OpenAPIRegistry, group, method, path, handler, openapi.RouteSpec{RequestType: reflect.TypeOf(req), ResponseType: reflect.TypeOf(resp), SuccessStatus: status}, openapi.Doc{Summary, Tags})`. Same registration path as the canonical wrapper siblings — only the RouteSpec is hand-rolled because the wrapper isn't there to populate it |
| Document a manual paged GET (emits via `fwweb.RespondPaged`) | `fwopenapi.RouteSpecOfPaged[ReqDTO, RespDTO](fiber.StatusOK)` sibling of `RouteSpecOf` — sets `Paged:true` so the spec assembler renders `data: []RespDTO` + `pagination: $ref(PaginationInfo)` instead of singular `data`. Canonical `HandleQueryWithParamsSpec` already sets the flag; only manual paged routes need this. Pairing `Paged:true` with `fwresponses.None` is rejected at `Mount` with a panic. |
| Document a route without a typed Request DTO (Whoami, vendor showcases) | call `openapi.MountRaw(d.OpenAPIRegistry, group, method, path, handler, openapi.RawSpec{Summary, Tags, Parameters, RequestBody, Responses, Public, Hidden, Deprecated})`. Declare parameters/body/responses inline; no reflection runs |
| Declare a typed `RawSpec.Responses` entry without `reflect.TypeOf` | `openapi.ResponseOf[T](description)` returns a `ResponseSpec{Description, Type: reflect.TypeOf((*T)(nil)).Elem()}` — symmetric to `RouteSpecOf[TReq, TResp]` on the canonical side. For responses that need `ContentType` / `Examples`, capture the helper output in a var and assign the extra fields before storing it in the map |
| Add a concrete example value to a Request/Response DTO field | annotate the struct field with `example:"<value>"` (e.g. `Name string \`json:"name" example:"Alice"\``). Supports scalars + `uuid.UUID` / `domain.ID` / `time.Time`. Nested struct fields propagate their own tags. Composite types on the same field (struct / slice / map) → boot panic — use `Doc.RequestExamples` / `Doc.ResponseExamples` instead (map-based path for N exemplos or shapes a single tag cannot express). Parse failure → boot panic naming the struct + field |
| Declare N exemplos per route (rich case) | populate `openapi.Doc.RequestExamples map[string]openapi.Example` and `openapi.Doc.ResponseExamples map[int]map[string]openapi.Example` on the `Mount` call. For `MountRaw`, use `RequestBody.Examples` and `ResponseSpec.Examples`. Each `Example` carries Summary / Description / Value (any JSON-marshalable Go value OR `json.RawMessage`). Validated at boot — typo or shape mismatch panics with route + slot diagnostic |
| Success-status examples vs error-status examples | success-status entries are wrapped automatically in the canonical Response envelope (`success:true`, `status`, `description`, `data:<value>`) — consumer fornece só o inner `data`. Error-status entries (and every `RawSpec.Responses[*].Examples`) render verbatim — consumer fornece o envelope completo (typically `success:false` with custom `errors[]`) |
| Auto-merge of framework default in error responses | when consumer declares any examples on an error status with a framework default (400/401/404/422/500), the canonical entry auto-merges under the key `"default"`. Override by declaring `"default"` explicitly; remove entirely by declaring `"default": openapi.Example{}` (empty Value). When consumer declares nothing for the status, the singular `example` (pre-Phase-2 shape) is preserved — back-compat |
| Reuse the framework's canonical error envelope | `openapi.DefaultErrorExample(status int) (openapi.Example, bool)` returns the canonical entry; `openapi.DefaultErrorExamples() map[int]openapi.Example` returns a fresh copy of every status the framework covers (400/401/404/422/500). Useful for customizing the default (override Description) or for assembling a Responses map programmatically |
| Exclude a route from the spec entirely | set `Doc.Hidden = true` (canonical) or `RawSpec.Hidden = true` (raw). The route still registers on Fiber — only the documentation surface omits it. Use for internal upstream/scaffolding routes (`/echo/*` showcase producers) |
| Mark a route as public (no bearerAuth in spec) | set `Doc.Public = true` (canonical) or `RawSpec.Public = true` (raw), OR list `METHOD /path` in `auth.publicRoutes` of `microservice.<profile>.yaml`. Either path is sufficient; both feed the same spec-assembly check |
| Emit a cross-service integration event | declare `integration.publishes.events.<key>` in `microservice.<profile>.yaml` (with `eventType`, optional `aggregate`, optional `version`) + call `fwintegration.Dispatch(ctx, key, payload, opts...)`. From inside a `BeforeCommit` hook pass `fwintegration.WithTx(tx)` so the row lands in the same TX as the data write + outbox + audit. Standalone events omit `WithTx`; the framework writes the row via single-statement autocommit on the PG pool |
| React to a cross-service integration event | declare `integration.subscribes.<source>.events.<key>` in YAML + implement `bootstrap.IntegrationFeature` on the feature struct. `MountReceivers(reg, deps)` registers via `reg.From(source).On(eventKey, sampleDTO, handler)`. The handler is the SAME `pipeline.Handler[TCmd, TResult]` HTTP routes consume; the sample DTO carries `ToCommand()` (same convention web `Request.ToCommand` follows) |
| Retry pending integration / upstream failures | consumer service exposes admin HTTP route (canonical pattern: `POST /admin/retries/integration` walks `d.IntegrationRegistry.Receivers()` calling `Receiver.RetryPendingFailures(ctx, exec, pipe, logger)`; `POST /admin/retries/upstream` loops `d.UpstreamSubscribers` calling `UpstreamSubscriber.RetryPendingFailures(ctx)`). Both behind `RequirePermission("admin:retry")`, under Swagger tag `Admin` |

## Integration events

The cross-service async-messaging surface — the canonical write-side counterpart to the sync `httpclient` + the read-side `UpstreamSubscription` paths. Producers emit typed events into the `integration_events` table (atomic with the data write when invoked from a `BeforeCommit` hook closure with `WithTx(tx)`); subscribers consume Kafka messages via the framework's `Receiver` registry, route each payload through the SAME `pipeline.Handler[TCmd, TResult]` HTTP routes consume, and dedup per consumer group via `omnicore_integration_processed`.

### Producer

```go
fwintegration.Dispatch(ctx, "userActivated", UserActivatedPayload{Email: email},
    fwintegration.WithTx(tx),                  // atomic with data row + outbox + audit
    fwintegration.WithAggregateID(userID),     // required when YAML declares `aggregate:`
    fwintegration.WithCorrelation(corrID),     // optional — defaults to ctx.CorrelationID()
    fwintegration.WithCausation(causID),       // optional — defaults to ctx.CausationID()
)
```

YAML:

```yaml
integration:
  publishes:
    events:
      userActivated:
        eventType: UserActivated   # wire header value
        aggregate: User            # optional — omit for standalone events
        version: 1                 # optional — defaults to 1
```

Lazy validation: an unknown `eventKey` at the first Dispatch call surfaces as `ErrIntegrationEventNotConfigured` — same posture httpclient adopts for unknown service/endpoint references. Matches the framework's "validate at call time, not at boot" rule for emission paths that may stay empty on consumer-only services.

**`WithTx` is the atomicity hook.** Threaded from inside a `BeforeCommit` hook closure where the framework already opened the TX for the entity write. The `integration_events` row lands in the same Postgres TX as the data row + outbox + audit; any error from Dispatch aborts the entire TX (the persister's `defer tx.Rollback(ctx)` runs). Without `WithTx`, the row commits via single-statement autocommit on the package's PG pool — independent of any other write.

Framework auto-fills `event_id` (`uuid.New()` per row), `thread_id` (`ctx.ID()`), `actor` (`ctx.ActorSubject()` — non-empty by contract, falls back to `"anonymous"`), `created_at` (`NOW()`). `correlation_id` and `causation_id` default to `ctx.CorrelationID()` / `ctx.CausationID()` when the receiver pipeline populated them — events emitted inside a receiver handler automatically carry the inbound trace chain.

### Subscriber

```go
// bootstrap/users_feature.go
func (f *UsersFeature) MountReceivers(reg *fwintegration.Registry, d bootstrap.Deps) {
    appweb.MountUserReceivers(reg, f.repo, d)
}

// web/user_receivers.go — parallel to web/user_routes.go
func MountUserReceivers(reg *fwintegration.Registry, repo userRepo, d bootstrap.Deps) {
    insertHandler := &handlers.InsertCommandHandler[*appdomain.User, *appcommands.InsertUserCommand, appcommands.InsertUserResult]{
        Repo: repo,
    }
    reg.From("partners").
        On("partnerOnboarded", requests.PartnerOnboardedRequest{}, insertHandler)
}
```

YAML:

```yaml
integration:
  defaults:
    consumerGroup: "${INTEGRATION_GROUP_ID:my-service-integration}"
    workers: 4
    startFrom: latest                          # latest | earliest
  subscribes:
    partners:                                  # source key (Go reference)
      topic: partners.integration.events       # Kafka topic name
      events:
        partnerOnboarded:                      # event key (Go reference)
          eventType: PartnerOnboarded          # wire header value
      workers: 8                               # per-source override (optional)
```

**Handler invariance.** The handler the consumer registers is the SAME `pipeline.Handler[TCmd, TResult]` instance an HTTP route consumes. The sample DTO carries `ToCommand()` (same convention web `Request.ToCommand` follows). The framework reflects on the sample's type once at MountReceivers time to plan the dispatch; per-message hot path is allocation-light (allocate request, `json.Unmarshal`, invoke `ToCommand()`, call `handler.Handle(ctx, cmd)`).

**Per-message pipeline (no outer TX).**

1. Read `event_id` from header (or `msg.Key`).
2. Pre-check `omnicore_integration_processed` — hit → ack Kafka, skip handler (already processed by this consumer group).
3. Build a fresh `*AppContext` for the invocation: `ID()` = new UUID; `ActorSubject()` = inbound `actor` header (falls back to `"anonymous"`); `CorrelationID()` = inbound `correlation_id` header or `event_id` fallback; `CausationID()` = `event_id`.
4. Unmarshal payload into a freshly-allocated sample DTO, call `ToCommand()`, dispatch the resulting Cmd via the registered handler.
5. Handler success → `INSERT INTO omnicore_integration_processed ... ON CONFLICT DO NOTHING` → ack Kafka.
6. Handler error → `RecordIntegrationFailure(...)` row written; Kafka offset still advances (failure isolation contract).

**At-least-once delivery.** Race window of milliseconds between handler COMMIT and the dedup INSERT may produce a double-invoke after a crash. Handlers MUST be idempotent by design — UPSERT on PG writes (the framework's `BaseRepository.ConstraintBindings` already supports this via `ON CONFLICT`), idempotency keys at the external side for side effects to non-PG systems (email, payment provider, downstream API).

**Failure registry + operator-driven retry.** Every handler failure is persisted to `omnicore_integration_failures` (mirror of `omnicore_upstream_failures` shape). `Receiver.RetryPendingFailures(ctx, exec, pipe, logger)` re-dispatches every pending row for the receiver's natural key — same posture upstream subscribers adopt. Consumer service exposes admin routes (`POST /admin/retries/integration`, `POST /admin/retries/upstream`) so operators drive retry explicitly; the framework does NOT auto-retry.

### DTO ownership and schema evolution

Per-consumer copy. Each service owns its own Go types for integration event payloads. The wire format (JSON) is the contract between producer and consumer; the Go struct is implementation detail per side. JSON unmarshal silently ignores unmapped fields, so additive producer-side changes are non-breaking. Breaking changes use sibling event keys (`userActivated` → `userActivatedV2`) with distinct `eventType` + `version` in YAML — old and new coexist during the migration window.

### Storage layout

| Table | Owner | Purpose |
|---|---|---|
| `integration_events` | producer side | authoritative store of every emitted event; written in-TX with the data row when `WithTx(tx)` supplied; forensic timeline indexed by `(aggregate_type, aggregate_id, created_at)` |
| `omnicore_integration_failures` | consumer side | one row per handler failure under natural key `(consumer_group, source_key, event_key, event_id)`; mirror of `omnicore_upstream_failures` shape |
| `omnicore_integration_processed` | consumer side | per-(event_id, consumer_group) dedup; BRIN index on `processed_at` for time-window pruning |

All three tables ship via framework migration `0002_integration_events.{up,down}.sql`. The `outbox` table stays UNTOUCHED — `integration_events` is a separate concept with its own retention semantics (long retention for replay / audit / compliance; outbox is throwaway after Debezium consumes). Operator-driven pruning via `DELETE WHERE created_at < NOW() - INTERVAL '30 days'` on `integration_events` is recommended; framework does not auto-prune.

### Coordinated shutdown

`shutdown.drainTimeoutSeconds` in YAML caps the parallel drain on SIGINT/SIGTERM (default 30s; aligns with kubernetes `terminationGracePeriodSeconds`). The framework drains HTTP server, integration consumer pool, upstream subscribers in parallel under the shared `shutdownCtx`. Per-stage drain timeouts surface as `slog.Warn` lines naming the stage so the operator knows what did not finish.

## Critical invariants

1. **`ValidEntity` instances are only created via `domain` package functions** — sealed types, private `entity()` method enforces this at compile time.
2. **Outbox is atomic with the data write** — `infra/executor.go` (simple path) and `infra/aggregate_persister.go` (aggregate path) both run INSERT/UPDATE + INSERT outbox + COMMIT in one `pgx.Tx`. Custom Repository implementations must preserve this.
3. **One outbox row per aggregate operation** (granularity B — the aggregate is the event unit). Children are persisted in the same transaction but contribute only to the snapshot in the single outbox row's payload. SyncEngine re-reads from Postgres on receipt, so payload is informational.
4. **Audit travels with the persister.** Every `Postgres.Insert/Update/Archive/Unarchive/Delete` builds an `AuditEvent` and routes it according to the boot-time `audit.destinations` configuration: when `database` is active the row is INSERTed into `audit_events` *inside the same TX* as the data row + outbox row (so a crash between writes cannot leave audit gaps); when `slog` is active the event is also emitted as a structured slog line after COMMIT (best-effort observability echo). Empty `destinations: []` disables audit entirely.
5. **Notifications are typed structs**, not strings. The string message comes from the translation layer at the boundary (Pipeline → Web). The typed identity (`NotificationKey`) flows through to the wire format.
6. **Domain has zero IO**. No DB, HTTP, Kafka. Pure types, validation, rules.
7. **`domain.NotificationCarrier`** is the cross-layer error contract. New error types in any layer must implement `error` + `NotificationContexts() []*NotificationContext`.
8. **Kernel notifications use the embedded base struct of their layer** — `DomainNotificationBase`, `ApplicationNotificationBase`, `InfrastructureNotificationBase`. Never mix.
9. **Every Archivable has a symmetric Unarchivable**. The framework treats Archive as reversible state transition, not destruction.
10. **Mongo mirrors PostgreSQL symmetrically by default.** DELETED is unconditional — always `mongo.Delete`. ARCHIVED is compose + upsert by default so the document survives with `deleted_at` populated; views that opt in via `ViewDefinition.DeleteOnArchive()` instead `mongo.Delete` on ARCHIVED (the hot-tier choice). UNARCHIVED → re-compose + upsert (covers both modes — for default views it just clears `deleted_at` on the existing document). Composer omits the `deleted_at IS NULL` filter everywhere by default; `DeleteOnArchive` views apply the filter on root + embeds (cascade — no per-embed override).
11. **Lifecycle hooks fire inside the TX, once per aggregate operation.** `afterBegin` fires INSIDE the persister's TX BEFORE any framework write (data + outbox + audit); `beforeCommit` fires INSIDE the TX AFTER all framework writes and BEFORE COMMIT. Single firing per `repo.Method()` call regardless of how many child rows the aggregate touches — granularity B, matching outbox and audit cardinality. Same firing positions on the flat path and the aggregate path; consumer code does not pronounce the dispatch.
12. **Hook error rolls the TX back; type identity is preserved end-to-end.** A non-nil error returned by the hook closure aborts the TX (the persister's `defer tx.Rollback(ctx)` runs) and propagates verbatim through the `repo.Method()` return. `domain.NotificationCarrier` identity reaches `pipeline.Run` unchanged → `Result.Failure` with the typed notification at the carrier's `Semantic()` HTTP status; non-carrier errors become `Result.Exception` (500). The persister emits a `slog.Warn("persistence.hook.error", verb, hookSlot, entityType, threadId, error)` line as a best-effort observability echo.
13. **Hook panic rolls the TX back AND propagates the panic.** `defer tx.Rollback(ctx)` fires automatically when the panic unwinds through the persister; the panic continues up to `pipeline.Run`'s `defer/recover`, which converts it to `Result.Exception` (500). The persister has no own recover — there is one canonical recover point.
14. **The `docs/` site is the source of truth for the public surface.** Every approved change that alters the exposed API (new method, new type param, contract change, new convention, deprecation) **must update the relevant page under `docs/content/sections/`** (and add a `docs/content/sections/changelog.html` entry) in the same round. `CLAUDE.md` (this file) is the agent/maintainer view; the `docs/` site (published via GitHub Pages at <https://claudioschirmer.github.io/omnicore/>) is the consumer-user view. The two must tell the same story. Purely internal changes (refactor without surface change, private helper, comment-only) may omit the `docs/` update — record the rationale in the commit/PR. See mandatory flow at the top of this file.
15. **Integration events are at-least-once delivered; consumer handlers must be idempotent by design.** The framework provides best-effort dedup via `omnicore_integration_processed` keyed by `(event_id, consumer_group)`; a race window of milliseconds between handler COMMIT and the dedup INSERT may produce a double-invoke after a crash. Handlers MUST be idempotent (UPSERT / natural-key constraints / external idempotency keys on side effects to non-PG systems).
16. **`integration_events` table IS the producer-side audit; `audit_events` is unchanged and never carries integration-event payload.** Cross-reference between an audit row and a related integration event is via `thread_id` (carried by both tables), not by extending the audit schema. Conflating the two would blur both contracts — audit is "data write forensics" (granularity B, aggregate as event unit); integration events are "cross-service emission" (per-Dispatch row, business-named).
17. **No outer TX on the Receiver path.** Each `Repo.Method` inside a handler invoked via a Receiver opens its own short TX, identical to the HTTP path. The framework does NOT inject a `TxHandle` into ctx for receiver invocations; persister code path is unchanged. The race window of milliseconds is documented — handlers designed idempotent absorb it without an outer TX's cost (long-lived TX during handler execution, connection pool pressure, replication lag risk).

## Migrations

Framework manages numbered SQL migration files in the `cfg.Migrations.Dir` directory (default `./migrations`). `bootstrap.Run` applies pending ones automatically before serving HTTP when `cfg.Migrations.AutoRun: true`.

**Profile-aware default.** `cfg.Migrations.AutoRun` is an `AutoRunMode` enum (`check | true | false`), parsed from yaml as either a quoted string (`"check"`/`"true"`/`"false"`) or a bare boolean (`true`/`false` normalized to the matching string). Default resolution: `dev → true`, any other profile → `check`. Explicit yaml value wins regardless of profile.

Wrapper over [`golang-migrate/migrate v4`](https://github.com/golang-migrate/migrate) — we don't reimplement lock/recovery/parsing.

### Conventions

```
migrations/
  0002_init.up.sql          ← service schema
  0002_init.down.sql        ← reverse (mandatory)
  0003_add_phone.up.sql
  0003_add_phone.down.sql
```

Filename: `{version}_{name}.{up|down}.sql`. `version` is a monotonic integer.

**Versions 1 and 2 are reserved for the framework's control plane** — injected via `embed.FS` from `omnicore/infra/migration/embedded/`. Version 1 (`0001_outbox.{up,down}.sql`) creates **two** tables: `outbox` (CDC source for the Debezium Outbox Event Router) and `omnicore_mongo_views` (the registry of materialized Mongo view shapes — see "Mongo schema evolution"). Version 2 (`0002_integration_events.{up,down}.sql`) creates the integration-events tables (`integration_events` + `omnicore_integration_failures` + `omnicore_integration_processed` — see "Integration events"). The framework tracks them in its own `omnicore_framework_migrations` table, so a service's own migrations start at `0002+` in the separate `omnicore_migrations` table with no collision. Do not write the framework SQL manually; the signatures are guaranteed identical across services (Debezium depends on the outbox shape; the Mongo schema-evolution path depends on the registry shape).

`.down.sql` is mandatory — validated by `Manager.ValidateDownExists` at startup. It may contain `-- intentionally empty: down not feasible` or no-op SQL when the migration has no technical reverse (DROP COLUMN, ALTER TYPE).

### Tracking

Two tracking tables, created automatically by `golang-migrate` on first execution:

| Table | Who writes | Contains |
|---|---|---|
| `omnicore_framework_migrations` | Framework (embedded) | version 1 (outbox + mongo views), version 2 (integration events) |
| `omnicore_migrations` | Service (files in `cfg.Migrations.Dir`) | service version 2+ |

Separate tables avoid version collisions — the framework table (versions 1–2) and a service's table (2+) can both carry a "version 2" row without conflict because each has its own history.

Each table stores only `(version BIGINT PRIMARY KEY, dirty BOOLEAN)`. A migration that fails mid-way leaves `dirty=true` — blocks subsequent calls to `Up` until remediated via `Force`. The `.down.sql` is **not stored**; it is read from disk/embed at the time of `Down(N)`.

### API

```go
mgr := migration.New(pg.Pool(), "./migrations")
mgr.Up(ctx)              // applies pending (framework + service, in that order)
mgr.Down(ctx, 1)         // reverts N service migrations (does not touch outbox)
mgr.Status(ctx)          // (version uint, dirty bool, err error) — service only
mgr.Pending(ctx)         // ([]uint, error) — versions on disk but not yet applied
mgr.Force(ctx, 5)        // recovery: marks version=5, dirty=false on service
mgr.ValidateDownExists() // checks .down.sql counterpart for each .up.sql in m.dir
```

`bootstrap.Run` runs `mgr.ValidateDownExists()` + `mgr.Up(ctx)` automatically when `cfg.Migrations.AutoRun: true`. Migration failure prevents the service from serving HTTP.

### Strict mode (autoRun=check)

When `cfg.Migrations.AutoRun: check` (the default in any non-dev profile), `bootstrap.Run` does NOT apply migrations automatically. Instead:

- Reads `mgr.Status(ctx)`. If `dirty=true`, aborts boot with a diagnostic naming the version + the `Force` recovery option.
- Reads `mgr.Pending(ctx)`. If non-empty, aborts boot with a diagnostic listing every pending version + the current DB version + the operator's recovery options.
- Otherwise: logs "migrations up to date (strict mode)" and proceeds.

Diagnostic shape:

```
[migrations] pending migration(s) detected:
  - version 3
  - version 4

current DB version: 2. required: 4.
migrations.autoRun=check (profile-aware default in non-dev profile) —
the framework will NOT apply migrations in check mode.

To proceed, choose one:
  A. set migrations.autoRun: true in microservice.<profile>.yaml; restart
  B. apply migrations manually + INSERT INTO omnicore_migrations (version, dirty) VALUES (3, false), (4, false);
  C. set migrations.autoRun: false in microservice.<profile>.yaml; restart
```

The operator chooses between A (framework does the work next boot), B (manual SQL reconcile), or C (skip the framework's check entirely).

### Boot order

`bootstrap.Run` applies migrations between `wire(deps)` and SyncEngine start — the schema is ready before the Kafka consumer attempts to compose views.

## Mongo schema evolution

Symmetric to the Postgres migration policy, but operating on the Mongo read-side projections rather than the PG schema. Covers drift detection between the code-declared `ViewDefinition` shape and the materialized Mongo collection state, the rebuild trigger, and the cleanup of orphan fields/documents. The control plane lives entirely in PostgreSQL — the `omnicore_mongo_views` table created by framework migration 0001. Mongo collections carry only domain data — no framework-reserved `_id`s.

### Three-mode control model

Both `migrations.autoRun` and `mongo.rebuild.autoRun` carry the same closed-set enum: **`check | true | false`**.

| Mode | Validates? | Acts when safe? | Aborts on doubt? |
|---|---|---|---|
| `check` | yes | **no** — boot aborts with diagnostic instructing operator | yes |
| `true` | yes | yes — reconcile when framework has certainty (linear drift, fresh init, wipe-and-recover, downgrade-with-opt-in) | yes (cases where intent is ambiguous) |
| `false` | **no** | n/a | **no** — operator takes responsibility |

Profile-aware defaults: dev → `true`, any other profile → `check`. Explicit yaml value (string `"check"`/`"true"`/`"false"` OR bare YAML bool `true`/`false`) wins. Same model applies to both PG migrations and Mongo rebuilds.

### View versioning

Every `ViewDefinition` declares a mandatory `Version(N)` (positive integer). The version is the developer-intent signal — bump it whenever the rebuild-relevant declarative state changes (root table, embeds, DeleteOnArchive, $jsonSchema, collation, capped, time-series). Index-only changes do NOT require a bump.

```go
fwinfra.View("users").
    Version(3).                                                    // ← mandatory
    Root("users").
    Schema(UserSchema()).                                          // ← mandatory on every view
    EmbedMany("addresses", fwinfra.FromSchema(AddressSchema())).
    Indexes(fwinfra.Index("email").Unique())
```

`Version(N)` participates in `RebuildHash`, so the framework detects:

- **Forgot-to-bump** — same version in code AND registry but hashes differ → `DriftForgotToBump` → boot aborts unconditionally (the version is the only intent signal; no escape).
- **Downgrade** — code version `<` registry version → `DriftDowngrade` → boot aborts unless `mongo.rebuild.allowDowngrade: true` opts in.

### Spec hash

Every `ViewDefinition` exposes three deterministic SHA-256 hashes:

```go
v.RebuildHash()   // version + rootTable + embeds + DeleteOnArchive + $jsonSchema +
                  // collation + capped + time-series
v.ArtifactHash()  // Indexes only — changes flow through ApplyMongoSpecs, no rebuild
v.Hash()          // Combined identity stamped on the registry row
```

Hashes are stable across runs and processes — the canonical serializer sorts map keys, normalizes numeric types, normalizes time-series granularity to lowercase, sorts the index list by deterministic key.

### PG control plane — `omnicore_mongo_views`

The framework migration 0001 creates the table alongside `outbox`. Every managed view has one row keyed by `view_name`:

```
view_name              TEXT PRIMARY KEY
version                INTEGER NOT NULL CHECK (version > 0)
rebuild_hash           VARCHAR(64)
artifact_hash          VARCHAR(64)
combined_hash          VARCHAR(64)
previous_version       INTEGER
previous_combined_hash VARCHAR(64)
previous_applied_at    TIMESTAMP
status                 TEXT NOT NULL DEFAULT 'done' CHECK (status IN ('done','processing'))
started_at             TIMESTAMP                    -- NULL when status='done'
pid                    TEXT                         -- holder pid during 'processing'
host                   TEXT                         -- holder host during 'processing'
applied_at             TIMESTAMP NOT NULL
applied_by             TEXT NOT NULL                -- "<svc>@pid:<n>" or "manual-reconcile-*"
code_version           TEXT                         -- OMNICORE_CODE_VERSION env
```

Partial index `omnicore_mongo_views_status_idx ON status WHERE status <> 'done'` keeps "show every rebuild mid-flight" queries on a constant-cost path.

### Hybrid concurrency primitive

The rebuild path uses two PG-side primitives in tandem:

- **`pg_advisory_lock`** — mutual exclusion across the cluster; auto-release on connection disconnect; no TTL math. Acquired on a pinned `pgxpool.Conn` via `infra.TryAcquireViewLock(ctx, conn, viewName)`.
- **Status column on the registry row** — `'done' | 'processing'`, with `started_at`/`pid`/`host` populated during a rebuild. Survives crashes (forensic signal) and exposes "what's mid-rebuild?" via plain SQL.

State machine (driven exclusively by `ExecuteRebuild`):

```
done  ──UPDATE status='processing', started_at=NOW(), pid, host──>  processing
                                                                    │
                                                            (rebuild executes)
                                                                    │
processing  ──UPDATE status='done', version, hashes, applied_at,
              started_at=NULL──>  done
```

Crash recovery: if a pod dies between the two transitions, the advisory lock auto-releases on TCP close. The next boot:

1. Acquires the lock cleanly.
2. Reads the row, sees `status='processing'` AND it just acquired the lock → emits `slog.Warn "previous rebuild died at started_at=X, pid=Y, host=Z; taking over"`.
3. Re-runs `BeginRebuild` (overwrites `started_at`/`pid`/`host` with own values).
4. Proceeds normally.

### Drift detection at boot

`bootstrap.Run` runs `infra.DetectViewDrift` between `ApplyMongoSpecs` and `SyncEngine.Start`. Eight branches over (registry × Mongo × code version):

| Decision | Condition | autoRun=true action | autoRun=check action |
|---|---|---|---|
| `DriftNone` | Registry combined hash matches | No-op | No-op |
| `DriftFreshInit` | No registry row + Mongo absent/empty | Write registry row (status='done') | **Abort** §14.9 |
| `DriftAlienData` | No registry row + Mongo populated | **Always abort** §14.4 | **Abort** §14.4 |
| `DriftMongoWiped` | Registry matches + Mongo absent/empty | Rebuild | **Abort** §14.7 |
| `DriftArtifactOnly` | Same version + rebuild_hash matches + combined differs | UPDATE registry (no doc rewrite) | **Abort** §14.8 |
| `DriftForgotToBump` | Same version + rebuild_hash differs | **Always abort** §14.5 | **Abort** §14.5 |
| `DriftRebuildRequired` | Registry version < code version | Rebuild | **Abort** §14.3 |
| `DriftDowngrade` | Registry version > code version | **Abort** §14.6 unless `allowDowngrade: true` → rebuild | **Abort** §14.6 |

Under `autoRun=false`, every branch (including `DriftAlienData` / `DriftForgotToBump`) is skipped — boot proceeds and runtime errors are the operator's concern.

### Rebuild execution

`SyncEngine.ExecuteRebuild(ctx, plan, cfg)` runs the §10.1 sequence on one view, on a pinned `pgxpool.Conn`:

1. `pool.Acquire(ctx)` — pin the connection for the lock's lifetime.
2. `TryAcquireViewLock` — if false, read holder via `pg_locks`+`pg_stat_activity`, abort with descriptive error.
3. Defer lock release + connection release.
4. Detect takeover: if `plan.Registry.Status=='processing'`, emit `slog.Warn` before the next write.
5. `BeginRebuild` — UPDATE registry to status='processing', `started_at=NOW()`, pid, host.
6. `slog.Info "view.rebuild.start"`.
7. Cleanup orphan fields (skip on empty collection): aggregation full-scan (`$objectToArray + $unwind + $group`) returns observed top-level field names; sample-Compose on ≤5 real Postgres ids returns expected; `$unset` the difference.
8. Snapshot existing `_id` set.
9. Compose+upsert from Postgres in batches of 1000. Each upsert decrements the snapshot.
10. Orphan reconciliation: `delete` (`deleteMany` the leftover snapshot) or `warn` per `cfg.Orphan`.
11. `EndRebuild` — UPDATE registry to status='done' + new hashes + `applied_at=NOW()`, captures `previous_*` from row's current state via the SQL itself. **Last data write.**
12. `slog.Info "view.rebuild.end"` with counts + duration.

Two fast-path siblings for non-rebuild cases:

- `SyncEngine.InitRegistryOnly(ctx, plan, serviceName)` — `DriftFreshInit` under `autoRun=true`. Writes the initial registry row; no rebuild loop.
- `SyncEngine.RefreshRegistryArtifactOnly(ctx, plan, serviceName)` — `DriftArtifactOnly` under `autoRun=true`. UPDATE-only; indexes already reconciled by `ApplyMongoSpecs`.

### YAML

```yaml
mongo:
  uri: ...
  database: ...
  rebuild:
    autoRun: check          # check | true | false — default by profile: dev=true / else=check
    orphan: delete          # delete | warn — default delete
    allowDowngrade: false   # default; opt-in for canary / blue-green rollback flow
```

Strict yaml decoding on `mongo.rebuild`: unknown keys abort boot — including the removed `lockTTL` field. `lockTTL` aborts with "unknown field" so consumer yamls must drop the line.

`OMNICORE_MONGO_FORCE_REBUILD=true` continues to govern only index divergence in `ApplyMongoSpecs` (drop+recreate divergent indexes). It does NOT trigger the rebuild path defined here and never drops collections.

### API

```go
// Registry helpers (PG-backed; consume any pgExec — pgxpool.Pool / Conn / Tx)
infra.ReadViewRegistry(ctx, exec, viewName) (*ViewRegistryRow, error)
infra.InitViewRegistry(ctx, exec, InitViewRegistryInput) error
infra.BeginRebuild(ctx, exec, viewName, now) error
infra.EndRebuild(ctx, exec, EndRebuildInput) error
infra.ListNonDone(ctx, exec) ([]ViewRegistryRow, error)

// Advisory lock helpers (require a pinned connection)
infra.ViewLockKey(viewName) int64
infra.TryAcquireViewLock(ctx, exec, viewName) (bool, error)
infra.ReleaseViewLock(ctx, exec, viewName) error
infra.ReadViewLockHolder(ctx, exec, viewName) (*ViewLockHolder, error)

// Drift detection
infra.DetectViewDrift(ctx, mongo, pg, views) (*DriftReport, error)
(*DriftReport).PlansBy(decision) []DriftPlan
(*DriftReport).HasAny(decisions...) bool
(*DriftReport).NeedsAction() bool

// Rebuild orchestration
(*SyncEngine).ExecuteRebuild(ctx, plan, cfg) error
(*SyncEngine).InitRegistryOnly(ctx, plan, serviceName) error
(*SyncEngine).RefreshRegistryArtifactOnly(ctx, plan, serviceName) error
```

Diagnostic formatters (boot-fatal messages, §14.x):

```go
infra.FormatFreshInitDiagnostic(plans)         // §14.9
infra.FormatAlienDataDiagnostic(plans)         // §14.4
infra.FormatMongoWipedDiagnostic(plans)        // §14.7
infra.FormatArtifactOnlyDiagnostic(plans)      // §14.8
infra.FormatForgotToBumpDiagnostic(plans)      // §14.5
infra.FormatRebuildRequiredDiagnostic(plans)   // §14.3
infra.FormatDowngradeDiagnostic(plans)         // §14.6
```

Operator inspection queries:

```sql
-- noinspection SqlNoDataSourceInspectionForFile
-- Anything mid-rebuild right now?
SELECT view_name, started_at, pid, host, NOW() - started_at AS elapsed
FROM omnicore_mongo_views
WHERE status = 'processing';

-- Forensics — most recent rebuilds, newest first
SELECT view_name, version, applied_at, applied_by,
       previous_version, previous_applied_at
FROM omnicore_mongo_views
ORDER BY applied_at DESC LIMIT 20;
```

### Required PG privileges

`SELECT, INSERT, UPDATE` on `omnicore_mongo_views`; `pg_try_advisory_lock` + `pg_advisory_unlock` (granted to all roles by default); read on `pg_locks` + `pg_stat_activity` (for the holder diagnostic). All present in any role with full DML.

### Required Mongo privileges

`find`, `insert`, `update`, `remove`, `aggregate` (cleanup), `collMod`, `createCollection`, `listCollections`. Reduced from the predecessor design: no `createIndex` for a TTL lock (no Mongo lock anymore), no reserved-`_id` writes.

## Required PostgreSQL schema

The outbox table is created automatically by the framework's embedded migration 1 — do not write its SQL manually. The canonical signature is in `omnicore/infra/migration/embedded/0001_outbox.up.sql` and the Debezium Outbox Event Router depends on these exact columns.

Service domain tables (defined by the service's migrations 2+) need to follow these conventions so that SQL generated by the executor and queries by the composer work:

```sql
-- noinspection SqlNoDataSourceInspectionForFile
CREATE TABLE users (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL,
    -- ... domain columns ...
    deleted_at  TIMESTAMP,                   -- soft-delete marker (name/presence declared via TableSchema.SoftDelete)
    created_at  TIMESTAMP    NOT NULL,        -- framework stamps NOW() on INSERT (no DB DEFAULT needed)
    updated_at  TIMESTAMP    NOT NULL         -- framework stamps NOW() on INSERT + UPDATE
);
```

The framework actively stamps `created_at` and `updated_at` with `NOW()` (it does not rely on a DB `DEFAULT` it does not own), so the `DEFAULT NOW()` is optional; declare each column's name/presence via `TableSchema.CreatedAt(col)` / `UpdatedAt(col)`. `deleted_at` is the soft-delete marker — required only when the entity is archivable; declare it via `TableSchema.SoftDelete(col)` (presence enables, omission disables). See "Schema mapping".

**Child tables** of an aggregate (e.g. `addresses` for `users`) carry an FK column to the root. The column name is declared explicitly in the child `TableSchema` via `.FK("user_id")` — the persister injects the root id into it on child INSERT:

```sql
-- noinspection SqlNoDataSourceInspectionForFile
CREATE TABLE addresses (
    id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID         NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- ... child columns ...
    deleted_at  TIMESTAMP,                   -- MANDATORY: symmetric cascade archive/unarchive
    created_at  TIMESTAMP    NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMP    NOT NULL DEFAULT NOW()
);
CREATE INDEX addresses_user_id_idx ON addresses (user_id);
```

Universal symmetric cascade: root archive → `UPDATE addresses SET deleted_at = NOW() WHERE user_id = $1` in the same TX; root unarchive → restores all archived children; root delete → relies on FK `ON DELETE CASCADE`.

Cross-service data is materialized into **B's own Mongo database** via `UpstreamSubscription` — there is no local PG cache table convention anymore. The local Mongo collection (e.g. `users` projected into B's database) is upsert-managed by the framework's `UpstreamSubscriber`; embeds reference it via an external `fwinfra.FromSchema(fwinfra.NewExternalSchema("users")…)`. See "Cross-service composition" under [Read side (CQRS)](#read-side-cqrs) for the full surface.

## Concurrency and lifecycle

| Type | Thread-safe? | Lifecycle |
|---|---|---|
| `translation.Translator` | Yes (`sync.RWMutex` internally) | Singleton or per-service |
| `configuration.AppContext` | Yes (`sync.RWMutex` for language/metadata) | Per HTTP request |
| `pipeline.Pipeline` | Yes (stateless after construction) | Singleton per service |
| `domain.BaseEntity` / user entities | **No** — mutable validation state | Per operation (don't share) |
| `domain.NotificationContext` | **No** — `[]NotificationMessage` is appended | Owned by the BaseEntity it belongs to |
| `infra.Postgres` (pgx pool) | Yes | Singleton per database |
| `infra.MongoDB` | Yes (driver handles pooling) | Singleton per database |
| `infra.SyncEngine` | Yes (single consumer goroutine) | Singleton per service (run as goroutine via `Start(ctx)`) |
| `infra.Postgres` (audit hooks) | Yes (audit emission is per-write, no shared state beyond the pool/config) | Singleton per service, configured once via `WithAudit` |

**Implication for HTTP handlers**: handlers hold the `persistence.ScopedRepository[T]` singleton (the same `BaseRepository[T]`-based struct, which exposes `FindByID`/`New` directly and `Scope` for writes) and call `repo.Scope(ctx, opts...).Method(valid)` — `ctx` is the request `*AppContext`, `opts` is the variadic of any `WriteOption[T]` the Auto handler derived from the Cmd's optional `AfterBegin` / `BeforeCommit` methods. Audit emission is automatic — configured once at boot on the `Postgres` adapter, every write call routes through `infra.Postgres` which builds and emits the event according to `audit.destinations`. Handlers never thread an Auditor.

## Go pitfalls specific to this codebase

1. **Methods can't be generic** — that's why `pipeline.Run[T]` and `pipeline.Dispatch[TReq,TRes]` are top-level functions taking `*Pipeline`, not methods. Same for `web.RespondFromResult[T]`.

2. **`errors.As` with the carrier interface** — to handle errors from multiple layers without importing each layer:
   ```go
   var carrier domain.NotificationCarrier
   if errors.As(err, &carrier) {
       // catches DomainError, ApplicationError, InfrastructureError, and any future carriers
   }
   ```

3. **Named return + `defer/recover`** — required in `pipeline.Run` for converting panics to `Result.Exception`.

4. **`reflect.TypeOf(n).Name()` strips pointers** — `NotificationKey` and `classNameOf` already handle this. `*Customer` and `Customer` both give `"Customer"`.

5. **Embedded base methods with private receivers cross packages via promotion** — `DomainNotificationBase.isNotification()` is unexported but works when user notifications in OTHER packages embed it, because method dispatch happens through the embedded type.

6. **`Fields` is a `map[string]any`** — iteration order is non-deterministic. `infra/executor.go` uses `sortedKeys` for deterministic SQL. Preserve that when modifying.

7. **`infra.validIdentifier` panics on bad input** — defense against SQL injection in identifiers (table/column names come from domain code, never user input). The panic is intentional.

8. **`google/uuid` is the canonical UUID lib** — already vendored via Fiber's transitive deps. Don't add `gofrs/uuid` or similar.

9. **`slog` levels** — pipeline uses `slog.LevelInfo` for routine ops, `slog.LevelWarn` for audit failures (non-blocking), `slog.LevelError` for unhandled errors. Don't escalate audit failures to ERROR — they're not write-path failures.

10. **Aggregate value objects are value types, not pointers** — `AggregateRoot` uses `reflect.DeepEqual` for change tracking. Use value types so equality is field-by-field, not pointer identity.

## Full request flow (concrete)

`POST /users` with body `{"name":"John","email":"j@x.com","username":"...","addresses":[{...}]}`:

```
1. Fiber middleware: build AppContext (UUID + Language from headers)
2. Body parse → CreateUserCommand struct
3. pipeline.Dispatch(pipe, appCtx, cmd, &CreateUserHandler{repo})
   └─ pipeline.Run wraps in defer/recover
      └─ handler.Handle(ctx, cmd)
         └─ user := &User{...}; for each addr: user.AddAddress(addr, svc)
         └─ domain.GetInsertable(user, nil, "GetInsertable")
            └─ ensureInit / resetEntity / validateForInsert
               └─ BuildRules: validate root fields (children auto-iterated downstream)
               └─ runAggregateValidations: iterates root.AllAggregateItems() → Address.BuildRules(actionName, svc, scopedRules) for each item with CurrentStatus != Removed (path segment via camelCase toLowerCamel(typeName))
               └─ checkAllNotifications → *DomainError if any
            └─ extractAggregateMeta(user) populates *aggregateMeta
            └─ build Insertable with aggregate metadata attached
         └─ opts := derive WriteOption[*User] from Cmd's AfterBegin / BeforeCommit (when declared)
         └─ repo.Scope(ctx, opts...).Insert(insertable) [your UserRepository.Scope]
            └─ Postgres.Insert(ctx, insertable, &Config, AdaptWriteOptions(opts))
               └─ AggregateInfo() returns ok=true → dispatch to insertAggregate
                  └─ BEGIN TX
                  └─ ⬇ afterBegin(ctx, user, txHandle)         ← position A (rolls back on error)
                  └─ INSERT users RETURNING id
                  └─ for each Added child: INSERT addresses (user_id injected)
                  └─ INSERT outbox (single row, payload = root + children snapshot)
                  └─ IF cfg.Audit.Includes(database):
                     └─ audit.InsertAuditEvent(ctx, tx, ev)    ← atomic with data + outbox
                  └─ ⬇ beforeCommit(ctx, user, id, txHandle)    ← position D (rolls back on error)
                  └─ tx.Commit(ctx)
                  (POST-COMMIT, best-effort:)
                  └─ IF cfg.Audit.Includes(slog):
                     └─ audit.EchoSlog(ctx, logger, ev)        ← observability echo
         └─ return id, nil
   └─ result = Success(id)
4. web.RespondFromResult(c, result, fiber.StatusCreated)
   └─ Status 201, body Response{Success:true, Data:id}

Async (eventually consistent):
5. Debezium tails outbox via WAL logical replication
6. Outbox Event Router transforms outbox row → Kafka message on "users.events":
     key:     aggregate_id (UUID string)
     headers: aggregate_type="users", event_type="INSERTED"
     value:   payload (aggregate snapshot)
7. SyncEngine consumes "users.events" (one of N replicas via consumer group partition)
   └─ extractEvent(msg): pulls aggregate_id from Key, type/eventType from Headers
   └─ composer.Compose(view, aggregate_id)
      └─ fetchRow(users, "id", id) WHERE deleted_at IS NULL → root doc
      └─ applyEmbeds: fetchWhere(addresses, "user_id", id) WHERE deleted_at IS NULL → embedded
   └─ mongo.Upsert("users", id, doc) → view materialized
```

Error path with validation failure: `BuildRules` adds notifications; `*DomainError` propagates up; `pipeline.Run` catches via `NotificationCarrier`; translates contexts to DTOs (each DTO message carries `NotificationKey` + `Semantic`); returns `Failure[id]`. `RespondFromResult` calls `statusFromNotifications`, finds all messages with `SemanticValidation`, returns **422** with `errors:[{context, messages:[{notificationKey, field, message, semantic}]}]`. For a `RecordNotFoundNotification` raised by `UserRepository.FindByID`, the message carries `SemanticNotFound` and `statusFromNotifications` returns **404**.

## microservice.&lt;profile&gt;.yaml — declarative config

Each service ships **one file per profile** at the module root: `microservice.dev.yaml` and `microservice.prd.yaml` are the canonical pair. `bootstrap.LoadConfig` reads the `APP_PROFILE` env var (required, non-empty) and loads the matching `microservice.${APP_PROFILE}.yaml`. `OMNICORE_CONFIG_PATH` overrides the path when needed (tests, custom layouts).

Profile names beyond `dev`/`prd` are accepted — services can ship extra variants such as `microservice.prd-pem.yaml`, `microservice.prd-external.yaml`, or `microservice.qa-canary.yaml` to swap whole configurations via `APP_PROFILE` without competing with the canonical pair. The framework treats every non-`dev` profile identically; only `dev` unlocks `auth.mode=disabled`. This is what makes QA suites able to exercise alternative auth or runtime modes through plain `APP_PROFILE` swaps.

Two files instead of one is intentional: the artifact deployed to prd does not carry dev configuration at all, so a service cannot accidentally ship with `auth.mode=disabled`. The profile is the env var's responsibility — it is **not** a YAML field, so the same file cannot drift between profiles.

Supports three substitution forms inside `${...}`:

- **`${VAR}` / `${VAR:default}`** — environment variable (Spring Boot style). `VAR` set and non-empty wins; otherwise the default after `:` is used; no `:default` and env unset → empty string. Env misses are silent (dev defaults are expected).
- **`${file:/abs/path}`** — file contents read once at boot. A trailing `\n` or `\r\n` is trimmed; everything else is preserved verbatim, so PEM blocks and similar multi-line payloads round-trip. Missing file or any I/O failure aborts the boot with the path in the error.
- **`${vault:store/path#field}`** — delegated to the registered `bootstrap.SecretResolver`. Without a registered resolver the default returns `ErrUnsupportedResolver` and the boot aborts. Plug a real implementation (HashiCorp Vault, AWS Secrets Manager, …) via `bootstrap.RegisterSecretResolver(impl)` at process init.

The reserved names `file` and `vault` shadow same-named env vars inside `${...}` — env-var conventions are uppercase, so the collision is theoretical, but documented here so it doesn't surprise. `file:` and `vault:` failures are strict (boot aborts) on purpose: a missing file or unresolved secret in prod is an operational bug, not a silent empty-string default.

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
    autoRun: check     # check | true | false — profile-aware default: dev=true / else=check
    orphan: delete     # delete | warn — default delete (reconcile orphan Mongo docs after rebuild)
    allowDowngrade: false  # opt-in for canary / blue-green rollback flow
kafka:
  brokers: ["${KAFKA_BROKERS:localhost:9092}"]
  syncGroupId: "${SYNC_GROUP_ID:my-service-sync}"
  syncWorkers: 4       # optional; default runtime.NumCPU(); 1 = serial
migrations:
  dir: ./migrations    # default
  autoRun: check       # check | true | false — profile-aware default: dev=true / else=check
query:
  maxLimit: 200        # service-wide ceiling on `?limit=`; 0/absent → framework default 100; per-view override via fwinfra.View("...").MaxLimit(N)
auth:
  mode: jwt            # jwt | disabled (disabled rejected unless APP_PROFILE=dev)
  jwt:
    algorithms: [RS256, ES256, EdDSA]  # optional; defaults to all three asymmetric
    issuer: https://idp.example.com
    audience: my-service
    leewaySeconds: 30
    jwksUrl: https://idp.example.com/.well-known/jwks.json
    # OR (mutually exclusive with jwksUrl):
    # publicKeyPem: |
    #   -----BEGIN PUBLIC KEY-----
    #   ...
  externalValidator:   # optional revocation check against the IdP
    method: POST       # GET | POST
    url: https://idp.example.com/realms/x/protocol/openid-connect/token/introspect
    tokenPlacement: form_field   # bearer_header | form_field | json_body | query_param
    tokenField: token            # required unless tokenPlacement=bearer_header
    extraHeaders:
      Authorization: "Basic ${IDP_CLIENT_CREDS}"
    success:
      jsonPath: $.active
      expectedValue: true
    timeoutMs: 2000              # optional; default 2000
    failMode: closed             # closed | open; default closed
    cacheTtlSeconds: 0           # 0 (default) disables cache; > 0 caches positive answers for N seconds
  publicRoutes:
    - GET /health
    - GET /ready
  auditClaims:                   # JWT claims to include in audit's actorClaims (forensics); empty default
    - tenant_id
    - roles
audit:                           # optional; omitted block = framework default = both destinations active
  destinations:
    - slog                       # post-commit echo (observability via ELK / Loki / stdout collector)
    - database                   # in-TX row in audit_events (atomic with the write — source of truth)
  # destinations: []             # explicit empty list disables audit entirely (no row, no slog line)
```

Mandatory fields: `service`, `postgres.dsn`, `mongo.uri`, `mongo.database`, `kafka.brokers`, `kafka.syncGroupId`. `bootstrap.LoadConfig` fails with an error listing missing ones.

The `auth:` block is optional and defaults to `{mode: disabled}` when absent — convenient under `APP_PROFILE=dev`, rejected at boot under any other profile so prd cannot ship without authentication wired. When `mode: jwt`, the schema requires `issuer` + `audience` + exactly one of `jwksUrl` / `publicKeyPem`; the `externalValidator` sub-block is optional. The external-validator cache is opt-in via `cacheTtlSeconds` and default off — see the Authentication section for the revocation-vs-throughput trade-off.

The `audit:` block governs where every successful write's `AuditEvent` is routed:

- **Default (block absent or `destinations:` key absent)** — both `slog` and `database` active. The recommended posture: the in-TX `audit_events` row guarantees compliance (atomic with the data row); the slog echo feeds whatever observability stack (ELK, Loki, Datadog) the operator already runs.
- **`destinations: [database]`** — only the PG row. Highest signal/noise; lowest log volume. Pair with SQL dashboards.
- **`destinations: [slog]`** — only the slog line. Matches the framework's pre-redesign posture (best-effort, observability-only). Use when compliance does not require atomic durability and the SQL row would cost too much.
- **`destinations: []`** — explicitly disables audit. The `audit_events` table still exists (migration 0001 provisions it unconditionally) but no row is written; no slog line is emitted. Use for test fixtures or services where audit is provided by an external layer.

Unknown destination tokens (`destinations: [kafka]`) and duplicates (`destinations: [slog, slog]`) abort the boot with a diagnostic naming the offender — operator typos surface at startup, not as silent runtime drift. `audit.destinations` is the **only** audit knob today; the `auth.auditClaims` allowlist controls which JWT claims appear inside the AuditEvent's payload and stays under `auth:` because it is a security concern (whitelist what leaves the JWT envelope), not a routing concern.

Known (intentional) limitations:
- **No recursion**: single substitution pass — `${A:${B:c}}` doesn't work.
- **Default cannot contain literal `}`**: the `[^}]*` regex prevents it. Workaround: set via env.
- **YAML 1.2** (yaml.v3 default): `yes/no/on/off` remain strings. Even so, when a value can be ambiguous (DSN, URIs), use double quotes in the YAML.

## Authentication middleware

When `auth.mode: jwt`, `bootstrap.Run` auto-registers `fwweb.AuthMiddleware` immediately after `fwweb.AppContextMiddleware`. Every request is validated end-to-end before any Feature route sees it; on success the middleware populates `AppContext.Identity`. When `auth.mode: disabled`, the middleware is not registered at all — `Identity()` stays `nil` and there is no per-request cost.

The middleware itself lives in `omnicore/web/auth_middleware.go` and takes a primitive `AuthOptions` struct + `*pipeline.Pipeline` (for translation of unauthorized responses). The web package does not import `bootstrap` — `authOptionsFromConfig` in `bootstrap/bootstrap.go` flattens the parsed `AuthConfig` into `AuthOptions`, keeping the dependency direction `bootstrap → web` clean.

### Per-request flow

1. **publicRoutes bypass.** Exact `METHOD /path` match against `auth.publicRoutes` (e.g. `GET /health`, `GET /ready`, login endpoints). Match → `c.Next()`.
2. **Bearer extraction.** Reads `Authorization: Bearer <token>` (scheme match case-insensitive). Empty/malformed → `notifications.MissingAuthorizationNotification`, **401**.
3. **Local JWT validation.** Signature verified against `jwksUrl` (preferred — `MicahParks/keyfunc` fetches + caches the JWKS, refreshing on `kid` cache miss so rotated keys work without redeploy) OR `publicKeyPem` (RSA/ECDSA/Ed25519 via `x509.ParsePKIXPublicKey`). Header `alg` must be in the allowlist (`jwt.WithValidMethods`); `iss` pinned (`jwt.WithIssuer`); `aud` pinned (`jwt.WithAudience`); `exp` enforced (`jwt.WithExpirationRequired` + `jwt.WithLeeway`).
4. **Identity attachment.** On success, `AppContext.SetIdentity(&Identity{Subject, Issuer, ExpiresAt, Claims})`. `Claims` is a copy of the parsed `jwt.MapClaims` so handler mutations cannot leak back. The middleware also calls `AppContext.SetBearerToken(token)` with the verified raw JWT — consumed exclusively by the `forward-bearer` httpclient auth provider; handlers should keep reading `Identity()` for principal data.
5. **Tenant gate (Layer 3, opt-in).** When `AuthOptions.TenantRequired` is true (bootstrap sets it from `auth.authorization.tenant.required`), the middleware checks `Identity.TenantID() != ""` right after Identity attachment. Empty → rejected with `TenantMissingNotification (403)` before any handler runs. Off by default.

### Error responses

| Scenario | Notification | Semantic → HTTP |
|---|---|---|
| No header, or doesn't match `Bearer <token>` | `MissingAuthorizationNotification` | Unauthorized → 401 |
| Token malformed / bad signature / wrong `iss`/`aud` / disallowed `alg` / `!Valid` | `InvalidTokenNotification` | Unauthorized → 401 |
| Token `exp` in the past (after leeway) | `ExpiredTokenNotification` | Unauthorized → 401 |
| `TenantRequired=true` and `Identity.TenantID() == ""` | `TenantMissingNotification` | Forbidden → 403 |

`Expired` is split from `Invalid` so clients can branch on refresh-vs-relogin. The tenant rejection lands as 403 (not 401) because the principal IS authenticated — the failure is "your authenticated identity is not scoped to a tenant the service can resolve". All notifications live in `application/notifications/core.go` with `ApplicationNotificationBase`; translations in all seven built-in catalogs. `respondAuthFailure` dispatches the HTTP status from each notification's own `Semantic()` so adding a new failure mode is a notification change, not a middleware change.

### External validator (revocation check)

When `auth.externalValidator` is set, the middleware adds an extra step **after** local validation passes: an outbound HTTP call to the IdP (RFC 7662 introspection or compatible). Catches revoked tokens — a JWT that locally still has a valid signature can already be revoked at the IdP, and only the IdP knows. Lives in `omnicore/web/external_validator.go`.

**Cache is opt-in, default off.** Set `cacheTtlSeconds` to a positive integer to enable an in-memory positive-only cache keyed by the SHA-256 hash of the bearer token (raw token is never stored — see `tokenCacheKey` in `web/external_validator.go`). Only successful validator answers are memoized; negative answers and transport errors deliberately bypass the cache so a revocation is honored on the very next request. Trade-off: tokens already cached as valid keep passing for up to TTL seconds after revocation at the IdP. Default `0` disables the cache entirely; every authenticated request calls the IdP.

**Token placement** — how the token is carried in the outgoing request:

| Placement | Outgoing shape |
|---|---|
| `bearer_header` | `Authorization: Bearer <token>` header. `tokenField` ignored |
| `form_field` | `application/x-www-form-urlencoded` body, `<tokenField>=<token>` (Keycloak introspection) |
| `json_body` | `application/json` body, `{"<tokenField>": "<token>"}` |
| `query_param` | URL query, `?<tokenField>=<token>` |

**Success check** — the response body is parsed as JSON, the configured `success.jsonPath` (dot notation: `$.active`, `$.data.is_active`) is walked, and the resulting value is compared to `success.expectedValue` via Go `==` on `any` (so `true` != `"true"`). Path miss, type mismatch, or value mismatch → reject.

**Fail mode** — controls what happens when the validator itself errors (transport, timeout, non-2xx, malformed JSON):
- `closed` (default): reject the request — a token whose validity cannot be confirmed is not honored
- `open`: accept on validator transport errors when local pre-validation already passed; explicit "not active" answers still reject

The validator is constructed at boot via `newExternalValidator`; invalid config (missing URL, unknown placement, etc.) fails boot rather than per request. `http.Client.Timeout` enforces `timeoutMs` (default 2000ms). The Fiber request context propagates so client disconnects cancel the outbound call.

### Audit and events carry the actor

The audit pipeline and `infra/events.SlogPublisher` both read the authenticated principal from the request `persistence.RequestContext` and surface it on every emitted artifact (audit row + slog line + domain event):

- `actor` — JWT `sub`, or the sentinel `"anonymous"` when no `Identity` is attached (auth disabled, public route, background job)
- `actorIssuer` — JWT `iss`, omitted when empty
- `actorClaims` — opt-in subset declared by `auth.auditClaims` (audit only; the events publisher never widens the claim surface)

The propagation works for **both Auto and manual handlers** without any code change in the handler itself, because every write goes through `repo.Scope(ctx, opts...).Method(valid)` and `ctx` IS the `*AppContext` the middleware populated — `*AppContext` satisfies `persistence.RequestContext` with `ActorSubject()` / `ActorIssuer()` / `ActorClaims()`, so the audit builder running inside the persister reads the actor straight from the request scope.

Audit-line shape (top-level flat — see "Audit event shape" above for the full grammar):

```json
{
  "msg": "audit",
  "threadId": "5f25…",
  "entityType": "User",
  "entityId": "abc-123-…",
  "verb": "insert",
  "actionName": "GetInsertable",
  "kind": "snapshot",
  "actor": "user-42",
  "actorIssuer": "https://idp.example",
  "actorClaims": {"tenant_id": "acme", "roles": ["admin"]},
  "dateTime": "2026-06-09T12:00:00Z",
  "snapshot": {"name": "Jane Doe", "email": "jane@x.test"},
  "children": {"Address": [{"id": "…", "op": "inserted", "snapshot": {"...": "..."}}]}
}
```

`auth.auditClaims` is the only knob that broadens the claim surface in audit artifacts. Empty/absent → `actorClaims` block omitted entirely (both in the PG row's payload and in the slog line); populated → the audit builder filters `ctx.ActorClaims()` by the allowlist before constructing the `AuditEvent`. The signing-secret-style claims (`secret`, `password`, …) you never declared do not leak.

### Token issuance is out of scope

The framework only validates. The IdP (Keycloak, Auth0, in-house auth service, …) mints tokens; each service consumes them. Login endpoints, refresh, credential storage live in the IdP or in a dedicated auth service — not in this framework.

### Library choices

- `github.com/golang-jwt/jwt/v5` — JWT parsing and validation. Standard Go choice; supports RSA / ECDSA / Ed25519. Pinned via `WithValidMethods` so only asymmetric `alg` values from the allowlist are accepted; symmetric (HS\*) is intentionally rejected to never give the service the IdP's signing secret.
- `github.com/MicahParks/keyfunc/v3` — JWKS fetcher and key cache. Pairs with jwt/v5 via `keyfunc.NewDefaultCtx(...).Keyfunc`. Handles refresh on `kid` cache miss; managed for the lifetime of the service (Go GC takes care of teardown on shutdown).

## Authorization

Authentication is solved by `AuthMiddleware` populating `AppContext.Identity` from the verified JWT. Authorization — what the authenticated principal is allowed to do — travels through **three concentric layers**, each with its own surface. All three consume `AppContext.Identity()` and surface rejections through the canonical envelope (`SemanticForbidden → 403`). The framework does NOT enforce identity at the infra layer: `Postgres.Insert/Update/Archive/Unarchive/Delete` and `MongoViewReader.ReadPage/ReadByID` execute what the application layer validated; they do not pronounce identity.

### Layer 1 — Coarse-grained declarative gate (transport)

**What it enforces.** "This route requires permission X." Static across all requests, identical for every principal. The most common case ("only callers with `users:write` reach POST /users") lives here.

**How.** `fwopenapi.RequirePermission(p)` is a `MountOption` consumed by `openapi.Mount` and `openapi.MountRaw`. The same option covers the three route registration paths (canonical via `*Spec` siblings, manual-with-pipeline with hand-rolled `RouteSpec`, raw via `MountRaw`):

```go
// canonical
fwopenapi.Mount(d.OpenAPIRegistry, users, fiber.MethodPost, "/",
    insertH, insertSpec,
    fwopenapi.Doc{Summary: "Create a user", Tags: tags},
    fwopenapi.RequirePermission("users:write"))

// raw
fwopenapi.MountRaw(d.OpenAPIRegistry, app, fiber.MethodGet, "/whoami",
    whoamiHandler,
    fwopenapi.RawSpec{Summary: "Whoami", Tags: tags},
    fwopenapi.RequirePermission("users:profile:read"))
```

Mount/MountRaw on receiving the option:
1. Patch `spec.RequiredPermission` so the OpenAPI generator appends `**Required permission:** \`<p>\`` to the operation description.
2. Wrap the handler with the framework's permission gate (`web.PermissionGate`) which short-circuits with the canonical 403 envelope carrying `MissingPermissionNotification` (field `permission`, value = the declared string) when `Identity.HasPermission(p)` returns false.

The 403 entry is auto-emitted in `/openapi.json` on EVERY non-public route when `auth.mode: jwt` is in effect — independent of whether `RequirePermission` is declared — because any authenticated route can produce a Forbidden outcome via Layer 2 or Layer 3.

**Permission string format: `resource:action`** (Auth0 / AWS / Stripe convention). The IdP emits a `permissions` claim (default name, configurable via `auth.authorization.permissionsClaim`) carrying an array of strings; `Identity.HasPermission(p)` matches with three rules:

| Claim entry | Matches request |
|---|---|
| `users:read` | exactly `users:read` |
| `users:*` | any `users:<anything>` |
| `*:*` | any request (super-admin) |

**Tolerated input shapes** (different IdPs emit different envelopes): `[]string`, `[]any`, space-separated `"users:read users:write"`, comma-separated `"users:read,users:write"`. nil/absent/unsupported types → empty set (request denied; never crashes). The parsed set is cached on the Identity after the first call without locking — Identity is per-request and never shared across goroutines.

**Wildcards on the caller side panic at runtime.** `ctx.Identity().HasPermission("users:*")` is a programming bug (the claim wildcards; the route declares the exact action). Panic is caught by `pipeline.Run`'s `defer/recover` → 500. Compose "any of A or B" with explicit OR over concrete actions:

```go
if ctx.Identity().HasPermission("users:audit") || ctx.Identity().HasPermission("users:admin") {
    ...
}
```

**Master switch — `auth.authorization.enabled`.** Default false. When false, the runtime gate no-ops (handlers run regardless of `RequirePermission`) AND the spec's `**Required permission:** \`<p>\`` description suffix is suppressed — so the documentation never advertises a constraint the server is not honoring. The value stays on `Spec.RequiredPermission` / `RawSpec.RequiredPermission` for introspection (codegen, contract diffs), and the `403` auto-emission still applies whenever auth is on. Flip the YAML when the IdP starts emitting the `permissions` claim — that turns on both enforcement AND the description suffix together, so spec and runtime stay in sync. Identity helpers (`HasPermission`, `TenantID`) work regardless of the switch.

**Boot validation.** When `auth.authorization.enabled: true`, bootstrap runs a Registry scan AFTER `Wiring.BeforeServe` and BEFORE `openapi.Register`. Every non-public route in the Registry MUST declare `RequirePermission(...)` — the scan panics with the offender list otherwise. Skipped silently when the flag is off so services not yet on authz boot normally.

**Adjacent enforcement (independent of authz.enabled).** Bootstrap also runs `scanRouteRegistration` whenever `Wiring.OpenAPI != nil` — it compares `app.GetRoutes(true)` against `Registry.Operations()` and panics on any Fiber route registered outside `Mount`/`MountRaw`. The framework's canonical channel (documentation + gating + observability) is the single registration path; bypassing it bypasses all three at once.

### Layer 2 — Fine-grained programmatic rule (domain)

**What it enforces.** Rules that depend on the specific resource state, the principal's claims, or the relationship between them. "Only the owner can archive." "Cannot delete the last admin." "Email cannot change after activation unless the principal is an admin." Anything Layer 1's static "needs permission X" cannot express.

**How.** `BuildRules(actionName, svc, r)` runs BEFORE the SQL fires on every write verb. The `actionName` parameter tells the entity which verb is firing (`"GetInsertable"` / `"GetUpdatable"` / `"GetPartialUpdatable"` / `"GetArchivable"` / `"GetUnarchivable"` / `"GetDeletable"`); the rule body branches accordingly:

```go
func (u *User) BuildRules(actionName string, svc domain.Service, r *domain.Rules) {
    r.IfUpdate(func() {  // IfUpdate fires for PUT, PATCH, Archive, Unarchive
        if actionName == "GetArchivable" && u.RequestingPrincipalEmail != "" {
            if u.Email != u.RequestingPrincipalEmail && !u.RequestingPrincipalIsAdmin {
                r.AddNotification("ID", domain.ArchiveNotAllowedNotification{})
            }
        }
    })
}
```

The identity-derived values (`u.RequestingPrincipalEmail`, `u.RequestingPrincipalIsAdmin`) reach the entity via the Command's mapper (`ToEntity(ctx)` / `ApplyTo(ctx, t)` / `ApplyPartiallyTo(ctx, t)`):

```go
func (*ArchiveUserCommand) ApplyTo(ctx *configuration.AppContext, u *User) {
    if id := ctx.Identity(); id != nil {
        if email, _ := id.Claims["email"].(string); email != "" {
            u.RequestingPrincipalEmail = email
        }
        u.RequestingPrincipalIsAdmin = id.HasPermission("users:admin")
    }
}
```

The Command is the only place identity → business field translation lives. Web stays Fiber-only (no ctx in `Request.ToCommand()`), domain stays IO-free, infra never sees the JWT. These runtime authz fields are runtime-only simply because they are NOT declared in the `TableSchema` — the persister never writes, scans, or audits an undeclared field, so request inputs, computed values, and runtime bookkeeping stay in memory automatically.

Once `BuildRules` emits the notification, the SQL `INSERT` / `UPDATE` / `DELETE` never runs; the wire response carries the canonical envelope with status from the notification's `Semantic()` (kernel `Update/Delete/Archive/UnarchiveNotAllowedNotification` all return `SemanticForbidden → 403`). Service-specific notifications (`NonOwnerArchiveForbiddenNotification`, `EmailLockedAfterActivationNotification`) follow the same pattern with their own typed identity.

### Layer 3 — Tenant scoping (cross-cutting)

**What it enforces.** Multi-tenant isolation — a principal in tenant A cannot see or modify resources of tenant B.

**How — three pieces wired by yaml.**

- **Claim presence gate at the middleware** (`auth.authorization.tenant.required: true`). After JWT validation succeeds, `AuthMiddleware` rejects any non-public request whose `Identity.TenantID()` returns empty — emits `TenantMissingNotification (403)` before any handler runs. Uniform across the service, no per-route declaration. Tenant is binary at the service level, not granular per route.
- **Reads** — `Query.ToCriteria(ctx)` injects `crit.Filter["tenant_id"] = ctx.Identity().TenantID()`. When `tenant.required: true`, the value is guaranteed non-empty (middleware enforced); when not, the defensive `if t != ""` guard remains.
- **Writes** — the Command's mapper populates `entity.TenantID = ctx.Identity().TenantID()`. `BuildRules` enforces that the resource's `TenantID` matches the requesting principal's claim on UPDATE / ARCHIVE / DELETE — emits `TenantMismatchNotification (403)` on mismatch.

The framework does **not** auto-inject tenant scoping at infra. Tenant is a domain concept; injecting it at `infra/postgres.go` or `MongoViewReader` would force them to pronounce domain vocabulary, violating the dependency rule (`infra → domain only`).

### Identity helpers

```go
// (i *Identity)
func (i *Identity) HasPermission(p string) bool
func (i *Identity) TenantID() string
```

Both are nil-safe (return false / empty string on nil Identity). `HasPermission` panics on caller-side wildcards (see Layer 1). `TenantID` reads the configured claim (default `tenant_id`; override via `auth.authorization.tenant.claim`) and tolerates `string`, `[]string{one}`, `[]any{one}` shapes.

The package-level setters live on `application/configuration` and are called by bootstrap from the yaml:

```go
configuration.SetPermissionsClaim(name string)  // default "permissions"
configuration.SetTenantClaim(name string)       // default "tenant_id"
```

### YAML — `auth.authorization` block

```yaml
auth:
  mode: jwt
  jwt: { ... }
  publicRoutes: [GET /health]
  authorization:                # default nil (layer off, identity helpers still work)
    enabled: true               # master switch — when false, runtime gate no-ops
    permissionsClaim: permissions  # default "permissions"
    tenant:
      enabled: false            # default false
      claim: tenant_id          # default "tenant_id"
      required: false           # true → AuthMiddleware emits TenantMissingNotification on empty
```

**Strict YAML decoding on `authorization` + `tenant`.** Unknown keys (e.g. `permissionClaim` typo for `permissionsClaim`) abort the boot with a diagnostic naming the offending key. Catches a class of silent misconfigurations.

**Validation cross-rules:**
- `authorization.enabled: true` requires `auth.mode: jwt` (cannot enforce on anonymous requests).
- `tenant.required: true` requires `tenant.enabled: true` (cannot enforce presence of a claim the layer is not reading).

### Notifications

| Notification | Emitter | Layer |
|---|---|---|
| `MissingPermissionNotification` | `web.PermissionGate` (runtime gate, Layer 1) | 1 |
| `Update/Delete/Archive/UnarchiveNotAllowedNotification` | service code in `BuildRules` | 2 |
| `TenantMissingNotification` | `AuthMiddleware` when `tenant.required: true` and Identity has empty TenantID | 3 |
| `TenantMismatchNotification` | service code in `BuildRules` or `Query.ToCriteria(ctx)` | 3 |

All four carry `SemanticForbidden → 403`. The Layer 1 emission carries `FieldName: "permission"` + `FieldValue: <the declared permission string>` so the wire response surfaces the missing scope.

### Why defense-in-depth at infra is deliberately absent

Adding a `ScopeFilter` knob to the `TableSchema` (e.g. an auto-appended `AND tenant_id = $X` on every write WHERE, or a forced filter entry on every read criteria) would force `infra/postgres.go` and `MongoViewReader` to pronounce domain concepts (tenant, owner, role). This violates the dependency rule (infra → domain only) and creates two sources of truth for the same rule — when domain and infra disagree, which wins? The canonical design keeps identity exclusively inside `application/` and `domain/`; infra executes what they approved. A bug bypassing `BuildRules` or `ToCriteria(ctx)` is a domain/application bug, fixed at that layer — not papered over by infra duplication.

## OpenAPI / Swagger UI

`omnicore/web/openapi` generates an OpenAPI 3.1.0 document from the same Go types the HTTP wrappers already consume — Request DTOs (with `json:`/`path:`/`query:`/`filter:` tags), Response DTOs (with `json:` tags), the FullBody marker, the HasPathID interface assertions. No `swag init`, no hand-written YAML, no separate annotations: the spec is a reflection-driven projection of the routes the consumer already wired.

### One registration path for typed routes, one for free-form routes

| Path | Used by | Carries |
|---|---|---|
| `openapi.Mount(registry, group, method, path, handler, spec, doc)` | Canonical wrappers (via `HandleCommand*Spec` / `HandleQuery*Spec` siblings) AND manual handlers that still parse a typed Request DTO | `RouteSpec{RequestType, ResponseType, SuccessStatus, Strict, HasPathID, Paged}` + `Doc{Summary, Description, OperationID, Tags, Deprecated, Hidden, Public}` |
| `openapi.MountRaw(registry, group, method, path, handler, raw)` | Routes without a typed Request DTO — auth identity demos, in-process upstreams, vendor-shaped showcase handlers | `RawSpec{Summary, Description, OperationID, Tags, Deprecated, Hidden, Public, Parameters, RequestBody, Responses}` |

Canonical wrappers expose **`*Spec` siblings** that return `(fiber.Handler, openapi.RouteSpec)`:

- `fwweb.HandleCommandWithBodySpec`
- `fwweb.HandleCommandWithBodyIDSpec`
- `fwweb.HandleCommandWithIDSpec`
- `fwweb.HandleQueryWithParamsSpec`
- `fwweb.HandleQueryWithIDSpec`

Each sibling is generic-by-generic identical to the non-`Spec` wrapper; the only addition is the `RouteSpec` value the sibling returns alongside the handler. `Strict` is detected by type-asserting the handler against `pipeline.FullBodyEnforcer`; `HasPathID` is true for every wrapper that auto-binds the Fiber `:id` segment; `Paged` is true on `HandleQueryWithParamsSpec` (paged listing) and false on every other sibling (single-item or write).

### Paged success envelope (RouteSpec.Paged)

Routes that emit via `fwweb.RespondPaged` have a different envelope from routes that emit via `fwweb.RespondWithSuccess`: the paged shape carries `data` as an array of `ResponseType` items AND a top-level `pagination` property (`web.PaginationInfo` — `has_next`, `has_prev`, `next_cursor` (omitempty), `prev_cursor` (omitempty), `total`). The single-item shape carries `data` as one `ResponseType` and no `pagination`.

`RouteSpec.Paged` controls which one the spec assembler renders, so the rendered schema matches the runtime output verbatim. `HandleQueryWithParamsSpec` sets `Paged:true` automatically — the canonical surface has zero change. Manual mounts opt in via `fwopenapi.RouteSpecOfPaged[TReq, TResp](status)` — sibling of `RouteSpecOf` that returns the same struct with `Paged:true` set. Pairing `Paged:true` with `fwresponses.None` (or a nil `ResponseType`) is a semantic contradiction — paging requires per-item shape — and panics at `Mount` time.

`PaginationInfo` is materialized as a named component schema (`#/components/schemas/PaginationInfo`) and referenced via `$ref` on every paged response, so the spec stays deduplicated. The `?onlyTotal=true` runtime variant (which morphs the envelope to `pagination: TotalOnlyPagination{total}` and drops `data`) is documented in prose on each operation's `Doc.Description` — keeping the success schema as the canonical listing shape rather than a `oneOf` keeps Swagger UI tidy on the common path.

### Schema generator coverage

`openapi.Generator` walks a `reflect.Type` and produces an in-memory `openapi.Schema`. Coverage in this round:

| Go type | OpenAPI schema |
|---|---|
| `string` / `*string` | `{type: string, nullable?: true}` |
| `int*` / `uint*` | `{type: integer, format: int32 \| int64}` |
| `float32` / `float64` | `{type: number, format: float \| double}` |
| `bool` / `*bool` | `{type: boolean, nullable?: true}` |
| `time.Time` / `*time.Time` | `{type: string, format: date-time, nullable?: true}` |
| `uuid.UUID` | `{type: string, format: uuid}` |
| `domain.ID` | `{type: string, format: uuid}` |
| `[]T` | `{type: array, items: <T>}` |
| `[]byte` | `{type: string, format: byte}` (base64-encoded by encoding/json) |
| `map[string]T` | `{type: object, additionalProperties: <T>}` |
| `map[string]any` | `{type: object, additionalProperties: true}` |
| Named struct | `$ref` into `components/schemas/<Name>` — registered once, referenced everywhere |
| Anonymous embed | Flattened inline (mirrors `encoding/json`'s field promotion) |
| Anonymous struct field (`Bar struct{...}`) | Inlined object (no `$ref`) |
| Field with `json:"-"` | Skipped |
| Field with `path:"X"` | Skipped from body schema; surfaces as a path parameter |
| Field with `query:"X" filter:"ops"` | Skipped from body schema; surfaces as one query parameter per operator (`name`, `name.in`, `name.gte`, `name.startswith`, `name.icontains`, …) |
| Field with `example:"value"` | Sets the property's `example` in the rendered schema — Swagger UI shows the concrete value instead of the type-default placeholder. Supported types: string, bool, signed/unsigned ints, floats, `uuid.UUID`, `domain.ID`, `time.Time` (RFC3339), and pointers to any of these. Composite types (struct / slice / map) on the same field → boot panic with a message pointing at `Doc.RequestExamples` / `Doc.ResponseExamples` (the map-based path for N exemplos / shapes the tag cannot express). Parse failure (`example:"not-a-number"` on int) → boot panic naming the struct + field |

Required-field rule:

- **Strict** (handler embeds `pipeline.FullBody`): every kept field is required.
- **Lenient**: required when the field is non-pointer AND the json tag does not carry `,omitempty`.

Schemas are cached by `(reflect.Type, strict)` in the Generator's `sync.Map`; named structs cache by name in `Components` so a self-referential type (linked list, tree) does not infinite-loop — the field finds the reserved placeholder and emits the `$ref` on its way back up.

### Response envelope

Successful responses wrap the consumer's `ResponseType` in the framework's canonical `web.Response` envelope (`success`, `status`, `description`, `data`). When the projection lands on `responses.None` (the `fwresults.None` / `fwresponses.NoBody` default for Archive/Unarchive/Delete), the envelope is emitted WITHOUT the `data` field — matching the runtime behavior of `respondWithProjection`.

Standard error envelopes are auto-added per route based on its shape:

| Status | Added when |
|---|---|
| 400 Bad Request | Route carries a request body (canonical `RequestType` with body fields OR `RawSpec.RequestBody`) |
| 401 Unauthorized | Auth is enabled AND the route is not public |
| 404 Not Found | `HasPathID=true` OR `RawSpec` declares a path parameter |
| 422 Unprocessable Entity | Always (every route can produce domain-validation rejections) |
| 500 Internal Server Error | Always (recovered panic / escaped error) |

All error responses reference the single `ErrorEnvelope` schema in `components/schemas` — mirrors `web/response.go::Response` with `errors[]` populated and the typed `ErrorMessage` shape (notificationKey, field, value, funcName, message, semantic).

**Custom error status via `Doc.ResponseExamples`.** A canonical route may emit a status outside the auto-added set above — typically when a service notification overrides `Semantic()` to `Conflict`/`Unavailable`/etc. Declaring `Doc.ResponseExamples[N]` for that status auto-creates the response entry in the spec, reusing the `ErrorEnvelope` schema and the consumer's examples. Description comes from `http.StatusText(N)`. The `default` auto-merge applies only on statuses with a `DefaultErrorExample` entry (400/401/403/404/422/500) — non-default statuses surface exactly the consumer's declared examples, no canonical merge.

### Path parameters

For canonical routes:

1. Walk `RouteSpec.RequestType` for `path:"X"` tags → emit one `parameters[].in=path` per tag.
2. Walk the Fiber path string for `:name` segments → for each segment not covered by (1), emit a string-typed stub.

Result: a request DTO that declares `path:"email"` AND a path `/users/:id` (with `HasPathID=true` and the framework's `:id` auto-bind) ends up with TWO path parameters — `email` from the tag, `id` auto-stubbed from the path. Symmetric routes like `/tenants/:tenantId/users/:id` work without special-casing.

### Auth declarations

`openapi.WithAuth(AuthContext{PublicRoutes: [...]})` is the functional option `bootstrap.Run` passes to `openapi.Register` when `auth.mode: jwt`. When applied:

- `components.securitySchemes.bearerAuth` is materialized (`http`/`bearer`/`JWT`).
- Every operation that is NOT public gains `security: [{bearerAuth: []}]` + a 401 entry in `responses`.
- "Public" is detected by `Doc.Public` / `RawSpec.Public` OR by exact `METHOD /path` match against `AuthContext.PublicRoutes` (the same allowlist `AuthMiddleware` enforces at runtime).

Bootstrap auto-extends `PublicRoutes` with `GET /openapi.json` + `GET /docs` so the documentation surface never lands behind the bearer wall.

### Bootstrap integration

`Wiring.OpenAPI *openapi.Config` is opt-in: nil → the framework registers nothing OpenAPI-related, `Deps.OpenAPIRegistry` stays nil, every `openapi.Mount` / `MountRaw` call becomes a Fiber-only passthrough (zero documentation cost). Non-nil → bootstrap constructs an `*openapi.Registry`, threads it through `Deps`, registers `GET /health` against it via `MountRaw`, and after every feature has mounted calls `openapi.Register(app, *Wiring.OpenAPI, deps.OpenAPIRegistry, WithAuth(...))` to serve `GET /openapi.json` + `GET /docs`.

The Swagger UI HTML at `/docs` loads `swagger-ui-dist` from unpkg.com CDN by default. Consumers needing offline operation override `/docs` after `Register` returns — last write wins on the Fiber router — and serve their own HTML pointing at `/openapi.json`.

### Language selector (Swagger UI dropdown)

`openapi.Config.LanguageSelector bool` (default false) opts the rendered `/docs` page into a global language dropdown. When true, the HTML carries a `<select id="omnicore-lang-selector">` above the Swagger UI surface and a `requestInterceptor` that writes the selected value into `Accept-Language` on every "Try it out" call. The dropdown content lives on `openapi.Config.Languages []LanguageOption` (`{Label, Value}` pairs).

**`Wiring.Translations` is mandatory.** `validateWiring` rejects boot when the slice is empty — independent of `LanguageSelector`. Notification messages, error envelopes (`ErrorMessage.Message`), and audit fields all flow through the `Translator`; booting with an empty Translator silently produces blank strings in production. Loud at boot is the framework's chosen failure mode: every consumer **must** declare at least one `translation.Module`.

**Auto-population.** Under `bootstrap.Run`, `Languages` is filled automatically when `LanguageSelector=true` AND the consumer left `Languages` empty. The bootstrap walks `Wiring.Translations`, dedupes by `configuration.Language` (a service that registers both the framework's `CorePTBR` and its own `apptrans.PTBR()` collapses to a single `PT_BR` entry), and emits `{Label: Lang.String(), Value: Lang.HTTPPrefix()}` per surviving entry. The microservice has the final word on **which languages appear in the dropdown** — registering only `apptrans.PTBR()` shows only `PT_BR`, even though `translation.Default()` loads four behind the scenes. `LangUnknown` and any language with an empty `HTTPPrefix()` are skipped.

**English-first default.** When `LangENG` is among the surviving entries, the bootstrap rotates it to position 0 so HTML's natural `<select>` behavior selects English as the default. Declaration order is otherwise preserved (after the rotation). When ENG is absent, declaration order is preserved as-is — the first declared language wins the default slot.

**Explicit override.** A non-empty `Languages` slice on `openapi.Config` bypasses auto-discovery entirely — useful for manual `Wire` flows that bypass `bootstrap.Run`, or for relabeling entries ("English" instead of "ENG"). The framework does not apply ENG-first rotation on an explicit slice (the consumer is in control of order). The CSS, HTML, and `requestInterceptor` are gated on `len(Languages) > 0`; an empty `Languages` (auto or explicit) renders the page byte-identical to the no-selector baseline.

**Wire format.** `HTTPPrefix()` returns the same prefix `web/app_context.go::parseLanguage` matches at the request boundary (`pt` / `en` / `es` / `fr`), so what the dropdown injects is exactly what the `AppContextMiddleware` already understands — error messages flip language without any code change in the handler or the notification.

### Hidden vs Public

- **Hidden=true**: operation is excluded from the rendered spec entirely. Still registers on Fiber so the route works at runtime; only the documentation surface omits it. Use for internal upstreams (the `/echo/*` showcase producer routes) that have no business appearing in a public spec.
- **Public=true**: operation appears in the spec WITHOUT `security: [{bearerAuth: []}]` and WITHOUT the 401 entry. Use for routes that genuinely bypass auth (health probes, OIDC discovery proxies, identity demos).

## Cache subsystem

`omnicore/infra/cache` is the framework's generic byte-level key-value cache subsystem. The `Cache` port is the single contract consumer code, domain services, infrastructure adapters, and the outbound httpclient all consult — there is no per-component cache anymore. Two canonical implementations ship with the framework (in-process LRU+TTL via `cache.NewMemory`, Redis via `cache.NewRedis`); consumers wanting a different backend (Memcached, Valkey, Hazelcast, …) implement the same `cache.Cache` interface and inject the implementation via `bootstrap.Wiring.Cache` / `bootstrap.Wiring.SharedCache`.

### Interface

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Delete(ctx context.Context, key string) error
}
```

Intentionally narrow:

- `Get` returns `(value, true, nil)` on hit, `(nil, false, nil)` on logical miss, `(nil, false, err)` on transport / decode failure.
- `Set` accepts `ttl == 0` as "no expiration" (backends with a native no-expire honor it; memory uses a zero `ExpiresAt` sentinel; Redis writes `SET` without `EX`/`PX`). Negative TTLs are rejected with `cache.ErrInvalidTTL`.
- `Delete` is idempotent — missing keys are not an error.
- No `Has` (use `Get`); no `Clear` (operator-owned via the backend's CLI; "wipe everything" is too sharp to expose as code); no batch ops yet.

**Typed JSON helpers** sit outside the interface so backends don't have to think about Go types:

```go
func cache.GetJSON[T any](ctx context.Context, c cache.Cache, key string) (T, bool, error)
func cache.SetJSON[T any](ctx context.Context, c cache.Cache, key string, value T, ttl time.Duration) error
```

Both tolerate a nil `Cache` and degrade to no-op (miss / no-op write) so feature code that opts the cache in-or-out via YAML doesn't need a nil guard around every call site.

### Two cache instances per service — Private and Shared

The framework exposes TWO `cache.Cache` instances on `bootstrap.Deps`:

| Field | Scope | When populated | Use for |
|---|---|---|---|
| `Deps.Cache` | Service-private | YAML `cache:` block declared | Anything scoped to this service — domain memoization, computed-value caching, the outbound httpclient response cache (framework wires its own middleware to consume this same instance) |
| `Deps.SharedCache` | Cross-service | YAML `cache.shared:` sub-block declared | Keys other services in the cluster are expected to read — feature flags coordinated across services, cluster-wide rate limits, sessions consumed by an API gateway |

The distinction is enforced at the **dependency-injection level**, not via a flag on the method. Code that wants to write a shared value writes to `Deps.SharedCache`; code that doesn't is structurally unable to. Default safety: `Deps.SharedCache` is `nil` unless the operator declares `cache.shared:` — feature code touching it MUST guard explicitly. The httpclient response cache uses `Deps.Cache` (private) unconditionally.

**`cache.shared.store: memory` is REJECTED at boot.** An in-process LRU cannot honor cross-service reads; the framework forces the operator to pick `redis` or `custom` for the shared cache.

### YAML — top-level `cache:` block

```yaml
cache:
  store: memory                     # memory (default) | redis | custom
  maxEntries: 10000                 # only memory; default 10000
  redis:                            # required when store: redis, rejected otherwise
    addr: ${REDIS_ADDR:localhost:6379}
    password: ${REDIS_PASSWORD}
    db: 0
    keyPrefix: "${SERVICE}-cache"
    failMode: open                  # open (default) | closed
    timeoutMs: 100

  shared:                           # OPTIONAL second cache instance
    store: redis                    # redis | custom — memory rejected at boot
    redis:
      addr: ${SHARED_REDIS_ADDR}
      keyPrefix: "${TEAM}-shared"
      failMode: open
      timeoutMs: 100
```

When the top-level `cache:` block is omitted entirely, `Deps.Cache` stays nil — the httpclient cache layer bypasses, and `cache.GetJSON`/`SetJSON` degrade to no-ops. Services that don't need caching pay zero cost.

### Backend selection cascade

The same matrix applies independently to `cache.store` and `cache.shared.store`. Mismatched wiring fails the boot with a structural-coherence error:

| `store` | `Wiring.Cache` (or `Wiring.SharedCache`) | Result |
|---|---|---|
| `memory` (or unset) | nil   | in-process LRU runs |
| `memory` (or unset) | non-nil | boot panic — "declare `store: custom` to use `Wiring.Cache`" |
| `redis`             | nil   | go-redis backed adapter constructed from `redis:` sub-block |
| `redis`             | non-nil | boot panic — same message |
| `custom`            | nil   | boot panic — "`Wiring.Cache` required when `store: custom`" |
| `custom`            | non-nil | injected instance runs |

`cache.shared.store: memory` is additionally rejected at boot — only `redis` and `custom` are accepted for the shared cache.

### Redis adapter — `failMode`

The framework's Redis backend (`cache.NewRedis(*RedisConfig)`) resolves the configured `failMode` internally:

- **`open` (default)** — swallows transport errors, emits `slog.Warn "cache.redis.transport.error"` with `{op, key, error, failOpen}`, and returns `(nil, false, nil)` on `Get` / `nil` on `Set`/`Delete`. The caller sees a miss; the call proceeds as if the cache were disabled for that request.
- **`closed`** — propagates the error verbatim. The middleware aborts (or the consumer code surfaces the error).

Logical misses (`redis.Nil`) and corrupted entries are NOT errors — they always behave as miss regardless of `failMode`. The connection is lazy — Redis unreachable at boot does NOT block `New()`; the first `Get`/`Set` surfaces the failure through `failMode`.

### Late injection — `Wiring.Cache` and `Wiring.SharedCache`

`Wiring.Cache` and `Wiring.SharedCache` are the escape hatches for `cache.store: custom`. The framework resolves them AFTER the `wire(deps)` callback runs, so consumer code that constructs the custom backend can use any infrastructure available to features (additional connections, secrets manager handles, …). The httpclient picks up the late-resolved private cache via `httpclient.HttpClient.SetCache` (atomic pointer swap; no chain rebuild).

### Quick reference — common patterns

| Need | Path |
|---|---|
| Read or write a private cache value from a handler | `value, ok, err := cache.GetJSON[Profile](ctx, deps.Cache, "user:42:profile")` / `cache.SetJSON(ctx, deps.Cache, "user:42:profile", v, 5*time.Minute)` |
| Read or write a shared cache value | Same helpers but against `deps.SharedCache` — guard against nil first if the operator may have omitted `cache.shared:` |
| Plug a custom backend (Memcached / Valkey / Hazelcast / …) | Implement `cache.Cache`, declare `cache.store: custom` (or `cache.shared.store: custom`) in YAML, inject the implementation via `bootstrap.Wiring.Cache` / `bootstrap.Wiring.SharedCache` |
| Disable the cache entirely | Omit the top-level `cache:` block. `Deps.Cache` and `Deps.SharedCache` stay nil; httpclient cache middleware short-circuits as `"bypass"` |
| Override the httpclient response cache TTL per endpoint | `httpClient.services.X.endpoints.Y.cache.ttl: 1m` — endpoint TTL wins over `httpClient.defaults.cache.defaultTTL`. The backend is whatever `Deps.Cache` resolved to |

## httpclient package

`omnicore/infra/httpclient` is the outbound HTTP subsystem. Services describe the external systems they talk to in `microservice.<profile>.yaml` under the `httpClient:` block; the framework constructs a singleton `*httpclient.HttpClient` registry on `bootstrap.Deps.HttpClient`. Each declared service gets its own `http.Transport` and `http.Client` so a misbehaving upstream cannot starve the pool of well-behaved ones.

The currently consumed surface is the **declarative config layer**: schema parsing, validation, defaults cascade, per-service transport materialization. The typed `Call[Req, Resp]` generic, request/response binding via `http:"..."` tags, JSON codec, middleware chain (correlation/logging/transport), `HttpError`, and slog observability land together as one canonical surface in the next phase. Auth providers, retry, cache, circuit breaker, redaction, signing, streaming and the `NewFake` harness arrive on their own phases. Until the call surface lands, `*httpclient.HttpClient` is the registry consumer services already wire into features by anticipation — `infra/external/<svc>.go` constructors accept it as a parameter.

### YAML schema (current scope)

```yaml
httpClient:
  defaults:
    timeout: 30s                       # default 30s
    threadIdHeader: X-Thread-Id        # default X-Thread-Id
    requestIdHeader: X-Request-ID      # default X-Request-ID
    logBodies: true                    # default true
    headers:
      User-Agent: "omnicore-svc/1.0"

  services:
    keycloak:
      baseURL: https://kc.example.com           # mandatory
      timeout: 10s                              # optional, overrides defaults.timeout
      headers:                                  # optional, merged after defaults.headers
        X-Tenant: acme
      endpoints:
        getUser:
          method: GET                           # mandatory (GET/POST/PUT/PATCH/DELETE/HEAD)
          path: /users/{id}                     # mandatory (must start with '/')
          requestCodec: json                    # optional, default json
          responseCodec: json                   # optional, default json
          acceptableStatus: [404]               # optional, default []
          headers:                              # optional, merged after service.headers
            Accept: application/json
```

### Validation guarantees at boot

`httpclient.New(cfg)` runs `applyDefaults` then `Validate` before constructing any transport. Issues are accumulated and emitted in a single error so the operator sees the whole list on one boot attempt:

- `services.<name>.baseURL` required + must parse as an absolute URL
- `services.<name>.endpoints` must declare at least one endpoint
- `endpoints.<name>.method` required + must be one of GET/POST/PUT/PATCH/DELETE/HEAD (case-insensitive)
- `endpoints.<name>.path` required + must start with `/`
- `requestCodec` / `responseCodec` must be `json` (other codecs arrive in the codec expansion phase)
- `acceptableStatus` values must be in `100..599`
- `timeout` must be non-negative

### Config block coverage

Every `httpClient` block is implemented and validated at boot — `auth`, `authProviders`, `retry`, `cache`, `circuitBreaker`, `pool`, `tls`, `redaction`, `idempotency`, `signing`, and the per-endpoint streaming flags `responseStream` / `responseSSE`. `Validate` runs per-block coherence checks (see the dedicated subsections); there are no reserved-and-rejected blocks.

### Defaults cascade

Headers compose `defaults → service → endpoint` (last write wins) at construction time, so the request path performs zero map lookups against the declarative YAML. Timeout cascades `service → defaults → 30s framework fallback`. Pool defaults applied to every per-service `http.Transport`:

- `MaxIdleConnsPerHost: 100`
- `MaxConnsPerHost: 200`
- `IdleConnTimeout: 90s`

The design schema reserves per-service `pool:` overrides for the TLS / connection-pool phase; rejected today.

### Tag binding (request/response shape)

DTOs that drive a request carry `http:"..."` tags on exported fields. The binding subpackage parses each tag once per `reflect.Type` (cached in a `sync.Map`) and the call path then walks a pre-built plan — there is no reflection on the hot path beyond the cached lookup.

| Tag form | Destination | Accepted field type |
|---|---|---|
| `http:"path,id"` | URL path placeholder `{id}` | string / numeric / bool / pointer to those |
| `http:"query,verbose"` | `?verbose=…` | same as path; empty values are omitted from the URL |
| `http:"query,tags,csv"` | `?tags=a,b,c` | slice / array of scalars |
| `http:"query,tags,multi"` | `?tags=a&tags=b` | slice / array of scalars |
| `http:"header,X-Tenant"` | request header | string-convertible scalar |
| `http:"headers"` | dynamic headers | `map[string]string` |
| `http:"body,json"` | request body | any JSON-marshalable value |
| `http:"body,xml"` | request body | struct with `xml:"..."` tags |
| `http:"body,form"` (alias of `body,form-urlencoded`) | request body | `url.Values`, `map[string]string`, or struct with optional `form:"key,omitempty"` tags |

Inspection enforces structural invariants at request build time so misuse fails fast:

- Every `{placeholder}` in `Path` must have a `http:"path,name"` field on the request DTO, and vice-versa.
- Only one `http:"body,..."` field per request DTO.
- The kind must match the field type (e.g. `csv`/`multi` require a slice/array; `headers` requires `map[string]string`).
- Currently only `body,json` is accepted; other body codecs surface a "not yet supported in the current phase" error.

The response DTO follows the same convention. A response field tagged `http:"header,ETag"` is populated from the corresponding response header; the body field tagged `http:"body,json"` is decoded via the endpoint's `responseCodec`. When the response DTO has no tagged fields the entire struct is decoded as the body — the common case. An empty `struct{}` discards the body silently (useful for `DELETE` or `204` endpoints).

Example:

```go
type GetUserRequest struct {
    ID       string            `http:"path,id"`
    Verbose  bool              `http:"query,verbose"`
    TenantID string            `http:"header,X-Tenant-ID"`
    Extra    map[string]string `http:"headers"`
}

type GetUserResponse struct {
    ID    string `json:"id"`
    Email string `json:"email"`
}
```

### Codecs

The codec registry is **package-private** — there is no `httpclient.RegisterCodec` and no consumer-facing extension surface. Three codecs ship today:

- `json` (default) — `encoding/json`; Content-Type `application/json`
- `xml` — `encoding/xml`; Content-Type `application/xml`; struct fields use `xml:"..."` tags
- `form-urlencoded` — `net/url.Values.Encode`; Content-Type `application/x-www-form-urlencoded`. Accepts `url.Values`, `map[string]string`/`map[string][]string`, or struct (field name lowercased by default; override with `form:"key[,omitempty]"`). `form` is the short alias on the tag side.

A YAML or tag that names an unknown codec rejects at boot.

### Call surface

`httpclient.Call[Req, Resp]` is the single typed entry point. There is no parallel untyped public path. The signature is:

```go
func Call[Req any, Resp any](
    ctx context.Context,            // AppContext satisfies it
    c *HttpClient,
    service, endpoint string,       // YAML keys
    req Req,                        // typed struct with http:"..." tags
    opts ...InvokeOption,
) (Resp, error)
```

The `InvokeOption` family is intentionally tight — three categories, each justified:

**1. `WithConfig(CallConfig)` — the canonical per-call YAML-override surface.** Every YAML-declared knob that makes sense to flip at call time is a field on `CallConfig`. One option, discoverable via IDE autocomplete, and adding a new YAML field automatically becomes runtime-overridable when the same field lands on the struct. Fields:

| Field | Overrides |
|---|---|
| `BaseURL string` | `services.<name>.baseURL` (wins over the resolver too) |
| `Timeout time.Duration` | service / defaults timeout (0 = inherit) |
| `AuthProvider string` | `services.<name>.auth.provider` (selects a YAML-declared provider) |
| `Method string` | `endpoint.method` |
| `Path string` | `endpoint.path` (must start with `/`) |
| `RequestCodec string` | `endpoint.requestCodec` |
| `ResponseCodec string` | `endpoint.responseCodec` |
| `AcceptableStatus []int` | extends `endpoint.acceptableStatus` (union) |
| `NoCache bool` | bypasses the GET cache when true |
| `CacheKey string` | overrides the framework-computed cache key |
| `IdempotencyKey string` | supplies the explicit key when `source: explicit` |
| `Retry *RetryOverride` | replaces `endpoint.retry` with a per-call policy |
| `InlineAuth *InlineAuth` | runtime credentials (Bearer / APIKey / Basic) — see below |

**`InlineAuth` — credentials supplied per call.** YAML-declared auth providers cover the case where credentials are known at boot. `InlineAuth` is the inverse: credentials are runtime values the framework cannot know in advance. Canonical motivation is a webhook delivery service with thousands of customer rows, each carrying its own Bearer/API key/Basic password — declaring one YAML provider per customer doesn't scale.

```go
type InlineAuth struct {
    Bearer string      // → Authorization: Bearer <Bearer>
    APIKey *APIKeyAuth // → custom header (default X-API-Key)
    Basic  *BasicAuth  // → Authorization: Basic base64(Username:Password)
}
type APIKeyAuth struct { Header, Value string } // Header empty → "X-API-Key"
type BasicAuth  struct { Username, Password string }
```

Exactly one scheme must be set. Setting zero or more than one returns `*HttpError{Cause: ErrTokenAcquire}` **before dialing** — the framework refuses ambiguous shapes rather than guessing. On the wire:

| Scheme | Outbound header | Value |
|---|---|---|
| `Bearer: "abc"` | `Authorization` | `Bearer abc` |
| `APIKey: &APIKeyAuth{Value: "k"}` | `X-API-Key` (default) | `k` |
| `APIKey: &APIKeyAuth{Header: "X-Tenant-Key", Value: "k"}` | `X-Tenant-Key` | `k` |
| `Basic: &BasicAuth{Username: "u", Password: "p"}` | `Authorization` | `Basic dTpw` |

Precedence: `InlineAuth` wins over `AuthProvider` when both are set in the same `CallConfig`. The slog field `authProvider` logs `inline:bearer` / `inline:apikey` / `inline:basic`. The framework materializes `InlineAuth` into an ephemeral `auth.AuthProvider` and runs it through the same middleware as a YAML provider (position 3 of the chain) — every other layer (logging, idempotency, cache, retry, breaker, transport) behaves identically.

What does NOT come with `InlineAuth`: token cache, single-flight, `revocationOnUnauthorized`. Inline credentials are static for one call — there is no acquisition flow to memoize or revoke. For OAuth client-credentials per tenant that DO need caching, declare a `credentials-exchange` provider with `requestFieldsFromCtx` instead — that path has per-identity token caching.

`RetryOverride` fully replaces the endpoint's YAML retry policy when non-nil — no field-level merging. The framework defaults fill any field the caller leaves zero (`MaxAttempts` falls back to the enabled-default 3, `Backoff` falls back to `exponential-jitter`, etc.). The POST/PATCH safety gate still applies: even an override forcing `MaxAttempts > 1` is clamped to 1 unless the endpoint declares idempotency.

**2. Additive runtime injection — `WithExtraHeader(k, v)` / `WithExtraQuery(k, v)`.** Pure runtime concerns that the YAML cannot anticipate (per-call tenant id generated in the handler, ad-hoc query parameter). Not overrides of any YAML field — true additions on top of the cascade.

**3. `WithClientCert(tls.Certificate)`.** Binary `tls.Certificate` value, not a YAML-declarable string. Builds an ephemeral cloned transport so the registry's pool stays clean. Use for vault-rotated certs delivered via the secrets manager.

**Why not include TLS minVersion / cipher suites / CA bundle / pool sizes in `CallConfig`?** Those are bound to the `http.Transport` materialized once at `New`. Overriding them per-call would require rebuilding the transport for that single call, invalidating the connection pool and defeating keep-alive. The trade-off is not worth it; declare those in YAML and reboot to change.

Flow:

1. Resolve the service + endpoint from the YAML (unknown name → descriptive error before dialing).
2. Apply `InvokeOption`s cumulatively.
3. `binding.BuildRequest` assembles the `*http.Request` (path placeholders, query, headers, body).
4. Correlation headers are injected from `AppContext.ID()`: `ThreadIDHeader` and `RequestIDHeader` both carry the same value (the codebase has a single per-request identifier).
5. Per-call extras are layered on top (header overrides existing, query appends).
6. `service.httpClient.Do(req)` dials.
7. Response body is read in full, the slog observation is emitted (one record per call), then the response is decoded.

Status branching:

- `2xx` → decoded `Resp`, `nil` error.
- Status in the endpoint's `acceptableStatus` or in `WithAcceptableStatus(...)` → decoded `Resp` *and* `*HttpError{Acceptable: true}`. The consumer uses `IsAcceptableStatus(err, codes...)` to branch.
- Other non-2xx → zero `Resp` and `*HttpError`.
- Transport failure / decode failure → zero `Resp` and `*HttpError` with `Cause` wrapping the underlying error.

### Error model

```go
type HttpError struct {
    Service, Endpoint string
    Method, URL       string
    Status            int
    Headers           http.Header
    Body              []byte
    Duration          time.Duration
    Cause             error
    Acceptable        bool
    Attempt           int  // attempt number reached when the call ended (1 when no retry policy, final attempt count otherwise)
}
```

Helpers:

- `IsAcceptableStatus(err error, codes ...int) bool` — true only for `*HttpError{Acceptable: true}` whose `Status` matches one of `codes` (or any acceptable status when no codes are given).
- `IsRetriable(err error) bool` — true for 502/503/504, timeouts, DNS failures, generic `net.OpError`. False for caller cancellation. Used today only as a slog hint; auto-retry arrives in the dedicated phase.
- `IsCircuitOpen(err error) bool` — pattern-matches `ErrCircuitOpen`; always false today, present so future code does not break when the breaker phase ships.

Sentinels:

- `ErrRequestBuild` — failure to assemble the `*http.Request` from the typed `Req`.
- `ErrResponseDecode` — failure to decode `Resp` from the body / headers.
- `ErrTokenAcquire`, `ErrCircuitOpen` — declared today, consumed by future phases.

### Slog observation

Every outbound call writes one structured record at `LogAttrs(level=Info|Warn, msg="http.outbound", ...)`. Default fields:

| Field | Source |
|---|---|
| `threadId` | `AppContext.ID()` |
| `downstreamThreadId` | response header (when present and differs from outbound) |
| `service`, `endpoint`, `method`, `url` | call arguments + computed |
| `status`, `durationMs`, `requestBytes`, `responseBytes` | response + timing |
| `requestHeaders`, `responseHeaders` | with default block-list redaction (see below) |
| `requestBody`, `responseBody` | only when `defaults.logBodies = true` |
| `attempt` | attempt number when the call ended (`1` without a retry policy, final attempt count when retry fired) |
| `cacheStatus` | `"hit"` / `"miss"` / `"bypass"` based on the cache middleware's decision (`"bypass"` when no cache policy is wired for the endpoint) |
| `breakerState` | `"closed"` / `"open"` / `"half-open"` snapshot from the breaker middleware (`"closed"` when no breaker policy is wired) |
| `authProvider` | configured provider `Name()` when an auth provider runs; `""` when the endpoint has no `auth:` block |

The level is `Warn` when the call ended in error (transport, non-2xx, decode), `Info` otherwise.

Redaction is cascaded — framework defaults always apply, then the YAML's `defaults.redaction` block extends, then per-service `redaction:` blocks extend further. Removal of an entry is not possible; the cascade is union-only.

```yaml
httpClient:
  defaults:
    redaction:
      headers: [X-Internal-Trace]                          # framework defaults always apply
      bodyJSONPath: [$.password, $.creditCard.number]      # JSON-only; non-JSON bodies emitted verbatim
      queryKeys: [client_secret]                           # extends framework token/api_key/access_token/signature/code

  services:
    keycloak:
      baseURL: https://kc.example.com
      redaction:
        headers: [X-KC-Internal]                           # per-service adds on top
        bodyJSONPath: [$.credentials.password]
```

**Framework defaults always applied** (cannot be removed):

- **Headers** (canonical MIME names): `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-API-Key`.
- **Query keys** (case-insensitive): `token`, `api_key`, `access_token`, `signature`, `code`.
- **Body JSONPath**: no defaults — operators opt in to mask sensitive request/response fields.

**Body redaction.** Best-effort JSON: the framework tries `json.Unmarshal`, walks each configured dot-notation path (e.g. `$.user.password`), replaces the leaf with `"[REDACTED]"`, and re-encodes. Non-JSON bodies and parse failures fall through verbatim — the slog line never fails because of a redaction step.

**Query redaction.** URL query parameters whose key (case-insensitive) matches any entry in the policy's queryKeys set have their value replaced with `"[REDACTED]"` in `obs.URL` before the slog line is written. The path and unlisted parameters survive unchanged.

The original headers, bodies, and URL on the wire are **never** modified — redaction only affects what slog observes.

### Middleware chain

`Call` dispatches every request through a fixed middleware chain. The order is canonical, not configurable by service or by call — there is one path that every outbound call traverses. New phases insert their middleware at the documented position; existing layers stay where they are.

The chain has up to nine positions. Positions 1, 2, 6, 7, 8, 9 are always wired; positions 3, 4, 5 are appended only when the corresponding endpoint policy is configured.

| Position | Layer | When wired | What it does |
|---|---|---|---|
| 1 (outermost) | `correlationMiddleware` | always | Injects `X-Thread-Id` and `X-Request-ID` from `AppContext.ID()`, records `obs.ThreadID` and the configured header name |
| 2 | `loggingMiddleware` | always | Captures request body, times the round-trip, buffers and captures response body, emits the single slog observation record, recovers `downstreamThreadId` from the response |
| 3 | `authMiddleware` | endpoint resolved an auth provider (`provider != nil`) | Applies the provider's credential to the request before signing; on `RevocableProvider` + 401, invalidates the cached credential and dispatches once more. Token-acquisition failures return `*HttpError{Cause: ErrTokenAcquire}` so retry bails out instead of burning attempts on a stuck IdP |
| 4 | `idempotencyMiddleware` | `endpoint.idempotency.enabled = true` | Injects the per-call idempotency key on the request once, before retry begins, so every retried attempt carries the same key — making POST/PATCH safe under an upstream that dedupes on the key. Default source: UUIDv7 (sortable). Explicit source requires the caller to supply the key via `CallConfig.IdempotencyKey` |
| 5 | `cacheMiddleware` | store wired AND `endpoint.cache.enabled = true` | Looks up the response by canonical key; on hit, short-circuits without calling `next` (skips breaker, signing, transport); on miss, stores the response after the call when its status is in `cacheAcceptable` |
| 6 | `retryMiddleware` | always | Re-dispatches on retriable failures per the effective policy. Sits outside the breaker so each retry attempt counts as a breaker observation (per design Q7) |
| 7 | `breakerMiddleware` | always | Records success/failure into the per-service circuit; short-circuits with `ErrCircuitOpen` when open. Inside retry, outside signing so rejected attempts don't waste signing work |
| 8 | `signingMiddleware` | always | Computes timestamp + content-sha256 + HMAC signature on every attempt. Inside breaker (rejected attempts skip signing) and outside transport (the dialed request carries the headers) |
| 9 (terminal) | `transportMiddleware` | always | Dials via the per-service `http.Client` (own `http.Transport`, own pool, own timeout) |

A middleware that short-circuits (e.g. a cache hit) returns without calling `next`; the chain stops there. Layers form a stack: pre-work happens on the way down (outer-to-terminal), post-work on the way back up.

### Retry

The chain inserts the retry middleware at position 8 — between the (future) breaker and the terminal transport. Configuration is declarative:

```yaml
httpClient:
  defaults:
    retry:
      maxAttempts: 3
      backoff: exponential-jitter      # constant | linear | exponential | exponential-jitter
      initialDelay: 100ms
      maxDelay: 5s
      retryOn: [502, 503, 504, "network", "timeout"]
      respectRetryAfter: true

  services:
    keycloak:
      baseURL: https://kc.example.com
      endpoints:
        getUser:
          method: GET
          path: /users/{id}
          retry:
            maxAttempts: 5             # endpoint overrides defaults field by field
```

Cascade: endpoint values override defaults field by field; framework defaults fill the gaps. When neither block is declared, the policy is single-attempt (no retry).

Backoff strategies (`n` is the 1-indexed attempt; the sleep happens **after** attempt `n` failed and before attempt `n+1`):

| Strategy | Formula |
|---|---|
| `constant` | `initialDelay` |
| `linear` | `initialDelay * n` |
| `exponential` | `initialDelay * 2^(n-1)` |
| `exponential-jitter` (default) | `random(0, min(initialDelay * 2^(n-1), maxDelay))` (full jitter) |

All durations are capped at `maxDelay`. `respectRetryAfter` honors RFC 7231 `Retry-After` headers (seconds or HTTP-date) and caps them at `maxDelay` too.

`retryOn` accepts numeric status codes and the sentinels `network`, `timeout`, `dns`. Caller cancellation (`context.Canceled`) is **never** retried even when `timeout`/`network` would otherwise match.

**POST/PATCH gate.** Non-idempotent methods are restricted to `maxAttempts: 1` by default. Declaring an `idempotency:` block on the same endpoint unlocks higher attempt counts — the upstream sees the same idempotency key on every retry attempt and dedupes. Without `idempotency:`, a `retry: {maxAttempts: 2+}` on POST/PATCH aborts boot.

The body is replayed on each attempt from `obs.RequestBody` (the logging middleware buffers it at chain position 2), so request handlers see the same payload byte-for-byte every time.

### Cache

The chain inserts the cache middleware at position 6 — before retry, so a hit short-circuits without traversing the retry loop. The **backend** is the framework's top-level cache subsystem (see ["Cache subsystem"](#cache-subsystem) below for the `cache:` YAML block, the `cache.Cache` port, and the memory / redis / custom backends). The `httpClient.defaults.cache:` block keeps only the POLICY knobs — whether the layer runs, the default TTL, Cache-Control honoring, per-endpoint `cache: { ttl, varyOn }` and `cacheAcceptable`.

```yaml
httpClient:
  defaults:
    cache:
      enabled: true                   # default true
      defaultTTL: 5m                  # framework default 5m
      honorCacheControl: true         # default true

  services:
    keycloak:
      baseURL: https://kc.example.com
      endpoints:
        getUser:
          method: GET
          path: /users/{id}
          cache:
            ttl: 1m                   # endpoint-specific TTL
            varyOn: [header:Accept-Language, query:tenant]
          acceptableStatus: [404]
          cacheAcceptable: false      # default false; opt-in to cache 404 etc.
```

Cascade: an endpoint with a `cache:` block participates in caching, subject to `defaults.cache.enabled` AND a non-nil `Deps.Cache` at boot. Endpoint `ttl` overrides `defaults.defaultTTL`; the framework default fills in when neither sets a value. When the operator omits the top-level `cache:` block entirely, the httpclient cache layer is silently disabled (the middleware short-circuits as `"bypass"`).

**Cacheable methods.** Only GET and HEAD enter the cache. Any other method bypasses unconditionally (`obs.CacheStatus = "bypass"`). The validator rejects `cache:` on POST/PATCH/PUT/DELETE endpoints at boot.

**Storable responses.** 2xx responses are stored by default. Responses whose status appears in `acceptableStatus` (e.g. 404 on a presence check) are stored only when `cacheAcceptable: true` on the endpoint — opt-in by design.

**Key formula.** `service|endpoint|method|path|sortedQuery|h:hash(value)...|q:hash(value)...`. Query parameters are sorted alphabetically so `?a=1&b=2` and `?b=2&a=1` hash to the same key. `varyOn` accepts `header:Name` and `query:Name` entries; values are SHA-256 hashed so they never leak verbatim into the key. The `service` segment guarantees no cross-service collision when several services share a Redis DB; the backend's `keyPrefix` (declared on the top-level `cache.redis:` block) adds an extra namespace scope across deployments.

**Cache-Control honoring** (when `honorCacheControl: true`):
- `Cache-Control: max-age=N` overrides the configured TTL with N seconds
- `Cache-Control: max-age=0` short-circuits the store entirely (the new byte-cache layer treats `ttl == 0` as "no expiration", which would be the opposite of what the upstream asked for, so the middleware skips persistence)
- `Cache-Control: no-store` or `no-cache` prevents storing the response

**Per-call overrides.** `WithoutCache()` bypasses the cache for one call without touching configuration; `WithCacheKey(key)` overrides the computed key (rare — typically for tenant-aware key schemes).

**Wire shape.** The middleware persists an internal `cacheEntry` JSON envelope (`body`, `headers`, `status`, `contentType`, `contentLength`, `expiresAt`) through the byte-level `cache.Cache` port. Stored entries remain operator-debuggable via the backend's CLI (e.g. `redis-cli GET <key>` + `json.loads`).

**Observation.** `obs.CacheStatus` is `"hit"`, `"miss"`, or `"bypass"` in the single slog record per call. The cache backend (when Redis) additionally emits `slog.Warn "cache.redis.transport.error"` on transport failures so operators see the underlying problem regardless of the backend's `failMode`.

### Circuit breaker

The chain inserts the circuit breaker middleware INSIDE the retry middleware — so the retry loop observes the breaker on every attempt. The state machine is per `(service, endpoint)` pair; configuration is shared at `defaults.circuitBreaker` (the design does not document per-endpoint overrides).

```yaml
httpClient:
  defaults:
    circuitBreaker:
      enabled: true             # default true (when block present)
      failureThreshold: 5       # default 5 consecutive failures trip
      successThreshold: 2       # default 2 successes in half-open close
      openFor: 30s              # default 30s recovery window
```

**State machine.** `closed → open → half-open → closed`:

- **Closed**: every call passes through. A success resets the failure counter. `failureThreshold` consecutive failures transition to **open**.
- **Open**: every call is rejected with `*HttpError{Cause: ErrCircuitOpen}` without dialing. After `openFor` since the open transition, the next call is admitted as the half-open probe.
- **Half-open**: one probe at a time (subsequent concurrent half-open requests are rejected as open). Each success counts toward `successThreshold`; reaching it transitions to **closed**. Any failure transitions back to **open**.

**Failure attribution.** Transport errors and 5xx responses count as failures; 2xx responses count as successes. 4xx is treated as a success — a client-side rejection is not an upstream health signal.

**Chain ordering — explicit deviation from design Section 15.** The design lists `circuitBreaker (7)` outer of `retry (8)`. Open question Q7 of the design then explicitly decides that **each retry attempt counts as a breaker observation**. That decision is only achievable when the breaker sits INSIDE retry — otherwise the breaker sees a single retry-exhausted outcome per call. The implementation honors Q7: the layer slice ends with `retry → breaker → transport`. This file is the source of truth on chain order.

When the breaker rejects a call, `shouldRetry` returns false on `ErrCircuitOpen` so the retry loop terminates without burning attempts on an open breaker.

**Observation.** `obs.BreakerState` reflects the state in effect for the call (`"closed"`/`"open"`/`"half-open"`).

### Idempotency

The chain inserts the idempotency middleware at position 5 — after logging, before cache and retry. Header injection runs once per call; the header is set on the `*http.Request` and persists naturally across retry attempts because the retry middleware replays the same request with a fresh body reader. The shared header is what lets the upstream dedupe retried writes.

```yaml
endpoints:
  charge:
    method: POST
    path: /charges
    idempotency:
      header: X-Idempotency-Key       # required; outbound header name
      source: ctx                     # ctx (default) | explicit
    retry:
      maxAttempts: 3                  # POST allowed because idempotency is configured
```

**Source semantics.**

- `source: ctx` (default) — the framework generates a UUIDv7 per call (sortable by creation time for upstream dedup logs). The same key is reused across all retry attempts of the same call.
- `source: explicit` — the caller must supply the key via `WithIdempotencyKey(key)`. Missing key returns `*HttpError{Cause: …requires WithIdempotencyKey}` before dialing.

**POST/PATCH retry unlock.** The retry phase restricts non-idempotent methods to `maxAttempts: 1` by default. Declaring `idempotency:` on the endpoint signals that the framework can safely retry — the upstream sees the same key on each attempt and dedupes. The boot validator accepts `retry: {maxAttempts: 2+}` on POST/PATCH only when `idempotency:` is present; the runtime `resolveRetryPolicy` mirrors the same gate.

**AppContext propagation.** When the context implements `Set(key, value any)` (AppContext does), the middleware writes the chosen key under `httpclient.idempotency-key`. Consumers that need to thread the same value to a parallel pipeline (audit, custom middleware) can read it via `AppContext.Get(httpclient.AppContextIdempotencyKey)`.

**Observation.** `obs.IdempotencyKey` carries the key into the single slog record per call when active. When the endpoint has no `idempotency:` block, the field is empty and not emitted.

### Auth providers

The chain inserts the auth middleware at position 3 — after logging (so the slog observation sees the headers post-attach) and before signing (so a future HMAC signature signs the request including the auth header). Static providers always succeed; future dynamic providers (`forward-bearer`, `oauth2-*`) can fail at token acquisition.

```yaml
httpClient:
  services:
    keycloak:
      baseURL: https://kc.example.com
      auth: { provider: kc-api-key }
      endpoints:
        getUser:
          method: GET
          path: /users/{id}

  authProviders:
    kc-api-key:
      type: header-static
      attach:
        as: header
        name: X-API-Key
        value: ${KC_API_KEY}

    vendor-basic:
      type: basic
      username: ${VENDOR_USER}
      password: ${VENDOR_PASS}

    legacy-bearer:
      type: bearer-static
      token: ${LEGACY_TOKEN}

    mtls-only:
      type: none
```

**Provider types (current phase):**

| Type | Behavior | Required fields |
|---|---|---|
| `none` | No-op (mTLS handles identity, or anonymous call) | — |
| `header-static` | Raw value via `attach` | `attach.name`, `attach.value` |
| `bearer-static` | Static token; default `Authorization: Bearer {token}` | `token` |
| `basic` | RFC 7617 base64(user:pass); default `Authorization: Basic ...` | `username`, `password` |
| `forward-bearer` | Propagates the inbound JWT from `AppContext.BearerToken()`; default `Authorization: Bearer {token}`; **never cached** by design | — (depends on `AppContext` carrying a bearer) |
| `oauth2-client-credentials` | RFC 6749 client_credentials grant; POSTs form-urlencoded to the token endpoint; token cached per-provider; single-flight collapses concurrent acquisitions; optional revocation-on-401 | `tokenEndpoint`, `clientId`, `clientSecret`, `tokenCache.source` |
| `credentials-exchange` | Generic "POST credentials, get token" path. Configurable body codec (json/form-urlencoded), arbitrary `requestFields` (k/v), optional `requestHeaders` (e.g. Basic Auth), JSONPath token extraction. Reuses tokenCache + single-flight + revocation from `oauth2-*` | `tokenEndpoint`, `requestFields`, `responseTokenPath`, `tokenCache.source` |

`oauth2-password` and `oauth2-refresh` from the original design are subsumed by `credentials-exchange` (declare the grant body via `requestFields` with the canonical RFC names — see example below). No dedicated provider types are planned for them.

**credentials-exchange details.** Generic escape hatch for IdPs that diverge from RFC 6749 — custom field names, JSON body instead of form-urlencoded, non-standard response shape. The provider POSTs `requestFields` (in the configured codec) to `tokenEndpoint`, applies any `requestHeaders` (e.g. Basic Auth for confidential clients), and extracts the token via `responseTokenPath` (dot-notation JSONPath).

```yaml
# Custom Keycloak-style endpoint expecting JSON body with non-standard field names
authProviders:
  kc-custom:
    type: credentials-exchange
    tokenEndpoint: https://kc.example.com/realms/x/auth/exchange
    requestCodec: json                # json | form-urlencoded (default form-urlencoded)
    requestFields:
      user: ${KC_USER}                # arbitrary keys
      pass: ${KC_PASS}
    responseTokenPath: $.access_token # dot-notation JSONPath; required
    tokenCache:
      source: response-field
      jsonPath: $.expires_in
      unit: seconds
      skew: 30s
    revocationOnUnauthorized: true

# RFC OAuth2 Resource Owner Password Credentials grant via credentials-exchange
  kc-password-rfc:
    type: credentials-exchange
    tokenEndpoint: https://kc/realms/x/.../token
    requestCodec: form-urlencoded
    requestFields:
      grant_type: password
      client_id: ${KC_CLIENT_ID}
      username: ${KC_USER}
      password: ${KC_PASS}
      scope: api.read
    requestHeaders:                   # confidential client → Basic Auth header
      Authorization: "Basic ${KC_BASIC}"
    responseTokenPath: $.access_token
    tokenCache: { source: jwt-exp, skew: 30s }
```

Token cache, single-flight, and `revocationOnUnauthorized` work the same way as in `oauth2-client-credentials`. The provider implements `RevocableProvider`, so the auth middleware honors the 401-invalidate-retry loop when configured.

**Multi-tenant via `requestFieldsFromCtx`.** When the credentials change per request (per-tenant SaaS, on-behalf-of-user calls), declare `requestFieldsFromCtx` mapping body field name → `AppContext` key. Values are read at `Apply` time and the token cache becomes per-identity (SHA-256 hash of the resolved values keys the cache slot — the raw credentials never leak into the key).

```yaml
authProviders:
  kc-multitenant:
    type: credentials-exchange
    tokenEndpoint: https://kc.example.com/auth/exchange
    requestCodec: json
    requestFields:                    # optional: static fields
      grant_type: password
      client_id: ${KC_CLIENT_ID}
    requestFieldsFromCtx:             # body field → AppContext key
      username: idp.username
      password: idp.password
    responseTokenPath: $.access_token
    tokenCache:
      source: response-field
      jsonPath: $.expires_in
      unit: seconds
      skew: 30s
    revocationOnUnauthorized: true
```

Code at the handler / external service:

```go
ctx.Set("idp.username", runtimeUser)
ctx.Set("idp.password", runtimePass)
// httpclient.Call automatically resolves the token per tenant and caches it.
```

Static and ctx fields are merged into a single body; on a same-named field, ctx wins. A missing key in AppContext (or a context that isn't an `AppContext`) returns an `ErrTokenAcquire`-shaped error before dialing. Per-tenant single-flight collapses concurrent first-time-misses for the same tenant; different tenants run independently.

**Security tradeoff.** The per-tenant cache holds bearer tokens in memory keyed by identity hash. Same surface area as the single-tenant cache, just multiplied by tenant count. Pair with `revocationOnUnauthorized` so a tenant's revoked session does not linger.

**oauth2-client-credentials details.**

```yaml
authProviders:
  keycloak-svc:
    type: oauth2-client-credentials
    tokenEndpoint: https://kc.internal/realms/main/protocol/openid-connect/token
    clientId: ${KC_CLIENT_ID}
    clientSecret: ${KC_CLIENT_SECRET}
    scope: ["api.read", "api.write"]
    audience: pay.example.com           # optional
    tokenCache:
      source: jwt-exp                   # jwt-exp | response-field | ttl
      skew: 30s                         # default 30s buffer before expiry
      singleFlight: true                # default true
    revocationOnUnauthorized: true      # default false; opt-in
```

The token endpoint is called via `POST application/x-www-form-urlencoded` with the standard `grant_type=client_credentials` body. The response's `access_token` is cached per-provider; concurrent cache misses collapse to a single endpoint call via single-flight.

**Token TTL source.**

- `jwt-exp` — the framework decodes the JWT payload (no signature verification — the upstream IdP signed it) and reads the `exp` claim.
- `response-field` — extract the expiry from the token endpoint's JSON response via dot-notation JSONPath (e.g. `$.expires_in`) with `unit: seconds|millis|iso8601` (default seconds).
- `ttl` — explicit duration (e.g. `ttl: 10m`).

In all three cases the cached entry is treated as expired `skew` seconds before its actual expiry, so the next call refreshes preemptively rather than racing against the upstream clock.

**revocationOnUnauthorized.** When true, a 401 from the resource server triggers the auth middleware to call `provider.Invalidate()`, re-acquire a fresh token, and dispatch the original request one more time. The retry happens inside the auth middleware — independent of the retry middleware's policy — so a cached-but-stale credential recovers transparently. Default is false because the optimistic-then-refresh path adds one round-trip on every 401 from the upstream; only opt in when the IdP supports key rotation or revocation paths that can outpace the cache TTL.

**forward-bearer details.** Reads the verified raw JWT the framework's `AuthMiddleware` stored on the inbound request via `AppContext.SetBearerToken`. The token is never cached — it IS the inbound user's credential, and caching would require keying by a hash of the token, risking cross-user contamination. Without a bearer on the AppContext (public route, `auth.mode=disabled`, background job), the provider returns an `Apply` error that the middleware wraps in `ErrTokenAcquire`; `shouldRetry` returns false on that sentinel so the call fails fast instead of looping.

**attach block.**

```yaml
attach:
  as: header                    # header (default) | query | cookie
  name: X-API-Key               # header/query/cookie name
  format: "Bearer {token}"       # optional; {token} placeholder
  value: literal                # header-static only
```

Defaults apply when omitted: `as: header`, `name: Authorization` (for `bearer-static`/`basic`), `format: Bearer {token}` (for `bearer-static`). `basic` ignores `format` because the value already contains the scheme.

**Per-call override.** `WithAuthOverride(providerName)` substitutes the provider for one call. Unknown names produce `*HttpError{Cause: ErrTokenAcquire}` before dialing.

**Q5 short-circuit.** When a provider returns an error from `Apply`, the middleware wraps it in `*HttpError{Cause: ErrTokenAcquire}` and `shouldRetry` returns false on that sentinel — burning retry attempts on a stuck IdP would only add load. Static providers never trigger this path; dynamic providers (forward-bearer, oauth2) will use it.

**Observation.** `obs.AuthProvider` carries the provider name on every slog record when auth is active.

### TLS + connection pool

Each per-service `http.Transport` is built once at boot from the cascaded `tls:` and `pool:` blocks. Service entries override defaults field by field; framework defaults fill any remaining gap.

```yaml
httpClient:
  defaults:
    tls:
      minVersion: "1.2"             # default 1.2 (use 1.3 for modern services)
      cipherSuites: modern          # modern | intermediate | legacy | [explicit names]
      insecureSkipVerify: false     # default false (test/dev only)
    pool:
      maxIdleConnsPerHost: 100      # framework defaults
      maxConnsPerHost: 200
      idleConnTimeout: 90s
      disableKeepAlives: false

  services:
    payment:
      baseURL: https://pay.example.com
      tls:
        clientCertFile: /etc/secrets/pay-client.pem
        clientKeyFile: /etc/secrets/pay-client-key.pem
        caBundle: /etc/secrets/pay-ca.pem
      pool:
        maxConnsPerHost: 20          # gateway-imposed limit
      endpoints:
        charge:
          method: POST
          path: /charges
    legacy:
      baseURL: https://soap.legacy.example.com
      tls:
        minVersion: "1.0"            # downgrade for legacy SOAP backends
```

**Cipher suite presets.** Follow Mozilla's published recommendations:

| Preset | Recommended for |
|---|---|
| `modern` (default) | TLS 1.3-only ciphers (AES-GCM + ChaCha20-Poly1305) |
| `intermediate` | TLS 1.2+ ECDHE suites — broadest compatibility |
| `legacy` | TLS 1.0+ with CBC/RSA suites for ancient backends |

Operators may also list explicit cipher names (any value in the framework's `explicitCipherNames` map). Unknown names abort the boot.

**mTLS.** Set `clientCertFile` and `clientKeyFile` (both required) to a PEM-encoded cert/key pair on disk. The loader runs once at boot; vault-rotated certs use the per-call `WithClientCert(tls.Certificate)` option, which builds an ephemeral cloned transport for the single call without polluting the service's pool.

**Custom CA bundle.** Set `caBundle` to a PEM file path; the bundle **replaces** (does not merge with) the system roots. Operators who need both should concatenate them in the file.

**Pool tuning.** Service overrides flow into `http.Transport` directly. Use `maxConnsPerHost` to match upstream-imposed limits; `disableKeepAlives: true` is the escape hatch for backends that misbehave under connection reuse.

### Streaming — download, upload, multipart, SSE

The framework supports four streaming surfaces that bypass the default "buffer the whole body" path:

**Download streaming (`responseStream: true`).** Endpoint marked in YAML; Resp must be `httpclient.StreamResponse`. Framework hands the open response body to the caller — the caller MUST close it.

```yaml
endpoints:
  downloadReceipt:
    method: GET
    path: /receipts/{id}/pdf
    responseStream: true
```

```go
type Req struct { ID string `http:"path,id"` }
resp, err := httpclient.Call[Req, httpclient.StreamResponse](ctx, c, "pay", "downloadReceipt", Req{ID: "ch_42"})
if err != nil { return err }
defer resp.Body.Close()
_, err = io.Copy(out, resp.Body)
```

`StreamResponse.ContentLength` is `-1` when the upstream uses chunked transfer.

**Upload streaming (`http:"body,stream"` tag).** Field type is `io.Reader`. The framework pipes the reader to the wire without buffering. Content-Type comes from a `http:"header,Content-Type"` field or `WithExtraHeader`. Used for arbitrary binary uploads (file→storage, log shipping, large CSV imports).

```go
type UploadAvatarRequest struct {
    UserID string    `http:"path,id"`
    Body   io.Reader `http:"body,stream"`
    Mime   string    `http:"header,Content-Type"`
}
```

**Multipart upload (`http:"body,multipart"` tag, `httpclient.Multipart` value).** Combines text fields with file streams. The framework writes the multipart body through an `io.Pipe`, so file content is never fully buffered:

```go
type UploadDocRequest struct {
    UserID string             `http:"path,id"`
    Body   httpclient.Multipart `http:"body,multipart"`
}
req := UploadDocRequest{
    UserID: "u42",
    Body: httpclient.Multipart{
        Fields: []httpclient.MultipartField{{Name: "category", Value: "id-proof"}},
        Files: []httpclient.MultipartFile{{
            Name: "file", Filename: "passport.pdf", MimeType: "application/pdf",
            Content: openedFile,
        }},
    },
}
```

**SSE (`responseSSE: true`).** Endpoint marked in YAML; Resp must be `httpclient.SSEResponse`. Framework spawns a goroutine that parses the upstream's `text/event-stream` per the WHATWG EventSource spec and delivers events on a buffered channel. The caller MUST call `Close()` to stop the goroutine and release the connection. `Accept: text/event-stream` is auto-injected when the request struct does not set it.

```go
resp, err := httpclient.Call[Req, httpclient.SSEResponse](ctx, c, "stream", "subscribe", Req{Topic: "orders"})
if err != nil { return err }
defer resp.Close()
for ev := range resp.Events {
    handle(ev) // ev.ID, ev.Event, ev.Data, ev.Retry
}
```

Reconnection is the caller's responsibility — the framework does not re-dial when the stream ends. The upstream's `retry:` hint is surfaced on `SSEvent.Retry` so the caller can honor it.

**Middleware interactions (the constraints):**

| Constraint | Reason |
|---|---|
| `responseStream` + `cache` → boot reject | caching a stream is undefined |
| `responseSSE` + `cache` → boot reject | same |
| `responseStream` + `responseSSE` → boot reject | mutually exclusive endpoint shapes |
| Streaming upload + retry → forced `maxAttempts: 1` at runtime | one-shot `io.Reader`; second attempt would send empty body |
| Streaming upload + signing → call-time reject | signing needs SHA256(body); cannot hash a stream without buffering |
| Streaming endpoints + logging | request/response body bytes NOT captured in slog (status, headers, byte counts still emitted) |

**What is not in scope for this phase:** automatic SSE reconnection (the consumer's responsibility), seekable-body retry for uploads (could be added later via a body-factory callback), per-call streaming overrides via `CallConfig` (streaming is an endpoint-level contract).

### HMAC request signing

Per-service `signing:` block declares an HMAC request-signing policy compatible with payment gateways, Twilio-style webhooks, and AWS SigV4-lite upstreams. The framework injects timestamp + content-sha256 + signature headers on every outbound request, with **re-signing per retry attempt** so each dial carries a fresh timestamp.

```yaml
httpClient:
  services:
    payment:
      baseURL: https://pay.example.com
      signing:
        type: hmac-sha256                       # required (only supported in this phase)
        keyId: ${PAY_KEY_ID}                    # optional
        keyIdHeader: X-Key-Id                   # required when keyId is set
        secret: ${PAY_SIGNER_SECRET}            # required
        signedHeaders:                          # required, non-empty
          - host
          - x-date
          - x-content-sha256
          - x-idempotency-key
          - content-type
        timestampHeader: X-Date                 # required
        timestampFormat: rfc1123                # optional; rfc1123 | iso8601 | unix-seconds
        contentSHA256Header: X-Content-SHA256   # optional; default X-Content-SHA256; "-" to disable
        signatureHeader: X-Signature            # required
        signaturePrefix: ""                     # optional; e.g. "HMAC-SHA256 "
```

**Canonical string (AWS SigV4-lite):**

```
METHOD\nPATH\nQUERY_CANONICAL\nHEADERS_CANONICAL\nSIGNED_HEADERS_LIST\nSHA256_HEX(BODY)
```

- `METHOD` — uppercased HTTP verb
- `PATH` — URL path component (no query); empty path becomes `/`
- `QUERY_CANONICAL` — sorted alphabetically by key; each `key=value` URL-encoded; repeated keys sorted by value
- `HEADERS_CANONICAL` — for each header in `signedHeaders` (lowercased + sorted): `name:trimmed_value\n`; multi-value headers joined by `,`
- `SIGNED_HEADERS_LIST` — `signedHeaders` joined by `;`
- `SHA256_HEX(BODY)` — lowercase hex of SHA256(body); SHA256 of empty input when body is absent (`e3b0c4...`)

**Signature:** `hex(HMAC_SHA256(secret, canonical))`. Lowercase hex output. Written to `signatureHeader` with `signaturePrefix` prepended.

**Chain position 8.** Signing sits **inner of retry and breaker, just before transport**. Rationale:

- Inner of retry → each attempt re-signs with a fresh timestamp (idempotent on the upstream side when the idempotency key is in `signedHeaders`).
- Inner of breaker → a rejected attempt does not waste signing work.
- Outer of transport → the request that actually dials carries the signature headers.
- Cache hits (short-circuit at position 5) skip signing entirely — correct, because no dial happens.

**What can be in `signedHeaders`.** Only headers that are present when signing runs (position 8). That includes: `host`, `authorization` (set by auth at position 3), `x-idempotency-key` (set by idempotency at position 4), `content-type` / any header from the defaults/service/endpoint cascade, `WithExtraHeader` / `CallConfig` overrides, plus the framework-injected `timestampHeader` and `contentSHA256Header`. Headers added by anything downstream of position 8 (none today) cannot be referenced.

**Auto-redaction.** The framework automatically adds `signatureHeader` and `keyIdHeader` (when set) to the per-service redaction policy — the values are redacted in slog observations but transmitted untouched on the wire. Cascade is union-only; operators cannot un-redact.

**Per-call override:** none. Signing is part of the API contract and cannot be flipped per call. Change the YAML and reboot.

### Composition pattern (consumer side)

Application handlers never depend on `*httpclient.HttpClient` directly. The DDD layering is:

- `bootstrap.Deps.HttpClient` exposes the registry singleton
- Each feature constructs an `infra/external/<svc>.go` service struct, passing `d.HttpClient` to its constructor (e.g. `appexternal.NewIdPService(d.HttpClient)`)
- The infra service struct holds the typed Call surface (request/response DTOs with `http:"..."` tags, mapping vendor→domain) and exposes business-vocabulary methods
- The handler in `application/handlers/` depends on the infra service struct and calls business methods — it never imports `httpclient` and never knows the call crossed the network

The same pattern as `infra/user_service.go` in `omnicore-example-users`: a Go struct in `infra/` that concentrates the I/O concern, consumed by handlers as a typed dependency. Swapping HTTP for gRPC or a fake never touches `application/`.

### Service discovery — `BaseURLResolver`

`httpClient.services.<name>.baseURL` in YAML covers the common case — one static URL per service per environment. For dynamic routing (Consul, Kubernetes Service DNS, per-tenant URL maps, region-aware failover) the framework exposes a plug-in point:

```go
type BaseURLResolver interface {
    Resolve(ctx context.Context, service string) (string, error)
}

// Register at construction
client, err := httpclient.New(cfg, httpclient.WithResolver(myResolver))
```

When a resolver is registered, **every call** consults it before dialing — implementations are responsible for their own cache / refresh policy; the framework does not memoize.

**Cascade with the YAML:**

| Source | Effective baseURL |
|---|---|
| Per-call `WithConfig(CallConfig{BaseURL: "..."})` non-empty | Wins over everything below; resolver is not consulted |
| Resolver not registered (nil) | YAML baseURL verbatim (zero-overhead hot path) |
| Resolver returns `(url, nil)` with non-empty url | `url` overrides the YAML for this call |
| Resolver returns `("", nil)` | Fall back to the YAML baseURL — resolver can opt out per-service |
| Resolver returns `(_, err)` | Call aborts with `*HttpError{Cause: err}` before dialing |
| Resolver and YAML both empty (no per-call override either) | Call aborts with `*HttpError` describing the missing baseURL |

**Schema implication.** `services.<name>.baseURL` is **optional** in YAML — services that delegate routing to a resolver leave it empty. A non-empty value is still validated as an absolute URL at boot so typos surface immediately. The "both empty" error happens at call time, with the service name in the message.

**Reference impl** — `StaticBaseURLResolver` is a `map[string]string` that returns the entry verbatim and `""` for unknown services (fall-through). Useful in tests and for static per-environment maps the YAML cannot express directly:

```go
resolver := httpclient.StaticBaseURLResolver{
    "keycloak": "https://kc.east.example.com",
    "payment":  "https://pay.east.example.com",
}
client, _ := httpclient.New(cfg, httpclient.WithResolver(resolver))
```

**Per-request routing via `AppContext`.** The `ctx` passed to `Resolve` is the same one the framework will use for the dial — `AppContext` satisfies it, so a resolver can read `Identity()`, `Get("key")`, or any custom value the caller stored on the context. Per-tenant routing is the canonical example:

```go
type TenantResolver struct{ Mapping map[string]string }

func (r TenantResolver) Resolve(ctx context.Context, service string) (string, error) {
    if appCtx, ok := ctx.(*configuration.AppContext); ok {
        if tenantID, _ := appCtx.Get("tenant.id").(string); tenantID != "" {
            if url := r.Mapping[service+"|"+tenantID]; url != "" {
                return url, nil
            }
        }
    }
    return "", nil // fall back to YAML
}
```

The handler stores the tenant id on `AppContext` before calling out (typically via service middleware after `AppContextMiddleware`); every outbound call automatically reaches the right upstream without per-call wiring. The same pattern works for feature-flag-driven routing, A/B testing, region failover, on-behalf-of-user calls.

**Versioning / path prefix via the resolver.** The resolver returns a baseURL that is concatenated with the endpoint's YAML `path` verbatim, so a path prefix lives naturally in the baseURL — useful for API versioning, per-tenant URL prefixes, gradual cutover between v1 and v2. The endpoint `path: /users` in YAML stays unchanged; only the resolver decides what comes before it:

```go
// Endpoint declared as: path: /users
// Per-tenant version selection via the resolver.
func (r VersionResolver) Resolve(ctx context.Context, service string) (string, error) {
    if appCtx, ok := ctx.(*configuration.AppContext); ok {
        if tier, _ := appCtx.Get("tenant.tier").(string); tier == "early-adopter" {
            return "https://api.example.com/v2", nil // → https://api.example.com/v2/users
        }
    }
    return "https://api.example.com/v1", nil         // → https://api.example.com/v1/users
}
```

**Path placeholders are runtime by design.** The endpoint's `path` template (`/users/{id}`) lets the request DTO supply placeholder values via `http:"path,name"` tags — that is the canonical "same endpoint, parameter varies per call" surface and never needs the resolver. Path *templates that vary in shape* across calls are not supported in runtime — declare each as its own endpoint in YAML and let the caller choose by endpoint name, so log/audit/observability stay tied to one HTTP semantic per endpoint name.

**Per-call escape hatch — `WithConfig(CallConfig{BaseURL: "..."})`.** Some shapes of dynamic routing do not fit the resolver pattern cleanly because the URL comes from the *current request* rather than from ambient context. The canonical cases:

- **Webhook callbacks** — the target URL arrives inside the request payload (`{"callback_url": "..."}`).
- **Payload-driven sharded routing** — the shard identifier comes from the request body, not from `AppContext`.
- **OAuth federation** — the IdP URL is discovered per request rather than known at boot.
- **Migration scripts / CLI tools** — a one-off binary that needs to target a specific endpoint without wiring a resolver.
- **Integration tests** — pointing at an `httptest.Server` without constructing a resolver around it.

For these, the per-call `CallConfig.BaseURL` takes precedence over both the resolver and the YAML — the URL is used as-is for this call only; the next call goes back through the normal cascade. Webhook callbacks typically need to vary method and path too — set `CallConfig.Method` and `CallConfig.Path` alongside; per-customer credentials come through `CallConfig.InlineAuth`. For cross-cutting ambient routing (per-tenant, per-region, feature-flag-based) the resolver is still the right tool — it centralizes the decision and runs on every call without per-call wiring.

**Also out of scope.** A refresh interface (the resolver owns its lifecycle) and framework-level memoization of resolver returns (the resolver decides what is cheap to recompute).

### Testing surface

`httpclient.NewFake()` constructs an in-memory test harness wrapping a `*HttpClient`. Drop the fake's `Client()` into any constructor parameter typed `*HttpClient` — production code does not change. The fake short-circuits the middleware chain (no retry, cache, breaker, auth, or transport runs), but the real `binding/` layer still validates tags and serializes / decodes bodies, so handler bugs around `http:"..."` tag misuse surface in fake tests too.

```go
fake := httpclient.NewFake()
fake.WhenCalled("kc", "fetchUser").
    MatchPath("id", "abc").
    Return(KeycloakUser{ID: "abc", Email: "x@y"})

svc := appexternal.NewKeycloakService(fake.Client())
user, err := svc.FetchUser(ctx, "abc")

require.NoError(t, err)
require.Equal(t, 1, len(fake.Calls("kc", "fetchUser")))
require.NoError(t, fake.AssertExpectations())
```

**Stub builder** (`*FakeStub`):

| Method | Effect |
|---|---|
| `Return(value any)` | 200 OK with JSON-marshaled value (default Content-Type `application/json`) |
| `ReturnBytes(body, contentType)` | 200 OK with raw body and explicit Content-Type |
| `ReturnError(status, body)` | Non-2xx response carrying body; `Call` returns `*HttpError` |
| `ReturnTransportError(cause)` | Simulated dial/transport failure; `*HttpError{Status: 0, Cause: cause}` |
| `Status(code)` | Override response status |
| `WithHeader(key, value)` | Add response header |
| `Times(n)` | Match exactly `n` calls (default 1). For "must not be called", omit the stub and assert `len(fake.Calls(...)) == 0` |
| `Always()` | Match unlimited calls; skipped by `AssertExpectations` |
| `Match(pred)` | Custom predicate over the recorded `FakeCall` |
| `MatchPath(name, value)` | Predicate over a path placeholder |
| `MatchQuery(key, value)` | Predicate over a query parameter |
| `MatchHeader(key, value)` | Predicate over an outbound request header |

**Harness handle** (`*Fake`):

| Method | Effect |
|---|---|
| `Client() *HttpClient` | Returns the in-memory client to inject into consumer code |
| `Register(service, endpoint, binding.EndpointMeta)` | Explicit endpoint metadata (method, path with `{placeholders}`, codecs). Optional — `WhenCalled` auto-registers `GET /{endpoint}` json/json on first use |
| `WhenCalled(service, endpoint) *FakeStub` | Register a stub; auto-registers default metadata if absent |
| `Calls(service, endpoint) []FakeCall` | Recorded calls in invocation order |
| `Reset()` | Clear stubs and recorded calls (useful between subtests) |
| `AssertExpectations() error` | Non-nil when any non-`Always()` stub did not match its `Times(n)` value |

**`FakeCall` shape** — service / endpoint / method / URL, plus parsed `Path map[string]string`, `Query url.Values`, `Headers http.Header`, raw `Body []byte`. The same data tests need to assert "the consumer sent the right thing".

**Sentinel.** Unmatched calls return `*HttpError{Status: 0, Cause: httpclient.ErrFakeUnstubbed}` — branch with `errors.Is(err, httpclient.ErrFakeUnstubbed)`.

**Trade-off (deliberate).** The fake tests *intent* — request shape, code path, error handling — not *runtime middleware behavior* (retry / cache / breaker / auth acquisition). Verifying middleware semantics belongs to integration tests against `httptest.Server` or to the `omnicore-example-users/qa/` suite. The fake stays small, deterministic, and free of timers / clocks.

## bootstrap package

`omnicore/bootstrap` orchestrates the whole service boot from `microservice.<profile>.yaml` + a `Wire` callback returning `Wiring`.

### Environment variables

The framework reads **exactly four** process environment variables at boot. Everything else that may appear as `${VAR:default}` inside a YAML value is consumer-defined (see [microservice.&lt;profile&gt;.yaml — declarative config](#microserviceprofileyaml--declarative-config) for the interpolation syntax). The four framework-reserved vars:

| Variable | Required | Default | What it controls | Read by |
|---|---|---|---|---|
| `APP_PROFILE` | yes | — (boot aborts if empty) | Selects `./microservice.${APP_PROFILE}.yaml`. `dev` and `prd` are canonical; any other non-empty string (`prd-pem`, `prd-external`, `qa-canary`, …) is accepted. `dev` is the only profile under which `auth.mode=disabled` is allowed AND the only profile where `migrations.autoRun` / `mongo.rebuild.autoRun` default to true. | `bootstrap.LoadConfig` |
| `OMNICORE_CONFIG_PATH` | no | `./microservice.${APP_PROFILE}.yaml` | Overrides the YAML path (tests, sidecar layouts, custom directories). Still requires `APP_PROFILE` set so profile-aware guards continue to fire. | `bootstrap.LoadConfig` |
| `OMNICORE_MONGO_FORCE_REBUILD` | no | unset (= strict) | Literal value `"true"` → `fwinfra.ApplyMongoSpecs` drops divergent indexes and recreates them from the declared spec. Operator escape for index conflicts; **does NOT authorize dropping collections** (collation / capped / time-series divergence is always strict) and **does NOT trigger the document rebuild path** (that is controlled by `mongo.rebuild.autoRun`). Comparison is exact: `"True"` / `"1"` / `"yes"` leave the strict default in place. | `fwinfra.ApplyMongoSpecs` (called by `bootstrap.Run` after `collectViews`) |
| `OMNICORE_CODE_VERSION` | no | unset (= empty) | Optional build-time identifier (typically a git SHA injected at compile) stamped on the `code_version` column of `omnicore_mongo_views` when the registry row is written (init / artifact-only refresh / rebuild end). Useful for "which deploy ran this?" forensics. Empty when unset; never a boot blocker. | `infra.InitViewRegistry` / `infra.EndRebuild` (called by `SyncEngine.InitRegistryOnly` / `RefreshRegistryArtifactOnly` / `ExecuteRebuild`) |

**Why env vars and not YAML fields.** YAML is checked into git → carrying a one-shot override (e.g., `forceRebuild: true`) there would deploy the override into the next release by accident. Env vars are scoped to the process lifetime; a reboot without them returns to the canonical posture. Same rationale behind the loader rejecting `auth.mode=disabled` on any non-`dev` profile — destructive / loose decisions stay out of versioned artifacts. `OMNICORE_CONFIG_PATH` is env-only for the bootstrapping reason: it tells the loader where the YAML lives, so it cannot itself live in the YAML.

**How operators set them.** Pick by deployment shape — shell for one-off runs, `docker-compose.yml` for the local bench, Kubernetes Deployment env for managed clusters:

```bash
# Local shell — one run:
APP_PROFILE=dev go run ./bootstrap
OMNICORE_MONGO_FORCE_REBUILD=true APP_PROFILE=prd ./bootstrap

# docker-compose.yml — service environment block:
services:
  api:
    environment:
      - APP_PROFILE=prd
      - OMNICORE_MONGO_FORCE_REBUILD=true

# Kubernetes Deployment — spec.template.spec.containers[].env:
env:
  - name: APP_PROFILE
    value: prd
  - name: OMNICORE_MONGO_FORCE_REBUILD
    value: "true"
```

### Main functions

| Function | Use |
|---|---|
| `bootstrap.Run(wire) error` | Loads yaml + builds singletons + calls wire + runs until SIGINT/SIGTERM |
| `bootstrap.Build() (Deps, *Config, error)` | Builds singletons without serving (tests or custom lifecycle) |
| `bootstrap.Serve(ctx, deps, wiring) error` | Runs the server with deps already built (does not import translations nor start SyncEngine — whoever uses the manual path takes care of that) |

### Types

`bootstrap.Deps` — built singletons: `Config`, `Logger`, `Postgres` (already configured with audit destinations + claims via `WithAudit`), `Mongo`, `Translator`, `Pipeline`, `ViewReader` (`queries.ViewReader`), `QueryHandler`, `HttpClient` (`*httpclient.HttpClient`; `nil` when `microservice.<profile>.yaml` carries no `httpClient:` block), `OpenAPIRegistry` (`*openapi.Registry`; `nil` when `Wiring.OpenAPI` is unset — `openapi.Mount` / `openapi.MountRaw` treat a nil registry as a Fiber-only passthrough). Audit is no longer an explicit dependency — every `Postgres.Insert/Update/Archive/Unarchive/Delete` emits the configured destinations automatically.

`bootstrap.Wiring` — service declarations: `Translations []translation.Module`, `Features []bootstrap.Feature`, `BeforeServe func(*fiber.App, Deps) error` (optional), `OnShutdown func(context.Context) error` (optional), `OpenAPI *openapi.Config` (optional — opt-in to `/openapi.json` + `/docs`; nil disables OpenAPI entirely and `Deps.OpenAPIRegistry` stays nil).

`bootstrap.Feature` — interface every service capability implements: `Mount(app *fiber.App, deps Deps)`. Each feature knows how to register its own routes.

`bootstrap.ReadableFeature` — opt-in for the read side: `Feature + Views() []*infra.ViewDefinition`. Bootstrap collects views from every `ReadableFeature` in `Wiring.Features` and passes them to the SyncEngine. A slice (not single) covers the "one feature, several projections" case (e.g.: `users` + `users_summary`).

### Default behavior of `Run`

- JSON logger on stdout at `LevelInfo` (`slog.SetDefault`)
- `signal.NotifyContext(SIGINT, SIGTERM)`
- Connects Postgres + Mongo (defer Close in reverse order)
- `validateWiring` — rejects boot if `Features == nil && BeforeServe == nil` (nothing to serve)
- Applies migrations (`ValidateDownExists` + `Up`) if `cfg.Migrations.AutoRun=true` (default) — before SyncEngine. Failure here aborts boot
- `collectViews(wiring.Features)` — aggregates Views from `ReadableFeature`s + rejects view name collisions (two aggregates declaring the same Mongo collection)
- "feature registered" log per feature, with flag `readable: true|false`
- Fiber `AppName = cfg.Service`, `DisableStartupMessage: true`, `ErrorHandler: fwweb.ErrorHandler(deps.Pipeline)` — every error escaping a route, middleware, or panic recovery is emitted as the canonical Response envelope. Specialized `*fiber.Error` codes: 404 → `RouteNotFoundNotification`, 405 → `MethodNotAllowedNotification`, 413 → `PayloadTooLargeNotification`. Any other code (and any non-`NotificationCarrier` panic/error) becomes 500 with `InternalServerErrorNotification`. Panic value + stack trace stay only on the server log (slog `LevelError` via `fwweb.Recover()`) and are never leaked to the wire.
- Fiber middlewares registered in order: `fwweb.Recover()`, `logger.New()` (Fiber's request logger), `fwweb.AppContextMiddleware()`, and `fwweb.AuthMiddleware` when `auth.mode: jwt` (skipped entirely under `disabled`)
- **`GET /health`** registered automatically by the framework (response `{"status":"ok"}`) — k8s/docker liveness probe without the service programming anything. Services that need custom health (DB ping etc) expose another route (`/healthz`, `/ready`) via feature or `BeforeServe`
- `f.Mount(app, deps)` for each feature in declaration order
- If `Wiring.OpenAPI != nil`: `Deps.OpenAPIRegistry` is set to a fresh `*openapi.Registry` BEFORE the auth middleware + `/health` registration, so both reach the spec. After features mount, the framework calls `openapi.Register(app, *Wiring.OpenAPI, registry, openapi.WithAuth(...))` so `GET /openapi.json` + `GET /docs` come online. AuthMiddleware's `publicRoutes` are automatically extended with `GET /openapi.json` + `GET /docs` when `auth.mode=jwt` so the documentation surface never sits behind the bearer wall
- SyncEngine starts only if any views were collected (write-only services or read-only services without CDC do not connect Kafka)
- HTTP drain in 10s on shutdown; then `Wiring.OnShutdown` if set

### Canonical main.go

`main()` calls `bootstrap.Run(Wire)`, where `Wire` is the service's `func(bootstrap.Deps) bootstrap.Wiring` declaring Translations + Features. Where `main` and `Wire` live (root, `cmd/<binary>/`, `bootstrap/`, etc.) is the service's decision — the framework just wires up in `bootstrap.Run`.

```go
package main

func main() {
    if err := bootstrap.Run(Wire); err != nil {
        log.Fatal(err)
    }
}
```

If `Wire` lives in another package, just import and reference (`bootstrap.Run(app.Wire)`, `bootstrap.Run(boot.Wire)`, etc.). See the checklist below.

## Bootstrap checklist for a new microservice

When creating `svc-foo` that imports OmniCore:

1. `go mod init github.com/ClaudioSchirmer/svc-foo`
2. `go get github.com/ClaudioSchirmer/omnicore`
3. Mandatory DDD layers: `domain/`, `application/{commands,handlers,translations}/`, `infra/`, `web/`. The composition layer (Features + Wire + `main`) and migration location are at the service's discretion — common examples below. The framework does not impose a filesystem; it only wires up via `bootstrap.Run(wireFn)` and reads `migrations.dir` from the YAML
4. **`microservice.dev.yaml` + `microservice.prd.yaml`** at the module root (one file per profile; canonical pair). Selected at boot via `APP_PROFILE` env (required, non-empty; extra variants like `microservice.prd-pem.yaml` accepted for QA/ops). Path override via `OMNICORE_CONFIG_PATH`. See "microservice.&lt;profile&gt;.yaml — declarative config"
5. **SQL Migrations** — starts at `0002_*.{up,down}.sql` with domain tables + child tables (all with `deleted_at`). Outbox is version 1, injected by the framework via embed; **do not write**. Each `.up.sql` requires a `.down.sql` counterpart (validated at boot). Path declared in `migrations.dir` of the YAML — default `./migrations` (idiomatic Go). Any path relative to CWD works; recommended placement is at the module root because migrations are part of the service's contract, versioned with `domain/*.go`
6. **Define entities** in `domain/` — embed `BaseEntity` (flat) or `AggregateRoot` (with children); implement `Entity` (+ `AggregateRootProvider` if aggregate)
7. **Repository** in `infra/` — embed `fwinfra.BaseRepository[T]` (inject `NewEntity func() T` in the constructor — mandatory, feeds `Repo.New()`) + delegate `FindByID` to `fwinfra.AggregateLoader[T]` (the same factory is typically shared)
8. **Commands** in `application/commands/` — `pipeline.CommandBase` (Insert) or `pipeline.CommandBaseWithID` (Update/Archive/Unarchive/Delete). Implements `ToEntity() T` or `ApplyTo(T)`
9. **Manual handlers** in `application/handlers/` only if there is cross-service / domain services logic (Auto Command Handlers cover trivial CRUD)
10. **Translations** in `application/translations/` for service-specific notifications
11. **Views** in `infra/views.go` via `fwinfra.View(...).Root(...).EmbedMany(...)`
12. **`web/`** exposes `MountXxx(app, repo, view, deps)` per aggregate — functions that register routes. Endpoints with body use `fwweb.HandleCommandWithBody{,ID}(pipe, requests.XxxRequest{}, handler, status)` consuming Request DTOs from `web/requests/` (with JSON tags + `ToCommand()`); endpoints without body (Archive/Unarchive/Delete) use `fwweb.HandleCommandWithID(pipe, handler, status)`. `/health` is **not** registered by the service (comes from the framework). Commands in `application/commands/` do not carry JSON tags — wire format lives exclusively in `web/requests/`. Request DTOs and Commands have identical shape per field (1:1 assignment, no hidden normalization)
13. **Feature struct per aggregate** implements `bootstrap.Feature` (write-only) or `bootstrap.ReadableFeature` (with read side). The `Mount` method delegates to `web.MountXxx` — `web/` is the owner of the routes; the feature only composes. Where the struct lives (`app/`, `bootstrap/`, `features/`, root) is the service's decision
14. **`Wire(d Deps) bootstrap.Wiring`** aggregates Translations + Features. Lives next to the Feature or in a separate file — the package name and location don't matter, only the signature. **Optional opt-in:** set `Wiring.OpenAPI = &openapi.Config{Title, Version, Description}` to publish `GET /openapi.json` + `GET /docs` (Swagger UI). When set, `Deps.OpenAPIRegistry` becomes non-nil and the `MountXxx` helpers register each route through `openapi.Mount` / `openapi.MountRaw` (see the "OpenAPI / Swagger UI" section)
15. **`func main()`** ≤ 10 lines — only calls `bootstrap.Run(wireFn)`. Framework registers middlewares + `/health` automatically; HTTP semantic comes from each notification's own `Semantic()`, with no global setup

```go
// service main — any package main, in any directory
func main() {
    if err := bootstrap.Run(Wire); err != nil {
        log.Fatal(err)
    }
}

// Wire — function, in any package (the caller decides how to reference)
func Wire(d bootstrap.Deps) bootstrap.Wiring {
    return bootstrap.Wiring{
        Translations: []translation.Module{apptrans.PTBR(), apptrans.ENG(), apptrans.ESP(), apptrans.FRA(), apptrans.DEU(), apptrans.ITA(), apptrans.NLD()},
        Features: []bootstrap.Feature{
            NewUsersFeature(d),
        },
    }
}

// Feature struct per aggregate — implements bootstrap.ReadableFeature
type UsersFeature struct {
    repo *appinfra.UserRepository
    view *fwinfra.ViewDefinition
}

func NewUsersFeature(d bootstrap.Deps) *UsersFeature {
    return &UsersFeature{
        repo: appinfra.NewUserRepository(d.Postgres),
        view: appinfra.UserView(), // called ONCE
    }
}

func (f *UsersFeature) Views() []*fwinfra.ViewDefinition { return []*fwinfra.ViewDefinition{f.view} }
func (f *UsersFeature) Mount(app *fiber.App, d bootstrap.Deps) {
    appweb.MountUsers(app, f.repo, f.view, d) // delegates — web remains the owner of the routes
}
```

**Common layouts** (all supported, none required):

- **Consolidated:** `bootstrap/` (`package main`) with `main.go` + `wire.go` + `<aggregate>_feature.go` side by side. Run: `go run ./bootstrap`. Good for a single-binary service that wants all composition in one place — it is the layout used by `omnicore-example-users` (canonical reference). Mirrors the name of the framework's `omnicore/bootstrap` package
- **Go `cmd/` convention:** `cmd/<binary>/main.go` + separate package (`app/`, `boot/`, etc.) for Wire and Features. Good for services with multiple binaries (CLI + server, worker + server) where `cmd/` gains real utility
- **Flat:** `main.go` at the root + Wire/Features in a sibling package. Good for small services

**Non-Go artifacts.** `infra/` is a DDD layer for Go code that implements domain ports (Postgres adapters, view definitions, repositories). Non-Go artifacts do not belong there. The convention used by `omnicore-example-users` separates them by ownership:

- `migrations/` at the module root — SQL DDL is the service's schema contract, versioned alongside `domain/*.go`. Idiomatic Go (also the framework's default `migrations.dir`).
- `devops/` at the module root — scaffolding the service does not read at runtime: `docker-compose.yml` (local bench), CDC connector configs + registration scripts (`devops/debezium/` — framework outbox→Kafka pipeline boilerplate, parameterized per service), IdP fixtures for the auth QA suite (`devops/keycloak/`). In production, `devops/` is replaced by whatever real infrastructure the operator provisions; the binary doesn't read it.

This split is the canonical example's choice. Services that prefer a different layout (everything under `ops/`, or `migrations/` co-located with infra Go code as a sibling package) work too — the framework only reads `migrations.dir` from the YAML and never touches the other folders.

Anyone needing exotic boot (custom logger, different lifecycle, specific start order) uses `bootstrap.Build()` + `bootstrap.Serve(ctx, deps, wiring)` separately.

## Glossary

- **AfterBeginHook[T]** / **BeforeCommitHook[T]** — function types declaring the persistence lifecycle hook signatures. `T` is the consumer's domain entity (`*User`, `*Order`); the persister fires the resolved closures at positions A and D of the TX.
- **AfterBeginHookProvider[T]** / **BeforeCommitHookProvider[T]** — interfaces in `application/persistence/` detected by Auto Command Handlers via type assertion. A Cmd that declares `AfterBegin(...)` / `BeforeCommit(...)` satisfies the matching provider automatically; the handler forwards the method value to the Writer call as a `WriteOption[T]`.
- **Aggregate root** — entity that owns a collection of `AggregateValueObject` items with state transitions
- **AggregateRootProvider** — opt-in interface (`GetAggregateRoot` + `AggregateChildren`) that activates aggregate-aware persistence **and validation** (auto-iteration of children in `runAggregateValidations`). `AggregateChildren()` declares the aggregate boundary; primitives `AddAggregateChild`/`ChangeAggregateChild`/`RemoveAggregateChild`/`ReplaceAggregateChildrenOf` reject VOs of undeclared types.
- **AggregateChildren** — method of the `AggregateRootProvider` interface. Returns sample instances of the types that belong to the aggregate. The framework reads `classNameOf` via reflect; samples are never instantiated for real value. Equivalent to the "aggregate boundary" — domain definition, separated from table/FK (infra)
- **Audit** — structured `AuditEvent` (flat top-level slog attrs) emitted after every successful write, plus emission of accumulated domain events. Best-effort, non-blocking. Body discriminated by `kind`: `snapshot` (Insert/Delete), `delta` (Update/PartialUpdate), `transition` (Archive/Unarchive)
- **Old** — pre-mutation snapshot of an entity captured by the framework's `Get*` functions. Exposed via `Entity.Old() Entity` and the typed wrapper `domain.Old[T](e) T`. Consumed by `BuildRules` (transition-aware invariants) and by the auditor (diff computation + Delete forensics)
- **Cache** — `cache.Cache` interface in `infra/cache` declaring `Get(ctx, key) ([]byte, bool, error)`, `Set(ctx, key, value, ttl) error`, `Delete(ctx, key) error`. The framework's single byte-level key-value port — consumed by domain handlers, infrastructure adapters, and the outbound httpclient response cache. Two canonical implementations ship: `cache.NewMemory` (in-process LRU+TTL) and `cache.NewRedis` (go-redis backed); custom backends (Memcached, Valkey, Hazelcast, …) implement the interface and inject via `Wiring.Cache` / `Wiring.SharedCache` paired with `cache.store: custom` in YAML.
- **Deps.Cache (private) / Deps.SharedCache (shared)** — the two cache instances exposed by `bootstrap.Deps`. Private is service-scoped, populated when the top-level `cache:` YAML block is declared (any store including memory). Shared is cross-service, populated when `cache.shared:` is declared (memory rejected at boot — an in-process LRU cannot honor cross-service reads). The distinction is enforced at the dependency-injection level, not via a flag on Set / Get. Default safety: SharedCache nil unless declared.
- **`cache.GetJSON[T]` / `cache.SetJSON[T]`** — typed package-level helpers that marshal Go values through `encoding/json` against `cache.Cache`. Tolerate a nil Cache and degrade to no-op so feature code that opts the cache in-or-out via YAML doesn't need a nil guard around every call.
- **`httpclient.WithCache(cache.Cache) Option` / `httpclient.HttpClient.SetCache`** — the wiring point between the outbound HTTP cache middleware and the byte-level cache backend. Bootstrap forwards `Deps.Cache` to `WithCache` at httpclient construction; the runtime swap (`SetCache`) honors late `Wiring.Cache` injection (`cache.store: custom` cases) without rebuilding the middleware chain.
- **Carrier** — short for `domain.NotificationCarrier`: any error with `NotificationContexts() []*NotificationContext`. Cross-layer error contract
- **RequestContext** (`persistence.RequestContext`) — request-scoped interface (`context.Context` + `ID()` + `ActorSubject()`/`ActorIssuer()`/`ActorClaims()`) the persistence/audit pipelines consume; satisfied by `*AppContext`. An application concern — the domain layer has NO context type (no `domain.Context`)
- **Domain event** — `DomainEvent` accumulated on entity via `RegisterEvent`, attached to ValidEntity, published by `events.Publisher` after persistence
- **Granularity B** — one outbox row per aggregate operation, regardless of how many child rows changed. Aggregate is the event unit
- **Notification** — typed marker (struct embeds a base) that the domain emits. Translation key = its Go type name
- **NotificationContext** — group of `NotificationMessage`s scoped to one entity/aggregate
- **NotificationKey** — the typed identity (struct name) of a Notification, preserved through translation in `MessageDTO.NotificationKey` and the wire-format `ErrorMessage.NotificationKey`
- **Outbox** — atomic pattern: domain row + event row written in one transaction. CDC (Debezium) tails the outbox table → publishes to Kafka
- **Pipeline** — application-layer wrapper that catches `NotificationCarrier` errors, translates contexts to DTOs, converts to `Result[T]`
- **Result[T]** — discriminated value: Success/Failure/Exception. Success carries `T`; Failure carries translated `[]ContextDTO`; Exception carries raw error
- **Service** (`domain.Service`) — marker interface for domain services injectable into an entity's `BuildRules`
- **Unarchivable** — symmetric inverse of `Archivable`. `Postgres.Unarchive` does `UPDATE deleted_at = NULL` + outbox `UNARCHIVED`. SyncEngine re-creates the Mongo doc
- **UpstreamSubscription** — declarative entry (yaml or `Wiring.UpstreamSubscriptions`) that ties one of A's Kafka topics to a local Mongo collection in B's database. The framework's `UpstreamSubscriber` materializes events into the collection and ripples recompose to every B view embedding it via an external `FromSchema` embed. The canonical cross-service composition path
- **FromSchema** — `fwinfra.FromSchema(*TableSchema) *Source`, the single embed source constructor. From the schema the framework derives the table/collection (`ts.Table()`), the store kind (type-anchored `NewTableSchema[T]` → local Postgres; type-less `NewExternalSchema` → external/Mongo — the schema's type IS the signal, no separate `From`/`FromMongo`), and — for an `EmbedMany` — the join FK (`ts.FKColumn()`). A local embed derives its parent-side Go segment from the schema's Go type (`.As(...)` optional); an external embed must declare `.As(...)`. `.On(key)` is one-to-one-`Embed`-only (the parent doc FK pointing at the source PK). `Source.IsMongo()` (type-less schema) discriminates the composer dispatch. View-on-view (an external `FromSchema` targeting another local `ViewDefinition.Name()`) is rejected by boot guard §8.3 because the recompose ripple is one-hop and the second view would drift silently
- **TxHandle** — sealed marker interface at `application/persistence/`, handed to in-TX lifecycle hooks. Carries no public methods (only an unexported sealing method), so application code cannot pronounce SQL through it. The framework's `infra/pgxTxHandle` is the only implementation; infra-layer port adapters call `fwinfra.UnwrapPgxTx(tx)` to recover the underlying `*pgx.Tx` and execute SQL. The canonical pattern for an in-TX side effect from a hook: declare a port in `application/` (or `domain/`) whose method receives a `persistence.TxHandle`; implement the port in `infra/` where the adapter unwraps and owns the SQL.
- **UnwrapPgxTx** — `fwinfra.UnwrapPgxTx(persistence.TxHandle) pgx.Tx` is the single bridge from the opaque `TxHandle` token to the live `pgx.Tx`. Lives in `infra/`, so only adapters in that layer can call it. Panics with a descriptive diagnostic on a foreign `TxHandle` implementation — defensive, never reached against framework-issued handles.
- **ValidEntity** — sealed types (`Insertable`/`Updatable`/`Archivable`/`Deletable`/`Unarchivable`/`Batch`) produced only by domain
- **WriteOption[T]** — functional option consumed by the `Scope(ctx, opts...)` write binding. Carries an `AfterBeginHook[T]` and/or a `BeforeCommitHook[T]`. Auto and manual handlers converge on the same option mechanism — Auto populates options by detecting the Cmd's provider interfaces; manual populates them by passing `WithAfterBegin` / `WithBeforeCommit` directly at the Scope call.
- **Reader[T] / Writer / Repository[T]** — the pure domain repository ports (`domain/repository.go`). `Reader[T]` = `FindByID` + `New`; `Writer` = `Insert/Update/Archive/Unarchive/Delete` taking only a ValidEntity (non-generic, no ctx); `Repository[T]` = `Reader[T] + Writer`. Pure (stdlib + google/uuid only) — what a consumer names for a read+write repository. No request-scoped concern crosses these signatures.
- **ScopedRepository[T]** — the application-layer write binding (`application/persistence/`): `domain.Reader[T]` + `Scope(ctx *AppContext, opts ...WriteOption[T]) domain.Writer`. Reads are direct on the handle; writes bind the ctx + hooks via `Scope` and return a pure `domain.Writer`. `infra.BaseRepository[T]` implements `Scope`; Auto handlers depend on `ScopedRepository[T]`.
- **fwintegration.Dispatch** — `infra/integration.Dispatch(ctx, eventKey, payload, opts...)` — the canonical producer entry point for cross-service async events. Resolves `eventKey` against `integration.publishes.events.<key>` in the loaded YAML; lazy validation surfaces an unknown key as `ErrIntegrationEventNotConfigured`. With `WithTx(tx)` from a `BeforeCommit` closure the row lands atomically with the data write + outbox + audit; without `WithTx` it commits standalone via the framework's PG pool.
- **Registry** — `*fwintegration.Registry` — receiver collection mounted by `bootstrap.IntegrationFeature.MountReceivers(reg, deps)`. Symmetric with `*fiber.App` for the HTTP transport: consumer features register receivers inline via `reg.From(source).On(eventKey, sample, handler)`. Constructed once in `buildDeps` and surfaced on `Deps.IntegrationRegistry`.
- **Receiver** — one entry in the Registry, representing one (sourceKey, eventKey) pair. Holds the reflection-based dispatch plan computed at MountReceivers time. `Receiver.RetryPendingFailures(ctx, exec, pipe, logger)` re-dispatches every pending row from `omnicore_integration_failures` matching the receiver's natural key — wired through the consumer service's admin HTTP route.
- **ConsumerPool** — `*fwintegration.ConsumerPool` — owner of the Kafka consumer goroutines that drive every Receiver. Spun by `bootstrap.serve` between Phase Receivers and `app.Listen`. `Shutdown(drainCtx)` drains every in-flight `processOne` (or returns `drainCtx.Err()` on timeout). Mirrors `UpstreamSubscriber.Shutdown` shape so the coordinated shutdown path drives both with the same shared `shutdownCtx`.
- **IntegrationFeature** — `bootstrap.IntegrationFeature` — opt-in interface a Feature satisfies to register integration receivers via `MountReceivers(reg, deps)`. Mirror of `ReadableFeature`. Detected via type assertion at Phase Receivers.
- **eventKey / sourceKey** — Go-side string identifiers consumer code passes to `Dispatch` and `From(source).On(eventKey, ...)`. The wire-side `event_type` lives in YAML; eventKey is stable across schema migrations. Renaming a wire `event_type` is a YAML edit, not a code sweep.
- **wire event_type** — the literal string the producer emits as the Kafka header value and the subscriber matches against. Lives ONLY in YAML (`integration.publishes.events.<key>.eventType` and `integration.subscribes.<src>.events.<key>.eventType`); never appears as a Go literal at a Dispatch / On call site.
- **omnicore_integration_failures** — consumer-side failure registry table. One row per (consumer_group, source_key, event_key, event_id) natural key; payload preserved verbatim for replay. Resolved via `ResolveIntegrationFailures` after `Receiver.RetryPendingFailures` re-dispatches successfully. Mirror of `omnicore_upstream_failures` shape.
- **omnicore_integration_processed** — consumer-side dedup table. Composite PK `(event_id, consumer_group)` so N consumer groups dedup the same event independently. BRIN index on `processed_at` for operator-driven pruning. Race-resilient via `ON CONFLICT DO NOTHING` on the post-success INSERT.
