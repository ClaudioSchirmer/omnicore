package openapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Spec is the lazily-built, cached representation of the OpenAPI 3.1
// document. NewSpec returns one without building anything; the first
// MarshalJSON / Build call walks the registry, generates the schemas
// for every RequestType / ResponseType, expands the standard error
// envelopes, and caches the JSON bytes. Subsequent calls return the
// cached payload directly — the spec is immutable for the lifetime of
// the Spec value, matching the framework's "registry assembled at boot,
// served forever" pattern.
type Spec struct {
	cfg      Config
	registry *Registry
	auth     *AuthContext

	mu     sync.Mutex
	cached []byte
}

// AuthContext carries the runtime authentication shape the framework
// decorates the spec with when bootstrap detects auth.mode=jwt. Pass
// it to a Spec via WithAuth (the functional option consumed by
// Register) so the rendered document declares the bearerAuth security
// scheme + the per-route security entries, plus adds 401 to the
// standard error responses on non-public routes.
//
// PublicRoutes is the operator-declared bypass list from
// microservice.<profile>.yaml `auth.publicRoutes` — exact "METHOD /path"
// match, no globs. Combined at runtime with the per-route Public flag
// (Doc.Public for canonical, RawSpec.Public for raw) — either is
// sufficient to skip the security requirement on a given operation.
//
// AuthorizationEnabled mirrors `auth.authorization.enabled` from the
// YAML. The rendered spec uses it to gate the
// "**Required permission:** `<p>`" suffix on operation descriptions:
// the suffix is emitted ONLY when the runtime gate is actually
// enforcing, so the spec never advertises a constraint the server
// is not honoring. Under disabled or jwt-without-authz, RequirePermission
// annotations stay on the route value (Spec.RequiredPermission /
// RawSpec.RequiredPermission) but the description suffix is omitted.
type AuthContext struct {
	PublicRoutes         []string
	AuthorizationEnabled bool
}

// NewSpec constructs a Spec that will build the document on demand from
// the given config + registry. Pass the same registry the consumer's
// Mount / MountRaw calls registered routes against — its components
// pool feeds /components/schemas in the rendered spec.
func NewSpec(cfg Config, registry *Registry) *Spec {
	return &Spec{cfg: cfg, registry: registry}
}

// MarshalJSON returns the cached spec bytes or builds and caches them
// on first call. Implements encoding/json.Marshaler so a Fiber handler
// can return the spec via `c.JSON(spec)` and net/http handlers via
// `json.NewEncoder(w).Encode(spec)`.
func (s *Spec) MarshalJSON() ([]byte, error) {
	bytes, err := s.Build()
	if err != nil {
		return nil, err
	}
	return bytes, nil
}

// Build returns the JSON-encoded OpenAPI 3.1 document. The first call
// assembles + caches; subsequent calls return the cached bytes. Safe
// for concurrent use.
func (s *Spec) Build() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil {
		return s.cached, nil
	}
	doc := s.build()
	bytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	s.cached = bytes
	return s.cached, nil
}

// fiberToOpenAPIPath converts Fiber's `:name` path syntax to OpenAPI's
// `{name}` templating. `:id`, `:tenantId`, etc. all transform. Other
// special characters (`+`, `*` Fiber suffixes for greedy match) are
// stripped — OpenAPI does not model them, and Swagger UI would render a
// literal placeholder if left in.
var fiberParamRE = regexp.MustCompile(`:([a-zA-Z_][a-zA-Z0-9_]*)[*+?]?`)

func fiberToOpenAPIPath(path string) string {
	return fiberParamRE.ReplaceAllString(path, "{$1}")
}

// pathSegmentNames extracts the ordered list of parameter names from a
// Fiber path (`:id`, `:tenantId`, etc.). Used by the canonical operation
// builder to add stub path parameters for segments the Request DTO
// didn't tag explicitly.
func pathSegmentNames(path string) []string {
	matches := fiberParamRE.FindAllStringSubmatch(path, -1)
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, m[1])
	}
	return names
}

