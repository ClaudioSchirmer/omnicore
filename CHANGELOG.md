# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While in `0.x.y`, the public API may change between minor versions; breaking
changes are highlighted under **Changed**. Stable contract semantics arrive
with `1.0.0`.

## [Unreleased]

### Added

- **`query.JoinView(view, goName, externalName)` is now accepted as an
  Embed/EmbedMany/EmbedInChild source — a local view can be MATERIALIZED inside
  another view.** The leg family and the verb family became fully symmetric:
  `Join*` says where the data comes from (`JoinView` = a registered local view,
  `JoinUpstream` = a locally materialized mirror of another service's data),
  `Embed*` vs `Link*` says when the join is paid (on every write, materialized,
  or on every read, composed per request). Composing read models over your OWN
  aggregates therefore no longer needs the self-subscription workaround
  (publishing to your own broker topic and consuming it back to build a mirror
  of data that is already local): declare the view as the leg and the framework
  keeps the segment fresh.

  What makes it correct — the freshness signal: every write the SyncEngine
  applies to a view document asks whether that view is materialized inside
  another one and, when it is, runs the same recompose ripple an upstream mirror
  change runs. The ripple's own writes ask the same question, so a chain
  (`products` → `sales` → `dashboard`) propagates hop by hop with no extra
  declaration. A view nobody embeds costs one map lookup.

  Boot guards: the embed graph must be ACYCLIC (a loop would recompose forever —
  a fatal boot error naming the exact cycle, self-embed included); the source
  view must be registered (contributed by a `ReadableFeature`); an `EmbedMany`
  over a view leg requires a covering index on the join column ON THE SOURCE
  VIEW (the composer runs one lookup per parent document), mirroring the rule
  `LinkMany` already applies to an internal leg.

  Version coupling: a `JoinView` leg folds the SOURCE view's `Version(n)` into
  the embedding view's `RebuildHash`, so bumping the source moves the embedder's
  hash too and the forgot-to-bump guard fires on it — the embedding projection is
  rebuilt against the new shape instead of keeping copies of the old one.

  Read-side parity, and the one boundary: a materialized segment is ordinary
  document content, so a filter on a segment field SELECTS rows (it is one
  document, not a join — the substantive difference from a `ComposedView` leg
  predicate, which only shapes the segment), `?fields=` descends into it at any
  depth, archived children inside the segment are hidden exactly as a direct read
  of the source view hides them, and a 1:1 segment field is a first-class sort
  key. ORDERING is the boundary: a 1:N segment cannot be a sort or cursor key (an
  array has no single value — the rule a native child collection already
  follows), and an embed has no per-segment `OrderBy`/ceiling knob (that pair
  exists only on a `ComposedView`'s `LinkMany`, which builds its array per
  request). Order a materialized 1:N segment in the consumer, model the ordering
  into the source view, or use `LinkMany` when a declared order and a per-parent
  cap are contract requirements.

  Lifecycle, level by level: the EMBEDDING document's own `deleted_at` gates the
  read as always (`?includeArchived` lifts it); a CHILD of the source inside the
  segment is stripped on default reads and surfaced by `?includeArchived`, so the
  segment reads like a direct read of the source view; and an ARCHIVED SOURCE
  under the default (keep) policy stays in the
  segment carrying its soft-delete stamp — the contract an upstream-mirror
  segment already has, and what both write paths (surgical ripple and full
  recompose) produce identically. Declare `DeleteOnArchive()` on the SOURCE view
  for the opposite semantics (archiving removes the document, so the segment
  becomes the explicit `null` / the element leaves the array).

  Rebuild ordering (automatic): because a rebuild composes its segments by
  reading the source's ACTIVE collection — a pointer that switches only at the
  source's flip — an embedder rebuilt first would materialize the content about
  to be replaced and finish stale, with no event left to repair it (rebuild
  writes raise no embed signal, by design). Since bumping a source's `Version`
  also moves every embedder's hash, the two always land in the same rebuild run,
  so the framework sequences it: the boot's rebuild plan list and
  `RebuildAllViews` now execute in EMBED-DEPENDENCY ORDER (each source before the
  views materializing it, transitively), and a view whose source this instance
  did not bring to a flip (a follower whose driver holds that source's lock) is
  deferred rather than rebuilt against stale input — the driving instance holds
  both plans in the same order. Views embedding no view keep their declaration
  order. The ad-hoc single-view entry points (`RebuildView` / `RebuildViewSince`)
  stay literal: rebuild a source that way and its dependents' copies are not
  refreshed, so rebuild them too (or use the ordered `RebuildAllViews`).

- **`EmbedMany(leg).OrderBy(column).Desc()` — a materialized 1:N segment can now
  declare the order of its elements.** Without it the array keeps whatever order
  the writes produced (the previous behavior, unchanged for every view that
  declares none). The order is materialized, not applied at read time: the array
  is stored sorted, so reads pay nothing.

  What makes it safe across every path: all three writers of a segment — the
  first compose, the rebuild backfill and the surgical ripple — emit the SAME
  server-side `$sortArray` stage, so a late-arriving element lands in its
  position instead of at the end, concurrent workers and pods converge on the
  identical array, and a blue-green rebuild's shadow matches its active slot.
  (Sorting the composed array in Go would have meant two implementations — Go's
  byte order versus the server's, which honors the view's declared collation —
  diverging as intermittent rebuild-verify failures.) The sort is TOTAL: the
  declared column, then `_id`, since an unbroken tie would let two writers store
  different arrays for identical state.

  `EmbedMany` now returns its own binding type, so `OrderBy`/`Desc` on a 1:1
  `Embed` or on an `EmbedInChild` is not expressible rather than rejected at
  boot. Boot guards: the order column must exist on the embedded source, and
  `.Desc()` without `.OrderBy(...)` is a declaration error. Declaring or changing
  an order moves the RebuildHash (it is projection shape), so it needs a
  `Version(N)` bump.

  There is deliberately NO per-parent ceiling to go with it: a cap on a
  materialized array would discard elements no later edit could promote back (the
  surgical edit never sees what was cut). A ceiling stays the read-time twin's —
  `MaxLinkManyLimit` on a `ComposedView`'s `LinkMany`, which rebuilds its array
  per request. That is now the ONE knob the two families do not share.

  **Requires MongoDB 5.2+**, and only for views that declare an order: an
  unordered `EmbedMany` emits the stages it always did.

### Changed

- **BREAKING — archived content is now hidden on a default read in EVERY
  segment, under one rule.** Previously the archived-entry strip covered only
  native child collections and `SharedBaseView` roles; a materialized `Embed`
  segment (1:1 or 1:N, over a local view or an upstream mirror) and an
  `EmbedInChild` enrichment were never filtered, so an archived source stayed
  visible in the segment while a direct read of that same source hid it. Every
  segment now behaves identically: a default read hides archived content and
  `?includeArchived=true` reveals all of it at once — the same contract a
  `ComposedView` leg already followed.

  **The one condition is the SOURCE SCHEMA**: a segment is filtered if, and only
  if, the schema behind it declares `SoftDelete(column)`. That declaration is
  what states the source HAS an archived state and names the column carrying it;
  a source declaring none has no archived concept and is never touched (the flag
  is a silent no-op there, never an error). The behavior is a property of the
  declaration — not of the verb, the leg kind, or whether the data is
  materialized.

  Hiding stays content-level, never row-level (a segment is a LEFT join, so the
  document always survives): a 1:1 segment becomes the explicit `null` — the value
  an unresolved reference already carries — and a 1:N segment drops the archived
  elements, keeping the rest in their declared order.

  *Migration*: a consumer that relied on seeing archived sources in a segment
  passes `?includeArchived=true`. A source whose archived state must remain
  visible unconditionally should not declare `SoftDelete` on its embed schema —
  and for an upstream mirror, remember the column must also survive the
  subscription's `filter:` allowlist (§8.5) for the rule to have anything to act
  on. Nothing changes for a source that declares no soft-delete column.

### Fixed

- **The §8.5 soft-delete-filter guard now covers a `ComposedView`'s external
  legs, not just embeds.** A locally materialized mirror has TWO kinds of
  consumer, and both apply its soft-delete column: a view that EMBEDS it
  (materialized gate) and a composed view that LINKS it (the composed reader
  gates each leg on its own schema unless `?includeArchived`). The guard walked
  embeds only, so declaring `.SoftDelete("deleted_at")` on the leg's
  `NewExternalSchema` while omitting `deleted_at` from the subscription's
  `filter:` booted cleanly when the mirror was consumed by a link — and archived
  upstream rows then looked active in the segment forever, exactly the
  silent-archive bug the guard exists to prevent. Both consumers are now
  cross-checked in one pass, with the same abort/advisory split.
  `ComposedViewDefinition.ExternalLegs()` is the accessor the guard reads (safe
  before validation: it resolves nothing).
- **The dangling 1:1 embed repair now covers a `JoinView` leg.** The post-write
  repair that heals a 1:1 segment after the EMBEDDING document changes its FK
  (field ownership keeps the consult write off the segment, and the source emits
  no event of its own) was gated to external legs only. With a view leg the
  segment would have kept pointing at the previously referenced document with no
  future event able to heal it.
- **A zombie-document removal now signals the views that embed it.** The
  post-write tombstone check deletes a document a fresher writer already
  disowned; that removal bypassed the embed fan-out, so a view materializing it
  could keep serving the removed content.
- **`?fields=` no longer leaks an auto-included soft-delete column out of a 1:N
  segment.** The reader re-includes a nested collection's soft-delete column so
  the archived-entry strip can see it, then removes it before responding; the
  removal walk stopped at an ARRAY intermediate segment, so with a 1:N embed of a
  view whose documents carry their own children the column reached the wire.

### Added

- **`ViewDefinition.EmbedInChild(childSchema, leg).On(col)` — read-side 1:1
  enrichment of a view's native child array.** For the "list of X with the name
  of Y per line" shape (e.g. a sale view whose line items each carry
  `product_id`, enriched with the product name from an upstream projection): each
  element of a native aggregate child (declared via `root.Child(...)`) is
  enriched with a 1:1 external lookup by the element's own FK, materialized into
  the view and kept fresh by the recompose ripple. The write model stays
  normalized — the element keeps only its FK; the enrichment lives only in the
  view. 1:1 only (no `EmbedManyInChild`: a 1:N would nest an array inside a child
  element, the forbidden grandchild shape). For a `SharedBaseView` it targets the
  BASE's native children; role-nested children are not supported. The enrichment
  participates in the RebuildHash (a change without a `Version(N)` bump is caught
  as `DriftForgotToBump`). The schema passed must be a native child of the view
  root — a non-child is rejected at boot.

- **`ComposedViewDefinition.LinkInChild(childSchema, leg).On(col)` — the
  read-time, non-materialized twin of `EmbedInChild`.** On a `ComposedView` whose
  primary carries a native child array, it enriches each child element with a 1:1
  sub-document looked up by the element's own FK at read time, never stored. Same
  signature shape as `EmbedInChild`; `childSchema` must be a native child of the
  PRIMARY's schema (boot-validated). Accepts BOTH leg kinds (`JoinUpstream` and
  `JoinView`), and — having no recompose ripple — requires no covering index
  (unlike `EmbedInChild`). Read-side leg parity: filter and `?fields=` into the
  `<childSeg>.<legSeg>.<field>` path shape what enters the enrichment per element
  (never which rows/lines return); sort/search/pagination stay the primary's (a
  sort into the segment is a 400). 1:1 only (no `LinkManyInChild`).

### Changed

- **breaking** — **a 1:1 `Embed` and every `EmbedInChild` now REQUIRE a covering
  index at boot** for the recompose ripple's reverse scan: a 1:1 `Embed` needs an
  index on its parent join column, an `EmbedInChild` a multikey index on
  `"<childSegment>.<fk>"`. The developer declares it via `.Indexes(query.Index(...))`;
  a missing index aborts boot (`ValidateViewSchemas`) instead of silently
  degrading the ripple to a collection scan. `EmbedMany` is exempt (its ripple
  resolves the parent by the child's FK → parent `_id`, never a reverse scan of
  the view). Existing services with an un-indexed 1:1 embed must add the index to
  boot.

