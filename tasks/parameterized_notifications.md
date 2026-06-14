# Parameterized notifications — Design

## Status

**Pending maintainer approval.** This document captures the agreed design after a planning round with the maintainer. The four open trade-offs were closed via `AskUserQuestion` before this file was written; the decisions are inlined under each section. No framework code has been changed yet — implementation waits for explicit go-ahead.

## Problem statement

Today every translation entry is a static string. A `Notification` carries no runtime data into the translated message — the catalog string for `RequiredFieldNotification` is the same byte-for-byte regardless of which field triggered it. That works for marker-style notifications ("required field is missing", "record not found") but breaks down for any rule whose message must show a runtime configuration value.

Canonical motivating example (from the maintainer, banking domain): the "nosso número" field has a maximum length that varies per bank (7 positions, 9 positions, 10 positions). The rule emits the same notification key `NossoNumeroMaxLengthExceededNotification` across every bank — the user-facing message has to read "excede o tamanho máximo de **7** posições" or "**9** posições" depending on the per-bank config. Today the only options are:

- **Create one notification type per length** (`NossoNumeroMax7Notification`, `NossoNumeroMax9Notification`, …) — explodes the catalog, leaks config into types, intractable.
- **Concatenate the value into the field name** or `FieldValue` — the message stays generic ("excede o tamanho máximo"); UX loses the actual limit.
- **Skip translation entirely** and build the message at emission with `fmt.Sprintf` — bypasses i18n, the consumer service ends up emitting messages in one language.

All three are bad enough that the framework needs a first-class answer.

## What the reference Kotlin framework already does

The maintainer's prior framework (`esales-kotlin-framework`) solved this with three pieces:

1. **`Notification` interface declares `getNonTranslatableVariables(): Map<String, String>?`** with a default `null` implementation. Concrete types override when they have variables.
2. **`Translator.getTranslationByKey(key, nonTranslatableVariables)`** resolves the message then iterates the map calling `message.replace(literalKey, value)` per entry — the map keys are the literal placeholders the translation strings carry (e.g. `"{maxLength}"`).
3. **`Pipeline.Handler.getNonTranslatableVariables(request): Map<String, String>?`** with a default `null` — handler-level vars propagated by the Pipeline when translating the wrapping NotificationContext label.

References in `/Users/claudio/Downloads/esales-kotlin-framework`:
- `lib/src/main/kotlin/.../translation/Translator.kt:27-36` — `getTranslationByKey` + replace loop.
- `lib/src/main/kotlin/.../translation/TranslationExtensions.kt:13-22` — context DTO assembly: notification vars on each message, handler vars on the context label.
- `lib/src/main/kotlin/.../domain/notifications/Notification.kt:3-5` — interface with default `null`.
- `lib/src/main/kotlin/.../application/pipeline/Handler.kt:5` — handler-level optional method.
- `lib/src/main/kotlin/.../application/pipeline/Pipeline.kt:34,44` — propagation through tryExecute.

## What changes in OmniCore

OmniCore reuses the *idea* and improves the ergonomics. Three differences over the Kotlin reference, all driven by Go idiom + maintainer decisions made during the design call:

1. **No interface method on `Notification`.** Go interfaces have no default impl; forcing every notification type (8 kernel + N per consumer) to declare an empty method just to opt out is hostile. Instead, the framework reflects directly over **`tvar`-tagged exported fields** of the notification struct. Notifications without `tvar` tags pay no resolution cost. **Decision 2.** *(Side note: the planning round also considered an opt-in `Parameterized` interface with a manual map; the maintainer picked the pure-tag path for the zero-boilerplate guarantee. The optional interface stays an open design escape hatch for cases where reflection-via-tag does not fit — re-evaluated if such a case actually appears.)*
2. **Placeholder syntax is `{name}` with a whitelist `[A-Za-z_][A-Za-z0-9_]*`.** Same visual shape as Kotlin's literal `{maxLength}`, but a single scanner pass over the string avoids accidental matches inside JSON snippets or other languages embedded in the translation. **Decision 1.**
3. **Placeholder present in the string but missing in the var map is logged once at `slog.Warn` per `(lang, key, placeholder)` tuple.** The literal `{maxLength}` survives in the output (so the bug surfaces in UI) AND the operator sees the warn-once in logs (so the catalog drift surfaces in observability). Kotlin's silent behavior loses the operator signal; this design keeps the visibility without spamming production. **Decision 4.**

