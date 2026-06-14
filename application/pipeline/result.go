package pipeline

import "github.com/ClaudioSchirmer/omnicore/application/notifications"

type ResultState int

const (
	StateSuccess ResultState = iota
	StateFailure
	StateException
)

type Result[T any] struct {
	value         T
	notifications []notifications.ContextDTO
	err           error
	state         ResultState
}

func Success[T any](v T) Result[T] {
	return Result[T]{value: v, state: StateSuccess}
}

func Failure[T any](n []notifications.ContextDTO) Result[T] {
	return Result[T]{notifications: n, state: StateFailure}
}

func Exception[T any](err error) Result[T] {
	return Result[T]{err: err, state: StateException}
}

func (r Result[T]) State() ResultState                       { return r.state }
func (r Result[T]) IsSuccess() bool                          { return r.state == StateSuccess }
func (r Result[T]) IsFailure() bool                          { return r.state == StateFailure }
func (r Result[T]) IsException() bool                        { return r.state == StateException }
func (r Result[T]) Value() T                                 { return r.value }
func (r Result[T]) Notifications() []notifications.ContextDTO { return r.notifications }
func (r Result[T]) Err() error                               { return r.err }

func (r Result[T]) OnSuccess(fn func(T)) Result[T] {
	if r.IsSuccess() {
		fn(r.value)
	}
	return r
}

func (r Result[T]) OnFailure(fn func([]notifications.ContextDTO)) Result[T] {
	if r.IsFailure() {
		fn(r.notifications)
	}
	return r
}

func (r Result[T]) OnException(fn func(error)) Result[T] {
	if r.IsException() {
		fn(r.err)
	}
	return r
}

func (r Result[T]) ValueOr(fallback T) T {
	if r.IsSuccess() {
		return r.value
	}
	return fallback
}

func (r Result[T]) MustValue() T {
	if !r.IsSuccess() {
		panic("Result: MustValue on non-success state")
	}
	return r.value
}

func FirstSuccess[T any](results []Result[T]) (Result[T], bool) {
	for _, r := range results {
		if r.IsSuccess() {
			return r, true
		}
	}
	var zero Result[T]
	return zero, false
}

func ForEach[T any](results []Result[T], fn func(Result[T])) {
	for _, r := range results {
		fn(r)
	}
}
