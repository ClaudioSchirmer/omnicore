package migration

import "embed"

// Framework migrations live under embedded/<dialect>/ — one flattened migration
// sequence per dialect (postgres, mysql, sqlserver, oracle, sqlite) describing
// the same logical control-plane schema.
//
// The embedded FS is BUILD-TAG-GATED and self-registering: each dialect's
// embed_<dialect>.go (//go:build <dialect>) embeds only its own SQL and
// registers it here in init() — the same pattern as the engine bindings and the
// migration runners. A build links exactly the dialects its tags select, so
// adding an engine is purely additive (a new embed_<dialect>.go file, no edit to
// a shared hardcoded //go:embed line), and each binary embeds only the SQL of the
// dialect it linked. frameworkFS is read by frameworkSourceFor (sources.go).
var frameworkFS = map[string]embed.FS{}

// registerFrameworkFS records a dialect's embedded framework migrations. Called
// from a tag-gated embed_<dialect>.go init(). A dialect maps to exactly one
// embed file, so there is never a real duplicate.
func registerFrameworkFS(dialect string, fsys embed.FS) {
	frameworkFS[dialect] = fsys
}
