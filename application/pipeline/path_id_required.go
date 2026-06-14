package pipeline

// PathIDRequired is an embeddable marker exposing the unexported method
// behind PathIDRequiredEnforcer. Auto handlers whose Handle calls
// handlers.RequirePathID embed it so the Fiber wrappers can detect the
// requirement at construction time:
//
//	type UpdateCommandHandler[T, Cmd, TResult any] struct {
//	    pipeline.FullBody
//	    pipeline.PathIDRequired
//	    ...
//	}
//
// Used by fwweb.HandleCommandWithBody / HandleQueryWithParams (Group A
// wrappers — they do NOT auto-bind :id via interface) to emit a boot
// WARNING when paired with a Request DTO that declares no `path:` tag at
// all. The warning catches the common misconfiguration of "I expected
// the wrapper to populate the ID" — the runtime guard
// handlers.RequirePathID still catches the actual failure if the warning
// is ignored.
type PathIDRequired struct{}

func (PathIDRequired) pathIDRequired() {}

// PathIDRequiredEnforcer is satisfied by any handler embedding
// PathIDRequired. Fiber wrappers consult it via type assertion at
// construction time — zero per-request cost.
type PathIDRequiredEnforcer interface {
	pathIDRequired()
}
