package pipeline

// FullBody is an embeddable struct-marker that handlers use to signal
// "I require ALL exported fields of Cmd present in the body JSON before
// dispatch". The Fiber wrapper (web.HandleCommand/HandleCommandByID)
// inspects this marker via type-assertion to FullBodyEnforcer and, when
// present, runs the reflective JSON presence check. Missing field → 422 with
// RequiredFieldNotification per missing field, via the normal pipeline.
//
// The method is unexported on purpose: only handlers defined in the pipeline
// package or that embed FullBody acquire the semantics. This prevents other
// types from satisfying the marker by accident.
//
// Usage (in the handler, opt-in by embedding):
//
//	type UpdateCommandHandler[T, Cmd] struct {
//	    pipeline.FullBody  // marker — enables strict body check in the wrapper
//	    Repo    domain.Repository[T]
//	    Auditor persistence.Auditor
//	}
type FullBody struct{}

func (FullBody) enforceFullBody() {}

// FullBodyEnforcer is the interface the wrapper uses to detect the marker.
// Implemented by the FullBody struct (and by handlers that embed it).
type FullBodyEnforcer interface {
	enforceFullBody()
}
