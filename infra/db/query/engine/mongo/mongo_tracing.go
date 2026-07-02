package mongo

import (
	"context"
	"sync"

	"go.mongodb.org/mongo-driver/v2/event"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var mongoTracer = otel.Tracer("github.com/ClaudioSchirmer/omnicore/infra/mongo")

// newMongoCommandMonitor builds a CommandMonitor that wraps each Mongo command
// in a client span. mongo-driver v2 has no off-the-shelf otel contrib (the
// published otelmongo targets the v1 driver), so the framework owns this thin
// monitor. The Started callback's context carries the active span (the dispatch
// span threaded through the AppContext handed to the ViewReader), so each
// command span attaches under it. Spans are matched start→finish by RequestID.
func newMongoCommandMonitor() *event.CommandMonitor {
	var mu sync.Mutex
	inflight := make(map[int64]trace.Span)

	return &event.CommandMonitor{
		Started: func(ctx context.Context, e *event.CommandStartedEvent) {
			_, span := mongoTracer.Start(ctx, "mongo."+e.CommandName,
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(
					attribute.String("db.system", "mongodb"),
					attribute.String("db.operation", e.CommandName),
					attribute.String("db.namespace", e.DatabaseName),
				),
			)
			mu.Lock()
			inflight[e.RequestID] = span
			mu.Unlock()
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if span := takeSpan(&mu, inflight, e.RequestID); span != nil {
				span.End()
			}
		},
		Failed: func(_ context.Context, e *event.CommandFailedEvent) {
			if span := takeSpan(&mu, inflight, e.RequestID); span != nil {
				span.RecordError(e.Failure)
				span.SetStatus(codes.Error, "mongo command failed")
				span.End()
			}
		},
	}
}

func takeSpan(mu *sync.Mutex, m map[int64]trace.Span, id int64) trace.Span {
	mu.Lock()
	defer mu.Unlock()
	span := m[id]
	delete(m, id)
	return span
}