// errorEnvelopeRef is the $ref pointer every standard error response
// uses. The ErrorEnvelope component schema is materialized in the
// components pool lazily by ensureErrorEnvelope on first use.
const errorEnvelopeRef = "#/components/schemas/ErrorEnvelope"

// paginationInfoRef is the $ref pointer every paged success envelope
// uses for the top-level `pagination` property. The PaginationInfo
// component schema is materialized in the components pool lazily by
// ensurePaginationInfo on first use — same lazy convention as
// ErrorEnvelope.
const paginationInfoRef = "#/components/schemas/PaginationInfo"

// build assembles the OpenAPI 3.1 document as a map[string]any ready
// for json.Marshal. The generator is constructed off the registry's
// existing components pool so any schemas produced during Mount /
// MountRaw registration are reused (rather than regenerated).
func (s *Spec) build() map[string]any {
	gen := NewGenerator(s.registry.Components())
	ensureErrorEnvelope(gen.Components())

	doc := map[string]any{
		"openapi": "3.1.0",
		"info":    s.infoBlock(),
	}
	if servers := s.serversBlock(); servers != nil {
		doc["servers"] = servers
	}

	paths := map[string]any{}
	for _, op := range s.registry.Operations() {
		if isHidden(op) {
			continue
		}
		template := fiberToOpenAPIPath(op.Path)
		pathItem, _ := paths[template].(map[string]any)
		if pathItem == nil {
			pathItem = map[string]any{}
			paths[template] = pathItem
		}
		pathItem[strings.ToLower(op.Method)] = s.operationFor(op, gen)
	}
	doc["paths"] = paths

	components := map[string]any{}
	if schemas := gen.Components().Schemas; len(schemas) > 0 {
		components["schemas"] = schemas
	}
	if s.auth != nil {
		components["securitySchemes"] = map[string]any{
			"bearerAuth": map[string]any{
				"type":         "http",
				"scheme":       "bearer",
				"bearerFormat": "JWT",
			},
		}
	}
	if len(components) > 0 {
		doc["components"] = components
	}
	return doc
}

// isPublic returns true when the operation should bypass the
// bearerAuth security entry: either the per-route Public flag is set
// (Doc.Public for canonical, RawSpec.Public for raw) OR the operation
// appears in the AuthContext.PublicRoutes allowlist (exact
// "METHOD /path" match — same shape the runtime AuthMiddleware
// enforces).
func (s *Spec) isPublic(op Operation) bool {
	if op.Raw != nil && op.Raw.Public {
		return true
	}
	if op.Raw == nil && op.Doc.Public {
		return true
	}
	if s.auth == nil {
		return true
	}
	key := strings.ToUpper(op.Method) + " " + op.Path
	for _, r := range s.auth.PublicRoutes {
		if strings.EqualFold(strings.TrimSpace(r), key) {
			return true
		}
	}
	return false
}

// securityForOperation returns the security block to attach to a given
// operation. nil means "omit the security key entirely" (public route
// or auth disabled); a non-nil slice produces the JSON
// `"security": [{"bearerAuth": []}]` decoration. Aligns with the
// runtime contract: a route the AuthMiddleware lets through
// unauthenticated should not advertise itself as requiring a bearer.
func (s *Spec) securityForOperation(op Operation) []map[string][]string {
	if s.auth == nil || s.isPublic(op) {
		return nil
	}
	return []map[string][]string{{"bearerAuth": {}}}
}

// isHidden returns true for operations explicitly excluded from the
// rendered spec — either via Doc.Hidden (canonical) or RawSpec.Hidden
// (raw). Hidden operations still register on Fiber so the route works
// at runtime; only the documentation surface omits them.
func isHidden(op Operation) bool {
	if op.Raw != nil {
		return op.Raw.Hidden
	}
	return op.Doc.Hidden
}

