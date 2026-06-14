package web

// RequestDTO is the contract for wire DTOs that produce Commands.
//
// Implemented by structs in `web/requests/` in the consumer service — each
// XxxRequest carries the `json:"..."` tags of the HTTP payload and produces
// the corresponding Command via ToCommand().
//
// The HandleCommandWithBody{,ID} wrapper (in handle_command_with_body.go)
// calls ToCommand() at the web→application boundary after validating the
// Request schema. Request lives in web/, Command in application/ — no JSON
// leak into application/ nor domain leak into web/.
//
// ToCommand is a pure mapper of body fields → Command fields. It does NOT
// receive AppContext: the request DTO is body-only, and the application
// layer (Command.ToEntity / Command.ApplyTo) is what interprets ctx into
// business-named entity fields. Putting ctx-translation in web would push
// transport concerns into a layer whose only job is to deserialize JSON.
//
// Typical usage in the consumer:
//
//	package requests
//
//	type InsertUserRequest struct {
//	    Name  string  `json:"name"`
//	    Email string  `json:"email"`
//	    Phone *string `json:"phone,omitempty"`
//	}
//
//	func (r InsertUserRequest) ToCommand() *commands.InsertUserCommand {
//	    return &commands.InsertUserCommand{Name: r.Name, Email: r.Email, Phone: r.Phone}
//	}
//
// The Command (TCmd) is typically a pointer type (*InsertUserCommand),
// because the pipeline operates over Commands as pointers (SetPathID on
// CommandWithID requires a pointer receiver).
type RequestDTO[TCmd any] interface {
	ToCommand() TCmd
}
