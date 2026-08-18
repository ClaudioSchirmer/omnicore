package bootstrap

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"

	"gopkg.in/yaml.v3"
)

// FrameworkDefaultAuditMaxLimit is the number of audit rows one timeline read
// returns when the yaml leaves audit.endpoint.maxLimit unset.
//
// Deliberately NOT tied to query.maxLimit (100): that ceiling governs paged
// reads over Mongo projections, and borrowing it here would couple two
// unrelated surfaces — an operator raising the projection page size would
// silently widen the audit trail's exposure. An audit timeline is read to
// answer "what happened to this record, most recently", so a small window is
// the honest default; an operator who wants more says so on this key.
const FrameworkDefaultAuditMaxLimit = 20

// defaultAuditRESTPath is the group prefix the audit read routes mount under
// when audit.endpoint.rest declares no path.
const defaultAuditRESTPath = "/audit"

// defaultAuditRESTPermission is the Layer-1 permission the audit read route
// demands when audit.endpoint.rest declares none.
const defaultAuditRESTPermission = "audit:read"

// AuditConfig is the `audit:` block of microservice.<profile>.yaml.
//
// It layers two concerns that belong to different layers of the framework and
// must not be merged into one type:
//
//   - the embedded audit.Config (infra/audit) — WHERE audit events are routed
//     on the WRITE path. It lives in infra because the relational persister
//     consumes it, and it stays free of any transport vocabulary.
//   - Endpoint — the optional READ surfaces the framework serves over the
//     audit_events table. A path and a permission are web concepts; typing
//     them here (the composition root, which already knows both web and infra)
//     keeps them out of the infra package the persister reads.
//
// The embedding is inline, so `audit.destinations` keeps its exact yaml
// position and `cfg.Audit.Destinations` / `cfg.Audit.Includes(...)` keep
// working through promotion.
type AuditConfig struct {
	audit.Config `yaml:",inline"`

	// Endpoint is opt-in: nil (default) means the framework mounts nothing and
	// keeps shipping only the audit.Reader port, exactly as before this block
	// existed. A non-nil (even empty) block turns the read surfaces on, each
	// connector enabled by its own sub-block — the same
	// presence-is-the-switch shape as auth.issuer.jwks.
	Endpoint *AuditEndpointConfig `yaml:"endpoint,omitempty"`
}

// AuditEndpointConfig is the `audit.endpoint:` sub-block — the knobs shared by
// every read connector, plus one sub-block per connector.
//
// READS THE DATABASE. Every connector here answers from the `audit_events`
// table, so the block REQUIRES `database` among audit.destinations; declaring
// it on a service that routes audit only to slog aborts the boot rather than
// mounting a surface that can only ever answer empty. A deployment that keeps
// audit on slog/ELK and still needs a read surface builds it itself over the
// log stream — the framework's translation primitives (audit.RenderLabels for
// the typed path, audit.RenderLabelsInJSON for a parsed log line) are
// independent of the storage and work there unchanged.
type AuditEndpointConfig struct {
	// RenderLabels resolves each FieldChange's catalog key into the actor's
	// locale before the event is serialized (Accept-Language), replacing
	// fieldLabelKey with the translated fieldLabel. nil (absent) means true.
	// Set it to false for machine consumers that want the raw, stable catalog
	// key instead of a translated string.
	RenderLabels *bool `yaml:"renderLabels,omitempty"`

	// MaxLimit is the ceiling on how many rows one timeline read returns, and
	// the window used when the caller names none. Rendered into the SQL by the
	// dialect, so the database never materializes more than this. Zero (or
	// unset) defers to FrameworkDefaultAuditMaxLimit; negative values are
	// rejected at boot.
	MaxLimit int `yaml:"maxLimit,omitempty"`

	// REST is opt-in: nil means no HTTP route is mounted. An empty block
	// (`rest: {}`) mounts with every default.
	REST *AuditRESTConfig `yaml:"rest,omitempty"`
}

// AuditRESTConfig is the `audit.endpoint.rest:` sub-block — the HTTP connector
// over the audit trail.
type AuditRESTConfig struct {
	// Path is the group prefix the read route mounts under. Default "/audit",
	// which yields GET /audit/{entityType}/{aggregateId}. Must start with "/"
	// and must not collide with a framework-reserved route or with another
	// self-mounted surface (the OpenAPI UI, the GraphQL endpoint).
	Path string `yaml:"path,omitempty"`

	// Permission is the Layer-1 permission the caller's Identity must carry,
	// enforced exactly like a RequirePermission declared on a service route:
	// inert while auth.authorization.enabled is false, a 403 once it is on.
	// Absent (nil) means "audit:read".
	//
	// Pointer-typed so an operator can say `permission: ""` and MEAN it — that
	// mounts the route with no permission gate, and is refused at boot when
	// authorization is enabled, since the audit trail is the last surface that
	// should be readable by anyone who merely authenticated. An absent key and
	// a deliberately blank one are different decisions and the framework
	// answers them differently.
	Permission *string `yaml:"permission,omitempty"`
}

// UnmarshalYAML enforces the closed key set on `audit:`.
func (c *AuditConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias AuditConfig
	if err := node.Decode((*alias)(c)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "audit", "destinations", "endpoint")
}

// UnmarshalYAML enforces the closed key set on `audit.endpoint:`.
func (c *AuditEndpointConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias AuditEndpointConfig
	if err := node.Decode((*alias)(c)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "audit.endpoint", "renderLabels", "maxLimit", "rest")
}