func (s *Spec) infoBlock() map[string]any {
	info := map[string]any{
		"title":   s.cfg.Title,
		"version": s.cfg.Version,
	}
	if s.cfg.Description != "" {
		info["description"] = s.cfg.Description
	}
	if s.cfg.Contact != nil {
		contact := map[string]any{}
		if s.cfg.Contact.Name != "" {
			contact["name"] = s.cfg.Contact.Name
		}
		if s.cfg.Contact.Email != "" {
			contact["email"] = s.cfg.Contact.Email
		}
		if s.cfg.Contact.URL != "" {
			contact["url"] = s.cfg.Contact.URL
		}
		if len(contact) > 0 {
			info["contact"] = contact
		}
	}
	if s.cfg.License != nil {
		license := map[string]any{"name": s.cfg.License.Name}
		if s.cfg.License.URL != "" {
			license["url"] = s.cfg.License.URL
		}
		info["license"] = license
	}
	return info
}

func (s *Spec) serversBlock() []map[string]any {
	if len(s.cfg.Servers) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(s.cfg.Servers))
	for _, srv := range s.cfg.Servers {
		entry := map[string]any{"url": srv.URL}
		if srv.Description != "" {
			entry["description"] = srv.Description
		}
		out = append(out, entry)
	}
	return out
}

// operationFor dispatches to the canonical or raw operation builder
// based on which side of the union Operation carries populated. Mount
// fills Spec + Doc; MountRaw fills Raw.
func (s *Spec) operationFor(op Operation, gen *Generator) map[string]any {
	if op.Raw != nil {
		return s.rawOperation(op, gen)
	}
	return s.canonicalOperation(op, gen)
}

func (s *Spec) canonicalOperation(op Operation, gen *Generator) map[string]any {
	out := map[string]any{}
	if op.Doc.Summary != "" {
		out["summary"] = op.Doc.Summary
	}
	if desc := s.descriptionWithPermission(op.Doc.Description, op.Spec.RequiredPermission); desc != "" {
		out["description"] = desc
	}
	if op.Doc.OperationID != "" {
		out["operationId"] = op.Doc.OperationID
	}
	if len(op.Doc.Tags) > 0 {
		out["tags"] = op.Doc.Tags
	}
	if op.Doc.Deprecated {
		out["deprecated"] = true
	}

	params := canonicalParameters(op, gen)
	if len(params) > 0 {
		out["parameters"] = params
	}

	if op.Spec.RequestType != nil && hasBodyFields(op.Spec.RequestType) {
		var bodySchema *Schema
		if op.Spec.Strict {
			bodySchema = gen.GenerateStrict(op.Spec.RequestType)
		} else {
			bodySchema = gen.Generate(op.Spec.RequestType)
		}
		mediaType := map[string]any{"schema": bodySchema}
		if examples := buildExamplesMap(op.resolvedRequestExamples, nil); examples != nil {
			mediaType["examples"] = examples
		}
		out["requestBody"] = map[string]any{
			"required": op.Spec.Strict,
			"content": map[string]any{
				"application/json": mediaType,
			},
		}
	}

	out["responses"] = s.canonicalResponses(op, gen)
	if sec := s.securityForOperation(op); sec != nil {
		out["security"] = sec
	}
	return out
}

func (s *Spec) rawOperation(op Operation, gen *Generator) map[string]any {
	raw := op.Raw
	out := map[string]any{}
	if raw.Summary != "" {
		out["summary"] = raw.Summary
	}
	if desc := s.descriptionWithPermission(raw.Description, raw.RequiredPermission); desc != "" {
		out["description"] = desc
	}
	if raw.OperationID != "" {
		out["operationId"] = raw.OperationID
	}
	if len(raw.Tags) > 0 {
		out["tags"] = raw.Tags
	}
	if raw.Deprecated {
		out["deprecated"] = true
	}

	params := rawParameters(op, gen)
	if len(params) > 0 {
		out["parameters"] = params
	}

	if raw.RequestBody != nil && raw.RequestBody.Type != nil {
		contentType := raw.RequestBody.ContentType
		if contentType == "" {
			contentType = "application/json"
		}
		mediaType := map[string]any{"schema": gen.Generate(raw.RequestBody.Type)}
		if examples := buildExamplesMap(op.resolvedRequestExamples, nil); examples != nil {
			mediaType["examples"] = examples
		}
		out["requestBody"] = map[string]any{
			"required": raw.RequestBody.Required,
			"content": map[string]any{
				contentType: mediaType,
			},
		}
		if raw.RequestBody.Description != "" {
			out["requestBody"].(map[string]any)["description"] = raw.RequestBody.Description
		}
	}

	out["responses"] = s.rawResponses(op, gen)
	if sec := s.securityForOperation(op); sec != nil {
		out["security"] = sec
	}
	return out
}

