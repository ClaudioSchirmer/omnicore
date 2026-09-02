package auditapi

import (
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	fwweb "github.com/ClaudioSchirmer/omnicore/web"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"github.com/gofiber/fiber/v3"
)

// Config is the resolved `audit.endpoint` posture this package needs, passed
// as plain values by the composition root.
//
// Deliberately NOT the bootstrap config type: bootstrap imports web, so the
// dependency can only run one way, and a transport package that named a
// yaml struct would also be claiming the yaml shape is its contract. What the
// routes actually need is four resolved values.
type Config struct {
	// Path is the group prefix, e.g. "/audit".
	Path string
	// Permission gates the routes (Layer 1). Empty mounts them ungated.
	Permission string
	// MaxLimit is the ceiling on one timeline read. Always positive.
	MaxLimit int
	// RenderLabels resolves each field-change label into the actor's locale.
	RenderLabels bool
}

// Deps are the runtime collaborators the handlers dispatch through. The
// Reader is the application-layer PORT — the concrete relational reader is
// built by the composition root, so this package never sees infra.
type Deps struct {
	Pipeline   *pipeline.Pipeline
	Reader     audit.Reader
	Translator *translation.Translator
}

// Mount registers the framework's audit read routes under cfg.Path:
//
//	GET {path}/{entityType}/{aggregateId} — the aggregate's timeline, newest first
//
// Registration goes through openapi.Mount like every other framework-owned
// route, so the routes appear in the spec, pass the boot's route-registration
// scan, and pick up the permission gate from one seat. The registry may be
// nil (a service that opted out of the OpenAPI surface): Mount still wires
// the Fiber route and simply records no operation.
//
// The response is the canonical success envelope with `data` carrying the
// event array. An aggregate with no rows answers 200 with an empty array —
// an absent timeline is a legitimate state, not a 404.
func Mount(router fiber.Router, registry *openapi.Registry, cfg Config, deps Deps) {
	group := router.Group(cfg.Path)

	var opts []openapi.MountOption
	if cfg.Permission != "" {
		opts = append(opts, openapi.RequirePermission(cfg.Permission))
	}

	openapi.Mount(registry, group, fiber.MethodGet, "/:entityType/:aggregateId",
		findByAggregateHandler(cfg, deps),
		openapi.RouteSpecOf[FindAuditByAggregateRequest, AuditEventResponse](fiber.StatusOK),
		openapi.Doc{
			Summary: "Get the audit timeline of an aggregate",
			Description: "Returns the audit_events rows written for the supplied aggregate, newest first, " +
				"served by the audit_events_entity_timeline_idx (entity_type, aggregate_id, occurred_at DESC). " +
				"`?first=N` narrows the window; absent it returns one full window (audit.endpoint.maxLimit), " +
				"and a value above that ceiling is refused with LimitExceededNotification rather than truncated. " +
				"Reads the relational audit trail, so it requires `database` among audit.destinations. " +
				"An aggregate with no rows answers 200 with an empty array.",
			Tags: []string{"Audit"},
		},
		opts...)
}

// findByAggregateHandler walks the canonical manual chain: AppContext →
// BindPath → parse the window control → Dispatch → project → respond.
//
// The application handler is constructed inside the per-request closure; only
// the resolved configuration and the long-lived collaborators are captured,
// matching the framework's own convention that application handlers are never
// cached across requests.
func findByAggregateHandler(cfg Config, deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		appCtx := fwweb.AppContext(c)
		appCtx.SetParent(c)

		var req FindAuditByAggregateRequest
		if v := fwweb.BindPath(c, &req); v != nil {
			return fwweb.RespondViolation(c, deps.Pipeline, v)
		}
		first, ok := parseFirst(c)
		if !ok {
			return fwweb.RespondSchemaViolation(c, deps.Pipeline, firstControl)
		}

		q := &audit.FindByAggregateQuery{
			EntityType:  req.EntityType,
			AggregateID: req.AggregateID.String(),
			First:       first,
			MaxLimit:    cfg.MaxLimit,
		}
		h := &audit.FindByAggregateQueryHandler{
			Reader:       deps.Reader,
			Translator:   deps.Translator,
			RenderLabels: cfg.RenderLabels,
		}
		result := pipeline.Dispatch(deps.Pipeline, appCtx, q, h)
		if !result.IsSuccess() {
			return fwweb.RespondFromResult(c, result, fiber.StatusOK)
		}
		events := result.Value()
		out := make([]AuditEventResponse, 0, len(events))
		for _, ev := range events {
			out = append(out, fromEvent(ev))
		}
		return fwweb.RespondWithSuccess(c, fiber.StatusOK, out)
	}
}

// firstControl is the wire spelling of the window control.
const firstControl = "first"

// parseFirst reads `?first=` off the query string. An absent key is 0 ("one
// full window"); anything that is not an integer is a schema violation, kept
// here at the wire boundary. The RANGE rules (non-positive, above the
// ceiling) are policy, not parsing, and live in the application handler where
// they produce typed notifications for every transport.
func parseFirst(c fiber.Ctx) (int, bool) {
	raw := c.Query(firstControl)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}