// UnmarshalYAML enforces the closed key set on `audit.endpoint.rest:`.
func (c *AuditRESTConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias AuditRESTConfig
	if err := node.Decode((*alias)(c)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "audit.endpoint.rest", "path", "permission")
}

// ApplyDefaults fills the write-path defaults (the embedded destinations) and,
// when the operator opted into a read surface, that surface's defaults. It
// shadows the promoted audit.Config.ApplyDefaults deliberately: one call site
// in Config.applyDefaults must reach both halves.
func (c *AuditConfig) ApplyDefaults() {
	c.Config.ApplyDefaults()
	if c.Endpoint == nil {
		return
	}
	if c.Endpoint.MaxLimit == 0 {
		c.Endpoint.MaxLimit = FrameworkDefaultAuditMaxLimit
	}
	if c.Endpoint.REST != nil {
		if c.Endpoint.REST.Path == "" {
			c.Endpoint.REST.Path = defaultAuditRESTPath
		}
		if c.Endpoint.REST.Permission == nil {
			d := defaultAuditRESTPermission
			c.Endpoint.REST.Permission = &d
		}
	}
}

// Validate runs the embedded destinations check and then the read-surface
// rules. Shadows the promoted audit.Config.Validate for the same reason
// ApplyDefaults does.
func (c *AuditConfig) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if c.Endpoint == nil {
		return nil
	}
	if !c.Includes(audit.DestinationDatabase) {
		return fmt.Errorf("audit.endpoint requires %q among audit.destinations: the read surfaces answer "+
			"from the audit_events table, and this service routes audit to [%s], so every read could only "+
			"ever return an empty timeline. Add the destination, or drop audit.endpoint and build the read "+
			"surface over the log stream (audit.RenderLabels / audit.RenderLabelsInJSON translate the field "+
			"labels there just the same)",
			audit.DestinationDatabase, destinationList(c.Destinations))
	}
	if c.Endpoint.MaxLimit < 0 {
		return fmt.Errorf("audit.endpoint.maxLimit must be >= 0 (0 = the framework default, %d)", FrameworkDefaultAuditMaxLimit)
	}
	if c.Endpoint.REST != nil {
		if !strings.HasPrefix(c.Endpoint.REST.Path, "/") {
			return fmt.Errorf("audit.endpoint.rest.path %q must start with %q", c.Endpoint.REST.Path, "/")
		}
		if collidesFrameworkPath(c.Endpoint.REST.Path) {
			return fmt.Errorf("audit.endpoint.rest.path %q collides with a framework route", c.Endpoint.REST.Path)
		}
	}
	return nil
}

// validateAuditEndpointWiring runs the audit read surface's cross-block rules
// — the ones that need to see the rest of the yaml.
//
// Path collision: two self-mounted surfaces claiming the same route is a
// registration race whose winner depends on mount order, so it is refused
// here rather than discovered at runtime. The audit routes live UNDER their
// path (it is a group prefix), which is why an exact match with a sibling
// surface is the check — a prefix that merely contains another path routes
// fine, because Fiber matches full segments.
//
// Ungated audit under authorization: scanAuthorization would catch this at
// mount time, but its diagnostic names an offending ROUTE and tells the
// maintainer to add RequirePermission to code the maintainer does not own.
// Caught here, the message names the yaml key that produced it.
func (c *Config) validateAuditEndpointWiring() error {
	if !c.Audit.auditRESTEnabled() {
		return nil
	}
	rest := c.Audit.Endpoint.REST
	if rest.Path == c.GraphQL.Path {
		return fmt.Errorf("audit.endpoint.rest.path %q collides with graphql.path", rest.Path)
	}
	if c.GraphQL.Playground && rest.Path == c.GraphQL.UIPath {
		return fmt.Errorf("audit.endpoint.rest.path %q collides with graphql.uiPath", rest.Path)
	}
	if rest.Path == c.OpenAPI.UIPath {
		return fmt.Errorf("audit.endpoint.rest.path %q collides with openapi.uiPath", rest.Path)
	}
	if rest.PermissionValue() == "" && c.Auth.Authorization != nil && c.Auth.Authorization.Enabled {
		return fmt.Errorf("audit.endpoint.rest.permission is empty while auth.authorization.enabled=true: "+
			"the audit trail would be readable by anyone who merely authenticated. Declare a permission "+
			"(the default is %q), or drop audit.endpoint.rest", defaultAuditRESTPermission)
	}
	return nil
}

// RenderLabelsEnabled resolves the tri-state RenderLabels knob: absent means
// on, which is the posture a human-facing read wants.
func (c *AuditEndpointConfig) RenderLabelsEnabled() bool {
	return c.RenderLabels == nil || *c.RenderLabels
}

// PermissionValue resolves the gate the route mounts with: the declared
// permission, or "" when the operator deliberately blanked it (no gate).
// ApplyDefaults has already filled the absent case.
func (c *AuditRESTConfig) PermissionValue() string {
	if c.Permission == nil {
		return defaultAuditRESTPermission
	}
	return *c.Permission
}

// auditRESTEnabled reports whether the HTTP connector should be mounted.
func (c *AuditConfig) auditRESTEnabled() bool {
	return c.Endpoint != nil && c.Endpoint.REST != nil
}

// destinationList renders the configured destinations for a diagnostic.
func destinationList(ds []audit.Destination) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = string(d)
	}
	return strings.Join(out, ", ")
}
