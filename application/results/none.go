// Package results carries the framework's reusable Result types — values
// returned by an Auto Command Handler's projection step (cmd.FromEntity)
// and dispatched upstream by the Pipeline. A service typically defines its
// own Result type per use case, co-located with the Command. Bodyless verbs
// (Archive / Unarchive / Delete) and any other "no data on the wire" path
// declare TResult = None and write a one-line FromEntity returning None{}.
package results

// None is the canonical zero-data Result. Auto Command Handlers whose Cmd
// does not project anything declare TResult = None; their FromEntity body
// is `return results.None{}`. The wire layer pairs this with responses.None
// so the success envelope is rendered without a "data" field.
type None struct{}
