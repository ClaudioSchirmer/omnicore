package read

import (
	"fmt"
	"strings"
	"sync"
)

// A declared traversal that reaches more than one hop is not a mistake — the
// framework sets no depth limit, because how far a read reaches is the caller's
// call. On an AGGREGATE repository it is worth one sentence at boot all the same.
//
// The reason is which reads pay for it. A root chain rides EVERY read through
// the loader and a child chain rides the child's batched SELECT on every load —
// FindByID included, which is the load the write-side Auto handlers go through.
// So a chain declared for one screen is charged to every command the service
// serves, and the developer who declared it cannot see that from the call site.
// The advisory says it once, names the chain, and suggests the repository that
// carries no write path at all when the reach is only ever read.
//
// A DirectRepository never produces one, at any depth: it has no write path to
// slow down, and reading in an arbitrary shape is the whole reason it exists.
//
// WHY IT IS COLLECTED RATHER THAN LOGGED HERE. A repository is constructed
// inside a Feature's wiring, where there is no logger — the service's is built by
// the bootstrap. Writing to the default slog handler from here would emit the
// line outside the service's own log format, and possibly before its logging is
// configured at all. So the declaration records, and the bootstrap drains with
// deps.Logger, alongside the other declaration advisories it already reports.

var joinAdvisories struct {
	mu   sync.Mutex
	msgs []string
}

// recordJoinAdvisory files one advisory for the bootstrap to report. Called at
// DECLARATION time, so it runs once per repository per process — never on a read.
func recordJoinAdvisory(msg string) {
	joinAdvisories.mu.Lock()
	defer joinAdvisories.mu.Unlock()
	joinAdvisories.msgs = append(joinAdvisories.msgs, msg)
}

// DrainJoinAdvisories returns every advisory filed since the last drain and
// clears the buffer. The bootstrap calls it once, after the features have wired
// their repositories, and logs what comes back.
//
// Draining rather than reading keeps a second boot in the same process (tests,
// an embedded host) from reporting the first boot's chains again.
func DrainJoinAdvisories() []string {
	joinAdvisories.mu.Lock()
	defer joinAdvisories.mu.Unlock()
	out := joinAdvisories.msgs
	joinAdvisories.msgs = nil
	return out
}

// reportDeepChains files one advisory per declared chain that reaches beyond the
// first hop — one per CHAIN, not per hop, so a chain four deep says its piece
// once rather than three times.
//
// A chain that hangs off a child is not a separate case: it costs the same on
// every load, so it reports through the same path, with the same threshold and
// the same sentence. Only the table the chain departs from differs.
func reportDeepChains(contextName string, root *TableSchema, joins []Join) {
	for _, j := range joins {
		if len(j.Through) == 0 {
			continue
		}
		anchor := root.Table()
		if j.Child != nil {
			anchor = j.Child.Table()
		}
		path, depth := deepestChain(j)
		recordJoinAdvisory(fmt.Sprintf(
			"read.WithJoins[%s]: chain of depth %d (%s) from %q on an AGGREGATE repository — these "+
				"joins are in EVERY read through this loader, FindByID included, which is the load the "+
				"write-side Auto handlers go through. If the reach is only ever read, a "+
				"read.NewDirectRepository over the same tables carries no write path and costs the "+
				"commands nothing.",
			contextName, depth, strings.Join(path, " → "), anchor))
	}
}

// deepestChain names the longest path through a declared chain and how many hops
// it has — what the advisory quotes, so the developer can find the declaration by
// reading the message.
func deepestChain(j Join) ([]string, int) {
	best := []string{j.FKColumn}
	for _, hop := range j.Through {
		if path, _ := deepestChain(hop); len(path)+1 > len(best) {
			best = append([]string{j.FKColumn}, path...)
		}
	}
	return best, len(best)
}
