// Package tracing is the framework's OpenTelemetry distributed-tracing
// subsystem. It is opt-in (default off) and installs a no-op tracer when
// disabled, so a service that does not declare observability.tracing pays
// essentially nothing — no spans, no exporter, no propagation.
//
// Tracing is an ADDITIONAL telemetry destination (OTLP → a collector such as
// Jaeger or Tempo), never the process stdout. The stdout logs + the audit echo
// are untouched except for an additive traceId/spanId field on records that
// carry a span in their context (see NewSlogHandler).
//
// Layering: this package (infra) owns the SDK, the exporter, and the global
// installation (otel.SetTracerProvider / otel.SetTextMapPropagator). The web
// and application layers never import it — they read the installed globals via
// otel.Tracer / otel.GetTextMapPropagator, a third-party dependency in the same
// class as fiber or uuid, so no web→infra / application→infra edge is created.
package tracing

import (
	"fmt"
	"strings"
)

// Subsystem identifies an instrumentation point the operator can toggle
// independently via the observability.tracing.instrument allowlist. The
// business-unit span (pipeline dispatch) is NOT a Subsystem — it is always on
// when tracing is enabled because it is one span per request (cheap) and the
// highest-value level. The toggles below cover the "edge" spans whose volume or
// library maturity warrants per-subsystem control.
type Subsystem string

const (
	// SubHTTP is the inbound server span (one per request).
	SubHTTP Subsystem = "http"
	// SubPgx is one span per PostgreSQL statement (highest volume).
	SubPgx Subsystem = "pgx"
	// SubMongo is one span per Mongo command (high volume on the read side).
	SubMongo Subsystem = "mongo"
	// SubKafka is the produce/consume spans on the async paths.
	SubKafka Subsystem = "kafka"
	// SubHTTPClient is the outbound client span (one per Call).
	SubHTTPClient Subsystem = "httpclient"
)

// AllSubsystems is the full toggle set, used as the default instrument list
// and to validate operator-supplied tokens.
func AllSubsystems() []Subsystem {
	return []Subsystem{SubHTTP, SubPgx, SubMongo, SubKafka, SubHTTPClient}
}

// Exporter selects where recorded spans go.
type Exporter string

const (
	// ExporterOTLP ships spans over OTLP/gRPC to a collector. The real mode.
	ExporterOTLP Exporter = "otlp"
	// ExporterStdout writes spans as JSON to stdout, synchronously. DEBUG ONLY:
	// it lets a developer see traces locally without standing up a collector,
	// but it mixes span output into the same stdout stream as the application
	// logs and the audit echo — the very noise tracing otherwise keeps off
	// stdout — and the synchronous write sits on the request path. Use it to
	// confirm instrumentation locally, then switch back to otlp. Never in prod.
	ExporterStdout Exporter = "stdout"
	// ExporterNone records + propagates traceparent but exports nothing — for
	// a middle service that must keep the trace chain intact for its
	// downstreams while it has no collector of its own yet.
	ExporterNone Exporter = "none"
)

// Sampler names the head-based sampling strategy.
type Sampler string

const (
	SamplerAlwaysOn  Sampler = "always_on"
	SamplerAlwaysOff Sampler = "always_off"
	// SamplerTraceRatio samples a fixed ratio, ignoring any inbound decision —
	// rarely what a service mesh wants (it can split a distributed trace).
	SamplerTraceRatio Sampler = "traceratio"
	// SamplerParentBasedTraceRatio honors the upstream decision when a
	// traceparent arrives, else samples at Ratio. The correct default for a
	// multi-service topology: the keep/drop decision is taken ONCE at the edge,
	// so a distributed trace is all-in or all-out, never cut in half.
	SamplerParentBasedTraceRatio Sampler = "parentbased_traceratio"
)

// Config is the resolved tracing configuration handed to Setup. bootstrap
// builds it from the observability.tracing YAML block + profile defaults.
type Config struct {
	// Enabled is the master switch. When false, Setup installs nothing and the
	// process keeps the OTel default no-op tracer + no-op propagator.
	Enabled bool

	Exporter Exporter
	// Endpoint is the OTLP/gRPC collector address (host:port). Only read when
	// Exporter == ExporterOTLP.
	Endpoint string
	// Insecure disables TLS on the OTLP/gRPC connection (plaintext gRPC). Only
	// read when Exporter == ExporterOTLP. A local sidecar collector (Jaeger in
	// dev) speaks plaintext; a remote managed collector (Honeycomb, Grafana
	// Cloud Tempo, …) requires TLS, so this is false outside dev.
	Insecure bool
	// Headers are attached to every OTLP export request — the slot for a managed
	// collector's auth token (e.g. "x-honeycomb-team"). Only read when
	// Exporter == ExporterOTLP; nil/empty sends none.
	Headers map[string]string

	Sampler Sampler
	// Ratio is the keep fraction (0..1) for the *traceratio samplers.
	Ratio float64

	// ServiceName labels every span emitted by this process (the name shown in
	// the collector). Defaults to the service identity from config.
	ServiceName string

	// Instrument is the set of enabled edge subsystems. A subsystem absent from
	// the map (or mapped to false) emits no spans even when Enabled is true.
	Instrument map[Subsystem]bool
}

// Instruments reports whether the given edge subsystem should emit spans: true
// only when tracing is enabled AND the subsystem is in the allowlist.
func (c Config) Instruments(s Subsystem) bool {
	return c.Enabled && c.Instrument[s]
}

// Validate checks the resolved configuration. It is a no-op when disabled — an
// operator can leave a malformed block behind a false switch without failing
// boot. When enabled, unknown enum values and an out-of-range ratio abort.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.Exporter {
	case ExporterOTLP, ExporterStdout, ExporterNone:
	default:
		return fmt.Errorf("tracing: unknown exporter %q (want otlp|stdout|none)", c.Exporter)
	}
	switch c.Sampler {
	case SamplerAlwaysOn, SamplerAlwaysOff, SamplerTraceRatio, SamplerParentBasedTraceRatio:
	default:
		return fmt.Errorf("tracing: unknown sampler %q", c.Sampler)
	}
	if c.Ratio < 0 || c.Ratio > 1 {
		return fmt.Errorf("tracing: ratio %v out of range [0,1]", c.Ratio)
	}
	if c.Exporter == ExporterOTLP && strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("tracing: exporter otlp requires a non-empty endpoint")
	}
	if c.ServiceName == "" {
		return fmt.Errorf("tracing: serviceName is required when enabled")
	}
	return nil
}
