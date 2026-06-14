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

func GetAddedItems[T any](items []AggregateItem[T]) []T {
	return FilterByStatus(items, StatusAdded)
}

func GetChangedItems[T any](items []AggregateItem[T]) []T {
	return FilterByStatus(items, StatusChanged)
}

func GetRemovedItems[T any](items []AggregateItem[T]) []T {
	return FilterByStatus(items, StatusRemoved)
}
