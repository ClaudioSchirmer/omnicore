package integration

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

func TestParseEventID(t *testing.T) {
	u := uuid.New()

	t.Run("valid-header-wins", func(t *testing.T) {
		got := parseEventID(u.String(), []byte("ignored"))
		if got != u {
			t.Fatalf("header UUID must win: got %v, want %v", got, u)
		}
	})

	t.Run("16-byte-key", func(t *testing.T) {
		key := make([]byte, 16)
		copy(key, u[:])
		got := parseEventID("", key)
		if got != u {
			t.Fatalf("16-byte key must decode to UUID: got %v, want %v", got, u)
		}
	})

	t.Run("string-key-fallback", func(t *testing.T) {
		got := parseEventID("", []byte(u.String()))
		if got != u {
			t.Fatalf("string key must parse: got %v, want %v", got, u)
		}
	})

	t.Run("invalid-header-falls-through-to-key", func(t *testing.T) {
		got := parseEventID("not-a-uuid", []byte(u.String()))
		if got != u {
			t.Fatalf("invalid header should fall back to key: got %v", got)
		}
	})

	t.Run("nothing-valid-returns-nil", func(t *testing.T) {
		if got := parseEventID("", []byte("garbage")); got != uuid.Nil {
			t.Fatalf("expected uuid.Nil, got %v", got)
		}
		if got := parseEventID("", nil); got != uuid.Nil {
			t.Fatalf("expected uuid.Nil for empty inputs, got %v", got)
		}
	})
}

func TestBucketOfMessage(t *testing.T) {
	t.Run("single-worker-always-zero", func(t *testing.T) {
		if got := bucketOfMessage(transport.Message{Key: []byte("anything")}, 1); got != 0 {
			t.Fatalf("workers<=1 must bucket to 0, got %d", got)
		}
		if got := bucketOfMessage(transport.Message{Key: []byte("x")}, 0); got != 0 {
			t.Fatalf("workers 0 must bucket to 0, got %d", got)
		}
	})

	t.Run("in-range", func(t *testing.T) {
		const workers = 4
		for _, key := range [][]byte{[]byte("a"), []byte("bbbb"), []byte("zzzzzz"), {0xff, 0xff, 0xff}, nil} {
			got := bucketOfMessage(transport.Message{Key: key}, workers)
			if got < 0 || got >= workers {
				t.Fatalf("bucket %d out of range [0,%d) for key %v", got, workers, key)
			}
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		msg := transport.Message{Key: []byte("aggregate-123")}
		a := bucketOfMessage(msg, 8)
		b := bucketOfMessage(msg, 8)
		if a != b {
			t.Fatalf("same key must bucket deterministically: %d vs %d", a, b)
		}
	})

	t.Run("empty-key-bucket-zero", func(t *testing.T) {
		if got := bucketOfMessage(transport.Message{Key: nil}, 8); got != 0 {
			t.Fatalf("empty key must bucket to 0, got %d", got)
		}
	})
}