- **breaking** — **view composition speaks one join vocabulary: the join column
  and the two segment names moved off the source onto the verb and the leg
  constructor; `FromSchema`, the `Source` type, `Source.FK`, `Source.As` and
  `Leg.FK`/`Leg.As` are removed.** An embed/link source is now a single *leg*
  built by `query.JoinUpstream(schema, goName, externalName)` (an external
  collection — the only kind an `Embed`/`EmbedMany`/`EmbedInChild` accepts) or
  `query.JoinView(view, goName, externalName)` (a registered view — a
  `ComposedView` leg only). Both segment names are mandatory, declared in
  `TableSchema.Field` order (Go first, external second). The join column is named
  on the verb via `.On(column)` — mandatory and compile-time enforced (the verb
  returns a binding whose only route back to the builder is `.On`) — and feeds
  the view hash uniformly for every embed/leg kind. The verb's multiplicity fixes
  which side holds the FK, so the same leg plugs into an `Embed` (re-materialized)
  and a `Link` (read-time). A `LinkMany`'s `OrderBy`/`Desc`/`MaxLinkManyLimit`
  move onto its binding, so they can no longer be misplaced on a 1:1 `Link` (the
  former boot guards for that misplacement are removed with the surface). The
  external-embed-missing-Go-segment and missing-`.FK` boot guards are gone too:
  the names and (compile-time) join are now mandatory at declaration.
  `TableSchema.FK` is unchanged — it declares an aggregate child's FK to its root
  (a write-side concern). *Migration*:
  `Embed("seg", query.FromSchema(sch).FK("col").As("Go"))` →
  `Embed(query.JoinUpstream(sch, "Go", "seg")).On("col")`; the inline
  `schema.FK(...)` on an EmbedMany source becomes the verb's `.On(...)`;
  `Link("seg", query.JoinUpstream(sch).FK("col").As("Go"))` →
  `Link(query.JoinUpstream(sch, "Go", "seg")).On("col")`;
  `LinkMany`'s `OrderBy`/`Desc`/`MaxLinkManyLimit` chain before the terminal
  `.On(...)`. Rebuild hashes are unchanged for an unchanged view, so no view
  rebuilds on upgrade.

## [0.38.0] - 2026-07-26

### Changed

- **breaking** — **embeds are single-level by construction; `Source.Embed`,
  `Source.EmbedMany` and `Source.Embeds()` are removed.** A view still declares
  any number of top-level `Embed`/`EmbedMany`, but a `*Source` no longer carries
  embeds of its own nor exposes a builder for them — so embed-of-embed is not
  expressible and fails to compile, never a runtime surprise (the former boot
  rejection in `ValidateViewSchemas` is removed with the surface it guarded).
  Single-level now holds end to end: the composer, rebuild-hash, index,
  recompose-ripple and export paths no longer descend past a view's top-level
  embeds, because a source has nothing below it. Nesting was never a supported
  shape — first-time compose and a full rebuild materialized a nested segment,
  but the recompose-ripple that keeps an embed fresh is one-hop and only reaches
  a view's top-level embeds, so a nested segment would materialize once and then
  drift silently. Rebuild hashes are unchanged (a real view's empty nested list
  always hashed the same), so no view rebuilds on upgrade. *Migration*: no
  working program breaks (a nested embed always failed at boot); to reach two
  external hops, embed each at the top level, or join at read time with a
  `ComposedView`.
### Fixed

- The view base-kind boot gate is now **symmetric**. A `query.SharedBaseView`
  already rejected a non-shared-base `.Schema(...)`; the mirror was missing — a
  regular `query.View` rooted at a `core.NewSharedBaseSchema` was silently
  accepted and would flatten a shared identity into an ordinary view document.
  `ValidateViewSchemas` now also rejects that mis-wire at boot with an
  explanatory message, so the two constructors are type-exclusive in both
  directions.

## [0.37.0] - 2026-07-25

### Changed

- **breaking** — **a view's root table is derived from its schema; `.Root(table)`
  is removed.** `query.View(name)` and `query.SharedBaseView(name)` no longer take
  a `.Root(...)` call — the root table (the broker routing key and the composer's
  `FROM`) is now `RootTable()` = the attached schema's `Table()`. The value always
  equalled the schema's table, so this removes a redundant, unvalidated second
  declaration (a misspelled `.Root("user")` used to leave the view silently muted).
  *Migration*: delete every `.Root(table)` call from view declarations.
- **breaking** — **`SharedBaseView` takes only the collection name; the base
  schema moves to `.Schema(...)`, matching a regular `View`.** `SharedBaseView(base,
  name)` → `SharedBaseView(name).Schema(base)`, and `.Schema(base)` must precede the
  first `.Role(...)` (the role is validated against the base). The "must be a shared
  base" check moved from the constructor panic to boot validation
  (`ValidateViewSchemas`), and a new guard rejects a `ComposedView` whose primary
  declares no schema — so the "a view must declare a schema" rule is now one boot
  error across regular views, shared-base views and composed-view primaries.
  *Migration*: `SharedBaseView(personBase(), "persons")` →
  `SharedBaseView("persons").Schema(personBase())`.
- **breaking** — **`core.NewSharedBase` is renamed `core.NewSharedBaseSchema`,**
  completing the schema-constructor family (`NewTableSchema` / `NewSiblingSchema` /
  `NewExternalSchema` / `NewSharedBaseSchema`). Behavior is unchanged.
  *Migration*: rename every `NewSharedBase(` call to `NewSharedBaseSchema(`.

### Fixed

- **The blue-green rebuild's shape verify treats an absent field and an explicit
  null field as equivalent — no more spurious `diverges in shape` aborts.** During
  a rebuild that adds a nullable column, a mid-rebuild writer still on the PREVIOUS
  binary (whose schema does not declare the new column) creates the shadow document
  without that key, while a fresh compose on the new binary emits it as an explicit
  null. The verify's field-shape sample compared key-presence value-blind, so it
  intermittently aborted the rebuild — `diverges in shape (fresh-only: [<col>])` —
  on a document that is in fact correct (the reader decodes an absent key and an
  explicit null to the same nil pointer, and the source row's value genuinely is
  null). The shape check now counts a key as drift only when it is present with
  a NON-null value on one side and ABSENT from the other, and stays value-blind
  for keys present on both sides — so neither an absent≡null difference nor a
  present-null-vs-populated difference (e.g. an embed array a later event fills)
  fails the rebuild, while a genuine drop (a non-null field present on one side
  and absent on the other) still does.

## [0.36.1] - 2026-07-23

### Changed

- **`BaseRepository.Schema` is now unexported — `WithSchema` is the only way to
  bind a repository's TableSchema.** The field was a direct-assignment escape
  hatch that bypassed the construction-time boot checks (PK, Revision,
  aggregate depth, old-clone safety, `Modes()` ⟺ `SoftDelete`); an entity that
  declared `ModeArchive`/`ModeUnarchive` without a `SoftDelete` column could
  slip past the boot panic and fail only at the first archive write. Binding
  now always runs the full validated path, so a misconfigured schema panics at
  construction as intended. No production or example code set the field
  directly (every repository already used `WithSchema`); the framework's own
  engine integration tests that constructed a repository via the struct literal
  were migrated to `WithSchema`.

## [0.36.0] - 2026-07-23

### Added

- **Upstream-mirror soft-delete boot guard (§8.5) — a subscription `filter`
  that would strip the archive state now fails loud instead of silently.** The
  `filter` on an `upstreamSubscriptions` entry is a string allowlist over the
  raw upstream payload and consults no schema, so an `ARCHIVED` event's
  soft-delete column (e.g. `deleted_at`) is dropped unless the `filter` lists
  it — and the local mirror would then never reflect the archive (archived rows
  look active forever). Boot now cross-checks each subscription against the
  soft-delete column declared on the external `FromSchema` embed(s) of its
  collection: a column DECLARED on the embed schema but OMITTED by a non-empty
  `filter` ABORTS the boot naming the offending subscription and column; a
  mirror whose embed schema declares no soft-delete column at all logs an
  ADVISORY warning (the consumer often cannot know whether the upstream
  soft-deletes); an empty `filter` mirrors the full payload and always passes.
  See the upstream-mirror note under Table schema and the `upstreamSubscriptions`
  block in the YAML reference.

