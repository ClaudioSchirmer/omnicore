package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Provider owns the SDK tracer provider's lifetime. When tracing is disabled it
// is inert: Shutdown is a no-op and no globals were touched. The caller
// (bootstrap) keeps the handle and calls Shutdown during the drain so buffered
// spans flush before the process exits.
type Provider struct {
	tp       *sdktrace.TracerProvider
	shutdown func(context.Context) error
}

// Setup installs the global tracer provider and the W3C trace-context
// propagator from cfg and returns a Provider for lifecycle control.
//
//   - Disabled                → installs nothing; the OTel default no-op tracer
//     and no-op propagator stay in place. Returns an inert Provider.
//   - Enabled, exporter none  → records + propagates traceparent but exports no
//     spans (a TracerProvider with no span processor drops them). Keeps a
//     middle service's trace chain intact for its downstreams.
//   - Enabled, exporter otlp  → BatchSpanProcessor over an OTLP/gRPC exporter.
//     Export is asynchronous and batched, OFF the request hot path; if the
//     collector is down the batch is dropped, never back-pressuring the caller.
//
// The propagator is the composite TraceContext + Baggage so both the standard
// traceparent and baggage headers cross service boundaries.
func Setup(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled {
		return &Provider{}, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Merge with resource.Default() so the SDK/telemetry attributes AND any
	// operator-supplied OTEL_RESOURCE_ATTRIBUTES / OTEL_SERVICE_NAME survive to
	// the collector; the explicit service.name wins on the key (second arg to
	// Merge). NewSchemaless alone would drop them, leaving a multi-env/multi-host
	// deployment unable to filter traces by deployment.environment / host.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", cfg.ServiceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(resolveSampler(cfg)),
		sdktrace.WithResource(res),
	}

	switch cfg.Exporter {
	case ExporterOTLP:
		grpcOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			// Plaintext gRPC — a local sidecar collector (Jaeger in dev). TLS is
			// used otherwise, as a managed collector requires.
			grpcOpts = append(grpcOpts, otlptracegrpc.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			// e.g. a managed collector's auth token header.
			grpcOpts = append(grpcOpts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		exp, err := otlptracegrpc.New(ctx, grpcOpts...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	case ExporterStdout:
		exp, err := stdouttrace.New()
		if err != nil {
			return nil, fmt.Errorf("tracing: stdout exporter: %w", err)
		}
		// Synchronous processor so spans print immediately while debugging — the
		// opposite of the batched, off-path otlp processor. Debug-only.
		opts = append(opts, sdktrace.WithSyncer(exp))
	}
	// ExporterNone: no span processor → spans are recorded (and traceparent
	// propagated) but never exported.

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Provider{tp: tp, shutdown: tp.Shutdown}, nil
}

// Shutdown flushes buffered spans and releases the exporter. Safe on a nil or
// inert Provider (disabled path), so the drain code never branches on config.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.shutdown == nil {
		return nil
	}
	return p.shutdown(ctx)
}

func resolveSampler(cfg Config) sdktrace.Sampler {
	switch cfg.Sampler {
	case SamplerAlwaysOn:
		return sdktrace.AlwaysSample()
	case SamplerAlwaysOff:
		return sdktrace.NeverSample()
	case SamplerTraceRatio:
		return sdktrace.TraceIDRatioBased(cfg.Ratio)
	case SamplerParentBasedTraceRatio:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Ratio))
	default:
		// Validate already rejected unknown samplers when enabled; fall back to
		// the safe multi-service default rather than panic.
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Ratio))
	}
}