// canonicalResponses assembles the responses block for a canonical
// operation: one success entry whose `data` field carries the
// ResponseType (omitted when the projection lands on responses.None),
// plus the framework-standard error envelopes the route can emit, plus
// any additional status the consumer declared examples for via
// Doc.ResponseExamples (typically a domain notification overrides
// Semantic() to a non-default status like 409 Conflict — without this
// branch the example would have no slot to land in).
func (s *Spec) canonicalResponses(op Operation, gen *Generator) map[string]any {
	responses := map[string]any{}
	status := op.Spec.SuccessStatus
	if status == 0 {
		status = http.StatusOK
	}
	responses[strconv.Itoa(status)] = canonicalSuccess(op, gen)
	standardErrs := s.standardErrors(op)
	for code, desc := range standardErrs {
		responses[strconv.Itoa(code)] = canonicalErrorResponse(op, code, desc)
	}
	// Statuses declared only via Doc.ResponseExamples — neither the
	// success status nor in standardErrors. Emit them as canonical error
	// envelopes so the consumer's examples render alongside the shared
	// ErrorEnvelope schema. The framework default auto-merge in
	// buildErrorExamplesMap naturally skips statuses without a default
	// entry (DefaultErrorExample covers only 400/401/403/404/422/500),
	// so non-default statuses surface exactly the consumer's examples.
	for code := range op.resolvedResponseExamples {
		if code == status {
			continue
		}
		if _, ok := standardErrs[code]; ok {
			continue
		}
		responses[strconv.Itoa(code)] = canonicalErrorResponse(op, code, http.StatusText(code))
	}
	return responses
}

// canonicalSuccess emits the success envelope.
//
// When the consumer declared examples for the success status in
// Doc.ResponseExamples, each Value is wrapped in the canonical Response
// envelope (success:true, status, description, data:<value>, and on
// paged routes pagination:<value>) and emitted under `examples`
// (plural) on the content. The schema's per-property `example`
// placeholders stay so Swagger UI keeps a fallback render for the bare
// schema view.
//
// When the consumer declared nothing, the envelope's per-property
// `example` values drive Swagger UI's rendering.
//
// Three branches for the schema's `data` shape:
//   - ResponseType is responses.None (or nil): emit envelope WITHOUT a
//     `data` field, matching the runtime behavior of
//     respondWithProjection on the None branch.
//   - Spec.Paged is true: emit envelope with `data` as an array of
//     ResponseType items AND a top-level `pagination` property
//     referencing the PaginationInfo component schema — mirrors the
//     runtime fwweb.RespondPaged shape. ResponseType:nil/None is
//     rejected at Mount by the Paged boot guard.
//   - Otherwise: emit envelope with `data` typed as the ResponseType
//     schema.
func canonicalSuccess(op Operation, gen *Generator) map[string]any {
	status := op.Spec.SuccessStatus
	if status == 0 {
		status = http.StatusOK
	}
	desc := http.StatusText(status)
	out := map[string]any{"description": desc}
	envelope := successEnvelopeSchema(op, gen)
	media := map[string]any{"schema": envelope}

	declared, hasDeclaration := op.resolvedResponseExamples[status]
	if hasDeclaration {
		wrap := successWrapper(status, op.Spec)
		if plural := buildExamplesMap(declared, wrap); plural != nil {
			media["examples"] = plural
		}
	}
	out["content"] = map[string]any{
		"application/json": media,
	}
	return out
}

