package pipeline

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
)

func TestResult_Success_StateAndValue(t *testing.T) {
	r := Success(42)
	if r.State() != StateSuccess {
		t.Errorf("State() = %v, want StateSuccess", r.State())
	}
	if !r.IsSuccess() || r.IsFailure() || r.IsException() {
		t.Errorf("flags inconsistent: success=%v failure=%v exception=%v",
			r.IsSuccess(), r.IsFailure(), r.IsException())
	}
	if r.Value() != 42 {
		t.Errorf("Value() = %d, want 42", r.Value())
	}
	if r.Err() != nil {
		t.Errorf("Err() on Success = %v, want nil", r.Err())
	}
	if r.Notifications() != nil {
		t.Errorf("Notifications() on Success = %v, want nil", r.Notifications())
	}
}

func TestResult_Failure_StateAndNotifications(t *testing.T) {
	dtos := []notifications.ContextDTO{{Context: "User"}}
	r := Failure[int](dtos)
	if r.State() != StateFailure || !r.IsFailure() {
		t.Errorf("expected Failure state, got %v", r.State())
	}
	if len(r.Notifications()) != 1 || r.Notifications()[0].Context != "User" {
		t.Errorf("unexpected Notifications(): %+v", r.Notifications())
	}
}

func TestResult_Exception_StateAndErr(t *testing.T) {
	cause := errors.New("oops")
	r := Exception[int](cause)
	if r.State() != StateException || !r.IsException() {
		t.Errorf("expected Exception state, got %v", r.State())
	}
	if !errors.Is(r.Err(), cause) {
		t.Errorf("Err() = %v, want %v", r.Err(), cause)
	}
}

func TestResult_OnSuccess_FiresOnlyOnSuccess(t *testing.T) {
	count := 0
	var captured int
	chain := Success(7).OnSuccess(func(v int) { count++; captured = v })
	if count != 1 || captured != 7 {
		t.Errorf("OnSuccess on Success: count=%d captured=%d", count, captured)
	}
	if !chain.IsSuccess() {
		t.Error("OnSuccess should return same Result")
	}

	count = 0
	Failure[int](nil).OnSuccess(func(int) { count++ })
	if count != 0 {
		t.Error("OnSuccess should NOT fire on Failure")
	}

	Exception[int](errors.New("x")).OnSuccess(func(int) { count++ })
	if count != 0 {
		t.Error("OnSuccess should NOT fire on Exception")
	}
}

func TestResult_OnFailure_FiresOnlyOnFailure(t *testing.T) {
	count := 0
	dtos := []notifications.ContextDTO{{Context: "X"}}
	Failure[int](dtos).OnFailure(func(got []notifications.ContextDTO) {
		count++
		if len(got) != 1 || got[0].Context != "X" {
			t.Errorf("OnFailure received %+v", got)
		}
	})
	if count != 1 {
		t.Errorf("OnFailure on Failure: count=%d, want 1", count)
	}

	Success(1).OnFailure(func([]notifications.ContextDTO) { count++ })
	Exception[int](errors.New("x")).OnFailure(func([]notifications.ContextDTO) { count++ })
	if count != 1 {
		t.Error("OnFailure must NOT fire on Success or Exception")
	}
}

func TestResult_OnException_FiresOnlyOnException(t *testing.T) {
	count := 0
	cause := errors.New("x")
	Exception[int](cause).OnException(func(e error) {
		count++
		if !errors.Is(e, cause) {
			t.Errorf("OnException received %v", e)
		}
	})
	if count != 1 {
		t.Errorf("OnException on Exception: count=%d, want 1", count)
	}

	Success(1).OnException(func(error) { count++ })
	Failure[int](nil).OnException(func(error) { count++ })
	if count != 1 {
		t.Error("OnException must NOT fire on Success or Failure")
	}
}

func TestResult_ValueOr(t *testing.T) {
	if got := Success(3).ValueOr(99); got != 3 {
		t.Errorf("ValueOr on Success = %d, want 3", got)
	}
	if got := Failure[int](nil).ValueOr(99); got != 99 {
		t.Errorf("ValueOr on Failure = %d, want 99", got)
	}
	if got := Exception[int](errors.New("x")).ValueOr(99); got != 99 {
		t.Errorf("ValueOr on Exception = %d, want 99", got)
	}
}

func TestResult_MustValue_ReturnsOnSuccess(t *testing.T) {
	got := Success("ok").MustValue()
	if got != "ok" {
		t.Errorf("MustValue() = %q, want %q", got, "ok")
	}
}

func TestResult_MustValue_PanicsOnNonSuccess(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustValue() on Failure")
		}
	}()
	Failure[int](nil).MustValue()
}

func TestResult_MustValue_PanicsOnException(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from MustValue() on Exception")
		}
	}()
	Exception[int](errors.New("x")).MustValue()
}

func TestFirstSuccess(t *testing.T) {
	results := []Result[int]{
		Failure[int](nil),
		Exception[int](errors.New("x")),
		Success(11),
		Success(99), // ignored — first wins
	}
	r, ok := FirstSuccess(results)
	if !ok {
		t.Fatal("expected FirstSuccess to find a Success")
	}
	if r.Value() != 11 {
		t.Errorf("FirstSuccess returned value %d, want 11", r.Value())
	}
}

func TestFirstSuccess_NoSuccess(t *testing.T) {
	r, ok := FirstSuccess([]Result[int]{
		Failure[int](nil),
		Exception[int](errors.New("x")),
	})
	if ok {
		t.Errorf("expected ok=false when no Success present, got %+v", r)
	}
	// zero Result is returned — IsSuccess is false but the state is StateSuccess
	// (the int zero). The bool flag is the contract; just sanity-check it.
	if r.IsFailure() {
		t.Error("zero Result should not be Failure")
	}
}

func TestForEach_VisitsEveryEntry(t *testing.T) {
	results := []Result[int]{
		Success(1),
		Failure[int](nil),
		Exception[int](errors.New("x")),
	}
	count := 0
	ForEach(results, func(Result[int]) { count++ })
	if count != 3 {
		t.Errorf("ForEach visited %d, want 3", count)
	}
}
