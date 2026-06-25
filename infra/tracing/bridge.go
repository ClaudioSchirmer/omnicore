package tracing

import (
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// The framework's correlation identity is a uuid.UUID (128 bits); an OTel
// TraceID is also 128 bits. The two map byte-for-byte, which is what lets the
// framework keep a single id across the whole topology: AppContext.CorrelationID()
// is kept equal to the active span's trace_id, so logs, traces, and the
// integration_events.correlation_id column all join on one value.

// TraceIDFromUUID reinterprets a uuid.UUID as an OTel TraceID.
func TraceIDFromUUID(id uuid.UUID) trace.TraceID {
	var t trace.TraceID
	copy(t[:], id[:])
	return t
}

// UUIDFromTraceID reinterprets an OTel TraceID as a uuid.UUID — the inverse of
// TraceIDFromUUID, used by the inbound bridge to set AppContext.CorrelationID
// from the adopted trace.
func UUIDFromTraceID(t trace.TraceID) uuid.UUID {
	var id uuid.UUID
	copy(id[:], t[:])
	return id
}
