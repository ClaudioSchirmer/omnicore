package httpclient

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sseEventBufferSize is the channel buffer size for the SSEResponse.Events
// channel. A small buffer lets the parser get ahead of slow consumers
// without unbounded memory growth.
const sseEventBufferSize = 16

// sseParserState holds the per-stream fields the WHATWG EventSource
// algorithm accumulates between events. The state lives in the goroutine
// stack — there is no shared access from outside the pump.
type sseParserState struct {
	eventType string
	dataLines []string
	lastID    string
	retryMS   int
}

// startSSEPump launches a goroutine that reads the upstream's
// text/event-stream body line by line per the WHATWG EventSource spec,
// emits parsed SSEvents on a channel, and shuts down cleanly when the
// caller's context is canceled or Close is invoked.
//
// The returned SSEResponse exposes:
//
//   - Events: a buffered receive-only channel. Closed when the stream
//     ends naturally, the parser hits an io error, the caller calls
//     Close, or ctx is done.
//   - Close: a synchronous stopper. Safe to call multiple times. Returns
//     the error from closing the upstream body (typically nil).
//
// Cancellation cascade: ctx.Done() triggers Close(); Close() closes the
// body which forces the underlying Read to return io.EOF / net.OpError;
// the parser detects the error and closes Events; the goroutine exits.
func startSSEPump(ctx context.Context, body io.ReadCloser) SSEResponse {
	events := make(chan SSEvent, sseEventBufferSize)
	var (
		closeOnce sync.Once
		closeErr  error
	)
	closeFn := func() error {
		closeOnce.Do(func() {
			closeErr = body.Close()
		})
		return closeErr
	}

	go func() {
		defer close(events)
		defer closeFn()

		// Context-driven shutdown: a separate goroutine waits on
		// ctx.Done and triggers Close, which unblocks the bufio Read.
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = closeFn()
			case <-watchDone:
			}
		}()
		defer close(watchDone)

		reader := bufio.NewReader(body)
		st := sseParserState{}
		dispatch := func() {
			if len(st.dataLines) == 0 {
				return
			}
			etype := st.eventType
			if etype == "" {
				etype = "message"
			}
			ev := SSEvent{
				ID:    st.lastID,
				Event: etype,
				Data:  []byte(strings.Join(st.dataLines, "\n")),
			}
			if st.retryMS > 0 {
				ev.Retry = time.Duration(st.retryMS) * time.Millisecond
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
			// Reset per-event fields. Per spec id and retry persist
			// across events until explicitly overwritten by the upstream.
			st.eventType = ""
			st.dataLines = st.dataLines[:0]
		}

		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				// Strip the trailing newline; tolerate \r\n by stripping
				// the carriage return too.
				line = strings.TrimRight(line, "\n")
				line = strings.TrimRight(line, "\r")
				if line == "" {
					dispatch()
				} else {
					handleSSELine(line, &st)
				}
			}
			if err != nil {
				if err == io.EOF {
					dispatch()
				}
				return
			}
		}
	}()

	return SSEResponse{
		Events: events,
		Close:  closeFn,
	}
}

// handleSSELine applies one non-empty SSE line per the WHATWG
// EventSource parsing algorithm. Lines starting with ":" are comments
// and ignored. Lines without a ":" are treated as a field with an
// empty value. Otherwise the prefix up to the first ":" is the field
// name and the rest (with one leading space stripped) is the value.
func handleSSELine(line string, st *sseParserState) {
	if strings.HasPrefix(line, ":") {
		return
	}
	var field, value string
	if idx := strings.IndexByte(line, ':'); idx >= 0 {
		field = line[:idx]
		value = line[idx+1:]
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
	} else {
		field = line
	}
	switch field {
	case "event":
		st.eventType = value
	case "data":
		st.dataLines = append(st.dataLines, value)
	case "id":
		st.lastID = value
	case "retry":
		if ms, err := strconv.Atoi(value); err == nil && ms >= 0 {
			st.retryMS = ms
		}
	}
}