The wrapping `NotificationContext` label gets its own vars via a new `SetVars` method on the context — the maintainer chose this over replicating the Kotlin Handler-level pattern, because per-context vars are localized to the emit site and don't couple Pipeline to request shape. **Decision 3.**

## Design

### 1. Catalog convention — `{name}` placeholders

Catalog entries opt into placeholders by writing them directly in the string. Existing entries (without placeholders) are unaffected — the scanner is a no-op on strings that don't carry `{...}`.

```go
// application/translations/ptbr.go (consumer side, example)
"NossoNumeroMaxLengthExceededNotification": "Nosso número excede o tamanho máximo de {maxLength} posições.",

// English mirror — same key, same placeholder, translator-friendly word order.
"NossoNumeroMaxLengthExceededNotification": "Nosso número exceeds the maximum length of {maxLength} positions.",
```

Whitelist: a placeholder is `\{[A-Za-z_][A-Za-z0-9_]*\}`. Literal `{` outside the whitelist passes through untouched. No escape syntax in this round (no `{{` → `{`); if a real catalog needs a literal `{maxLength}` in the output, that becomes a follow-up — defer the complexity until a real case shows up.

### 2. Notification declaration — `tvar` struct tag

```go
// consumer side — application/notifications/nosso_numero.go (example)
type NossoNumeroMaxLengthExceededNotification struct {
    domain.DomainNotificationBase
    MaxLength int `tvar:"maxLength"`
}
```

Rules:

- **Tag name = placeholder name.** A field tagged `tvar:"maxLength"` populates the `{maxLength}` placeholder. The struct field name is irrelevant; only the tag value matters.
- **Field type may be anything `fmt.Sprint` can render** — string, int / uint / float of any width, bool, `uuid.UUID`, `domain.ID`, `time.Time`. Pointers are dereferenced; nil pointers render as empty string.
- **Multiple fields, multiple placeholders.** A notification carrying `{minLength}` and `{maxLength}` tags both fields independently.
- **Unexported fields are skipped** — only exported fields with `tvar` participate. Notifications that want to keep the value private to the type construct the map via an explicit helper method (escape hatch — see section 7).
- **No tag = no participation.** `RequiredFieldNotification`, `RecordNotFoundNotification`, etc. stay untouched; the framework's reflection returns nil for them.

Internal cache: the framework caches the (field index, tag name, kind) plan per `reflect.Type` in a module-level `sync.Map`. First emission of a given notification type pays one reflection walk; subsequent emissions are direct field reads.

### 3. NotificationMessage carries per-emit override

```go
// domain/notification.go — new field, additive
type NotificationMessage struct {
    Path         []PathSegment
    Override     string
    FieldName    string
    FieldValue   string
    FuncName     string
    Err          error
    Notification Notification
    Vars         map[string]string  // NEW — per-emit vars, merged on top of tag-derived ones
}
```

Use case: the same notification type carries a default set of vars from its tags, but the call site wants to add one more value (e.g. `{context}` derived from the request). The Rules DSL gains a sibling helper:

```go
// domain/rules.go — new helper alongside AddNotification
func (r *Rules) AddNotificationWithVars(name string, n Notification, vars map[string]string)
```

The framework's full extraction is:

```
notif vars (from tvar)  +  per-message Vars (call-site override; wins on key collision)
```

### 4. NotificationContext carries label-scoped vars

```go
// domain/notification.go — new field on NotificationContext
type NotificationContext struct {
    context     string
    contextVars map[string]string  // NEW
    messages    []NotificationMessage
    parent      *NotificationContext
    prefix      []PathSegment
}

func (c *NotificationContext) SetVars(vars map[string]string)
```

