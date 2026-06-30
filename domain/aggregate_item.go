package domain

type AggregateItem[T any] struct {
	Item           T
	OriginalStatus AggregateItemStatus
	CurrentStatus  AggregateItemStatus
}

func NewAggregateItem[T any](item T, status AggregateItemStatus) AggregateItem[T] {
	return AggregateItem[T]{
		Item:           item,
		OriginalStatus: status,
		CurrentStatus:  status,
	}
}

func FilterByStatus[T any](items []AggregateItem[T], status AggregateItemStatus) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if it.CurrentStatus == status {
			out = append(out, it.Item)
		}
	}
	return out
}

func GetCurrentItems[T any](items []AggregateItem[T]) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if it.CurrentStatus != StatusRemoved {
			out = append(out, it.Item)
		}
	}
	return out
}

// GetAddedItems / GetChangedItems / GetRemovedItems categorize by the persistence
// operation (OperationOf — original + current status), NOT currentStatus alone, so
// a re-added or changed DB item is "changed" (UPDATE) and a new item added then
// removed is in none of them. Mirrors the reference ddd-kernel.
func GetAddedItems[T any](items []AggregateItem[T]) []T {
	return filterByOp(items, OpInsert)
}

func GetChangedItems[T any](items []AggregateItem[T]) []T {
	return filterByOp(items, OpUpdate)
}

func GetRemovedItems[T any](items []AggregateItem[T]) []T {
	return filterByOp(items, OpDelete)
}

func filterByOp[T any](items []AggregateItem[T], op AggregateItemOp) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if OperationOf(it.OriginalStatus, it.CurrentStatus) == op {
			out = append(out, it.Item)
		}
	}
	return out
}
