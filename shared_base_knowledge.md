# SharedBase — knowledge note

How a **SharedBase** (party-role / Modelagem 2) behaves today, end to end: what the
write side records in the outbox, how the read side projects it, and — the point that
trips people up — **what a `SharedBaseView` role segment actually holds**.

> **The one thing to remember:** a `SharedBaseView` role segment is **ONE
> specialization, never a list**. It carries the identity's **current** role row — the
> **active** one, or, if none is active, the **most recently deactivated (archived)**
> one. This is **domain-shaped, not relational-shaped**, and it is the same whether the
> role links by a separate FK or shares the base's PK.

---

## 1. The model

A shared identity lives in **two tables**:

- the **base** (`pessoa`) — the deduplicated identity + its shared fields (name, document…);
- the **role table** (`aluno`, `funcionario`) — the role-private fields.

The base `id` is **deterministic**: `UUIDv5(natural_key)`. App and infra derive the same
id with no read-back. A role is *an identity playing a role* — the person **is** a
student — not *an identity that owns a collection*.

Two link models (the developer's choice):

| Link model | Declaration | Rows per identity in the role table |
|---|---|---|
| **shared-PK** | `SharedBase(person, "id")` — the role's FK **is** its PK | **exactly 1** (the PK caps it; an archived row blocks re-insert on the same id) |
| **separate-FK** | `SharedBase(person, "person_id")` — a distinct FK column | depends on the DDL uniqueness index: a **full** `UNIQUE(fk)` → 0..1; an **active-only** unique index → **0..1 active + 0..N archived remnants** |

The **active-only separate-FK** model is the only one that admits *multiplicity* — an
archived remnant sitting next to a fresh active row. That multiplicity is exactly what
makes the read side interesting (§4).

---

## 2. The write side — what lands in the outbox

A role write (`POST /alunos`, `PUT /alunos/{id}`, archive, …) runs in **one relational
transaction** (`insertWithBase` / `updateWithBase`):

1. **UPSERT the base** under the base row's lock. The base carries a framework-managed
   `revision BIGINT` column: `revision = revision + 1` on every touch, initialized to 1
   on creation. Because the UPDATE takes the row lock, concurrent role writes of the
   same identity **serialize in real commit order**; the resulting value is the base's
   commit-order token (`base_revision`).
2. Write the role row (its own `revision` too).
3. Children (role children FK→role, base-native children FK→base), by `OperationOf`.
4. Lifecycle convergence (reactivate the base if it was archived).
5. **Exactly ONE outbox row**, keyed by the **role table**, with a self-sufficient payload.
6. Audit row, hooks, COMMIT.

### The outbox row

One row per write, keyed by the **role** table (not the base):

| outbox column | value |
|---|---|
| `aggregate_type` | `aluno` (the role table) |
| `event_type` | `INSERTED` / `UPDATED` / `ARCHIVED` / `UNARCHIVED` / `DELETED` |
| `aggregate_id` | the role row id |
| `payload` | flat, self-sufficient JSON (below) |

```jsonc
{
  // scalars, column-keyed, flat at the top:
  "enrollment": "2026-1",      // role-private fields
  "person_id":  "p1",          // FK to the base (separate-FK model)
  "name":       "Ana",         // shared-base BUSINESS fields, flattened here too
  "updated_at": "2026-...Z",   // managed timestamps (app-clock authored)

  "_ids": {                    // structural identity
    "id":            "a1",
    "revision":      4,        // the role row's own commit-order token
    "base_id":       "p1",     // deterministic base id
    "base_revision": 7         // the base's token (serialized under the row lock)
  },

  "_children":      { "Endereco": [ { "_op": "insert", "id": "e1", ... } ] },
  "_base_children": { "Telefone": [ { "_op": "update", "id": "t9", ... } ] }
}
```

Key facts:

- The base's **business fields travel flattened in the payload** alongside the role's —
  the consumer never re-reads the base for them.
- `_ids.base_id` + `_ids.base_revision` are what let the read side propagate shared
  changes to the *other* roles **without touching the database**.
- **There is no separate base-table outbox row** for INSERTED/UPDATED. The only
  base-table (`aggregate_type = pessoa`) row still emitted is the **orphan-purge
  `DELETED`** (last role gone → identity purged), carrying `_ids.base_purged = true`.

---

## 3. The read side — one row, three effects

`SyncEngine.process` parses the payload **once** and routes. For a role event carrying
`_ids.base_id`:

1. **The role's own entity view** (`View` rooted at `aluno`, no external embed) —
   **payload-direct**: the document is written straight from the payload as one atomic
   Mongo aggregation-pipeline update. No relational read.
2. **Shared-identity fan-out to the OTHER roles** — **payload-direct**: takes only the
   base fields from the payload, finds the other roles' documents by
   `FindIDsByField(fk, base_id)`, and applies just the shared fields — so a `name`
   changed through `aluno` reaches the read model of `funcionario`. No relational read.
   Writes with `upsert=false` (never resurrects a concurrently-deleted role).
3. **The `SharedBaseView` document** (the all-in-one "person" doc) — **recomposed from
   the relational source** (`recomposeBaseRooted` → `composer.Compose`). The `base_id`
   still comes from the payload (`_ids.base_id`, no DB); only the aggregated document is
   re-read. **This is the one deliberate re-read** — see §4 for why.

Last-writer-wins is enforced on the Mongo side by revision guards: own fields behind
`_revision`, base fields behind `_base_revision` (the row-lock's commit order replayed
on the read side), base-children elements behind a per-element `_rev`. A stale replay
(zombie consumer after a partition handoff) is rejected, not applied.

---

## 4. Why the `SharedBaseView` re-reads (the crux)

The `SharedBaseView` document is keyed by the **base id** (`_id = "p1"`), and each role
segment holds **exactly one** sub-document — chosen `fetchRoleRow`-style:

```
1. the ACTIVE row:            SELECT ... FROM aluno WHERE person_id = 'p1' AND deleted_at IS NULL LIMIT 1
2. else the latest remnant:   ... AND deleted_at IS NOT NULL ORDER BY deleted_at DESC LIMIT 1
3. else null
```

There is **no document keyed by a role id** — a role row is only a *candidate* to fill
the base document's single slot. Filling the slot is a query **over the set of role rows
of that identity**, filtered by `deleted_at`. A single event's payload describes **one
row**; it cannot answer "is there an active sibling that should win the slot instead?"

### The breaking case (why payload-direct would be silently wrong)

Under active-only separate-FK, `pessoa p1` has `aluno a1` (active) and `aluno a2`
(archived). The person document's `aluno` slot shows **a1**.

- An `UPDATED(a2)` arrives. "Just upsert a2 into the slot" → the slot now shows the
  archived `a2`. **No error — but wrong:** the person now advertises a dead remnant as
  its current specialization. The `a2` event has no way to know `a1` exists and is
  active.
- Correct: re-run `WHERE person_id='p1' AND deleted_at IS NULL` (which *sees* `a1`) →
  the slot stays `a1`.

That cross-row "which row wins the slot" decision is why this — and only this —
document is recomposed from the SoR.

> **Note:** in the **shared-PK** model there is never more than one role row, so there
> is no slot contention; a payload-direct projection *would* be correct there. The
> framework recomposes **all** `SharedBaseView`s uniformly today (it does not branch on
> link model). Making shared-PK `SharedBaseView`s payload-direct is a legitimate future
> optimization, not a correctness fix.

---

## 5. Getting a LIST out of a SharedBase — the misconception, and what actually works

People reach for `SharedBase` expecting "a person **plus a list** of N role records"
(e.g. a person with 20 student enrollments). **That is not what party-role means.** A
`SharedBaseView` gives you `person + one current role slot` — never a list. Archived
remnants are role *history*, surfaced through the per-role views with `?includeArchived`,
not enumerated in the person document.

If you genuinely want `person + list of N role rows`, there are two honest paths — and
only one of them keeps the existing SharedBase + Role write model.

### Keeping SharedBase + Role unchanged → a read-time ComposedView (the usual answer)

Add **read-side-only** artifacts, no write change: a plain `query.View` over the role
table (a role is allowed its own view — the reference service ships `query.View("users")`
and `query.View("employees")` next to the SharedBaseView), then a `ComposedView` that
`LinkMany`s it onto the person by the role's FK:

```go
query.ComposedView("person_with_students").
    Primary(PersonView()).                    // the SharedBaseView you already have
    LinkMany("students", query.JoinView(StudentView()).FK("person_id").As("Students"))
```

The join runs at read time; the list stays fresh via the role's own events. Only
meaningful on the **separate-FK** model (the only one that admits N role rows and has an
FK column to join on — shared-PK caps the role table at one row).

> **Mind the base duplication.** A role view whose schema declares `.SharedBase(...)`
> merges the base's shared fields flat into every role document (`mergeSharedBase`). So if
> the leg reuses the role schema, `name`/`document` appear once at the person root AND
> again inside every list item. To avoid it, give the leg a **lean schema without
> `.SharedBase`** (only PK + Revision + the FK + role-private fields — the base merge is a
> no-op without a shared-base reference), or trim on the wire with `?fields=` / the
> Response DTO.

> **Do NOT try to fake a list with a `.Child()` view over the role table.** A
> CDC-materialized view only recomposes on events for its **root / shared-base / role**
> tables — never on plain `.Child` tables (`buildViewIndex` indexes only the root;
> `NewSyncEngine` subscribes only to root + base + roles). A role write emits a *role*
> event and the base upsert emits no base event, so a `persons`-rooted view with a
> `students` child would **never update the list**. It does not work.

### Only if you are willing to change the write model → aggregate children

Model the role as an **aggregate child** of the identity (the person OWNS the rows, one
aggregate write, carried in the payload's `_children`). The view then projects a nested
`"students": [ ... ]` array, payload-direct, no re-read. But the child is then no longer
an independently-written role — this is a **write-model change**, not applicable when you
keep SharedBase + Role.

`SharedBase` is the wrong tool for "owns many"; it is the right tool for "is a".

---

## 6. Decision cheat-sheet

| You want… | Model it as… |
|---|---|
| Identity **is** a role (0..1), locked to one row, no remnants | `SharedBase` **shared-PK** (`SharedBase(base, "id")`) |
| Identity **is** a role, but archive-and-reopen a fresh one | `SharedBase` **separate-FK** + active-only unique index (accepts the recompose) |
| Identity **has** N role rows (a list), model unchanged | read-time **ComposedView** + a plain role view — mind the base-duplication caveat |
| Identity **owns** N records, willing to change the write model | **aggregate children** (payload-direct nested array) |

**In one line:** a `SharedBaseView` role segment always shows the identity's *current*
specialization — the active row, or the most recently archived one — following the
domain (party-role) rather than the raw relational rows, in every link model.
