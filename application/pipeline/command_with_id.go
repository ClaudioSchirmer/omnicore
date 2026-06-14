package pipeline

// CommandWithID is the contract for Commands that receive the ID via the
// URL path. Implemented by Update/Delete/Archive/Unarchive Commands; consumed
// by fwweb.HandleCommandWithID, which injects cmd.SetPathID(c.Params("id"))
// before dispatch.
type CommandWithID interface {
	Command
	SetPathID(id string)
	PathID() string
}

// CommandBaseWithID is the embeddable helper that satisfies CommandWithID.
// Typical usage:
//
//	type UpdateUserCommand struct {
//	    pipeline.CommandBaseWithID
//	    Name string `json:"name"`
//	}
type CommandBaseWithID struct {
	CommandBase
	id string
}

func (c *CommandBaseWithID) SetPathID(id string) { c.id = id }
func (c CommandBaseWithID) PathID() string       { return c.id }
