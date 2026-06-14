package pipeline

type Request interface {
	isRequest()
}

type Command interface {
	Request
	isCommand()
}

type Query interface {
	Request
	isQuery()
}

type RequestBase struct{}

func (RequestBase) isRequest() {}

type CommandBase struct{ RequestBase }

func (CommandBase) isCommand() {}

type QueryBase struct{ RequestBase }

func (QueryBase) isQuery() {}
