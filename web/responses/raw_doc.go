package responses

// RawDoc is the canonical identity projector for query wrappers. Pair with
// HandleQueryWithParams / HandleQueryByID when the consumer is happy to
// emit the view document verbatim — same wire shape the framework produced
// before the projector parameter became mandatory:
//
//	app.Get("/users/:id", web.HandleQueryByID(pipe,
//	    requests.FindUserByIDRequest{},
//	    responses.RawDoc,
//	    handler))
//
// Use a typed Response (consumer-defined struct + FromDoc method) when the
// wire contract should be declared instead of mirroring the view shape.
func RawDoc(doc map[string]any) map[string]any { return doc }
