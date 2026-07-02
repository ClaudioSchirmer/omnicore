package core

import (
	"sort"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// SortedKeys returns the column keys of a domain.Fields map in deterministic
// (lexicographic) order. domain.Fields is a map, so its iteration order is
// random; every engine builds its INSERT/UPDATE column list and bound-arg order
// from this single helper so the generated SQL is stable and the two backends
// never diverge on column ordering (see the non-deterministic-map pitfall).
func SortedKeys(fields domain.Fields) []string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