- **Projection-state registry (`omnicore_projection_state`) — the durable
  rendezvous that makes the projection deterministic under multiple workers
  and pods.** Two write-then-check handshakes close the windows a per-document
  revision guard cannot see: (1) *base revision*: every shared-base role
  event stamps its `base_revision` into the registry BEFORE the fan-out probes
  for target documents; a role document that materializes AFTER a fan-out that
  could not see it (its INSERTED/UNARCHIVED projection was still in flight)
  re-checks the registry after writing and, when a newer base revision proves a
  fan-out already passed, repairs itself by one consult recompose applied
  guarded — the store's write order guarantees one of the two sides always
  fires. (2) *document tombstones*: DELETED records the row's LAST revision
  (now stamped on the DELETED payload's `_ids.revision`) as a tombstone before
  a GUARDED delete (`ReadModelStore.DeleteGuarded` — a document a fresher
  writer advanced past the delete's revision survives), and every
  document-creating upsert re-checks the tombstone after writing: a zombie
  consumer's older upsert racing the delete removes its own write instead of
  resurrecting the document. Tombstones carry the dead row's
  `created_at` as the INCARNATION discriminator (stamped on the DELETED
  payload's `_ids.created_at`, read in the same statement as the pre-delete
  revision): the guarded delete and the creator-side self-remove only kill
  documents whose stored `created_at` falls in a two-second window around it
  (created_at columns round OR truncate to their DDL precision per engine), so a DETERMINISTIC id (a shared-PK role, the
  base) re-created under the same natural key — same id, revision restarted —
  is never mistaken for a zombie of the dead life. A schema without
  `CreatedAt()` falls back to the revision-only tombstone. *Upgrade note (pre-release builds only):* tombstones written by
  builds before the `created_at` discriminator lack it — a deterministic
  identity deleted on such a build and re-created after upgrading stays
  blocked until the tombstone's TTL; clear them once
  (`deleteMany({_id: /^doc:/, created_at: {$exists: false}})`) or wait out
  the 24h. Tombstones expire after 24h via a TTL index
  (`EnsureProjectionState`, provisioned at SyncEngine start); base-revision
  records never expire. The registry lives outside the blue-green slots (like
  `omnicore_mongo_views`), is never rebuilt, and its writes carry
  MAJORITY write concern — the registry is the fiduciary of the handshakes,
  and a failover rollback of a primary-only stamp would silently dissolve
  their ordering premise (view writes keep the deployment default; they
  reconverge through redelivery). On a standalone node majority degrades to
  the primary ack — zero cost. Custom `ReadModelStore` implementations must add
  `DeleteGuarded`, `BulkApplyProjection` and `EnsureProjectionState`.

### Changed

- **BREAKING: manual aggregate scanners decode by column NAME, not by position.**
  `read.RootScanner`/`read.ChildScanner` now receive a `map[string]any` — the row
  read by name, values normalized per backend (uuid → string, etc.) — instead of
  a positional `core.Row`/`core.Rows` scanned with `row.Scan(&a, &b, ...)`. The
  loader's manual path selects explicit columns (never `SELECT *`), so a manual
  scanner is order-independent and DDL-safe (an online ADD COLUMN can no longer
  shift the positions it reads), and the dialect-specific id decode (e.g. a MySQL
  BINARY(16) id via `uuid.FromBytes`) is gone — the map already carries the
  normalized string. *Migration*: change `func(core.Row) (T, error)` doing
  `row.Scan(&id, &name, …)` to `func(map[string]any) (T, error)` reading
  `m["id"]`/`m["name"]`; same for `WithChildScanner`. The manual scanner remains
  the ONLY read that is not schema-driven-column-explicit (its query names the
  schema's columns; only the developer's decode is manual).

- **BREAKING: the day-to-day projection is PAYLOAD-DIRECT — the SyncEngine no
  longer re-reads the relational source per event.** For entity-rooted views
  without external embeds, each event applies as ONE atomic
  aggregation-pipeline update (new `ReadModelStore.ApplyProjection`, dual-apply
  to the blue-green shadow preserved): typed decode against the TableSchema
  (json.Number keeps int64 precision; RFC 3339 → time.Time; base64 → []byte),
  unconditional own-field sets, shared-base fields behind the `_base_revision`
  document watermark (the base row-lock's commit order replayed on the read
  side — the last writer into the base wins on every document), and SURGICAL
  child-array edits (per-element by child PK, base-children elements carrying a
  `_rev` watermark) that preserve archived child history the payload does not
  carry. The shared-identity fan-out applies the same guarded stages to the
  other roles' documents — no `ComposeBatch` on the hot path. The composer
  remains for: SharedBaseView documents (cross-row remnant pick), views with
  external embeds, rebuild/verify/drift, and the upstream ripple. A payload
  without the `_ids` block (corrupt or foreign) is skipped — never silently
  re-read. Custom `ReadModelStore` implementations must add `ApplyProjection`.
- **BREAKING: the outbox payload is the event-carried-state contract — one
  self-sufficient shape for every verb, ONE outbox row per write.** Every body
  verb now emits: all scalars column-keyed flat at the top (root/role fields ∪
  sibling fields ∪ shared-base business fields ∪ the verb's managed
  timestamps — the exact app-clock values the DML bound), an `_ids` structural
  block (`id`, and for SharedBase roles `base_id` + `base_revision` +
  `base_purged` on the purging DELETED), and `_children`/`_base_children`
  groups (per-item column-keyed fields + an `_op` verb — insert/update/
  archive/delete/noop — from the same OperationOf categorization the persister
  executed; the warm shared-base insert hydrates the base children, so a
  second role's INSERTED payload carries the full shared collection). DELETED
  keeps its historical structural keys (PK + shared-base FK) and only ADDS
  `_ids`. The empty base-table `UPDATED` fan-out row of a SharedBase write is
  NO LONGER EMITTED — the SyncEngine fans out to the other roles' documents
  from the role event's `_ids.base_id` (the orphan-purge base `DELETED` row
  remains, external cascade depends on it). The `"_"` column-name prefix is
  now a reserved framework namespace (boot failure at TableSchema
  declaration). External `UpstreamSubscription` consumers: the mirror decode
  strips `_`-prefixed keys unless the `Filter` allowlists them, so a foreign
  producer's flat payload passes through untouched. Migration: after upgrading,
  run a view rebuild to converge documents produced during the rollout window.
- **BREAKING: EVERY root schema requires a `Revision(column)` declaration —
  the deterministic last-writer-wins token of the read side.** (Initially
  shipped for the shared base, then generalized: the zombie-consumer window —
  a slow pod finishing an in-flight event after a partition handoff — affects
  every aggregate, so every root table carries the token.) A root schema
  attached to a repository without `Revision` is a boot failure; siblings and
  aggregate children declare none (owner-guarded). The projector refuses any
  document write whose `_ids.revision` is older than the document's
  `_revision` watermark — the whole own-data pipeline no-ops atomically.
  For the SHARED BASE the column is a BIGINT
  NOT NULL the framework fully manages: initialized to 1 on the identity's
  creation and incremented (`revision = revision + 1`) UNDER THE BASE ROW'S
  LOCK by EVERY write that touches the identity — the shared-field upsert, the
  lifecycle convergence (archive/reactivate), a role's archive/unarchive even
  without a base transition, a role's hard-delete (non-purge branches
  included; the purge removes the row) and the batch role verbs — so
  concurrent writes of one identity serialize in real relational commit order
  and the value totally orders the identity's closure: shared scalars, base
  children, the SharedBaseView segment pick and the role rows themselves
  (which is what makes the guarded consult writes below sound). The resulting
  value travels on the outbox payload (`_ids.base_revision`) and orders every
  read-model write of identity data: the last writer to enter the base wins
  regardless of consumer latency, worker interleaving, pod count or
  redelivery. The write path's extra cost is one UPDATE where a SELECT of the
  base revision already ran. A role attaching a base without `Revision` is a
  boot failure. Migration: add the column
  (`revision BIGINT NOT NULL DEFAULT 0`) to each shared-base table and declare
  `.Revision("revision")` on its `NewSharedBase`.
- **BREAKING: every consult-composed write is revision-guarded — the
  full-document `$set` upsert is gone from the projection.** A consult
  recompose (SharedBaseView documents, views with embeds, the base-event
  fan-out) and the rebuild/verify backfill (`ReadModelStore.
  BulkApplyProjection`, replacing the backfill's plain `BulkUpsert`) now write
  ONE guarded pipeline: own fields behind the document's `_revision`,
  shared-base fields behind `_base_revision`, a SharedBaseView document as a
  single base-revision scope, embed segments still owned by the
  recompose-ripple (written only on document creation). A consult that read
  the relational earlier but reaches the store later than a fresher writer is
  now a no-op instead of a lost update — and it can no longer regress the
  document's watermarks. The composed document's data is always at least as
  fresh as its watermark (the composer reads the root/base row first), so the
  guard only ever suppresses stale writes. At the EQUAL revision the two write
  families split by what they carry — the rolling-deploy closure: a pod on
  the previous binary can advance a document to the current revision through
  its own (older) column list, leaving the columns its schema does not know
  behind (missing, null or stale-valued). The PAYLOAD-DIRECT stage applies its
  full carried state at the equal revision — the payload is the emitting
  transaction's own truth (one revision ↔ one committed state, so a
  redelivered duplicate overwrites idempotently) and re-asserting it replaces
  any value a schema-blind consult left stale under the watermark; columns the
  event does not carry are never touched. The same equal arm rides the OWN-child
  edits (the surgical per-element array stages guarded by `_revision`): a child
  op at the document's current revision re-asserts its element, restoring child
  columns a schema-blind composition dropped — shared-base children already
  converge through the per-element `_rev` rule (a consult-composed element
  carries none and counts as older). The CONSULT pipelines are READ
  snapshots, so at the equal revision they only FILL what the document lacks,
  always stored-wins, at every level a column can surface (root/sibling/base
  scalars, a whole new child segment, a child ELEMENT's column via per-element
  PK-keyed shallow merge, a role segment's scalar via sub-document shallow
  merge). One level limit: an array nested inside a role segment of a person
  document keeps the stored array whole — additions there converge on that
  role's next write or a rebuild.
- **Managed timestamps are now application-clock authored.** The managed
  columns (`created_at`/`updated_at` and the soft-delete stamp written by
  archive and its cascades, the shared-base lifecycle convergence included)
  are no longer stamped with the dialect's `NOW()`/`CURRENT_TIMESTAMP`/
  `SYSTIMESTAMP` expression inside the generated DML. Instead the write path
  mints ONE `time.Now()` instant per write operation (UTC, truncated to
  microseconds — the precision every supported backend stores) and binds it as
  an ordinary argument, the same move the ids made with the Go-minted UUID v7:
  root, children, siblings and the shared-base cascade of one operation all
  carry the exact same instant, and the value is known in Go before COMMIT —
  the prerequisite for the outbox payload to carry authoritative timestamps.
  The ARCHIVED outbox payload's soft-delete value is now that same bound stamp
  (previously an informational Go-side approximation of the DB stamp).
  Control-plane timestamps (`outbox.created_at`, the integration/upstream
  failure registries, the replay admin) keep the dialect `NOW()` expression,
  and `Dialect.NowExpr()` remains on the interface for them. Operational note:
  the authoritative clock for row timestamps is now the application host's
  (keep pods on NTP — a correctness requirement); the database server's clock
  no longer participates. Under clock skew across pods, `updated_at` is not
  monotonic between concurrent writers — the ordering token is `Revision`,
  never `updated_at`.
- **BREAKING: `audit_events` is now a plain table on every backend; the
  Postgres-only partitioning is removed.** The audit id is a time-ordered
  UUID v7, so the primary key alone gives append-only insert locality — the
  Postgres table drops `PARTITION BY RANGE (created_at)`, its default partition,
  and the composite `(id, created_at)` PK (now just `(id)`). Monthly-partition
  maintenance is gone. Because `audit_events` is written inside every write's
  transaction, the index set is now the minimum that serves the framework's own
  reads — the primary key (`FindByID`) and `audit_events_entity_timeline_idx`
  (`entity_type, aggregate_id, occurred_at DESC`, serving `FindByAggregate`) —
  identical on all four dialects. The four forensic indexes (`actor`,
  `tenant_id`, `thread_id`, `created_at`/BRIN) are removed on every dialect;
  retention and any forensic lookup index are now devops concerns, added against
  the live table when a deployment needs them. The framework's embedded `0001`
  is edited in place: a fresh install gets the plain table; an existing Postgres
  deployment keeps its already-applied partitioned table (writes land in the
  default partition; no future partitions are created), and converting or
  pruning it is a devops migration.
- **BREAKING: `postgres.AsPostgres` and `audit.EnsureFuturePartitions` are
  removed; `migration.New` → `migration.NewPostgres(dsn, dir)`.** With
  partitioning gone, the migration runner was the last framework code recovering
  the concrete PG adapter through the neutral port; it now opens its own
  connection from `relational.dsn` like the MySQL/SQL Server/Oracle runners. No
  production code recovers a concrete engine from `core.RelationalEngine`
  anymore — custom reads use the neutral `DB.Querier()`, and the only remaining
  PG-specific escape is the in-TX `UnwrapPgxTx` (one of the per-engine
  `Unwrap<Engine>Tx` family). Migration: replace `postgres.AsPostgres(DB).Pool()`
  reads with `DB.Querier()`, and any direct `migration.New` call with
  `migration.NewPostgres(dsn, dir)`.

### Fixed

- **Schema-driven reads name their columns explicitly — an online column-add
  can no longer transiently break an in-flight projection.** The composer's read
  path issued `SELECT *`; a cached prepared-statement plan of a `SELECT *` binds
  to the table's column layout, so a blue-green view rebuild that adds a
  projected column (online, while a pod keeps serving the old version)
  invalidated the plan and the next execution failed with "cached plan must not
  change result type" (Postgres SQLSTATE 0A000) — one CDC event then failed to
  project until a later redelivery (a transient off-by-one on the new slot; no
  permanent loss). Every schema-driven read now emits an explicit column list
  (new `TableSchema.ReadColumns()` — PK + business fields + shared-base/child
  FKs + managed columns, in a deterministic order that keeps the prepared-plan
  cache to one entry per shape), so an added-but-unlisted column never changes
  the result type. The developer's manual aggregate scanner (which decodes raw
  rows) and the offline admin replay tool keep `SELECT *` by design.

- **Consult-class view documents no longer lose fields to a concurrent-writer
  race.** A view with external embeds has TWO independent writers converging on
  the same document `_id` — the SyncEngine (recomposing on the entity's own
  events) and the UpstreamSubscriber recompose-ripple (recomposing on upstream
  events) — and both wrote full-document Upserts, so whichever landed last
  regressed the other's freshly-read fields (observed as an `EmbedMany` array
  frozen without its newest item whenever the parent's own event out-raced the
  item ripples across CDC topics; the wider the relay's batching skew, the more
  likely). Both writers now write ONE atomic pipeline upsert claiming only the
  fields they read freshly — the ripple owns the embed segments, the SyncEngine
  owns everything else; whoever arrives first still materializes the complete
  document. (Embed-less consult views moved off the plain Upsert too — see the
  revision-guarded consult writes under Changed.)
- **The recompose-ripple edits embed segments PER ELEMENT, so concurrent
  ripples commute.** The ripple used to `$set` each embed segment to a
  freshly-composed snapshot of the mirror; two concurrent ripples for
  different upstream ids converging on the same parent (`workers > 1` in one
  subscription, or partitions spread over pods) could interleave and the older
  snapshot would erase the newer one's element. Each ripple now applies the
  event's own change surgically (strip/append the one element keyed by the
  upstream id; conditionally set the 1:1 sub-document) — edits for different
  ids touch disjoint elements and commute, and events for the same id are
  already serialized end to end (broker partitioning + hash-bucketed worker
  dispatch). The ripple hot path no longer reads the relational source at all;
  the full recompose remains for materializing a parent document that does not
  exist yet (non-upsert on the surgical write keeps a ripple racing a
  concurrent document delete from resurrecting a skeleton — the
  `ReadModelStore.ApplyProjection` port gained the `upsert bool` parameter for
  this). An embed source that declares nested embeds keeps the full-recompose
  path. A 1:1 embed's last ordering window — a document written with an FK
  whose segment is unresolved (the composing read raced the mirror write) or
  stale (a consult update changed the FK, which by ownership never rewrites
  segments) — closes with a post-write repair handshake: after every consult
  upsert and ripple fallback create the mirror is re-read fresh and the
  segment set under a double guard (FK still matches AND the stored element is
  not already that id), so either the repair heals it or the mirror doc's own
  later insert ripple does, and a repair can never regress the element's own
  fresher ripple.