// canonicalErrorResponse produces an OpenAPI response entry for a
// standard error status on a canonical operation. Behavior:
//
//   - Consumer declared nothing for this status → emit a single
//     `example` (singular) carrying the framework's canonical envelope
//     (matches pre-Phase-2 rendering).
//   - Consumer declared at least one example → emit `examples` (plural)
//     with auto-merge of the framework default under the key `default`,
//     UNLESS the consumer set `default` themselves (override) or set
//     `default` with an empty Value (consumer-removal of the canonical
//     entry).
//
// Both branches keep the schema as the shared `$ref` so the components
// pool stays deduplicated.
func canonicalErrorResponse(op Operation, status int, description string) map[string]any {
	media := map[string]any{"schema": map[string]any{"$ref": errorEnvelopeRef}}
	declared, hasDeclaration := op.resolvedResponseExamples[status]
	if !hasDeclaration {
		media["example"] = errorEnvelopeExample(status)
	} else if plural := buildErrorExamplesMap(declared, status); plural != nil {
		media["examples"] = plural
	}
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": media,
		},
	}
}

// rawResponses builds the responses block for a raw operation. The
// consumer's declared ResponseSpec entries take precedence; standard
// error envelopes are appended for any status the consumer did not
// override.
func (s *Spec) rawResponses(op Operation, gen *Generator) map[string]any {
	responses := map[string]any{}
	for code, rs := range op.Raw.Responses {
		responses[strconv.Itoa(code)] = rawResponseEntry(op, code, rs, gen)
	}
	for code, desc := range s.standardErrors(op) {
		key := strconv.Itoa(code)
		if _, declared := responses[key]; declared {
			continue
		}
		responses[key] = canonicalErrorResponse(op, code, desc)
	}
	return responses
}

// rawResponseEntry renders one entry from RawSpec.Responses. Examples
// declared on the ResponseSpec emit verbatim — no wrap, no auto-merge —
// because raw responses are entirely consumer-controlled (the schema
// itself is whatever the consumer chose).
func rawResponseEntry(op Operation, status int, rs ResponseSpec, gen *Generator) map[string]any {
	desc := rs.Description
	if desc == "" {
		desc = "Response"
	}
	out := map[string]any{"description": desc}
	if rs.Type == nil {
		return out
	}
	contentType := rs.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	media := map[string]any{"schema": gen.Generate(rs.Type)}
	if declared, ok := op.resolvedResponseExamples[status]; ok {
		if plural := buildExamplesMap(declared, nil); plural != nil {
			media["examples"] = plural
		}
	}
	out["content"] = map[string]any{contentType: media}
	return out
}

