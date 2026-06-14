package httpclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/auth"
)

// roundTripper is the chain unit consumed by Call. It mirrors
// http.RoundTripper but additionally threads the per-call observation so
// middleware can record state (cache hit, breaker decision, retry attempt
// count) into the single slog record emitted at the end of the call.
//
// A roundTripper either fully handles the request (the terminal transport)
// or delegates to next. Short-circuiting middleware (for example a cache
// hit) returns without calling next; the chain stops at the first short-
// circuit.
type roundTripper interface {
	RoundTrip(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error)
}

// rtFunc adapts an ordinary function to roundTripper. Used for the common
// case where a middleware is stateless after construction.
type rtFunc func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error)

// RoundTrip satisfies roundTripper.
func (f rtFunc) RoundTrip(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
	return f(ctx, req, obs, next)
}

// chain composes the layers in the order they were supplied. The ordering is
// the canonical chain from the design — correlation outermost, transport
// terminal — and is NOT exposed to consumers. Future phases insert their
// middleware at the documented position; the order remains a framework
// concern, never a service or per-call concern.
type chain struct {
	layers []roundTripper
}

// newChain builds a chain from the supplied layers. The first layer is the
// outermost; the last layer must be terminal (typically transportMiddleware).
func newChain(layers ...roundTripper) *chain {
	cp := make([]roundTripper, len(layers))
	copy(cp, layers)
	return &chain{layers: cp}
}

// dispatch executes the chain. Each layer receives a `next` reference that
// points at the chain starting one position later. The final layer receives
// a terminal sentinel that returns an error if invoked (it never is, by
// construction — the terminal layer handles the request itself).
func (c *chain) dispatch(ctx context.Context, req *http.Request, obs *observation) (*http.Response, error) {
	if c == nil || len(c.layers) == 0 {
		return nil, fmt.Errorf("httpclient: empty middleware chain")
	}
	return invokeAt(c.layers, 0, ctx, req, obs)
}

// invokeAt walks the layers slice without allocating per-call closures.
// Each layer's next is a recursive call into invokeAt(idx+1). Index out of
// range yields the terminal error so callers detect a non-terminal final
// layer at boot time of the call (in practice that never happens — every
// chain ends with transportMiddleware).
func invokeAt(layers []roundTripper, idx int, ctx context.Context, req *http.Request, obs *observation) (*http.Response, error) {
	if idx >= len(layers) {
		return nil, fmt.Errorf("httpclient: chain reached end without terminal layer")
	}
	next := rtFunc(func(ctx context.Context, req *http.Request, obs *observation, _ roundTripper) (*http.Response, error) {
		return invokeAt(layers, idx+1, ctx, req, obs)
	})
	return layers[idx].RoundTrip(ctx, req, obs, next)
}

// correlationMiddleware injects the configured thread / request id headers
// on the outbound request. It populates obs.ThreadID for the slog record
// when AppContext carries one. Empty header names disable injection.
func correlationMiddleware(svc *serviceClient) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		id := correlationID(ctx)
		obs.ThreadID = id
		obs.threadIDHeader = svc.threadID
		policy := svc.redaction
		obs.redaction = &policy
		if id != "" {
			if svc.threadID != "" && req.Header.Get(svc.threadID) == "" {
				req.Header.Set(svc.threadID, id)
			}
			if svc.requestID != "" && req.Header.Get(svc.requestID) == "" {
				req.Header.Set(svc.requestID, id)
			}
		}
		return next.RoundTrip(ctx, req, obs, nil)
	})
}