- **Ripple writes dual-apply to the blue-green shadow.** During a rebuild
  window the recompose-ripple wrote only the active slot, so an upstream event
  landing mid-rebuild could be missing from the flipped collection (only a
  lucky verify sample would catch it). Ripple writes now follow the same
  dual-apply discipline as the SyncEngine — active + shadow, bounded retry,
  abort the rebuild rather than fail the live path.
- **An unresolved 1:1 embed writes an explicit `null`.** When the FK is null
  or the source document is gone, the composer omitted the segment key — and
  under `$set`-merged document writes the stale sub-document survived
  indefinitely (the documented contract is "null when unset/unresolved"). Both
  compose paths (per-row and batched) now write the explicit `null`, and the
  surgical delete path clears the 1:1 segment the same way.
- **Clearing a 1:1 sibling now reaches the projected document.** The
  payload omitted an all-nil sibling facet ("mirroring the write"), so a PUT
  that removed the sibling row (e.g. nulling every notification flag) left the
  projected document carrying the stale sibling values forever — under
  event-carried state an absent key is indistinguishable from "untouched".
  The payload now emits the sibling columns unconditionally (explicit nulls
  when the facet is nil), and the projector recognizes the all-null group as
  the removed row and DROPS the keys (`$$REMOVE`) — matching the composer,
  which omits a missing sibling row, so the blue-green verify's shape
  comparison stays exact. A live row with a null column still projects the
  explicit null.
- **The `anonymize` upstream-delete policy ripples the retained document.**
  The post-anonymize recompose was triggered with a delete-shaped (nil) after
  state; it now carries the blanked-but-retained mirror document, so dependent
  views keep embedding it with the blanked fields rather than treating the
  event as a source deletion.

## [0.35.0] - 2026-07-19

### Added

- **Online blue-green view rebuild.** A full rebuild (a `Version` bump or the
  drift path at boot) no longer mutates the live Mongo collection. It builds a
  fresh physical *shadow* slot from the relational source while the *active*
  slot keeps serving, keeps the shadow current with in-flight writes via
  dual-apply, verifies it (reverse + forward completeness + a field-shape
  sample), then flips readers to it with a single registry write and reclaims
  the retired slot — no downtime, no half-built collection ever exposed.
  Migration `0002` (×4 dialects) adds the `active_collection`/`shadow_collection`
  pointer columns to `omnicore_mongo_views` (NULL active = the bare `<view>`
  collection, so no backfill on upgrade). Reads resolve `view → active slot`
  through an in-memory pointer cache refreshed on a bounded-staleness lease,
  never per query. See `mongo-schema-evolution.html`.

- **The boot view rebuild is now non-blocking.** It runs in the background:
  `/livez` comes up immediately (a long rebuild is never killed by a liveness
  probe) while `/readyz` stays 503 until the rebuild finishes and the consumer
  joins — the pod is alive but out of rotation until its read model is ready. A
  fatal rebuild error still exits the process non-zero. A pod that boots while
  another instance holds the rebuild lock is a follower — it serves the active
  slot and picks up the flip at runtime instead of aborting. While the pod waits,
  the `/readyz` 503 body's `reason` names the view under rebuild and its position
  in the run (`initializing: rebuilding view "users_view" (2/5)`), falling back to
  the generic `initializing: view rebuild in progress` in the drift-reconcile
  window before the first view starts. New
  `mongo.rebuild.pointerLeaseSeconds` (0 = default 15s) tunes the activation
  fence / settle lease and thus the boot-rebuild window.

- **Parallel rebuild backfill.** The shadow backfill is now a bounded
  producer/consumer pipeline instead of a serial read→compose→write loop: the
  streaming root-id scan cuts fixed-size batches and hands them to a pool of
  workers that set-based-compose and bulk-upsert concurrently, so the relational
  scan+compose overlaps the Mongo write and independent batches run in parallel.
  Two new `mongo.rebuild` knobs tune it — `workers` (0 = default 4; the
  relational pool must carry ≥ workers+1 connections, one pinned by the scan) and
  `batchSize` (0 = default 1000). Every root document is independent and the
  upsert is idempotent on `_id`, so batch order never matters. See
  `mongo-schema-evolution.html`.

- **Batched external-embed resolution on rebuild.** A view's external `Embed`
  (1:1) / `EmbedMany` (1:N) sources are now resolved SET-BASED during a rebuild —
  and on any multi-root write-time recompose (the shared-base identity fan-out and
  the upstream embed ripple, where one event fans out to many documents): one
  `{field: {$in: …}}` per embed source for the whole batch, grouped by the join
  key, instead of one Mongo lookup per parent — nested embeds collapse the same
  way per level. The composed document is
  identical to the per-event result (same 1:1 sub-document / 1:N array, same null
  semantics); only the round-trip count drops. Carried by the new
  `ReadModelStore.FindManyByFieldIn` port method (below).

### Changed

- **breaking: the `query.ReadModelStore` port signatures.** Every method now
  takes a typed `query.PhysicalCollection` instead of a `collection string`, so
  a raw view name can no longer reach the store as a collection by accident (it
  will not compile) — physical names come only from the shared `ViewResolver`.
  The orphan-field-cleanup methods `ObservedFieldNames`/`UnsetFields` are
  **removed** from the port (blue-green builds a fresh shadow, so there are no
  orphan fields to `$unset`); `ProvisionSlot`/`DropCollection` are added. A
  custom `ReadModelStore` implementation must adopt the new signatures.
  `query.DetectViewDrift` gains a trailing `*ViewResolver` argument.
  `SyncEngine.ExecuteRebuild` now runs the blue-green sequence; the operator
  ad-hoc `RebuildView`/`RebuildViewSince` remain in-place upsert-only. The
  `mongo.rebuild.orphan` YAML key is retained but no longer suppresses deletion.
  The port also gains `FindManyByFieldIn` (a set-based `{field: {$in: …}}` read)
  for the batched external-embed path — a custom `ReadModelStore` must add it.

## [0.34.1] - 2026-07-17

### Fixed

- **docs-only: the `domain` layer's dependency boundary no longer cites
  `google/uuid`.** `architecture.html` (the layer diagram and the dependency
  table) described the domain layer as "stdlib + google/uuid", which read as if
  a consumer could type a domain field or PK as `uuid.UUID`. Identity is
  `domain.ID` — a `uuid.UUID` field boot-fails against the closed persistable
  field-type set (see `table-schema.html`). The boundary now reads "stdlib
  only; identity is domain.ID"; the framework's own internal use of google/uuid
  stays recorded in the stack list. No Go code changed.

## [0.34.0] - 2026-07-17

### Added