Used when the wrapping context label itself is parameterized:

```go
ctx := domain.NewNotificationContext("UserOf{tenantId}")
ctx.SetVars(map[string]string{"tenantId": "acme"})
// the context label translation will render as e.g. "UserOf acme" (per the catalog entry)
```

`SetVars` replaces the previous map; semantic is "set this set of vars", not "merge". Call site can call multiple times if needed.

### 5. Translator — new `Render`, `Get` stays compat

```go
// application/translation/translator.go — new method
func (t *Translator) Render(lang configuration.Language, key string, vars map[string]string) string
```

Behavior:

1. Resolve the message via the same path as `Get` — same module map, same fallback. **Missing translation key** continues to return `key` itself (existing fallback in `GetOr`/`Render` chain), and logs `slog.Warn("translation.key.missing", "lang", lang, "key", key)` once per `(lang, key)` tuple via `sync.Map`-based dedup.
2. Scan the resolved string for `{name}` placeholders matching the whitelist.
3. For each placeholder:
   - If `vars[name]` is set → replace the literal `{name}` substring with the value.
   - If absent → leave `{name}` as-is in the output AND `slog.Warn("translation.var.missing", "lang", lang, "key", key, "placeholder", name)` once per `(lang, key, placeholder)` tuple via the same dedup primitive.
4. `vars == nil` is treated as "no vars" — same as an empty map. No warn fires for missing placeholders in this case, because the caller signaled "I have no vars to provide". (Operators tracking notif-without-tvar-but-catalog-has-placeholder cases use the existing audit of catalog entries — see Test plan.)

Package-level helper for the "I already have the translated string" case:

```go
// application/translation/translator.go — new package-level function
func Interpolate(s string, vars map[string]string) string
```

Same scanner, same fallback semantics — no logging (because there is no `lang` / `key` context). For programmatic interpolation outside the translator.

