package domain

type EventType int

// Explicit values (never bare iota): reordering this block must never change a
// persisted number.
const (
	EventUnknown EventType = 0
	EventLog     EventType = 1
	EventDebug   EventType = 2
	EventError   EventType = 3
	EventWarning EventType = 4
)

var eventTypeMembers = []EventType{EventLog, EventDebug, EventError, EventWarning}

// Values is the closed set (Unknown excluded); the framework validates
// membership against it — EventType writes no IsValid.
func (t EventType) Value() int          { return int(t) }
func (t EventType) Values() []EventType { return eventTypeMembers }

func (t EventType) UnknownNotification() Notification {
	return InvalidEventTypeNotification{}
}

func (t EventType) String() string {
	switch t {
	case EventLog:
		return "log"
	case EventDebug:
		return "debug"
	case EventError:
		return "error"
	case EventWarning:
		return "warning"
	default:
		return "unknown"
	}
}
