package httpclient

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var outboundTracer = otel.Tracer("github.com/ClaudioSchirmer/omnicore/infra/httpclient")

// tracingMiddleware starts the outbound client span and injects the W3C
// traceparent into the request headers so the downstream service continues the
// same trace. It is the outermost layer when client tracing is enabled, so the
// span times the whole call (retries included) and the injected header names
// this span as the downstream parent. Added to the chain only when the operator
// lists "httpclient" in the instrument allowlist; a no-op span otherwise.
//
// The span name is the low-cardinality "<service>/<endpoint>" YAML identity,
// not the concrete URL, so the collector groups calls cleanly.
func tracingMiddleware(service, endpoint string) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		ctx, span := outboundTracer.Start(ctx, service+"/"+endpoint,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("omnicore.service", service),
				attribute.String("omnicore.endpoint", endpoint),
				attribute.String("http.request.method", req.Method),
				attribute.String("url.full", req.URL.String()),
			),
		)
		defer span.End()

		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

		resp, err := next.RoundTrip(ctx, req, obs, nil)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return resp, err
		}
		if resp != nil {
			span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
			// 5xx is a server-side failure; 4xx is a normal client outcome and
			// leaves the span status unset (not every 404 is a trace error).
			if resp.StatusCode >= 500 {
				span.SetStatus(codes.Error, resp.Status)
			}
		}
		return resp, err
	})
}
