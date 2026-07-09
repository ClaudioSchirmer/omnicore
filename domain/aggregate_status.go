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

// AggregateItemOp is the persistence operation an aggregate child resolves to,
// derived from BOTH its original and current status (see OperationOf).
type AggregateItemOp int

const (
	OpNoop AggregateItemOp = iota
	OpInsert
	OpUpdate
	OpDelete
)

// OperationOf maps an aggregate item's (originalStatus, currentStatus) pair to the
// persistence operation. The pair — NOT currentStatus alone — is what
// distinguishes a brand-new item from a re-touched database item; mirrors the
// reference ddd-kernel (getAddedItems/getChangedItems/getRemovedItems):
//
//   - Insert: a new item still present       (original != CONSTRUCTOR, current != REMOVED)
//   - Update: a DB item changed or re-added  (original == CONSTRUCTOR, current ADDED|CHANGED)
//   - Delete: a DB item removed              (original == CONSTRUCTOR, current == REMOVED)
//   - Noop:   the rest — an untouched DB item (CONSTRUCTOR/CONSTRUCTOR), or a new item
//     added then removed (never persisted, so nothing to do).
func OperationOf(original, current AggregateItemStatus) AggregateItemOp {
	if original != StatusConstructor {
		if current != StatusRemoved {
			return OpInsert
		}
		return OpNoop // new item added then removed → never persisted
	}
	switch current {
	case StatusAdded, StatusChanged:
		return OpUpdate
	case StatusRemoved:
		return OpDelete
	default: // CONSTRUCTOR — untouched
		return OpNoop
	}
}
