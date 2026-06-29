//go:build mysql

package mysql

import (
	"strings"
	"testing"
)

// TestRebuildLockName mirrors the Postgres viewLockKey unit tests on the MySQL
// named-lock side: the derived GET_LOCK name must be deterministic, distinct per
// view, framework-namespaced (so a consumer's own GET_LOCK on a bare view name
// can never collide with the framework's rebuild lock), and within MySQL's
// 64-char lock-name limit. The acquire/release/holder helpers need a live
// *sql.Conn and are covered by the MySQL integration suite; rebuildLockName is
// pure and is the load-bearing identity, so it is locked here.
func TestRebuildLockName(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		if rebuildLockName("users") != rebuildLockName("users") {
			t.Error("rebuildLockName is not deterministic")
		}
	})

	t.Run("distinct per view", func(t *testing.T) {
		if rebuildLockName("users") == rebuildLockName("orders") {
			t.Error("rebuildLockName collides across distinct view names")
		}
	})

	t.Run("framework-namespaced (isolates from a bare consumer lock)", func(t *testing.T) {
		name := rebuildLockName("users")
		if !strings.HasPrefix(name, "omcv_") {
			t.Errorf("rebuildLockName(%q) = %q, want the omcv_ prefix", "users", name)
		}
		// A naive consumer taking GET_LOCK('users') uses the bare name; the
		// framework's prefixed name must never equal it.
		if name == "users" {
			t.Error("framework lock name equals a bare consumer name — isolation broken")
		}
	})

	t.Run("within MySQL's 64-char lock-name limit", func(t *testing.T) {
		// Even a long view name hashes to a fixed-width name.
		long := strings.Repeat("very_long_view_name_", 10)
		if got := rebuildLockName(long); len(got) > 64 {
			t.Errorf("rebuildLockName length = %d (%q), exceeds MySQL's 64-char limit", len(got), got)
		}
	})
}