- **Offset pagination on the write-side criteria engine — `criteria.Query.Offset(n)`.**
  The query envelope that already carried `Limit` now also carries `Offset`, so
  `FindAll` can page over the authoritative aggregate:
  `OrderByDesc("CreatedAt").Limit(50).Offset(100)`. It renders in each engine's
  native form — `LIMIT n OFFSET m` on Postgres/MySQL, `OFFSET m ROWS FETCH NEXT n
  ROWS ONLY` on Oracle and SQL Server — via a new `Dialect.ApplyLimitOffset`
  seam; the existing `ApplyLimit` cap path (SQL Server's `TOP`) is untouched, so
  unordered existence probes still need no `ORDER BY`. Because a row skip over an
  arbitrary physical order is meaningless — and SQL Server's `OFFSET…FETCH`
  mandates it — a non-zero `Offset` requires **both a positive `Limit` and at
  least one `OrderBy`**; violating either is a loud error on `FindAll`, never a
  silently wrong page. `FindOne` is single-row and ignores `Offset`. For
  end-user-facing listings the Mongo read side's keyset pagination remains the
  recommended path. See `custom-command-handler.html` (Loading by criteria).

## [0.33.2] - 2026-07-17

### Fixed

- **docs-only: `architecture.html` now presents the write→read data flow as a
  first-class subsection — and states explicitly that it is not event
  sourcing.** The architecture page covered the 4 layers, the two seams and the
  dependency rules, but the framework's spine — CQRS with event-driven
  projections via the transactional outbox + CDC relay — only appeared as a
  detail inside the transport-seam paragraph; the full narrative lived
  scattered across `transport.html`, `aggregate-persistence.html` and the
  lifecycle maps. A new "The write→read data flow" subsection names the
  pattern, diagrams the pipeline (relational TX → CDC relay → broker →
  SyncEngine → composer → Mongo view), and carries a callout making the
  boundary explicit: the relational database remains the single source of
  truth, the outbox payload is a routing hint, and the composer re-reads
  current state on each event — state is never derived by folding an event
  log, which is what makes the read side self-healing and views rebuildable.
  No Go code changed.

## [0.33.1] - 2026-07-17

### Fixed

- **docs-only: the SharedBase "Active-only uniqueness" passage in
  `table-schema.html` now covers all four dialects and drops a stale id type.**
  The passage only showed the Postgres (partial unique index) and MySQL
  (generated column) shapes — a reader targeting SQL Server or Oracle had no
  documented shape for the separate-FK role model — and the MySQL example
  still typed the generated column `CHAR(36)`, predating the `BINARY(16)` id
  standard. It now adds the SQL Server shape (a filtered unique index — same
  statement, same `WHERE deleted_at IS NULL` clause; load-bearing there
  because a plain `UNIQUE` admits a single NULL) and the Oracle shape (a
  function-based unique index over `CASE WHEN deleted_at IS NULL THEN
  person_id END` — a fully-NULL index expression gets no index entry, so
  archived remnants drop out of the index), and retypes the MySQL example to
  `BINARY(16)`. No Go code changed.

## [0.33.0] - 2026-07-16

### Added

- **Oracle joins PostgreSQL, MySQL and SQL Server as a first-class relational
  engine — Oracle Database 23ai or higher.** A complete engine ships behind the
  `oracle` build tag (`infra/db/engine/oracle`, driver `sijms/go-ora/v2` — pure
  Go, no Oracle Client libraries): selecting `relational.dialect: oracle` +
  `go build -tags 'oracle <transport>'` runs a service on Oracle through the
  same engine seam as the other backends — same write path (one TX for data +
  outbox + audit), criteria reads, composer projection, rebuild lock, migration
  runner, admin tooling. Dialect specifics: `:N` placeholders, QUOTED-UPPERCASE
  identifiers (equivalent to the catalog's unquoted uppercase folding — manual
  queries stay natural — while reserved-word column names work with no
  reserved-word list; the engine lowercases result-set column keys and
  extracted constraint names back to the declared form), a single-statement `MERGE`
  upsert with NULL-safe conflict comparison (Oracle has no HOLDLOCK equivalent;
  the concurrent-MERGE `ORA-00001` classifies as a unique violation),
  `SYSTIMESTAMP` as the now expression (server-timezone parity; Oracle's
  `CURRENT_TIMESTAMP` is session-TZ), a tail `FETCH FIRST n ROWS ONLY` row cap,
  unique/FK classification from `ORA-00001`/`ORA-02291`/`ORA-02292`, and a
  session-scoped `DBMS_LOCK` rebuild lock (requires
  `GRANT EXECUTE ON SYS.DBMS_LOCK`). **Identity is stored `RAW(16)`** — bytewise
  comparison preserves the UUID v7 time order. **The 23ai floor is deliberate**:
  booleans are native `BOOLEAN` columns and JSON payloads native `JSON` columns
  (the engine forces go-ora's `lob fetch=post` option into the DSN — without it
  the driver truncates native-JSON reads at 32 KiB) — with ONE deliberate
  exception: the two CDC-tailed payloads (`outbox.payload`,
  `integration_events.payload`) are `CLOB`, because LogMiner cannot decode a
  native-JSON column's binary (OSON) redo on any Debezium version — the same
  wire-crossing rule that keeps ids uuid text; a consumer-created table the
  relay tails must follow it too. **Empty string is NULL**
  (the Oracle semantic, no sentinel encoding): a NOT NULL text column rejects
  `""`, and a `*string` holding `""` reads back `nil`. The control plane ships
  as `embedded/oracle/0001_framework.{up,down}.sql` (constraint names identical
  across dialects; `omnicore_upstream_failures.local_id` is nullable on Oracle
  — the `''`-is-NULL consequence) with its own runner (`migration.NewOracle`)
  over a framework-owned golang-migrate driver (golang-migrate ships no Oracle
  driver): statements split on top-level semicolons — migration files carry
  plain SQL, PL/SQL blocks are not supported. **Deadline fidelity on a frozen
  database**: go-ora's cancellation rides the same (frozen) connection, so the
  engine bounds every Querier call itself — reads and control-plane
  side-channels return `context.DeadlineExceeded` on time (the 2s readiness
  probe and the request-timeout 504 hold on Oracle); the write path is
  deliberately unwrapped (mid-transaction abandonment would change delivery
  semantics — go-ora's `TIMEOUT=<seconds>` DSN option is the operator bound
  there).

## [0.32.1] - 2026-07-15

### Fixed

- **Docs: the "Domain Service via Auto handler" example in `auto-handlers.html`
  dereferenced `*u.GetID()` inside `IfInsertOrUpdate`.** On Insert the id is
  minted after validation, so `GetID()` is nil inside `BuildRules` and the
  example, copied verbatim, panics with a nil-pointer dereference on the first
  insert. The example now nil-guards and passes a zero `excludeID` — which
  excludes nothing, exactly right for a brand-new entity. Docs-only release:
  no Go code changed.

## [0.32.0] - 2026-07-14

### Changed

- **BREAKING: old-clone safety is now a boot check — a persisted field tagged
  `json:"-"`, or a custom `json.Marshaler`/`json.Unmarshaler` on the entity
  type, panics at `WithSchema`.** The framework builds the `domain.Old()`
  pre-write snapshot (consumed by `BuildRules` transition checks and the
  transition auditor) by cloning the root entity through an `encoding/json`
  round-trip, so a `json:"-"` persisted field silently vanished from the ghost
  (the prior state read back as the zero value) and a custom (un)marshaler took
  the round-trip over entirely — both corrupted rules/audit with no visible
  failure. The check covers every field persisted through the root struct
  (the root's declared fields, sibling partitions, and shared-base fields
  resolved on the role's type); aggregate children are exempt (the clone copies
  them by value, never through JSON), and renaming tags (`json:"name"`) remain
  allowed — the round-trip marshals and unmarshals the same type, so renames
  are symmetric. *Migration*: drop the `json:"-"` tag (wire names belong on the
  web-layer DTOs) or move the custom JSON codec off the entity type.

## [0.31.0] - 2026-07-14

### Changed

- **BREAKING: every framework control-plane table now follows the framework's
  own id standard — a UUID v7 minted in Go as the PRIMARY KEY, stored in the
  dialect's native id form (`UUID` on Postgres, `BINARY(16)` on MySQL/SQL
  Server), with no `AUTO_INCREMENT`/`BIGSERIAL`/`IDENTITY`/DB default
  anywhere.** The control plane previously mixed four key regimes (DB-generated
  uuid on the PG outbox, BIGINT auto-increment elsewhere, text uuid on
  audit_events, natural-key PKs on the registry/dedup tables). Now: `outbox`,
  `audit_events` (v7 replaces the previous v4), `integration_events`,
  `omnicore_upstream_failures` and `omnicore_integration_failures` carry the
  Go-minted uuid PK; `omnicore_mongo_views` and `omnicore_integration_processed`
  gain the same surrogate PK with their former natural keys preserved as UNIQUE
  constraints (`omnicore_mongo_views_view_name_key`,
  `omnicore_integration_processed_natural_key`) — every lookup still keys on
  the natural columns. Wire-crossing id references (`outbox.aggregate_id`,
  `integration_events.event_id`/`aggregate_id`/`correlation_id`/`causation_id`/
  `thread_id`, audit references) are canonical uuid TEXT (`CHAR(36)`) on EVERY
  dialect — Postgres included, which previously used native `UUID` columns for
  them — because they cross the Debezium/Kafka boundary as strings.
  `UpstreamFailureRecord.ID`/`IntegrationFailureRecord.ID` are now the
  canonical uuid `string` (were `int64`); `audit.InsertAuditEvent`/
  `audit.NewReader` take the dialect's value codec. The admin replay pages by
  keyset over the PK (no `LIMIT/OFFSET` literal — a PG/MySQL-ism T-SQL
  rejects), and the non-done registry listing orders via ANSI `CASE` instead of
  a bare `IS NULL` sort key. *Migration*: the `0001_framework` files were
  rewritten IN PLACE (pre-1.0; golang-migrate tracks no checksums) — recreate
  the framework control plane on existing databases (bench: `docker compose
  down -v`; a deployed database needs the equivalent ALTERs or a
  drop-and-recreate of the seven framework tables). Performance notes shipped
  with the same rewrite: the uuid-text reference columns carry binary/ASCII
  collations (`COLLATE "C"` on PG, `CHARACTER SET ascii` on MySQL,
  `Latin1_General_100_BIN2` on SQL Server — comparisons are memcmp; values are
  always the lowercase canonical form the framework writes), the dedup
  pre-check is an index-only probe on the natural-key UNIQUE (the surrogate PK
  costs reads nothing), and the outbox drops its `(aggregate_type,
  aggregate_id)` index — no framework code SELECTs the outbox by aggregate
  (Debezium reads the replication log/CDC, not the table), so every write was
  paying maintenance for nothing; `created_at` stays for pruning.

### Added

- **SQL Server joins PostgreSQL and MySQL as a first-class relational engine.**
  A complete engine ships behind the `sqlserver` build tag
  (`infra/db/engine/sqlserver`, driver `microsoft/go-mssqldb`): selecting
  `relational.dialect: sqlserver` + `go build -tags 'sqlserver <transport>'`
  runs a service on SQL Server through the same engine seam as the other
  backends. Dialect specifics: `@pN` placeholders, bracket-quoted identifiers,
  a single-statement `MERGE … WITH (HOLDLOCK)` upsert, `CURRENT_TIMESTAMP` as
  the now expression, a SELECT-head `TOP n` row cap, unique/FK violation
  classification from errors 2627/2601/547, and a session-owned
  `sp_getapplock` rebuild lock. **Identity is stored `BINARY(16)`, never
  `UNIQUEIDENTIFIER`**: SQL Server orders GUIDs last-byte-group-first, which
  would destroy the UUIDv7 time order and fragment the clustered PK;
  `BINARY(16)` compares bytewise, so the Go-minted v7 ids keep the clustered
  index append-friendly (the InnoDB rationale, verified against SQL Server
  2022). JSON rides as `NVARCHAR(MAX)` text. The framework control plane ships
  as `embedded/sqlserver/0001_framework.{up,down}.sql` (filtered indexes where
  PG uses partial ones; constraint names identical across dialects so
  `ConstraintBinding` maps the same violations) with its own migration runner
  (`migration.NewSQLServer`). The "Supported column shapes — Go ↔ SQL Server"
  table in the manual is the canonical type map. Internal enablers, no
  consumer surface: the bootstrap engine twins collapsed into a per-dialect
  boot registry (build-tag negation pairs do not compose at three engines —
  any tag combination now links), and the control-plane JSON payloads
  (outbox/audit/integration) bind as text instead of raw bytes (byte-identical
  on PG/MySQL; SQL Server refuses the implicit varbinary→NVARCHAR conversion).

### Changed

- **`core.Dialect` also gains the savepoint trio — `Savepoint(name)`,
  `RollbackToSavepoint(name)`, `ReleaseSavepoint(name)`.** The shared-base
  orphan purge (the database-vetoable delete) used to emit the standard
  `SAVEPOINT`/`ROLLBACK TO SAVEPOINT`/`RELEASE SAVEPOINT` literals, which
  T-SQL spells `SAVE TRANSACTION`/`ROLLBACK TRANSACTION` — with NO release
  statement (a savepoint is discarded at COMMIT), so `ReleaseSavepoint`
  returns "" on SQL Server and the caller skips the empty statement. The
  `Dialect` implementations moved to their own `dialect.go` files (core and
  each engine) — the seam long outgrew the read path `read.go` named.

- **The last dialect-specific SQL literals left shared code — `core.Dialect`
  gains `NowExpr()` and `ApplyLimit(sql, n)`.** Shared statement builders used
  to bake in `NOW()` (managed `created_at`/`updated_at` stamps, the soft-delete
  archive stamp, the outbox `created_at`, the failure registries' timestamps)
  and a tail `LIMIT n` (existence probes, `FindOne`'s bounded load, the
  composer's row fetches) — both happen to parse on Postgres AND MySQL, so the
  coupling was invisible, but they would not survive a third engine. Every
  generated statement now obtains the current-timestamp expression from
  `Dialect.NowExpr()` and its row cap from `Dialect.ApplyLimit(sql, n)`, which
  receives the COMPLETE SELECT so an engine whose cap is not a tail clause
  (e.g. a SELECT-head `TOP n`) can rewrite the statement. Both engines render
  exactly the SQL they rendered before (`NOW()` / trailing ` LIMIT n`), so no
  emitted statement changes; the operator-facing drift-reconcile scripts, which
  have no dialect at hand, switch from `NOW()` to the ANSI
  `CURRENT_TIMESTAMP`. Groundwork for the planned SQL Server engine; a custom
  `core.Dialect` implementation (none is expected outside the framework's
  engines) must add the two methods.

## [0.30.0] - 2026-07-13

### Changed

- **BREAKING: identity is a TYPE — `domain.ID` becomes the persistable
  identity field type, and the MySQL value-shape codec heuristic is removed.**
  A field holding an id (a cross-aggregate reference like a
  `BuyerID`/`TenantID`) is now declared `domain.ID` — nullable ⇒ `*domain.ID`
  — and the field's Go type alone drives every leg on every supported
  engine: the write
  bind and the criteria probe lift render it in the dialect's native id form
  (`UUID` on Postgres, `BINARY(16)` on MySQL) and the new id scan proxies
  restore it on read, with `nil` ⇄ SQL `NULL` on the pointer (the nullable
  BINARY(16) reference was previously impossible). A plain `string` field is
  text, ALWAYS — the old MySQL `EncodeArg` heuristic that bound any
  canonical-shaped (36-char) uuid string as 16 raw bytes is gone, so
  `CHAR(36)`/`VARCHAR(36)` uuid-valued text columns are first-class. The
  managed PK/FK slots are unaffected (always native — including bare-string
  `ID` criteria probes such as the exclude-self `Ne("ID", id)`, lifted by the
  translator by construction). *Migration (MySQL consumers only)*: retype
  every uuid reference field stored as `BINARY(16)` from `string` to
  `domain.ID` (`*string` → `*domain.ID`) — the DDL and the data stay exactly
  as they are; fields stored as text keep `string` and need nothing. Postgres
  consumers: same retyping recommended for native `UUID` reference columns
  (text binds work either way on PG, so nothing breaks unretyped).

- **BREAKING: the identity contracts speak `domain.ID` end to end.** The
  type-driven sweep reaches every contract that used to carry a bare id
  string: `Updatable`/`Archivable`/`Deletable`/`Unarchivable.ID()` now return
  `domain.ID` (the redundant `IDValue()` twin is absorbed — one method, one
  type; `Insertable.ID()` already returned `*domain.ID`),
  `AggregateValueObject.GetID()` returns `domain.ID` and the documented AVO
  shape carries an exported `ID domain.ID` field (the persister's minted-id
  write-back sets it via reflection and rejects any other type),
  `domain.WriteResult.ID` is `domain.ID` (JSON shape unchanged — it marshals
  to the canonical string), and `criteria.ByID` takes `domain.ID`.
  *Migration*: mechanical — AVO structs retype `ID string` → `ID domain.ID`
  (`GetID` returns it directly), call sites needing text call `.Value()`,
  `IDValue()` call sites become `ID()`, and bare-string `ByID(s)` becomes
  `ByID(domain.NewID(s))`.

### Added

- **`domain.ID.UnmarshalJSON`** — the symmetric half of `MarshalJSON`, so an
  ID round-trips through every JSON boundary (outbox payload, audit maps, DTO
  mapping) with NewID parity (no uuid validation; `IsValid` stays the explicit
  seam).
- **Closed persistable-type set — boot fail on an unknown field type.**
  `Field(...)` now validates the declared Go type against the exact set the
  framework composes on every supported relational engine
  (string/bool/int·16/32/64/float32/64/
  time.Time — each plus its pointer form — `domain.ID`/`*domain.ID`, `[]byte`,
  `json.RawMessage`). Anything else panics at construction with the fix: a
  `google/uuid.UUID` field points at `domain.ID`; every other unknown type
  (named enums, structs/maps, Go slices — the PG-only array mapping) points at
  `table-schema.html`, the canonical home of the supported set. Matching is by
  identical type, never by kind, so the both-engine guarantee stays literal.
- **Identity scan proxies + criteria probe lift.** The auto-scan detects
  `domain.ID`/`*domain.ID` fields by their reflected type and substitutes
  `sql.Scanner` proxies that own the decode (16-byte BINARY(16), uuid text and
  CHAR(36) forms all restore canonical; SQL `NULL` resolves to `nil` on the
  pointer field and to a loud error on the value field); the criteria
  translator lifts bare `string`/`*string` probes on identity-typed fields
  into `domain.ID` (`TableSchema.IDKindOf`, derived from the Go struct —
  mirroring how BoolColumns infers bool columns). Proven by unit +
  integration tests across the full matrix (PG/MySQL × native/text ×
  required/nullable × write/scan/criteria/NULL).

## [0.29.0] - 2026-07-13

### Changed

- **Dev-only empty-shell boot.** Under `APP_PROFILE=dev`, `validateWiring` now
  accepts a fully empty `Wiring{}` (no `Features`, no `BeforeServe`, no
  `Translations`) with a loud `slog.Warn` instead of aborting with "nothing to
  serve". The boot serves only the framework surfaces (`/livez` + `/readyz`;
  OpenAPI/GraphQL when wired) — the legitimate state of a freshly scaffolded
  service, letting the environment (config, connections, migrations, CDC
  relay) be proven live before the first entity exists. The waiver covers only
  the fully empty wiring: a dev wiring WITH features still requires at least
  one `translation.Module`, and any profile other than `dev` rejects the empty
  wiring exactly as before.

### Fixed

- **`migration.Manager.Up` no longer errors on an empty (or missing) service
  migrations directory.** It went straight into golang-migrate's file source,
  which errors on an empty source instead of no-oping — so a fresh service
  whose `migrations.dir` held only a `.gitkeep` could not boot under
  `autoRun: true`, even though `Pending` and `ValidateDownExists` already
  treated the same state as "nothing to do". `Up` now applies the framework's
  embedded control plane and skips the service stage when the directory
  carries no versioned `*.up.sql`.

## [0.28.0] - 2026-07-12

### Changed

- **Sibling schemas now reject every identity/lifecycle declaration AT THE
  DECLARATION CALL — including the previously half-honored managed timestamps
  (breaking for that misuse).** A sibling is a 1:1 slice of the OWNER's row: it
  borrows the owner's PK (the physical PK column of the sibling table must
  carry the SAME NAME as the owner's PK column — the framework writes and joins
  by that name; an FK-style `<owner>_id` column used to surface only as an
  *Unknown column* 500 at the first write), the shared PK is the link (no FK),
  and the owner controls lifecycle and dating. `PK`/`FK`/`SoftDelete` already
  panicked at `Sibling(...)` attach; they now fail earlier, at the call itself,
  with messages that teach the DDL contract. `CreatedAt`/`UpdatedAt` on a
  sibling — previously accepted and stamped on INSERT but never re-stamped on
  UPDATE (a silently stale timestamp) — are now rejected the same way: declare
  them on the owner, whose timestamps already date the whole 1:1 row. See
  Table schema → Sibling tables.

