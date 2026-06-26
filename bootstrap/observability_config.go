package bootstrap

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/tracing"
)

// ObservabilityConfig is the top-level `observability:` block. Currently it
// carries only the tracing sub-block; metrics/profiling would land here as
// sibling keys without spinning a new top-level block per concern.
type ObservabilityConfig struct {
	Tracing TracingConfig `yaml:"tracing"`
}

// TracingConfig is the operator-facing `observability.tracing:` block. It maps
// 1:1 onto infra/tracing.Config via Resolve. Default off: an absent block (all
// zero values) yields Enabled=false and the framework installs the no-op tracer.
type TracingConfig struct {
	// Enabled is the master switch. Off → no SDK, no exporter, no propagation.
	Enabled bool `yaml:"enabled"`

	// Exporter selects the span destination: otlp (a collector), stdout
	// (debug-only — prints spans to stdout, mixing with logs/audit) or none
	// (record + propagate traceparent, export nothing). Defaults to otlp.
	Exporter string `yaml:"exporter"`

	// Endpoint is the OTLP/gRPC collector address; supports ${VAR:default}
	// substitution. Defaults to localhost:4317. Read only for exporter otlp.
	Endpoint string `yaml:"endpoint"`

	// Insecure disables TLS on the OTLP/gRPC export (plaintext gRPC).
	// Profile-aware default: dev → true (a local sidecar collector speaks
	// plaintext), any other profile → false (a managed collector needs TLS). An
	// explicit yaml value wins. Read only for exporter otlp.
	Insecure *bool `yaml:"insecure"`

	// Headers are attached to every OTLP export request — the slot for a managed
	// collector's auth token (e.g. x-honeycomb-team). Supports ${VAR:default}
	// substitution on the values; empty sends none. Read only for exporter otlp.
	Headers map[string]string `yaml:"headers"`

	// Sampler is the head-based strategy. Profile-aware default: dev →
	// always_on, any other profile → parentbased_traceratio.
	Sampler string `yaml:"sampler"`

	// Ratio is the keep fraction for the *traceratio samplers. Defaults to 0.1.
	// Treated as unset when 0 — declare always_off to drop everything.
	Ratio float64 `yaml:"ratio"`

	// ServiceName labels this process's spans in the collector. Defaults to the
	// top-level service identity.
	ServiceName string `yaml:"serviceName"`

	// Instrument is the edge-subsystem allowlist (http, pgx, mongo, kafka,
	// httpclient). Empty → all enabled. The business dispatch span is always on
	// when Enabled and is not a member of this list.
	Instrument []string `yaml:"instrument"`
}

// applyDefaults sets the profile-agnostic defaults. Only touches a block the
// operator turned on — a malformed block left behind enabled:false is ignored,
// mirroring the rest of the config surface.
func (o *ObservabilityConfig) applyDefaults() {
	t := &o.Tracing
	if !t.Enabled {
		return
	}
	if t.Exporter == "" {
		t.Exporter = string(tracing.ExporterOTLP)
	}
	if t.Endpoint == "" {
		t.Endpoint = "localhost:4317"
	}
	if t.Ratio == 0 {
		t.Ratio = 0.1
	}
}

// applyProfileDefaults resolves the profile-dependent defaults, each with an
// explicit-yaml-wins override:
//   - Sampler: dev favors always_on (see everything while developing); any other
//     profile favors parentbased_traceratio (the keep/drop decision is taken once
//     at the edge so a distributed trace is never split).
//   - Insecure: dev favors plaintext gRPC (a local sidecar collector); any other
//     profile favors TLS (false), as a managed collector requires.
//
// Runs after Validate, like Migrations/Mongo.Rebuild autoRun, so validate
// tolerates an empty sampler.
func (o *ObservabilityConfig) applyProfileDefaults(profile string) {
	t := &o.Tracing
	if !t.Enabled {
		return
	}
	dev := profile == profileDev
	if t.Sampler == "" {
		if dev {
			t.Sampler = string(tracing.SamplerAlwaysOn)
		} else {
			t.Sampler = string(tracing.SamplerParentBasedTraceRatio)
		}
	}
	if t.Insecure == nil {
		t.Insecure = &dev
	}
}

// validate checks the block when enabled. Tolerates an empty sampler (the
// profile default is applied after Validate); the fully-resolved enum/range
// gate runs again in tracing.Setup with every default in place.
func (o *ObservabilityConfig) validate() error {
	t := o.Tracing
	if !t.Enabled {
		return nil
	}
	switch tracing.Exporter(t.Exporter) {
	case tracing.ExporterOTLP, tracing.ExporterStdout, tracing.ExporterNone:
	default:
		return fmt.Errorf("observability.tracing.exporter %q invalid (want otlp|stdout|none)", t.Exporter)
	}
	switch tracing.Sampler(t.Sampler) {
	case "", tracing.SamplerAlwaysOn, tracing.SamplerAlwaysOff,
		tracing.SamplerTraceRatio, tracing.SamplerParentBasedTraceRatio:
	default:
		return fmt.Errorf("observability.tracing.sampler %q invalid", t.Sampler)
	}
	if t.Ratio < 0 || t.Ratio > 1 {
		return fmt.Errorf("observability.tracing.ratio %v out of range [0,1]", t.Ratio)
	}
	for _, tok := range t.Instrument {
		if !validSubsystem(tok) {
			return fmt.Errorf("observability.tracing.instrument %q invalid (want http|pgx|mongo|kafka|httpclient)", tok)
		}
	}
	return nil
}

func validSubsystem(tok string) bool {
	for _, s := range tracing.AllSubsystems() {
		if string(s) == tok {
			return true
		}
	}
	return false
}

// Resolve maps the YAML block onto infra/tracing.Config, filling ServiceName
// from the top-level service identity and expanding an empty Instrument list to
// the full subsystem set.
func (t TracingConfig) Resolve(serviceName string) tracing.Config {
	name := t.ServiceName
	if name == "" {
		name = serviceName
	}
	instr := make(map[tracing.Subsystem]bool, len(tracing.AllSubsystems()))
	if len(t.Instrument) == 0 {
		for _, s := range tracing.AllSubsystems() {
			instr[s] = true
		}
	} else {
		for _, tok := range t.Instrument {
			instr[tracing.Subsystem(tok)] = true
		}
	}
	insecure := false
	if t.Insecure != nil {
		insecure = *t.Insecure
	}
	return tracing.Config{
		Enabled:     t.Enabled,
		Exporter:    tracing.Exporter(t.Exporter),
		Endpoint:    t.Endpoint,
		Insecure:    insecure,
		Headers:     t.Headers,
		Sampler:     tracing.Sampler(t.Sampler),
		Ratio:       t.Ratio,
		ServiceName: name,
		Instrument:  instr,
	}
}
