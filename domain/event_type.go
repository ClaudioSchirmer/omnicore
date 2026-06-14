package domain

type EventType int

const (
	EventUnknown EventType = iota
	EventLog
	EventDebug
	EventError
	EventWarning
)

func (t EventType) Value() int {
	return int(t)
}

func (t EventType) UnknownNotification() Notification {
	return InvalidEventTypeNotification{}
}

func (t EventType) IsValid(fieldName string, ctx *NotificationContext) bool {
	return ValidateEnum[int](t, fieldName, ctx)
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