### Added

- **`AggregateBy` + `read.By` — the grouped half of the aggregate DSL: the same
  typed scalar facts, computed per `GROUP BY` group in ONE SELECT.** The v0.27.0
  DSL answers "how many / how much over THIS criteria"; a rule over per-group
  facts (per-category caps, distinct-key cardinality, balance checks) still had
  to fetch rows and bucket them in Go. `AggregateBy(ctx, q, read.By("Category"),
  specs...)` compiles `SELECT <keys>, <specs> … GROUP BY <keys> ORDER BY <keys>`
  under the same criteria, field resolution (sibling/shared-base fields pull
  their LEFT JOINs) and active-by-default scope gate as `Aggregate`. The passed
  specs act as templates (they carry no result); each returned `read.Group`
  holds its own copy of every fact, read back type-safely via
  `read.GroupResult(group, template)` — the template's concrete type, no
  assertion — plus `Key`/`KeyString` access to the group key(s) by Go field
  name (driver-neutral: MySQL's `[]byte` text keys land as `string`). An empty
  set yields zero groups; `len(groups)` is the distinct-key cardinality; a
  group's spec reports `Found=false` only when every matched row holds NULL in
  that column; groups come back ordered by key ascending (deterministic).
  Order/limit on the Query are ignored, exactly like `Aggregate`. Both drivers
  proven by integration tests (grouped NUMERIC/DECIMAL normalization, `[]byte`
  key normalization, the scope gate riding the grouped SELECT). See Custom
  command handler → Grouped aggregates.

## [0.27.0] - 2026-07-12

### Added

- **`Exists` and the aggregate DSL (`Aggregate` + typed specs) on the aggregate
  loader — hydration-free scalar facts for write-path business rules.**
  `FindAll` was the only criteria read, so a uniqueness pre-check inside a
  `domain.Service` paid a full aggregate load (root + sibling + shared-base +
  children hydration) to answer a yes/no question. `Exists(ctx, q)` compiles to
  a bare `SELECT 1 … LIMIT 1`; `Aggregate(ctx, q, specs...)` computes any
  combination of scalar facts over the same criteria in ONE SELECT — each fact
  is a typed spec carrying its own result: `read.Count()` (int64), the
  exact-integer trio `read.SumInt`/`read.MinInt`/`read.MaxInt` (int64 — the
  money shape, minor units; a fractional scalar errors loudly) and the float64
  quartet `read.Sum`/`read.Avg`/`read.Min`/`read.Max`. Field specs carry a
  `Found` flag (whether any row matched) so a rule can distinguish "the average
  is 0" from "there is nothing to average" (and 0 can be a real Min/Max); Count
  is 0 on an empty set, a sum with `Found=false` is the empty sum. Same field
  resolution as `FindOne`/`FindAll` (sibling/shared-base fields pull the same
  LEFT JOINs whether they appear in the predicate or in a spec), same
  active-by-default scope gate, nothing hydrated. The PK is addressable in
  criteria as the fixed Go field `"ID"` (already supported, now documented and
  pinned by tests), so the exclude-self uniqueness shape is one expression:
  `Where(And(Eq("Email", v), Ne("ID", excludeID)))`. Both drivers are proven by
  integration tests (Postgres NUMERIC arrives as `pgtype.Numeric`, MySQL
  DECIMAL as text — both normalized exactly). See Custom command handler →
  Loading by criteria.

## [0.26.0] - 2026-07-12

### Added

- **A shared base's managed columns are honored when declared.** `NewSharedBase(...)`
  accepting `.CreatedAt()`/`.UpdatedAt()` used to be silently ignored by the write
  path (the base row was never stamped — declaring the columns with `NOT NULL` and no
  DB default failed at the first insert). The base write now stamps them exactly like
  any other table: on the identity's creation, and `UpdatedAt` on every role-driven
  change of the shared fields (warm upsert + role update). A base that declares
  neither — the common shape — keeps byte-identical SQL. Lifecycle convergence
  (archive/unarchive) still touches only the soft-delete column, on the base as on
  every table. See TableSchema → Shared base.

### Fixed

- **Docs: domain-service port file placement made explicit.** The service-layout
  standard said the port "lives beside the entity", which read as "same file" to a
  generator. Now explicit: the interface is one more domain type — one-type-per-file
  applies — so it gets its own `<entity>_service.go` in the domain package (never
  embedded in the entity's file); the implementation keeps its own file in
  `internal/infra/`. The interface stays with its consumer, never with its
  implementation.

## [0.25.0] - 2026-07-12

### Added

- **Docs: "Service layout & naming — the recommended standard" (new section, Reference
  group).** One page standardizing how a consumer service is laid out and named:
  directory skeleton, per-layer file granularity (one domain type per file, one wire
  file per operation, one schema per file), command/query file naming per verb,
  migration granularity (one pair per entity, owner-prefixed child/sibling tables,
  `<table>_<col>_key` constraints), bootstrap feature rules, HTTP verb honesty, and
  cross-cutting names (permissions, notifications, translation keys, GraphQL fields).
  Dual-audience by design: a **baseline** for developers (deviate deliberately; nothing
  is boot-enforced) and **normative** for code generators (produce exactly this shape
  unless the developer explicitly instructs otherwise). Deliberately code-free: API
  mechanics stay in their mapped sections, which remain the sole authority on any
  perceived conflict.

## [0.24.0] - 2026-07-12

### Fixed

- **Docs: the CDC relay must pass the outbox payload through as opaque text —
  JSON expansion off.** The transport section now states the relay config
  contract explicitly: `table.expand.json.payload=false` + the pass-through
  value format (Debezium Server `simplestring` / Kafka Connect
  `StringConverter`). With expansion on, Debezium infers a typed schema from
  each event's JSON text, and JSON number shapes are not stable types (Go
  marshals the float `3.0` as `3`), so a numeric array like `[3.7, 3]` fails
  the expansion and crashes the relay — a poison message halting all
  read-model refresh. No OmniCore consumer reads the typed structure (the
  payload is a routing hint; the composer re-reads the database), so the
  expansion bought nothing. Behaviour change is config-side only; the
  reference consumer's relay configs were aligned in the same round.

### Added

- **Aggregate child ids are written back into the aggregate map after INSERT.**
  The relational persister mints each child's PK inside the child INSERT; that id
  is now stamped back onto the tracked item (`domain.AssignAggregateItemID` — sets
  the item's exported string `ID` field, statuses untouched), so post-write readers
  see the child exactly as persisted. Concretely: a command's `FromEntity` can now
  project the write response as a **full aggregate mirror including child ids**
  (`GetCurrentItemsOf` returns them populated), and the outbox/audit snapshots —
  built after the child INSERTs — carry real ids instead of empty ones. An item
  without a settable string `ID` field is skipped silently. No API change to the
  write path; `AssignAggregateItemID` is new public domain surface.

## [0.23.0] - 2026-07-11

### Fixed

- **A SharedBase base carrying a second unique column no longer 500s on MySQL.**
  The shared-identity write was a DB-native upsert (`ON CONFLICT (pk)` on Postgres,
  `ON DUPLICATE KEY UPDATE` on MySQL). Postgres scopes the conflict to the primary
  key, but MySQL's `ON DUPLICATE KEY UPDATE` fires on **any** unique key — so a base
  with a second unique column (e.g. a unique `email` beside the natural-key PK) let
  a new-identity write whose email already existed hijack the upsert onto the wrong
  `persons` row: the new base was never inserted and the role FK failed → **500**
  instead of a clean **409**. The shared-base write is now an explicit **INSERT**
  (new identity) / **UPDATE by PK** (existing identity) branch — identical on both
  dialects — so a second unique column raises a normal unique violation the
  repository's `ConstraintBinding` maps to 409. Restores the backend-agnostic
  parity invariant. Minor semantic sharpening: two concurrent COLD inserts of the
  same brand-new identity now yield one PK-conflict 409 instead of a silent
  last-write-wins merge (the more correct outcome). No API change.

## [0.22.0] - 2026-07-11

### Added

- **HTTP server hardening knobs — `http.bodyLimitBytes`, `http.readTimeoutSeconds`,
  `http.idleTimeoutSeconds`.** Three optional `http:` fields, each left at Fiber's
  default when unset (no behaviour change on upgrade). They sit at different layers,
  so their client-visible outcome differs:
  - `bodyLimitBytes` caps the request body; a larger body is rejected with **413**
    (rendered by the existing `ErrorHandler` envelope). Unset → Fiber's 4 MiB default.
  - `readTimeoutSeconds` / `idleTimeoutSeconds` are **transport-level** (fasthttp),
    the slowloris/keep-alive hardening. A **read** timeout surfaces as **408 Request
    Timeout** (the new `ReadTimeoutNotification` / `SemanticRequestTimeout`): fasthttp
    routes the read-deadline breach through Fiber's server error handler, which the
    framework maps to a typed 408 envelope (the client was too slow — a 408, not a
    misleading 500), written best-effort before the connection closes. An **idle**
    timeout silently closes the keep-alive with no response (normal reaping). Unset/0
    → no timeout. Both are distinct from `http.requestTimeoutSeconds`, which bounds
    *handler processing* and maps to **504**. Negative values are rejected at boot.
- **`SemanticRequestTimeout` + `ReadTimeoutNotification`.** A new notification
  semantic mapping to **408** (HTTP) / `CodeDeadlineExceeded` (gRPC), emitted by the
  web `ErrorHandler` when the fasthttp read timeout fires. Translated in all seven
  catalogs. Distinct from `RequestTimeoutNotification` (the 504 handler deadline).
- **Readiness probe — `GET /readyz`.** The framework now registers a Kubernetes
  readiness probe alongside liveness. It answers "can this pod take traffic now?":
  `200` when the request-path stores respond and the process is not draining, `503`
  otherwise. Failing it makes Kubernetes remove the pod from the load balancer
  (not restart it). Two behaviours:
  - **Drain flip.** On `SIGTERM` the probe flips to `503` immediately — the missing
    half of the existing coordinated drain. Kubernetes stops routing new traffic
    while the in-flight requests finish, so the graceful shutdown no longer competes
    with fresh arrivals. Derived from the same `signal.NotifyContext` that governs
    the drain, so no new state to reason about.
  - **Store checks.** A dialect-neutral relational `SELECT 1` plus a Mongo ping,
    under a short deadline so a wedged store fails the probe fast. The message
    transport is **deliberately excluded**: the outbox decouples writes from the
    broker (a write still commits and reads still serve when the broker is down), so
    a broker outage must not pull the pod from the balancer — async-consumer health
    is an alerting concern, not readiness.

  Like `/livez`, `/readyz` is framework-owned but **not** auto-public — list it in
  `auth.publicRoutes` so a tokenless kubelet can reach it. `*mongo.MongoDB` gains a
  `Ping` method backing the read-store leg.
- **Graceful-shutdown drain knobs — `shutdown.tracingDrainSeconds`,
  `shutdown.hardGraceSeconds`.** Two optional `shutdown:` fields alongside
  `drainTimeoutSeconds`. `tracingDrainSeconds` (default 5) gives the final telemetry
  flush its OWN short budget so a dead/slow OTLP collector cannot consume the whole
  drain window. `hardGraceSeconds` (default 5) is the watchdog margin over the drain
  budget: at `drainTimeoutSeconds + hardGraceSeconds` the process force-exits even if
  a non-cooperative stage never returns; a **negative** value disables the watchdog
  (an embedder that owns the process lifecycle). Both default when 0/absent, so an
  upgrade with no yaml change gains the bounded behaviour automatically.

### Changed

- **Graceful shutdown is now bounded no matter what.** A context is cooperative — it
  cannot interrupt a stage that ignores it — so the drain budget alone never
  guaranteed termination. Three defences close the gap: the `tracing` flush runs on
  its own short budget (see `tracingDrainSeconds`); the `onShutdown` hook is raced
  against the drain deadline instead of awaited inline, so a hook that blocks ignoring
  its context can no longer stall the process; and a watchdog force-exits at
  `drain + hardGraceSeconds` while a second SIGINT/SIGTERM exits immediately. Also:
  the gRPC listener is force-closed when its graceful `Shutdown` overruns the deadline,
  and the final Mongo disconnect is bounded by a 5s timeout. This fixes a real
  operational hazard — with a dead OTLP collector during a rolling deploy the drain
  could otherwise burn the full window and get SIGKILLed mid-flush. Not breaking:
  defaults preserve behaviour; only a shutdown that was already overrunning is now
  cut short (deliberately).
- **breaking** — **the liveness probe moved from `GET /health` to `GET /livez`.**
  The route is renamed to the Kubernetes-idiomatic `-z` family (the pair `/livez` +
  `/readyz`; k8s core itself retired the single `/healthz`). Behaviour is otherwise
  identical: static `{"status":"ok"}`, no dependency checks (a liveness probe must
  fail only when a restart is the cure, never on a store blip). Update every
  `auth.publicRoutes` entry (`GET /health` → `GET /livez`, and add `GET /readyz`),
  any k8s/Docker probe path, and the `openapi.uiPath` / `graphql.path` collision set
  (`/health` is no longer reserved; `/livez` and `/readyz` are).

### Fixed

- **Docs: a service's own migrations start at `0001`, not `0002`.** The migration
  numbering guidance (`migrations.html`, the `migration` package comments, CLAUDE.md)
  incorrectly stated services begin at `0002+`. Because the framework's `0001_framework`
  is tracked in a *separate* table (`omnicore_framework_migrations`) from the service's
  own (`omnicore_migrations`), the two sequences never collide — a service numbers its
  own files independently from `0001`. No behaviour change (the runner always supported
  this); the guidance was wrong.

## [0.21.0] - 2026-07-08

### Added

- **Message-transport seam — pluggable broker behind a build tag.** The three
  async consumers (read-side `SyncEngine`, integration `ConsumerPool`,
  `UpstreamSubscriber`) now read through a backend-neutral `transport.Subscriber`
  port (`infra/transport`) instead of importing a broker client directly,
  mirroring the relational-engine seam: an adapter self-registers in `init()`
  behind its own build tag, and the composition root resolves it by name via the
  subscriber registry (`RegisterSubscriber` / `NewSubscriber`). `Deps.Transport`
  exposes the built subscriber. The producer path is unchanged (the write still
  lands an outbox row in-TX; a CDC relay ships it), so the at-least-once, dedup
  and per-aggregate ordering semantics are identical whichever broker backs it.
  Two adapters ship:
  - **Kafka/Redpanda** (`infra/transport/kafka`, `-tags kafka`) — wraps
    `segmentio/kafka-go`; being Kafka-wire-compatible it backs both brokers with
    no code change (the choice is the broker address).
  - **NATS JetStream** (`infra/transport/nats`, `-tags nats`) — a durable pull
    consumer over a file-backed stream (Kafka-parity durability: survives broker
    and consumer restarts, redelivers unacked messages, retains the log for
    replay). Maps the relay's subject/headers into the neutral envelope
    (aggregate_id from a header → the message key), so consumers are unchanged.
    The fresh-consumer start position honours the same contract as Kafka:
    `earliest` replays the retained log (`DeliverAll`), while `latest` and an
    omitted `StartFrom` both begin at new messages (`DeliverNew`) — so a caller
    that leaves the start position unset behaves identically on either transport.

### Changed

- **breaking** — **a message-transport build tag is now mandatory**, exactly like
  the relational-engine tag. A build must link one transport (`-tags kafka` or
  `-tags nats`), so a runnable build is e.g. `-tags 'postgres kafka'`. Building
  without a transport tag compiles but aborts at boot with "no transport linked"
  (the neutral bootstrap still compiles tagless, mirroring the no-engine case).
  Update build/run/test invocations and any tooling to add the transport tag.
- **breaking** — **the `kafka:` YAML block is renamed to a neutral `transport:`
  block.** `kafka.brokers` → `transport.endpoints` (Kafka/Redpanda bootstrap
  servers or NATS URL(s)), `kafka.syncGroupId` → `transport.syncGroup`,
  `kafka.syncWorkers` → `transport.syncWorkers`. The block is transport-neutral
  because the build tag — not the YAML — selects the adapter. Rename the block in
  every `microservice.<profile>.yaml`.

## [0.20.0] - 2026-07-07

### Added

- **Internal-plane security posture — `grpc.auth.mode` + producer-side
  connection recycling.** The gRPC listener gains its own posture,
  independent of the edge: `inherit` (default — the global `auth:` block
  governs, unchanged), `internal` (the trusted plane, the synchronous
  sibling of the integration-events posture: an anonymous call passes and
  rides the framework's existing anonymous-actor vocabulary; trust is the
  private network), and `mtls` (internal + required client certificates
  from `grpc.clientCAFile`; an anonymous call carries a synthetic Identity
  from the certificate SAN, so the audit trail names the calling service).
  On the internal plane a forwarded bearer is ATTRIBUTION, not an entry
  credential: signature/iss/aud verified locally (cached-JWKS, µs, NEVER
  the external validator — introspection belongs to the edge), and an
  expired-but-authentic token still passes carrying the user who entered
  the main door plus a synthetic `attribution: "stale-token"` claim on the
  audited identity; forged tokens reject. The tolerance never reaches the
  edge — separate validator constructions, proven side by side in tests
  and in `qa/grpc.sh`. `RequirePermission` gates pass on anonymous
  internal calls (the chain design is the flow designer's responsibility)
  and evaluate a forwarded user normally — including the mTLS certificate
  identity, which is ATTRIBUTION (it names the calling service for the
  audit trail), never an authorization subject: cert-attributed anonymous
  calls pass the gates like nil-identity ones, while still flowing into
  `ToCriteria` overlays (a `Restrict` keyed on permissions fires for a
  service identity exactly as the security suite proves). Also:
  `grpc.idleTimeoutSeconds`
  (default 120) — the producer-side load-balancing lever: kube-proxy
  balances per NEW connection, so recycling idle keep-alives forces
  callers to re-dial and redistributes traffic (scale-up pods join);
  120 sits above the Go client's 90s idle (no reuse race) and below
  NAT/conntrack floors (~350s, no zombie pipes). Consumers may fine-tune
  their side with the optional `grpcClient.services.<name>.pool`
  (`maxIdleConnsPerHost`, `connMaxLifetimeSeconds` background sweep).
  New surface: yaml `grpc.auth.mode`/`grpc.clientCAFile`/
  `grpc.idleTimeoutSeconds`, `grpcClient` `pool` block,
  `grpc.AuthPosture`/`Registry.SetAuthPosture`/`WithClientCertIdentity`,
  `authcore.ValidateAttribution`/`AttributionResult`.

- **Shared read-side proto components — `omnicore/v1/query.proto`, converted
  for you.** List RPCs compose the framework contract (`web/grpc/proto`,
  generated Go in `web/grpc/pb`): `PageRequest`
  (after/before/limit/only_total/include_archived), `SortField`,
  `google.protobuf.FieldMask read_mask` (the `?fields=` sibling), the typed
  operator wrappers `StringFilter` (full 12-operator REST vocabulary),
  `Int64Filter`/`DoubleFilter`/`TimestampFilter` (eq/ne/in/nin/gt/gte/lt/lte),
  `BoolFilter` (eq/ne) — repeated conditions per field AND-combine
  (MultiClause) — and, on the response, the mirror envelope: exactly one
  repeated items message + the NEW `PageInfo`
  (total/next_cursor/prev_cursor), located BY TYPE. `QueryWithParams`
  discovers the components on the descriptors at boot and assembles the
  INPUT `queries.ReadCriteria` (Go-field-path keys) itself: filters bind to
  the SAME Request DTO the REST list consumes and **inherit its `filter:`
  tag operator allowlist on the wire** (an operator outside the tag rejects
  as SchemaViolation, the REST 400's twin; a proto filter with no tagged
  DTO leaf aborts boot); `read_mask`/`sort` speak WIRE names (the item
  message's proto fields — FieldMask's canonical snake_case JSON form),
  resolved against the projector's Response DTO — an undeclared path fails
  before the reader, where an unresolved spelling would pass through as a
  physical column and bypass `ToCriteria` overlays such as `Restrict`
  (hardening found and locked by the gRPC security suite). The criteria
  then reaches the DTO's `ToQuery(criteria)` — the same one-liner REST
  calls — and the query type's `ToCriteria(ctx)` overlays are untouched.
  Emission delegates to the NEW `queryschema.ApplyFilterValues` (the
  list-level core of the shared REST/OpenAPI/GraphQL emitter;
  `ApplyFilterParam` now delegates, behavior-preserving), so operator
  semantics are unit-locked byte-identical across wires — and proto's
  `repeated values` remove the query-string comma-in-value limitation on
  the gRPC plane. Timestamps convert to the RFC3339 string form the REST
  string-leaf coercion produces. `grpc.NewCriteria` / `CriteriaBuilder`
  stay public as the MountRaw path's companion (a hand-rolled list reuses
  the same emitter, never re-implements operators). New surface:
  `web/grpc/pb` (generated, incl. `PageInfo`), `grpc.NewCriteria` /
  `CriteriaBuilder`, `queryschema.ApplyFilterValues`.

- **`infra/grpcclient` — the outbound gRPC/Connect toolbox.** The sibling of
  `infra/httpclient` for the gRPC plane: one `Client` built at boot from the
  yaml `grpcClient:` block (exposed on `Deps.GRPCClient`), per-service
  connect interceptor chain — correlation (threadID/X-Request-ID from the
  AppContext) → tracing (client span + W3C inject, `httpclient` instrument)
  → auth (`forward` re-sends the caller's bearer, failing loudly when
  absent; `static`) → idempotency (UUIDv7, configurable header, stable
  across retry attempts) → per-service default deadline → logging → retry
  (Connect-code triggers, default `["unavailable"]`, plus transport dial
  errors) → circuit breaker (per service+procedure). Typed entrypoint
  `grpcclient.For(client, "users", usersv1connect.NewUsersServiceClient)`.
  Deliberate gaps vs the HTTP chain (documented): cache and HMAC signing;
  oauth2 providers follow once their acquisition core is extracted. New
  surface: `infra/grpcclient` (`New`, `Client`, `For`, `Config` family,
  `WithClientTracing`, `WithTransport`), `bootstrap.Deps.GRPCClient`, yaml
  `grpcClient:` block.

- **`infra/resilience` — the transport-neutral resilience cores.** The
  circuit-breaker state machine, the retry backoff curves (constant /
  linear / exponential / exponential-jitter + jitter source + context-aware
  sleep) and the UUIDv7 idempotency-key generator extracted from
  `infra/httpclient` (behavior-preserving; the httpclient keeps its
  historical seams as thin delegations) and shared with `infra/grpcclient`
  — one implementation, two transports, semantics that cannot drift.

- **gRPC attachment — `reg.Register` + the constructor family over the
  REST DTO seats.** `web/grpc` attaches one declarative `Procedure` per RPC
  with the REST/GraphQL operation names — `CommandWithBody` (create),
  `CommandWithBodyID` (body + id; `SetPathID` injected after `ToCommand`),
  `CommandByID` (id only; request = the `id` field, response = an EMPTY
  message, both enforced at boot — the 204 sibling), `QueryWithParams`
  (list) and `QueryByID` (view document) — and each constructor consumes
  EXACTLY the REST Spec ingredients: the same Request DTO
  (`ToCommand`/`ToQuery` seat), the same `Response{}.FromResult` /
  `fwresponses.AutoFromDoc[Response]`; the pb message types ride as
  explicit type parameters and the id extractor is the generated getter as
  a method expression. The framework crosses pb ↔ DTO mechanically: a
  bridge plan compiles at `Register` (case/underscore-insensitive name
  matching with `json:`/`query:` tags honored, nested/repeated recursion,
  proto3 `optional` presence ↔ DTO pointers, `google.protobuf.Timestamp` ↔
  `time.Time`); an unmatched field ABORTS BOOT (`grpc.Alias("wire_field",
  "GoField")` declares the exceptional pairing) — semantic transformation
  stays in `ToCommand`/`ToQuery`/`FromResult`, and a shape that cannot
  mirror DTOs is a `MountRaw` procedure. Request-side bridge failures
  reject as SchemaViolation (the REST body-parse 400's twin); response-side
  ones are an opaque INTERNAL. ONE attachment API: every constructor
  accepts any `pipeline.Handler`. Includes `grpc.RequirePermission` — the
  Layer-1 permission gate, twin of the REST/GraphQL options: enforced via
  `Identity.HasPermission` under `auth.authorization.enabled`
  (`Registry.EnableAuthorization`, wired by bootstrap), inert otherwise;
  rejection = PERMISSION_DENIED with the canonical
  `MissingPermissionNotification` envelope. `Strict` remains the FullBody
  option on the same variadic seat.

- **gRPC transport surface — `web/grpc`, served with Connect.** The fourth
  consumer of the application-layer handlers: the wrapper family —
  `CommandWithBody` / `CommandWithBodyID` / `CommandByID`
  / `QueryWithParams` / `QueryByID`, the SAME vocabulary as the
  REST and GraphQL surfaces — adapts any `pipeline.Handler` to a generated
  Connect service method over the REST DTO seats (`ToCommand` /
  `ToQuery` / `FromResult`, bridged mechanically by the framework), so
  REST, GraphQL, export and gRPC dispatch to the same handler instances. One `net/http` endpoint on a dedicated
  listener (yaml `grpc:` block — `addr` default `:9090`,
  `certFile`/`keyFile`, `reflection`, `requestTimeoutSeconds`,
  `publicProcedures`) speaks the gRPC, gRPC-Web and Connect protocols; h2c
  without TLS. Registration follows the GraphQL precedent: `Wiring.GRPC`
  carries a `grpc.Registry` built with `grpc.New(deps.Pipeline)`; bootstrap
  injects policy and wires the listener into the coordinated shutdown drain
  (stage `grpc`). Server interceptors: recovery, AppContext
  (X-Request-ID/threadID, Accept-Language, protocol deadline as the
  cancellation parent, optional W3C server span under the `http` tracing
  instrument), and auth. Failures carry the notification envelope in
  `google.rpc.Status.details` (`ErrorInfo.reason` = NotificationKey +
  `BadRequest` field violations, translated) with the Semantic→code table
  documented in the new manual section. `grpc.Strict(fields...)` is the
  FullBody sibling over proto3 `optional` presence (protoreflect). New
  surface: `web/grpc` (`New`, `Registry.Register`, the five procedure constructors,
  `Strict`, `RequirePermission`, `AuthPolicy`, `ErrorFromNotifications`,
  `AppContextFrom`),
  `bootstrap.Wiring.GRPC`, yaml `grpc:` block. Dependencies:
  `connectrpc.com/connect`, `connectrpc.com/grpcreflect`.

- **`web/authcore` — the transport-neutral JWT validation core.** Extracted
  from `web.AuthMiddleware` (behavior-preserving): key sourcing (JWKS/PEM),
  token parsing and claim pinning, the external revocation seam
  (`TokenChecker`) and Identity construction now live in one package
  consumed by two thin shells — the Fiber middleware and the gRPC auth
  interceptor. New surface: `web/authcore` (`Options`, `Validator`, `New`,
  `ValidateAuthorization`, `ValidateToken`, `Failure`/`Error`,
  `ExtractBearerToken`, `BuildIdentity`, `BuildKeyfunc`,
  `ParsePublicKeyPEM`, `TokenChecker`) and `web.NewAuthCoreValidator`.

### Changed

- **Breaking: wrapper symmetry — one deterministic vocabulary across REST,
  GraphQL, gRPC and the pipeline/queries contracts.** The rule is now
  uniform on every transport: the constructor name states the route shape
  (`CommandWithBody`, `CommandWithBodyID`, `CommandByID`,
  `QueryWithParams`, `QueryByID`), the base to embed is the constructor
  name plus the `Base` suffix, and the generic constraint carries the same
  name as the constructor. Dry rename — no behavior, envelope, pipeline or
  wire change anywhere.
  - **REST loses the `Handle` prefix**: `HandleCommandWithBody` →
    `web.CommandWithBody`, `HandleCommandWithBodyID` →
    `web.CommandWithBodyID`, `HandleCommandByID` → `web.CommandByID`,
    `HandleQueryWithParams` → `web.QueryWithParams`, `HandleQueryByID` →
    `web.QueryByID`, with all `…Spec` siblings renamed the same way; the
    tabular exports follow the same pattern: `HandleQueryExport` →
    `web.QueryExport`, `HandleQueryAsCSV` → `web.QueryAsCSV`,
    `HandleQueryAsXLSX` → `web.QueryAsXLSX` (+ their `…Spec` siblings).
    REST now spells the family exactly like `web/grpc` — same names,
    different package.
  - **Write-side contracts (`application/pipeline`)**:
    `pipeline.CommandWithID` → `pipeline.CommandByID` and
    `pipeline.CommandBaseWithID` → `pipeline.CommandByIDBase`. New
    same-shape aliases complete the deterministic rule:
    `pipeline.CommandWithBody` (= `Command`), `pipeline.CommandWithBodyID`
    (= `CommandByID`), `pipeline.CommandWithBodyBase` (= `CommandBase`) and
    `pipeline.CommandWithBodyIDBase` (= `CommandByIDBase`) — "WithBody"
    describes the wire input (the Request DTO's job), "ByID" describes how
    the command is addressed, which is why the two id-carrying shapes share
    one method set. The auto contracts follow: `UpdateCommand` /
    `PartialUpdateCommand` embed `CommandWithBodyID`; `ArchiveCommand` /
    `UnarchiveCommand` / `DeleteCommand` embed `CommandByID`.
  - **Read-side contracts (`application/queries`)**:
    `queries.FindByParamsQuery` → `queries.QueryWithParams`,
    `queries.FindByIDQuery` → `queries.QueryByID` (the intermediate
    `queries.QueryWithID` is absorbed into it and removed),
    `queries.QueryBaseWithID` → `queries.QueryByIDBase`, and the getter
    `GetID() domain.ID` → `PathID() domain.ID` — the exact
    `SetPathID`/`PathID` pair `pipeline.CommandByID` carries on the write
    side. New alias `queries.QueryWithParamsBase` (= `pipeline.QueryBase`)
    completes the rule for list queries.
  - **`grpc.QueryWithParams` tightens to the shared contract**: the query
    must satisfy `queries.QueryWithParams` and the handler returns
    `queries.Page` (previously any `pipeline.Query` and a free `TResult`) —
    the same constraint REST and GraphQL always demanded, so one
    application query serves every transport.
  - **Migration** (mechanical, word-boundary): `s/\bHandleCommand/Command/`,
    `s/\bHandleQuery/Query/`, `s/\bCommandBaseWithID\b/CommandByIDBase/`,
    `s/\bCommandWithID\b/CommandByID/`,
    `s/\bQueryBaseWithID\b/QueryByIDBase/`,
    `s/\bFindByParamsQuery\b/QueryWithParams/`,
    `s/\bFindByIDQuery\b/QueryByID/`, and `GetID()` → `PathID()` on query
    types only (entity `GetID` is untouched). Handlers listing queries on
    gRPC swap the projector input to `queries.Page`. GraphQL constructor
    names are unchanged; by design there is still no `graphql.QueryByID` —
    by-id reads on GraphQL are a `where` filter on the connection.

- **Breaking: the conflict vocabulary is split — new `SemanticStateConflict`,
  and `EntityIsNotActiveNotification` is reclassified to it.**
  `domain.SemanticStateConflict` joins the semantic enum for "the entity or
  system is not in the state the operation requires" — a precondition
  failure, distinct from `SemanticConflict`, which now carries the duplicate
  meaning only (`EntityAlreadyAddedNotification` stays). Both map to HTTP
  409, so REST status behavior is unchanged; the split exists because
  transports with a richer status vocabulary map the two flavors to
  different codes (the upcoming gRPC surface: `ALREADY_EXISTS` vs
  `FAILED_PRECONDITION`). **Wire-visible change**: the response envelope's
  `semantic` string for `EntityIsNotActiveNotification` (and any consumer
  notification reclassified the same way) changes from `"Conflict"` to
  `"StateConflict"`. Migration: consumers that switch on the envelope's
  `semantic` value must add the `"StateConflict"` case; consumers keying on
  HTTP status or `NotificationKey` are unaffected.

- **Graceful shutdown now narrates each drain stage.** The coordinated drain
  previously logged only its two bookends (`shutdown signal received, draining...`
  and `shutdown complete`) plus a warning on timeout, so an operator watching a
  slow shutdown had no idea which component was being stopped or how long it
  took. Every stage that runs through the drain (`http`, `integration`,
  `upstream[i]`, `sync`) and the sequential `tracing` / `onShutdown` steps now
  emit an `INFO draining stage=<name>` line on entry and an `INFO drained
  stage=<name> elapsed=<duration>` line on success; the failure path logs
  `WARN drain failed stage=<name> err=<…> elapsed=<duration>` (renamed from the
  previous `drain timeout`, since not every drain error is a timeout).

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
