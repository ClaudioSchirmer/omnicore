package domain

type Event interface {
	EventType() EventType
	ClassName() string
	Message() string
	Values() any
	Err() error
}

type DomainEvent struct {
	Type   EventType
	Class  string
	Msg    string
	Vals   any
	Reason error
}

func (e DomainEvent) EventType() EventType { return e.Type }
func (e DomainEvent) ClassName() string    { return e.Class }
func (e DomainEvent) Message() string      { return e.Msg }
func (e DomainEvent) Values() any          { return e.Vals }
func (e DomainEvent) Err() error           { return e.Reason }
