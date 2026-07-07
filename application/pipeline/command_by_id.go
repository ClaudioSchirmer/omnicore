package pipeline

// Route-shape vocabulary. Every transport constructor (web, graphql, grpc)
// is named after one of the canonical command shapes — CommandWithBody,
// CommandWithBodyID, CommandByID — and each shape pairs with the base of the
// same name plus the Base suffix: embed the base, satisfy the constraint.
// "WithBody" describes the wire input, which is the Request DTO's job
// (ToCommand); "ByID" describes how the command is addressed. That is why
// the two id-carrying shapes share one method set and one underlying base.

// CommandByID is the contract for Commands addressed by an id taken from
// the transport (URL path, GraphQL id argument, request-message field).
// Implemented by Update/Delete/Archive/Unarchive Commands; consumed by the
// *ByID and *WithBodyID constructors on every transport, which inject
// cmd.SetPathID(id) after the mapper and before dispatch.
type CommandByID interface {
	Command
	SetPathID(id string)
	PathID() string
}

// CommandWithBody is the constraint the *WithBody constructors demand. The
// command carries no transport id — the body is mapped by the Request DTO's
// ToCommand — so "being a command" is all the shape requires.
type CommandWithBody = Command

// CommandWithBodyID is the constraint the *WithBodyID constructors demand.
// Same method set as CommandByID: the body is the DTO's business, the id is
// the command's.
type CommandWithBodyID = CommandByID

// CommandByIDBase is the embeddable helper that satisfies CommandByID.
// Typical usage:
//
//	type ArchiveUserCommand struct {
//	    pipeline.CommandByIDBase
//	}
type CommandByIDBase struct {
	CommandBase
	id string
}

func (c *CommandByIDBase) SetPathID(id string) { c.id = id }
func (c CommandByIDBase) PathID() string       { return c.id }

// CommandWithBodyBase is the base to embed on commands attached through the
// *WithBody constructors (insert-style: body only).
type CommandWithBodyBase = CommandBase

// CommandWithBodyIDBase is the base to embed on commands attached through
// the *WithBodyID constructors (update-style: body + id). Typical usage:
//
//	type UpdateUserCommand struct {
//	    pipeline.CommandWithBodyIDBase
//	    Name string `json:"name"`
//	}
type CommandWithBodyIDBase = CommandByIDBase
