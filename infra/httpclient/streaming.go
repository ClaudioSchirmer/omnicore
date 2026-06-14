package httpclient

import (
	"io"
	"net/http"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/binding"
)

// Multipart is the request body shape for endpoints expecting
// multipart/form-data uploads. Place an instance on a request struct
// field tagged http:"body,multipart" and the framework will pipe text
// fields and file streams into a multipart writer, producing a request
// with the correct Content-Type (including boundary) and (when file
// sizes are unknown) chunked transfer encoding.
//
//	type UploadDocRequest struct {
//	    UserID string             `http:"path,id"`
//	    Body   httpclient.Multipart `http:"body,multipart"`
//	}
//	req := UploadDocRequest{
//	    UserID: "u42",
//	    Body: httpclient.Multipart{
//	        Fields: []httpclient.MultipartField{{Name: "category", Value: "id-proof"}},
//	        Files: []httpclient.MultipartFile{{
//	            Name: "file", Filename: "passport.pdf", MimeType: "application/pdf",
//	            Content: openedFile,
//	        }},
//	    },
//	}
//
// Endpoints that upload a multipart body are subject to the streaming
// constraints: retry is disabled (the file readers are one-shot), the
// logging middleware does not capture the request body, and signing is
// rejected at boot for services that combine signing with multipart
// endpoints.
type Multipart = binding.Multipart

// MultipartField re-exports binding.MultipartField for consumer ergonomics.
type MultipartField = binding.MultipartField

// MultipartFile re-exports binding.MultipartFile for consumer ergonomics.
type MultipartFile = binding.MultipartFile

// StreamResponse is the response surface for endpoints declared with
// responseStream: true. The framework hands the raw response body to the
// caller without decoding; the caller is responsible for reading the body
// and closing it.
//
// Typical use is downloading PDFs, large CSV exports, file contents, or
// any payload where holding the bytes in memory would be wasteful. The
// caller can stream the body directly to disk, to another HTTP request,
// or to any io.Writer without ever buffering it.
//
//	type DownloadReceiptRequest struct {
//	    ID string `http:"path,id"`
//	}
//	resp, err := httpclient.Call[DownloadReceiptRequest, httpclient.StreamResponse](
//	    ctx, c, "pay", "downloadReceipt",
//	    DownloadReceiptRequest{ID: "ch_42"},
//	)
//	if err != nil { return err }
//	defer resp.Body.Close()
//	_, err = io.Copy(out, resp.Body)
//
// The framework still runs the full middleware chain — auth, retry,
// breaker, signing — and the slog observation is emitted with status,
// headers, and byte counts. The body itself is not captured for logging
// (it would defeat streaming). Cache and SSE are mutually exclusive with
// responseStream and rejected at boot.
type StreamResponse struct {
	// Body is the open response body. The framework does NOT close it;
	// the caller must call Close() when done.
	Body io.ReadCloser

	// ContentType is the response's Content-Type header verbatim, when
	// present. Empty when the upstream omitted it.
	ContentType string

	// ContentLength carries the upstream-declared length. -1 when the
	// upstream did not declare one (chunked transfer or unknown).
	ContentLength int64

	// Headers is a clone of the response headers so the caller can read
	// metadata without keeping the *http.Response alive.
	Headers http.Header
}

// SSEResponse is the response surface for endpoints declared with
// responseSSE: true. The framework spawns a goroutine that parses the
// upstream's text/event-stream body and emits each event on the Events
// channel. The caller MUST call Close() when done to stop the goroutine
// and release the underlying connection.
//
// Reconnection is the caller's responsibility — the framework does not
// re-dial when the stream ends. The upstream's `retry:` field, when sent,
// is surfaced on the SSEvent so the caller can honor it.
type SSEResponse struct {
	// Events is the channel of parsed events. Closed when the stream
	// ends (either naturally or on Close()).
	Events <-chan SSEvent

	// Close stops the parser goroutine and releases the underlying
	// connection. Safe to call multiple times; subsequent calls are
	// no-ops.
	Close func() error
}

// SSEvent is one parsed event from the upstream SSE stream. Fields map
// directly to the WHATWG EventSource specification (id, event, data,
// retry). Multi-line data: fields are joined with a single newline.
type SSEvent struct {
	// ID is the last id: field's value seen for this event. Empty when
	// none was set.
	ID string

	// Event is the event: field's value. Defaults to "message" when the
	// upstream did not declare one — matches the EventSource default.
	Event string

	// Data is the joined data: field payload (one or more lines joined
	// by \n). The bytes are surfaced verbatim — no JSON decoding, no
	// trimming — so the caller chooses how to interpret them.
	Data []byte

	// Retry is the upstream-suggested retry hint when a retry: field
	// was sent on the event. Zero when no hint was present.
	Retry time.Duration
}
