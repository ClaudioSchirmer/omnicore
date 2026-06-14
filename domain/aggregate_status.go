package domain

type AggregateItemStatus int

const (
	StatusUnknown AggregateItemStatus = iota
	StatusConstructor
	StatusAdded
	StatusChanged
	StatusRemoved
)

func (s AggregateItemStatus) String() string {
	switch s {
	case StatusConstructor:
		return "CONSTRUCTOR"
	case StatusAdded:
		return "ADDED"
	case StatusChanged:
		return "CHANGED"
	case StatusRemoved:
		return "REMOVED"
	default:
		return "UNKNOWN"
	}
}

func (s AggregateItemStatus) IsValid() bool {
	return s >= StatusConstructor && s <= StatusRemoved
}
