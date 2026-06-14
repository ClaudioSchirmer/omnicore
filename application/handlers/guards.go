package handlers

import "fmt"

// RequirePathID panics with a developer-focused message when pathID is the
// empty string. Called by every auto handler whose pipeline reads the path
// ID — Command side via cmd.PathID() (pipeline.CommandWithID), Query side
// via q.GetID().Value() (queries.FindByIDQuery). Both contracts already
// return a string-shaped identifier; passing the raw value avoids defining
// a shared sub-interface just to hand off one field.
//
// Manual handlers are not required to call it — they own their own
// validation. The panic is caught by pipeline.Run's defer/recover and
// surfaces to the wire as the canonical 500 InternalServerErrorNotification
// envelope; the full diagnostic stays on slog at LevelError.
func RequirePathID(pathID, handlerName string) {
	if pathID == "" {
		panic(formatPathIDMissingMessage(handlerName))
	}
}

// formatPathIDMissingMessage renders the multi-line panic body documented
// in path-tag-design.md §5.3. Kept concise: handler name + remediation
// menu (canonical wrapper OR ToCommand mapping).
func formatPathIDMissingMessage(handlerName string) string {
	return fmt.Sprintf(
		"\n[omnicore] FATAL: %s requires a non-empty path ID, but PathID() returned the empty string.\n\n"+
			"The Command/Query reached the handler without an ID set. The framework cannot guess where\n"+
			"the ID should come from — that decision belongs to the wire layer (the wrapper) or to your\n"+
			"ToCommand / ToQuery.\n\n"+
			"Pick ONE of:\n\n"+
			"  (1) Use a canonical :id wrapper:\n"+
			"        fwweb.HandleCommandWithBodyID(...)   // PUT / PATCH with body\n"+
			"        fwweb.HandleCommandWithID(...)       // Archive / Unarchive / Delete (bodyless)\n"+
			"        fwweb.HandleQueryWithID(...)         // GET by id\n"+
			"      They call cmd.SetPathID(c.Params(\"id\")) automatically from the :id URL segment.\n\n"+
			"  (2) Set the ID explicitly in your Request.ToCommand() / ToQuery():\n"+
			"        func (r MyRequest) ToCommand() *MyCommand {\n"+
			"            cmd := &MyCommand{...}\n"+
			"            cmd.SetPathID(r.SomeField)         // <- here\n"+
			"            return cmd\n"+
			"        }\n"+
			"      Where r.SomeField is populated by a `path:\"someName\"` tag on the Request struct,\n"+
			"      OR pulled from any other source you choose (JWT subject, header, query, computed).\n\n"+
			"Diagnostic:\n"+
			"  handler: %s\n"+
			"  path id: \"\"\n",
		handlerName, handlerName,
	)
}
