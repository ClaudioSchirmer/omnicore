package httpclient

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// observationLogger is the message label used by the slog record. Constant so
// dashboards and grep patterns can pin the line type.
const observationLogger = "http.outbound"

// observation accumulates the per-call slog fields. The struct is populated
// at distinct points during Call (build, dial, response read, decode) and
// emitted exactly once at the end so every outbound call shows up as a
// single record in the operator-visible log.
//
// Fields reserved for future phases (cacheStatus, breakerState, authProvider,
// attempt) are emitted today with stable defaults so dashboards can already
// rely on the shape and switch logic survives the dedicated phases.
type observation struct {
	ThreadID           string
	DownstreamThreadID string
	Service            string
	Endpoint           string
	Method             string
	URL                string
	RequestHeaders     http.Header
	RequestBody        []byte
	Status             int
	ResponseHeaders    http.Header
	ResponseBody       []byte
	DurationMS         int64
	RequestBytes       int
	ResponseBytes      int
	Attempt            int    // always 1 in the current phase (no retry)
	CacheStatus        string // "bypass" in the current phase (no cache)
	BreakerState       string // "closed" in the current phase (no breaker)
	AuthProvider       string // "" in the current phase (no auth)
	IdempotencyKey     string // populated by the idempotency middleware when active
	LogBodies          bool
	Err                error
	Started            time.Time
	// threadIDHeader is the per-service threadIdHeader name, copied here so
	// the logging middleware can look up the downstream echo without holding
	// a serviceClient reference.
	threadIDHeader string

	// noCache mirrors CallConfig.NoCache. The cache middleware
	// short-circuits when true.
	noCache bool

	// cacheKey overrides the computed key when set via CallConfig.CacheKey.
	// Empty means the middleware computes the key from the request shape.
	cacheKey string

	// idempotencyKey mirrors CallConfig.IdempotencyKey. Read by
	// the idempotency middleware to decide between generating a UUIDv7
	// (source=ctx) or rejecting (source=explicit) when missing.
	idempotencyKey string

	// redactionPolicy is the per-service cascade policy consulted by the
	// logging middleware when it emits the slog record.
	redaction *redactionPolicy

	// streamingRequest tells the logging middleware to skip request body
	// capture — set when the request struct carries a body,stream or
	// body,multipart tag. The body is not buffered into memory; only
	// status and byte counts reach the slog record.
	streamingRequest bool

	// streamingResponse tells the logging middleware to skip response
	// body capture — set when the endpoint declares responseStream: true
	// or responseSSE: true. The body stays open for the caller; logging
	// records status, headers, and ContentLength only.
	streamingResponse bool
}

// newObservation seeds an observation with the immutable per-call fields
// known at request-build time. Subsequent helpers (recordResponse, recordErr)
// fill in the rest.
func newObservation(service, endpoint, method string, logBodies bool) *observation {
	return &observation{
		Service:      service,
		Endpoint:     endpoint,
		Method:       method,
		Attempt:      1,
		CacheStatus:  "bypass",
		BreakerState: "closed",
		LogBodies:    logBodies,
		Started:      time.Time{},
	}
}

// emit writes the accumulated record via the supplied logger. Bodies are
// suppressed when LogBodies is false; only the byte counts then survive.
// Header maps are redacted in place via redactHeaders so dashboards never
// observe a raw Authorization value.
func (o *observation) emit(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	urlForLog := o.URL
	var bodyPaths []string
	var queryKeys map[string]struct{}
	if o.redaction != nil {
		bodyPaths = o.redaction.bodyJSONPaths
		queryKeys = o.redaction.queryKeys
	}
	urlForLog = redactURL(urlForLog, queryKeys)
	attrs := []slog.Attr{
		slog.String("threadId", o.ThreadID),
		slog.String("service", o.Service),
		slog.String("endpoint", o.Endpoint),
		slog.String("method", o.Method),
		slog.String("url", urlForLog),
		slog.Int("status", o.Status),
		slog.Int64("durationMs", o.DurationMS),
		slog.Int("requestBytes", o.RequestBytes),
		slog.Int("responseBytes", o.ResponseBytes),
		slog.Int("attempt", o.Attempt),
		slog.String("cacheStatus", o.CacheStatus),
		slog.String("breakerState", o.BreakerState),
		slog.String("authProvider", o.AuthProvider),
	}
	if o.DownstreamThreadID != "" {
		attrs = append(attrs, slog.String("downstreamThreadId", o.DownstreamThreadID))
	}
	if o.IdempotencyKey != "" {
		attrs = append(attrs, slog.String("idempotencyKey", o.IdempotencyKey))
	}
	if rh := redactHeaders(o.RequestHeaders, o.redaction); len(rh) > 0 {
		attrs = append(attrs, slog.Any("requestHeaders", rh))
	}
	if rh := redactHeaders(o.ResponseHeaders, o.redaction); len(rh) > 0 {
		attrs = append(attrs, slog.Any("responseHeaders", rh))
	}
	if o.LogBodies {
		if len(o.RequestBody) > 0 {
			attrs = append(attrs, slog.String("requestBody", string(redactBody(o.RequestBody, bodyPaths))))
		}
		if len(o.ResponseBody) > 0 {
			attrs = append(attrs, slog.String("responseBody", string(redactBody(o.ResponseBody, bodyPaths))))
		}
	}
	level := slog.LevelInfo
	if o.Err != nil {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("err", o.Err.Error()))
	}
	logger.LogAttrs(ctx, level, observationLogger, attrs...)
}
