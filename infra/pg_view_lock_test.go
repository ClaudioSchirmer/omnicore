package infra

import (
	"strings"
	"testing"
)

func TestViewLockKey_Deterministic(t *testing.T) {
	a := ViewLockKey("users")
	b := ViewLockKey("users")
	if a != b {
		t.Fatalf("ViewLockKey not deterministic: %d vs %d", a, b)
	}
}

func TestViewLockKey_PerViewKeyDiffers(t *testing.T) {
	a := ViewLockKey("users")
	b := ViewLockKey("orders")
	if a == b {
		t.Error("ViewLockKey collides across distinct view names")
	}
}

func TestViewLockKey_PrefixIsolatesFromConsumerKeys(t *testing.T) {
	// A consumer that hashes a bare "users" string for its own advisory
	// lock must NOT collide with the framework's key for the same view
	// name. The prefix is what enforces this.
	frameworkKey := ViewLockKey("users")
	// Recompute without prefix — this is what a naive consumer might do.
	bareKey := bareFNV64a("users")
	if frameworkKey == bareKey {
		t.Error("framework key matches a bare-FNV consumer hash — prefix isolation broken")
	}
}

func bareFNV64a(s string) int64 {
	// Replicates fnv.New64a().Write([]byte(s)) → Sum64() WITHOUT the
	// framework prefix. Used in the prefix-isolation test only.
	h := fnvNew64a()
	h.write([]byte(s))
	return int64(h.sum64())
}

// Minimal stand-in for hash/fnv so the test does not depend on the same
// import that the production code uses. Same FNV-1a constants.

type fnv64a struct{ h uint64 }

func fnvNew64a() *fnv64a {
	return &fnv64a{h: 0xcbf29ce484222325}
}

func (f *fnv64a) write(p []byte) {
	for _, b := range p {
		f.h ^= uint64(b)
		f.h *= 0x100000001b3
	}
}

func (f *fnv64a) sum64() uint64 { return f.h }

func TestSplitAdvisoryKey_RoundTrip(t *testing.T) {
	// Splitting the int64 key into the (classid, objid) pair used in
	// pg_locks must be deterministic — reassembling them yields the
	// original key. Both halves are uint32 (matching the oid columns in
	// pg_locks), so the reassembly is bit-shift + OR with no sign-extension
	// games.
	cases := []int64{0, 1, -1, 1 << 31, 1<<31 - 1, ViewLockKey("users")}
	for _, k := range cases {
		classid, objid := splitAdvisoryKey(k)
		reassembled := int64((uint64(classid) << 32) | uint64(objid))
		if reassembled != k {
			t.Errorf("splitAdvisoryKey(%d) → (%d, %d) round-trip = %d", k, classid, objid, reassembled)
		}
	}
}

func TestSQLConstants_PointAtAdvisoryFunctions(t *testing.T) {
	if !strings.Contains(sqlTryAdvisoryLock, "pg_try_advisory_lock") {
		t.Error("sqlTryAdvisoryLock does not call pg_try_advisory_lock")
	}
	if !strings.Contains(sqlAdvisoryUnlock, "pg_advisory_unlock") {
		t.Error("sqlAdvisoryUnlock does not call pg_advisory_unlock")
	}
	if !strings.Contains(sqlReadViewLockHolder, "pg_locks") || !strings.Contains(sqlReadViewLockHolder, "pg_stat_activity") {
		t.Error("sqlReadViewLockHolder must join pg_locks and pg_stat_activity")
	}
	if !strings.Contains(sqlReadViewLockHolder, "'advisory'") {
		t.Error("sqlReadViewLockHolder must scope to advisory locks (locktype = 'advisory')")
	}
}