// buildExamplesMap renders the OpenAPI 3.x `examples` plural map from a
// resolved rawExample map. wrap, when non-nil, transforms each
// example's raw bytes into the wire value the spec emits (used by
// success-status responses to wrap the inner `data` in the canonical
// envelope). Entries whose Raw is nil are skipped — that is how the
// consumer opts out of the framework's canonical default under the
// `default` key.
//
// Returns nil when no entry survived; callers interpret nil as "do not
// emit the examples block at all" (fall back to a singular `example` or
// nothing, depending on the slot).
func buildExamplesMap(declared map[string]rawExample, wrap func(json.RawMessage) any) map[string]any {
	if len(declared) == 0 {
		return nil
	}
	out := map[string]any{}
	for name, ex := range declared {
		if ex.Raw == nil {
			continue // consumer-removal entry
		}
		entry := map[string]any{}
		if ex.Summary != "" {
			entry["summary"] = ex.Summary
		}
		if ex.Description != "" {
			entry["description"] = ex.Description
		}
		if wrap != nil {
			entry["value"] = wrap(ex.Raw)
		} else {
			entry["value"] = jsonValue(ex.Raw)
		}
		out[name] = entry
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildErrorExamplesMap is the error-status variant of buildExamplesMap.
// It auto-merges the framework's canonical entry under the key
// `default` when (a) the framework has a default for this status AND
// (b) the consumer did not declare `default` themselves. Declaring
// `default` with an empty Value is the consumer's explicit
// remove-the-default signal.
func buildErrorExamplesMap(declared map[string]rawExample, status int) map[string]any {
	out := map[string]any{}
	if def, ok := DefaultErrorExample(status); ok {
		consumerEntry, consumerDeclaredDefault := declared["default"]
		if !consumerDeclaredDefault {
			// Inject the canonical default — Marshal here is on the
			// framework's own deterministic envelope shape, so the
			// error path is genuinely unreachable.
			if rawBytes, err := json.Marshal(def.Value); err == nil {
				entry := map[string]any{}
				if def.Summary != "" {
					entry["summary"] = def.Summary
				}
				if def.Description != "" {
					entry["description"] = def.Description
				}
				entry["value"] = jsonValue(rawBytes)
				out["default"] = entry
			}
		} else if consumerEntry.Raw == nil {
			// Consumer-removal — do not emit a default entry at all.
		}
	}
	for name, ex := range declared {
		if ex.Raw == nil {
			continue
		}
		entry := map[string]any{}
		if ex.Summary != "" {
			entry["summary"] = ex.Summary
		}
		if ex.Description != "" {
			entry["description"] = ex.Description
		}
		entry["value"] = jsonValue(ex.Raw)
		out[name] = entry
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// successWrapper produces the closure that wraps an inner-data
// rawExample value into the canonical Response envelope.
//
// Three branches mirror canonicalSuccess / successEnvelopeSchema:
//   - ResponseType nil or responses.None → emit envelope WITHOUT
//     `data`, matching respondWithProjection's None branch.
//   - Spec.Paged → emit envelope with `data` AND a canonical
//     placeholder for `pagination` so the rendered example matches the
//     runtime fwweb.RespondPaged shape. Consumer-supplied raw value
//     populates `data` (they provide the inner items array).
//   - Otherwise → emit envelope with `data` populated, no pagination.
func successWrapper(status int, spec RouteSpec) func(json.RawMessage) any {
	return func(raw json.RawMessage) any {
		out := map[string]any{
			"success":     true,
			"status":      status,
			"description": http.StatusText(status),
		}
		if !isResponseNone(spec.ResponseType) {
			out["data"] = jsonValue(raw)
		}
		if spec.Paged {
			out["pagination"] = paginationInfoExample()
		}
		return out
	}
}

// paginationInfoExample returns the canonical envelope example value
// the framework folds into paged success responses. Same shape the
// PaginationInfo component schema declares; values picked so the
// rendered Swagger UI example carries a coherent demo (one page total,
// no further cursors).
func paginationInfoExample() map[string]any {
	return map[string]any{
		"has_next": false,
		"has_prev": false,
		"total":    1,
	}
}

// errorEnvelopeExample returns a representative ErrorEnvelope payload
// for the given HTTP status — one typed notification, one message,
// the canonical context name and semantic string the framework emits
// at runtime for that status. Backed by the public DefaultErrorExample
// registry so the runtime renderer and the consumer-facing API stay in
// lockstep. For statuses outside the framework's default set the
// fallback envelope reuses the Internal Server Error notification key
// (matches the previous behavior of treating "anything else" as 500).
func errorEnvelopeExample(status int) map[string]any {
	if ex, ok := DefaultErrorExample(status); ok {
		if value, ok := ex.Value.(map[string]any); ok {
			return value
		}
	}
	return errorEnvelopeValue(status, "Request", "InternalServerErrorNotification", "", "", "Internal")
}

// successEnvelopeSchema returns the inline schema of the framework's
// Response envelope wrapping the operation's ResponseType. The
// envelope shape mirrors web/response.go::Response — success / status /
// description / data (omitted when ResponseType is responses.None) /
// pagination (paged routes only).
//
// Per-property `example` values are populated so Swagger UI renders a
// concrete envelope (success=true, status=<SuccessStatus>,
// description=<reason phrase>) instead of the type-default placeholders
// (success=true, status=0, description="string").
//
// Paged branch: `data` becomes an array of ResponseType items AND a
// `pagination` property is added pointing to the PaginationInfo
// component schema (lazily materialized by ensurePaginationInfo).
// Mirrors the runtime fwweb.RespondPaged shape, which emits
// `Response{Data: []R, Pagination: PaginationInfo{...}}`. The
// ?onlyTotal=true variant (data omitted, pagination collapses to
// {total}) is documented in prose on the operation description —
// modelling the union as oneOf would add noise on the common path.
func successEnvelopeSchema(op Operation, gen *Generator) *Schema {
	status := op.Spec.SuccessStatus
	if status == 0 {
		status = http.StatusOK
	}
	envelope := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"success":     {Type: "boolean", Example: true},
			"status":      {Type: "integer", Format: "int32", Example: status},
			"description": {Type: "string", Example: http.StatusText(status)},
		},
		Required: []string{"description", "status", "success"},
	}
	if op.Spec.ResponseType != nil && !isResponseNone(op.Spec.ResponseType) {
		itemSchema := gen.Generate(op.Spec.ResponseType)
		if op.Spec.Paged {
			envelope.Properties["data"] = &Schema{Type: "array", Items: itemSchema}
		} else {
			envelope.Properties["data"] = itemSchema
		}
	}
	if op.Spec.Paged {
		ensurePaginationInfo(gen.Components())
		envelope.Properties["pagination"] = &Schema{Ref: paginationInfoRef}
	}
	return envelope
}

// isResponseNone matches the framework's runtime detection of the
// "envelope without data" projection (responses.None). Recognizes the
// canonical responses.None{} struct in omnicore/web/responses by name +
// package path so consumers cannot accidentally trigger the omission
// with their own `None` typedef.
func isResponseNone(t reflect.Type) bool {
	if t == nil {
		return true
	}
	if t.Name() != "None" {
		return false
	}
	pkg := t.PkgPath()
	return pkg == "github.com/ClaudioSchirmer/omnicore/web/responses"
}

// descriptionWithPermission decides whether the
// "**Required permission:** `<p>`" suffix is folded into the operation's
// description. Gated on the runtime authorization state so the spec
// never claims a constraint the server is not honoring:
//
//   - s.auth == nil (auth.mode=disabled) → suffix omitted; base verbatim.
//   - s.auth.AuthorizationEnabled == false (jwt without authz) → suffix
//     omitted; base verbatim. RequirePermission stays on the route
//     value (Spec.RequiredPermission / RawSpec.RequiredPermission) for
//     introspection — only the user-facing description suppresses it.
//   - s.auth.AuthorizationEnabled == true → suffix is appended via
//     appendPermissionSuffix.
//
// Returned empty string signals "do not emit a description property"
// (the caller skips the field when the return is empty).
func (s *Spec) descriptionWithPermission(base, permission string) string {
	if permission == "" || s.auth == nil || !s.auth.AuthorizationEnabled {
		return base
	}
	return appendPermissionSuffix(base, permission)
}

// appendPermissionSuffix folds the "**Required permission:** `<p>`"
// markdown line into the operation's description. Pure renderer — the
// decision of WHEN to call it lives on (*Spec).descriptionWithPermission.
//
//   - permission == ""           → description unchanged (returns base verbatim)
//   - base == ""  + permission set → only the suffix
//   - base set    + permission set → base + blank line + suffix
//
// The suffix lives on the same description property — not on a separate
// vendor extension — because that is what every OpenAPI viewer
// (Swagger UI, Redoc, Scalar) renders without additional configuration.
func appendPermissionSuffix(base, permission string) string {
	if permission == "" {
		return base
	}
	suffix := "**Required permission:** `" + permission + "`"
	if base == "" {
		return suffix
	}
	return base + "\n\n" + suffix
}

// standardErrors returns the status→description map for the standard
// error envelopes a route can emit. Heuristics:
//   - 422 + 500 universally (wrapper validation + recovered panic)
//   - 404 when HasPathID (route addresses a single record by id)
//   - 400 universally for body-carrying routes (Schema violation)
//   - 401 when auth is enabled AND the route is not public (the
//     AuthMiddleware can reject the request with MissingAuthorization
//     / InvalidToken / ExpiredToken)
//   - 403 when auth is enabled AND the route is not public — any
//     authenticated route can produce a Forbidden outcome regardless of
//     whether the gate is coarse (Layer 1 via RequirePermission), fine
//     (Layer 2 via BuildRules emitting *NotAllowedNotification), or
//     cross-cutting (Layer 3 via tenant mismatch). The illustrative
//     emitter on the example body is MissingPermissionNotification but
//     consumers may surface any Forbidden-semantic key at runtime.
func (s *Spec) standardErrors(op Operation) map[int]string {
	out := map[int]string{
		http.StatusUnprocessableEntity: http.StatusText(http.StatusUnprocessableEntity),
		http.StatusInternalServerError: http.StatusText(http.StatusInternalServerError),
	}
	if op.Spec.HasPathID || (op.Raw != nil && hasPathInParameters(op.Raw.Parameters)) {
		out[http.StatusNotFound] = http.StatusText(http.StatusNotFound)
	}
	if op.Spec.RequestType != nil && hasBodyFields(op.Spec.RequestType) {
		out[http.StatusBadRequest] = http.StatusText(http.StatusBadRequest)
	}
	if op.Raw != nil && op.Raw.RequestBody != nil {
		out[http.StatusBadRequest] = http.StatusText(http.StatusBadRequest)
	}
	if s.auth != nil && !s.isPublic(op) {
		out[http.StatusUnauthorized] = http.StatusText(http.StatusUnauthorized)
		out[http.StatusForbidden] = http.StatusText(http.StatusForbidden)
	}
	return out
}

func hasPathInParameters(params []Parameter) bool {
	for _, p := range params {
		if p.In == InPath {
			return true
		}
	}
	return false
}

// ensurePaginationInfo registers the canonical paged-success
// `pagination` schema in the components pool. Mirrors
// web/response.go::PaginationInfo verbatim — has_next, has_prev,
// next_cursor (omitempty → optional), prev_cursor (omitempty →
// optional), total. Referenced by `$ref: #/components/schemas/PaginationInfo`
// from every paged route's success envelope.
//
// The ?onlyTotal=true variant collapses the runtime pagination to
// {total} via TotalOnlyPagination — the schema here documents the
// default listing shape; the variant lives in prose on each paged
// operation's description.
func ensurePaginationInfo(c *Components) {
	if _, exists := c.Schemas["PaginationInfo"]; exists {
		return
	}
	c.Schemas["PaginationInfo"] = &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"has_next":    {Type: "boolean", Example: false},
			"has_prev":    {Type: "boolean", Example: false},
			"next_cursor": {Type: "string"},
			"prev_cursor": {Type: "string"},
			"total":       {Type: "integer", Format: "int64", Example: 1},
		},
		Required: []string{"has_next", "has_prev", "total"},
	}
}

// ensureErrorEnvelope registers the canonical error response schema in
// the components pool. Mirrors web/response.go::Response with `errors`
// populated and the wire's ErrorMessage shape (notificationKey, field,
// value, funcName, message, semantic).
func ensureErrorEnvelope(c *Components) {
	if _, exists := c.Schemas["ErrorEnvelope"]; exists {
		return
	}
	c.Schemas["ErrorEnvelope"] = &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"success":     {Type: "boolean"},
			"status":      {Type: "integer", Format: "int32"},
			"description": {Type: "string"},
			"errors": {
				Type: "array",
				Items: &Schema{
					Type: "object",
					Properties: map[string]*Schema{
						"context": {Type: "string"},
						"messages": {
							Type: "array",
							Items: &Schema{
								Type: "object",
								Properties: map[string]*Schema{
									"notificationKey": {Type: "string"},
									"field":           {Type: "string"},
									"value":           {Type: "string"},
									"funcName":        {Type: "string"},
									"message":         {Type: "string"},
									"semantic":        {Type: "string"},
								},
								Required: []string{"message"},
							},
						},
					},
					Required: []string{"context", "messages"},
				},
			},
		},
		Required: []string{"description", "errors", "status", "success"},
	}
}
