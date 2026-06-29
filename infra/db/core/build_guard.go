//go:build postgres && mysql

package core

// Building with both the `postgres` and `mysql` build tags is supported: the
// binary links both relational engines and selects the active dialect at runtime
// from `relational.dialect`. Each engine package self-registers in the engine
// registry via its init(); core.NewEngine resolves the configured dialect. No
// duplicate-symbol guard is needed — the dialect-specific bootstrap wiring
// dispatches on the runtime dialect (see bootstrap/engine_both.go).
