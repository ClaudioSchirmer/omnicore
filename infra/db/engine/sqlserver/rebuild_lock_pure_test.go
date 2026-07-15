//go:build sqlserver

package sqlserver

import (
	"strings"
	"testing"
)

// TestRebuildLockName mirrors the MySQL/Postgres lock-name unit tests on the
// SQL Server applock side: the derived sp_getapplock resource name must be
// deterministic, distinct per view, framework-namespaced (so a consumer's own
// sp_getapplock on a bare view name can never collide with the framework's
// rebuild lock), and within SQL Server's 255-char resource limit. The
// acquire/release/holder helpers need a live *sql.Conn and are covered by the
// integration suite; rebuildLockName is pure and is the load-bearing identity.
func TestRebuildLockName(t *testing.T) {
	t.Run("deterministic", func(t *testing.T) {
		if rebuildLockName("users") != rebuildLockName("users") { //nolint:staticcheck // SA4000: deliberate self-comparison — asserts rebuildLockName is deterministic across calls.
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
		if name == "users" {
			t.Error("framework lock name equals a bare consumer name — isolation broken")
		}
	})

	t.Run("within SQL Server's 255-char resource limit", func(t *testing.T) {
		long := strings.Repeat("very_long_view_name_", 20)
		if got := rebuildLockName(long); len(got) > 255 {
			t.Errorf("rebuildLockName length = %d (%q), exceeds the 255-char limit", len(got), got)
		}
	})
}
