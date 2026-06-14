package domain

type WorkUnit[T any] func() (T, error)

func RunWorkUnit[T any](work WorkUnit[T]) (T, error) {
	return work()
}
