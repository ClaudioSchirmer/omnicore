package cache

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSetJSON_MarshalError exercises the encode-failure branch: a channel
// value cannot be JSON-marshalled, so SetJSON wraps the json.Marshal error
// before reaching the backend.
func TestSetJSON_MarshalError(t *testing.T) {
	c := NewMemory(0)
	err := SetJSON(context.Background(), c, "bad", make(chan int), time.Minute)
	if err == nil || !strings.Contains(err.Error(), "encode") {
		t.Fatalf("expected encode error for unmarshalable value, got %v", err)
	}
}
