//go:build oracle

package oracle

import (
	"context"
	"database/sql"
	"errors"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"testing"
	"time"
)

// frozenExecutor simulates the go-ora behavior this engine defends against: a
// driver call that BLOCKS ignoring the context (the real driver's
// ctx→read-deadline plumbing is commented out upstream, so against a frozen
// database the call returns only when the network thaws). Calls block until
// release is closed.
type frozenExecutor struct{ release chan struct{} }

func (f frozenExecutor) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	<-f.release
	return nil, errors.New("thawed")
}
func (f frozenExecutor) QueryRowContext(context.Context, string, ...any) *sql.Row {
	<-f.release
	return nil
}
func (f frozenExecutor) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	<-f.release
	return nil, errors.New("thawed")
}

// TestQuerier_ContextBoundedWaits proves the deadline promise this engine adds
// on top of the driver: every Querier method returns ctx.Err() the moment the
// context expires, even when the underlying driver call never comes back —
// the readiness probe's 2s answer and the request-timeout 504 depend on it.
// The abandoned goroutine finishes on its own after the "thaw" (the closed
// release channel) — the test releases it and only asserts the early return.
func TestQuerier_ContextBoundedWaits(t *testing.T) {
	frozen := frozenExecutor{release: make(chan struct{})}
	defer close(frozen.release) // thaw at test end so the goroutines finish
	q := oracleQuerier{exec: frozen}

	deadline := 50 * time.Millisecond
	limit := 2 * time.Second // generous guard: an unbounded call would block forever

	t.Run("Query returns ctx.Err() on expiry", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		start := time.Now()
		_, err := q.Query(ctx, "SELECT 1 FROM dual")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Query err = %v, want context.DeadlineExceeded", err)
		}
		if time.Since(start) > limit {
			t.Fatalf("Query blocked %s past its deadline", time.Since(start))
		}
	})

	t.Run("QueryRow's Scan returns ctx.Err() on expiry", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		start := time.Now()
		var one int
		err := q.QueryRow(ctx, "SELECT 1 FROM dual").Scan(&one)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("QueryRow.Scan err = %v, want context.DeadlineExceeded", err)
		}
		if time.Since(start) > limit {
			t.Fatalf("QueryRow.Scan blocked %s past its deadline", time.Since(start))
		}
	})

	t.Run("Exec returns ctx.Err() on expiry", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		start := time.Now()
		err := core.Exec(q, ctx, "DELETE FROM t")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Exec err = %v, want context.DeadlineExceeded", err)
		}
		if time.Since(start) > limit {
			t.Fatalf("Exec blocked %s past its deadline", time.Since(start))
		}
	})

	t.Run("QueryMaps returns ctx.Err() on expiry", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		start := time.Now()
		_, err := q.QueryMaps(ctx, "SELECT * FROM t")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("QueryMaps err = %v, want context.DeadlineExceeded", err)
		}
		if time.Since(start) > limit {
			t.Fatalf("QueryMaps blocked %s past its deadline", time.Since(start))
		}
	})
}

// TestQuerier_CompletedCallsPassThrough proves the wrapper is transparent when
// the driver answers before the deadline: errors surface verbatim (no
// swallowed results on the happy path).
func TestQuerier_CompletedCallsPassThrough(t *testing.T) {
	released := frozenExecutor{release: make(chan struct{})}
	close(released.release) // never blocks
	q := oracleQuerier{exec: released}
	ctx := context.Background()

	if _, err := q.Query(ctx, "SELECT 1"); err == nil || err.Error() != "thawed" {
		t.Fatalf("Query err = %v, want the driver error verbatim", err)
	}
	if err := core.Exec(q, ctx, "DELETE FROM t"); err == nil || err.Error() != "thawed" {
		t.Fatalf("Exec err = %v, want the driver error verbatim", err)
	}
	if _, err := q.QueryMaps(ctx, "SELECT 1"); err == nil || err.Error() != "thawed" {
		t.Fatalf("QueryMaps err = %v, want the driver error verbatim", err)
	}
	var one int
	if err := q.QueryRow(ctx, "SELECT 1").Scan(&one); err == nil || err.Error() != "thawed" {
		t.Fatalf("QueryRow.Scan err = %v, want the driver error verbatim", err)
	}
}
