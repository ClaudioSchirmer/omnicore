// Package binding implements the request/response binding layer of the
// outbound HTTP subsystem: tag parsing, type inspection, request assembly,
// response decoding, and the codec registry. The parent httpclient package
// orchestrates calls and middleware; this package is the pure transformation
// layer between typed Go values and *http.Request / *http.Response.
//
// The subpackage has no awareness of services, transports, retries, caches or
// circuit breakers. Tests cover the full transformation surface against
// in-memory http.Request / http.Response values without any networking.
package binding

// EndpointMeta is the per-endpoint metadata that BuildRequest and
// DecodeResponse need to assemble outbound requests and decode inbound
// responses. The parent httpclient package translates its private
// endpointSpec into this exported view at the Call boundary, keeping
// binding/ independent of the registry above it.
type EndpointMeta struct {
	// Method is the HTTP method, uppercased (GET/POST/PUT/PATCH/DELETE/HEAD).
	Method string

	// Path is the URL path template with {placeholders} that match path-tagged
	// fields on the request struct. The leading "/" is required.
	Path string

	// RequestCodec selects the body encoder; "json" is the only value
	// accepted today.
	RequestCodec string

	// ResponseCodec selects the body decoder; "json" today.
	ResponseCodec string

	// Headers carries the pre-merged defaults→service→endpoint header
	// cascade. The request builder applies these first, then per-field
	// header tags (tag wins on conflict).
	Headers map[string]string

	// AcceptableStatus is the set of non-error statuses the consumer
	// expects (e.g. 404 on a presence check). The binding layer does not
	// branch on this set — it is carried through so the call surface can
	// surface the right HttpError shape.
	AcceptableStatus map[int]struct{}
}