`Get(lang, key)` continues to return the raw string. Callers who explicitly want no interpolation (today's audit field rendering of raw column names, etc.) stay on `Get`.

### 6. Pipeline integration — automatic resolution

`pipeline.Run`/`Dispatch` already translate notifications when converting `*DomainResult` / `NotificationCarrier` errors into `Result.Failure` DTOs. The change is one helper inside `application/notifications/convert.go`:

```go
func resolveMessageVars(msg domain.NotificationMessage) map[string]string {
    notifVars := domain.ExtractVarsFromTags(msg.Notification) // reflection helper, internal
    if len(notifVars) == 0 && len(msg.Vars) == 0 {
        return nil
    }
    out := make(map[string]string, len(notifVars)+len(msg.Vars))
    for k, v := range notifVars {
        out[k] = v
    }
    for k, v := range msg.Vars {
        out[k] = v
    }
    return out
}
```

Two `Get → Render` substitutions:

- **Context label** — `translator.Render(lang, ctx.Context(), ctx.contextVars)` instead of `translator.Get(lang, ctx.Context())`. ContextVars only — context-level scope.
- **Each message** — `translator.Render(lang, NotificationKey(msg.Notification), resolveMessageVars(msg))`. Merged notification + per-emit vars.

The internal helper `ExtractVarsFromTags` lives in `domain/notification.go` (package-internal cache + reflection). Surface for tests; not part of the public API.

### 7. Escape hatch for private-field vars

Some notifications may want to keep variable data private (e.g. a redaction-sensitive value that ends up only in the translated message, never in the wire envelope). Tag-based reflection only handles exported fields. For that case, the consumer declares a helper method:

```go
type SensitiveLimitNotification struct {
    domain.DomainNotificationBase
    threshold int  // unexported — won't appear via tag reflection
}

func (n SensitiveLimitNotification) TranslationVars() map[string]string {
    return map[string]string{"threshold": strconv.Itoa(n.threshold)}
}
```

`ExtractVarsFromTags` checks for an optional method `TranslationVars() map[string]string` via type assertion FIRST; if present, its output replaces (does not merge with) the tag-based extraction. This is the **only** convention the framework recognizes besides `tvar` — explicitly kept as an escape hatch, NOT a parallel canonical path. The expected ratio is ≥95% pure `tvar`, ≤5% `TranslationVars` method. Documented in `DOCS.html` as the rare-case opt-out.

### 8. Wire payload — vars stay server-side

`ErrorMessage` (wire envelope) carries today: `notificationKey`, `field`, `value`, `funcName`, `message`, `semantic`. The translated `message` after this change carries the substituted text, which is enough for the UX. **`vars` is NOT exposed as a separate field on the wire envelope.** Two reasons:

- Consumers' frontends that want to format on their own already have the `notificationKey` + `value` (the rejected input echo). Adding `vars` to the wire surface duplicates information without a real consumer.
- Defer until a concrete use case shows up. The internal `MessageDTO` (`application/notifications/convert.go`) carries the resolved vars only for the rendering step — they don't escape that boundary.

### 9. Audit / events

The audit pipeline (`infra/audit/event.go` + `echo.go` + `persister.go`) and the events publisher (`infra/events/slog_publisher.go`) both translate notification messages today via `Get`. They switch to `Render` so the audit row's `message` field and the slog echo's translated text carry the substituted value — consistent with what the wire response shows. No new field on `AuditEvent` (the `notificationKey` plus the rendered `message` cover audit's needs).

### 10. Backwards compatibility

- Notifications without `tvar` tags **and** without a `TranslationVars` method → zero behavioral change. Reflection returns nil; `Render` behaves identically to `Get`.
- Existing catalog entries (no placeholders) → zero behavioral change. The scanner finds no `{name}` matches and emits the string verbatim.
- Existing `Get` callers continue working. No deprecation in this round.
- The new `Vars` field on `NotificationMessage` defaults to nil; no existing constructor needs touching.
- The new `contextVars` on `NotificationContext` defaults to nil; existing constructors stay unchanged.

100% compat in source and on the wire.

## Files touched

| File | Change |
|---|---|
| `domain/notification.go` | + reflection cache + helper `ExtractVarsFromTags(n Notification) map[string]string` (package-internal); + `Vars map[string]string` field on `NotificationMessage`; + `contextVars` field + `SetVars(map[string]string)` method on `NotificationContext` |
| `domain/rules.go` | + `AddNotificationWithVars(name string, n Notification, vars map[string]string)` helper alongside `AddNotification` |
| `application/translation/translator.go` | + `Render(lang, key, vars) string` method; + package-level `Interpolate(s, vars) string`; placeholder scanner (whitelist `[A-Za-z_][A-Za-z0-9_]*`); warn-once `sync.Map` dedup for two cases (missing key, missing var) |
| `application/notifications/convert.go` | + `resolveMessageVars(msg)` helper; switch context label translation and per-message translation from `Get` to `Render` |
| `application/pipeline/pipeline.go` | wires through unchanged — `Run`/`Dispatch` already call `convert.go`; no direct edits |
| `web/error_handler.go`, `web/respond.go`, `web/from_notifications.go` | switch `Get` to `Render` where notification messages are emitted on the wire envelope; same `resolveMessageVars` helper |
| `infra/audit/persister.go`, `infra/audit/echo.go`, `infra/audit_builder.go` | switch `Get` to `Render` where audit messages are translated for the row / slog echo |
| `infra/events/slog_publisher.go` | switch `Get` to `Render` for the event title translation |
| `DOCS.html` | new section "Parameterized notifications" under the existing "Notification system" — covers tag syntax, escape hatch, catalog conventions, per-emit vars |
| `CLAUDE.md` | update the "Notification system" section to describe the tag-driven mechanism (current state, not "now supports"); update "Quick reference — where to add things" with a row for "Notification carrying runtime variables" |
| `CHANGELOG.md` | new entry under `[Unreleased] → Added` |
| `application/translation/translator_test.go` | new cases: Render basic, Render with missing key (logs once), Render with missing placeholder (logs once), Interpolate, repeat-warn dedup, scanner whitelist (`{1invalid}` is left as-is) |
| `domain/notification_test.go` (or new `notification_vars_test.go`) | new cases: ExtractVarsFromTags with exported fields, with `TranslationVars` method override, with both (method wins), with no participation (returns nil), with pointer fields (deref + nil-safe), cache hit on repeat |
| `application/notifications/convert_test.go` | new case: pipeline path renders vars from notification AND from per-message override (override wins) |

### Consumer-side updates

The reference consumer `omnicore-example-users` does NOT consume parameterized notifications today (none of its domain rules carry a runtime-shaped variable in the message). The chosen showcase is a **maximum length for `User.Name`** — kept as a **pure domain rule** of the User aggregate, not a per-request configuration. The limit lives in `domain/user.go` as the package-private constant `nameMaxLength = 100`; the application layer (commands/handlers) never references it. Scope of the consumer-side delta:

**Domain layer — `omnicore-example-users/domain/user.go`:**

- Declare the cap as a package-private constant alongside the entity it constrains:

  ```go
  // nameMaxLength is the User's hard cap on the Name field length — a pure
  // domain rule of THIS aggregate, not a configurable per-tenant value.
  // Acts as the runtime value the parameterized-notification mechanism
  // substitutes into the translated message via the NameMaxLengthExceeded
  // Notification's `tvar:"maxLength"` field. If a future requirement
  // demanded per-tenant variability, the rule would migrate from a
  // constant to a domain.Service lookup consulted inside BuildRules —
  // same notification type, same wire shape, only the source of the
  // value changes.
  const nameMaxLength = 100
  ```

- Extend `User.BuildRules` inside `IfInsertOrUpdate`, right after the existing `Name == ""` branch:

  ```go
  if u.Name == "" {
      r.AddNotification("Name", domain.RequiredFieldNotification{})
  } else if len(u.Name) > nameMaxLength {
      r.AddNotification("Name", NameMaxLengthExceededNotification{
          MaxLength: nameMaxLength,
      }, u.Name)
  }
  ```

**Domain layer — `omnicore-example-users/domain/notifications.go`:**

```go
// NameMaxLengthExceededNotification is the canonical parameterized-notification
// showcase of this example. The MaxLength field carries the value the domain
// emitted (today: the constant nameMaxLength); the framework's translation
// layer reflects the `tvar` tag and substitutes {maxLength} in the catalog
// string. Default Semantic (Validation → 422) — wire field carries the
// rejected input via the AddNotification value parameter, mirroring
// InvalidEmail/InvalidPhone shape.
type NameMaxLengthExceededNotification struct {
    domain.DomainNotificationBase
    MaxLength int `tvar:"maxLength"`
}
```

**No application-layer changes.** The rule is internal to the domain — `Insert/Update/Patch` Cmds (canonical + manual) stay untouched. The constant is invisible above `domain/`; the layering rule (`application/` may import `domain/`, never the inverse) is preserved without exception.

**Translation catalogs — all 7 modules in `application/translations/`:**

The example's English-only rule + canonical-example status requires updating all seven catalogs (PT-BR, ENG, ESP, FRA, DEU, ITA, NLD) — same posture as every existing notification in the consumer. Two kinds of entries land in this round:

1. **`NameMaxLengthExceededNotification`** — the parameterized notification showcase.
2. **`"User"`** — context-label translation, closing a pre-existing gap. The framework has always translated the `NotificationContext.context` string via the same Translator (`convert.go::ToContextDTOs` already calls `t.GetOr(lang, ctx.Context(), ctx.Context())` — the example just never declared a catalog entry for it, so the literal Go struct name reached the wire envelope unchanged). The implementation round closes this gap by declaring the entry alongside the new notification — proving the mechanism is alive and documenting the convention "the context name IS the entity struct name, translated through the same catalog as notifications".

Suggested wording per language:

```go
// Catalog entries — both keys per language module

// NameMaxLengthExceededNotification + "User" context
"NameMaxLengthExceededNotification": "Name exceeds the maximum allowed length of {maxLength} characters.",          // ENG
"User":                              "User",
"NameMaxLengthExceededNotification": "O nome excede o tamanho máximo permitido de {maxLength} caracteres.",         // PTBR
"User":                              "Usuário",
"NameMaxLengthExceededNotification": "El nombre supera la longitud máxima permitida de {maxLength} caracteres.",    // ESP
"User":                              "Usuario",
"NameMaxLengthExceededNotification": "Le nom dépasse la longueur maximale autorisée de {maxLength} caractères.",    // FRA
"User":                              "Utilisateur",
"NameMaxLengthExceededNotification": "Der Name überschreitet die maximal zulässige Länge von {maxLength} Zeichen.", // DEU
"User":                              "Benutzer",
"NameMaxLengthExceededNotification": "Il nome supera la lunghezza massima consentita di {maxLength} caratteri.",    // ITA
"User":                              "Utente",
"NameMaxLengthExceededNotification": "Naam overschrijdt de maximaal toegestane lengte van {maxLength} tekens.",     // NLD
"User":                              "Gebruiker",
```

Parametrized context labels (`SetVars` on `NotificationContext`) are framework-side and unit-tested there; the consumer does NOT declare a parameterized context label in this round — the User aggregate has no natural per-tenant / per-request label variation, and forcing one would be artificial. If a future aggregate (`Order` with `OrderOf{tenantId}`?) shows up the pattern is one line away.

**E2E coverage — `omnicore-example-users/qa/e2e.sh`:**

Add one case in the existing notification-coverage section: POST `/users` with a name longer than 100 chars; assert HTTP 422 with `errors[0].messages[0].notificationKey == "NameMaxLengthExceededNotification"` AND `errors[0].messages[0].message` contains the literal `"100"` (the substituted value), NOT the literal `"{maxLength}"` (the placeholder). Also assert the EN variant by passing `Accept-Language: en-US` so the rendered message starts with "Name exceeds the maximum…". The dual-language assertion covers the cross-locale guarantee in one case.

**Why this point of insertion:**

- Touches the same `IfInsertOrUpdate` block that already validates email and phone — same shape, same emission helper, same `AddNotification(name, n, value)` convention.
- The rule + the limit both live inside `domain/` — the application layer (`commands/`, `handlers/`) imports `domain/` to consume the entity, never to thread limits back into it. Layering invariant (`application → domain only`) is preserved without exception.
- `100` is a generic, non-controversial value; any consumer reading the example understands the showcase intent ("the framework substitutes the value the domain emitted into the translated message") without needing per-tenant context to follow it. A future requirement for per-tenant variability becomes a straightforward refactor: replace the constant read inside `BuildRules` with a `domain.Service` lookup — same notification type, same wire shape, no Cmd changes.

The framework's unit suite covers the mechanism in isolation; the consumer's E2E covers it through the whole pipeline (wire → domain → translation → wire) with both PT-BR and ENG renderings asserted.

## Test plan

### Framework unit tests

1. **Tag extraction** — table-driven over `(struct shape, expected map)`:
   - Single tag.
   - Multiple tags.
   - Tag on int / string / bool / float / `uuid.UUID` / `time.Time` / pointer-to-string / nil-pointer.
   - Unexported field with tag → skipped.
   - Field with tag `tvar:""` → skipped (empty tag value).
   - Field with tag `tvar:"-"` → skipped (convention mirror to `json:"-"`).
   - Cache: two extractions on the same type → second hits the cache (verify via spy or by mutating the cached plan and observing the second call's behavior — implementation detail).

2. **Escape hatch method** — `TranslationVars()` method present + tag fields present → method output is used, tags ignored.

3. **`Render`** — table-driven:
   - Lookup hit + no placeholders → identical to `Get`.
   - Lookup hit + placeholders + complete vars → all substituted.
   - Lookup hit + placeholders + missing var → literal left, warn-once fires; second call with the same `(lang, key, placeholder)` does NOT fire warn again.
   - Lookup miss → returns `key`, warn-once for missing key.
   - `vars == nil` → no warn for absent placeholders.
   - `vars == {}` → no warn for absent placeholders (same as nil — both signal "no vars provided").
   - Scanner whitelist: `{1bad}` left literal, no warn (not a valid placeholder name).
   - Scanner: `{maxLength}` consumed only as full match (`{maxLengthFoo}` is a different placeholder).
   - Scanner: multiple occurrences of the same `{name}` all substituted.

4. **`Interpolate`** — same as `Render` minus the lookup step. No warn-on-missing in this entry point (no `lang` / `key` context).

5. **Pipeline path** — `pipeline.Run` over a handler returning a `*DomainError` with a parameterized notification → assert the resulting `Result.Failure` DTO carries the substituted message; assert `notificationKey` is the type name (unchanged); assert `semantic` is correct (notif-side).

6. **Context vars** — emit a notification on a context that has `SetVars(...)`; assert the context label in the DTO is the rendered string (the context-label translation, with the context vars applied); assert the message inside still resolves only the notification's own vars.

### Consumer E2E

1. Trigger the parameterized notification (mechanism TBD per maintainer's choice from "Consumer-side updates" above).
2. Assert HTTP 422 response body's `errors[0].messages[0].message` contains the substituted value (e.g. "máximo de 11 posições" instead of "máximo de {maxLength} posições").
3. Assert `errors[0].messages[0].notificationKey` is the canonical type name (unchanged across all values).

### Negative observation

The warn-once dedup uses a `sync.Map` for keys; reset between test runs is via a package-level helper (`resetVarWarnOnce()` in `_test.go` only, build-tagged or under an internal API). Without reset, the first test populating a (lang, key, placeholder) would mask the warn for subsequent tests on the same tuple — common Go testing trap. Documented in the test file.

## Open questions / non-goals

- **No nesting / chained placeholders.** `{outer{inner}}` is undefined. The scanner consumes the outer match (non-recursive). If a future case needs nesting, this is the moment to revisit — current design says no.
- **No format directives.** Kotlin's literal `String.replace` has no format string ("show with 2 decimal places", "uppercase"). Same here. Values come from `fmt.Sprint`; the consumer is responsible for shaping them before storing on the struct (the canonical pattern: derive the displayable string at construction time, e.g. `MaxLength string \`tvar:"maxLength"\`` instead of `int`). If a real case needs format directives, that becomes a separate design — Go's `text/template` is one path but adds complexity.
- **No locale-aware number formatting.** Same reasoning. `golang.org/x/text/message` would do it but is a heavyweight dependency for a feature that's "show the bank's per-config length". Defer.
- **No `vars` field on the wire `ErrorMessage`.** Per section 8. Re-evaluate if a real frontend consumer asks for it.
- **No replication of the Kotlin Handler-level pattern.** The Pipeline does NOT extract vars from the request type. If a request-derived var must appear in the response, the Cmd's `ApplyTo(ctx, t)` (or analogue) is the canonical place to translate `ctx` into business fields — including transient fields that emit notifications. The pattern matches the existing `RequestingPrincipalEmail` flow used in Layer 2 authz. Adding a parallel pipeline-level vars channel would duplicate it.

## Migration / rollout

Single round. The change is additive and backwards-compatible; no consumer has to change anything until they want to start using `tvar`. The framework's own `RequiredFieldNotification` / `RecordNotFoundNotification` / etc. stay tag-less — none of them have runtime-config-driven vars.

The consumer-side test case (section "Consumer-side updates") is the first real exercise. Subsequent consumers adopt `tvar` per their own catalog needs.

Rollback is trivial: revert the feature branch. No persisted state changes; no schema migration; no Mongo recompose.