// loggingMiddleware captures the request body and emits the observation
// after next returns. It sits just inside the correlation layer so it
// observes the headers correlation has already injected.
//
// Streaming endpoints skip body capture:
//   - obs.streamingRequest = true → request body is NOT read into
//     obs.RequestBody (avoids buffering a multi-GB upload into memory).
//     This also disables retry replay since the original io.Reader is
//     handed to the transport unchanged.
//   - obs.streamingResponse = true → response body is NOT read into
//     obs.ResponseBody. The body is left open for the caller (the Call
//     path then assembles a StreamResponse or pumps SSE events).
//
// In both streaming modes the slog record still carries status, headers,
// timing, and the ContentLength reported by the upstream so the operator
// sees the call happened even though the bytes were never visible to
// logging.
func loggingMiddleware(logger *slog.Logger) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, next roundTripper) (*http.Response, error) {
		captureRequestObservation(obs, req)
		obs.Started = time.Now()

		resp, err := next.RoundTrip(ctx, req, obs, nil)
		obs.DurationMS = time.Since(obs.Started).Milliseconds()

		if err != nil {
			obs.Err = err
			obs.emit(ctx, logger)
			return nil, err
		}

		obs.Status = resp.StatusCode
		obs.ResponseHeaders = resp.Header
		if obs.ThreadID != "" && obs.threadIDHeader != "" {
			if v := resp.Header.Get(obs.threadIDHeader); v != "" && v != obs.ThreadID {
				obs.DownstreamThreadID = v
			}
		}

		if obs.streamingResponse {
			// Body stays open for the caller; record only what we know
			// without consuming bytes. ContentLength may be -1 when the
			// upstream uses chunked transfer.
			if resp.ContentLength > 0 {
				obs.ResponseBytes = int(resp.ContentLength)
			}
			obs.emit(ctx, logger)
			return resp, nil
		}

		bodyBytes, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			obs.Err = readErr
			obs.emit(ctx, logger)
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		obs.ResponseBody = bodyBytes
		obs.ResponseBytes = len(bodyBytes)
		obs.emit(ctx, logger)
		return resp, nil
	})
}

// transportMiddleware is the terminal layer that dials. It uses the per-
// service http.Client so each upstream sees the timeout / pool tuning
// configured for it.
func transportMiddleware(svc *serviceClient) roundTripper {
	return rtFunc(func(ctx context.Context, req *http.Request, obs *observation, _ roundTripper) (*http.Response, error) {
		return svc.httpClient.Do(req)
	})
}

// buildChain composes the canonical middleware chain for a service call.
// Order matches the design — correlation outermost, transport terminal —
// with explicit insertion points for the phases that have not yet shipped.
//
// The order is intentionally hard-coded: there is one canonical path per
// the framework rule, and middlewares cannot be reordered by the consumer.
// Future phases register their layer at the documented position without
// changing the surrounding ones.
func buildChain(svc *serviceClient, serviceName, endpointName string, ep endpointSpec, effectiveRetry retryPolicy, store Cache, breaker *breakerState, provider auth.AuthProvider, revocationOnUnauthorized bool, logger *slog.Logger) *chain {
	layers := []roundTripper{
		correlationMiddleware(svc), // 1
		loggingMiddleware(logger),  // 2
	}
	if provider != nil {
		layers = append(layers, authMiddleware(provider, revocationOnUnauthorized)) // 3
	}
	if ep.idempotency.enabled {
		layers = append(layers, idempotencyMiddleware(ep.idempotency)) // 4 (idempotency runs before signing so the key can be in signedHeaders)
	}
	if store != nil && ep.cache.enabled {
		layers = append(layers, cacheMiddleware(serviceName, endpointName, store, ep.cache, ep.cacheAcceptable, ep.acceptableStatus)) // 5
	}
	// retry (outer) → breaker (inner) → signing (innermost before
	// transport). Reasoning:
	//   - Q7 of the design's open questions: each retry attempt counts as
	//     a breaker observation; breaker must sit INSIDE retry.
	//   - Phase 6: signing must sit INSIDE retry so every attempt gets a
	//     fresh timestamp + content-sha256 + signature; INSIDE breaker so
	//     a rejected attempt does not waste signing work; OUTSIDE
	//     transport so the request that actually dials has the headers.
	// Cache hits (short-circuit at position 5) skip both breaker and
	// signing — correct, because no dial happens.
	layers = append(layers,
		retryMiddleware(effectiveRetry),                // 6 (outer of breaker)
		breakerMiddleware(breaker),                     // 7 (inner of retry)
		signingMiddleware(svc.signing, nil),            // 8 (inner of breaker; innermost before transport)
		transportMiddleware(svc),                       // 9 — terminal
	)
	return newChain(layers...)
}

