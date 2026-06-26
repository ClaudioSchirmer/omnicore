package configuration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Compile-time proof that *AppContext satisfies context.Context. This is the
// load-bearing contract that lets every ViewReader/Repository call accept the
// AppContext directly and propagate cancellation/deadline.
var _ context.Context = (*AppContext)(nil)

func TestAppContext_NoParent_BackgroundFallback(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	if ctx.Done() != nil {
		t.Error("expected Done() to be nil for context.Background() default")
	}
	if ctx.Err() != nil {
		t.Errorf("expected nil Err on background fallback, got %v", ctx.Err())
	}
	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no Deadline on background fallback")
	}
}

func TestAppContext_CancelPropagatesFromParent(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	parent, cancel := context.WithCancel(context.Background())
	ctx.SetParent(parent)

	if ctx.Err() != nil {
		t.Fatalf("expected nil Err before cancel, got %v", ctx.Err())
	}

	cancel()

	select {
	case <-ctx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Done() to fire after parent cancel within 100ms")
	}
	if ctx.Err() == nil {
		t.Error("expected Err to be non-nil after cancel")
	}
}

func TestAppContext_DeadlinePropagates(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	want := time.Now().Add(5 * time.Second)
	parent, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	ctx.SetParent(parent)

	got, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected Deadline to be reported")
	}
	if !got.Equal(want) {
		t.Errorf("expected Deadline=%v, got %v", want, got)
	}
}

func TestAppContext_SetParentIfAbsent_SetsWhenUnset(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx.SetParentIfAbsent(parent)

	cancel()
	select {
	case <-ctx.Done():
		// expected: the parent was wired
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Done() to fire — SetParentIfAbsent should have wired the parent")
	}
}

func TestAppContext_SetParentIfAbsent_KeepsExisting(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	ctx.SetParent(first)

	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	ctx.SetParentIfAbsent(second) // must be a no-op: a parent is already set

	cancelSecond() // canceling the rejected parent must NOT cancel ctx
	select {
	case <-ctx.Done():
		t.Fatal("ctx canceled by the rejected parent — SetParentIfAbsent clobbered the existing parent")
	case <-time.After(50 * time.Millisecond):
		// expected: still alive, the original parent drives it
	}

	cancelFirst()
	select {
	case <-ctx.Done():
		// expected: the original parent still drives cancellation
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Done() from the original parent")
	}
}

func TestAppContext_ValueDelegatesToParent(t *testing.T) {
	type ctxKey struct{}
	ctx := NewAppContextWithRandomID(LangPTBR)
	parent := context.WithValue(context.Background(), ctxKey{}, "stack-value")
	ctx.SetParent(parent)

	if got := ctx.Value(ctxKey{}); got != "stack-value" {
		t.Errorf("expected parent value to propagate via Value(), got %v", got)
	}
}

// --- BearerToken ---

func TestAppContext_BearerToken_DefaultEmpty(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	if got := ctx.BearerToken(); got != "" {
		t.Errorf("default BearerToken should be empty, got %q", got)
	}
}

func TestAppContext_BearerToken_SetThenGet(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	token := "eyJ.fake.bearer"
	ctx.SetBearerToken(token)
	if got := ctx.BearerToken(); got != token {
		t.Errorf("BearerToken = %q, want %q", got, token)
	}
}

func TestAppContext_BearerToken_ClearWithEmpty(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	ctx.SetBearerToken("populated")
	ctx.SetBearerToken("")
	if got := ctx.BearerToken(); got != "" {
		t.Errorf("BearerToken after clear should be empty, got %q", got)
	}
}

func TestAppContext_BearerToken_ConcurrentSafe(t *testing.T) {
	// Run with `go test -race` to confirm no data race between BearerToken /
	// SetBearerToken under contention. Test passes either way; the value here
	// is the race-detector signal.
	ctx := NewAppContextWithRandomID(LangPTBR)
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(i int) {
			for j := 0; j < 200; j++ {
				ctx.SetBearerToken("token")
				_ = ctx.BearerToken()
			}
			done <- struct{}{}
			_ = i
		}(i)
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

// --- ID / Language / Set / Get / NewAppContext branches ---

func TestNewAppContext_PreservesID(t *testing.T) {
	// Concrete UUID — ID() must return exactly what NewAppContext was given.
	id := mustUUID(t, "11111111-1111-1111-1111-111111111111")
	ctx := NewAppContext(id, LangPTBR)
	if got := ctx.ID(); got != id {
		t.Errorf("ID() = %v, want %v", got, id)
	}
	if got := ctx.Language(); got != LangPTBR {
		t.Errorf("Language() = %v, want LangPTBR", got)
	}
}

func TestNewAppContext_UnknownLanguageFallsBackToENG(t *testing.T) {
	ctx := NewAppContext(uuidNew(t), LangUnknown)
	if got := ctx.Language(); got != LangENG {
		t.Errorf("Language() = %v, want LangENG fallback for LangUnknown", got)
	}
}

func TestAppContext_SetLanguage(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	ctx.SetLanguage(LangES)
	if got := ctx.Language(); got != LangES {
		t.Errorf("Language() after SetLanguage(LangES) = %v, want LangES", got)
	}
}

func TestAppContext_SetGet(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)

	if _, ok := ctx.Get("missing"); ok {
		t.Error("Get on absent key should report ok=false")
	}

	ctx.Set("tenant.id", "acme")
	v, ok := ctx.Get("tenant.id")
	if !ok {
		t.Fatal("expected ok=true for previously-set key")
	}
	if v != "acme" {
		t.Errorf("Get() value = %v, want acme", v)
	}

	// Overwrites in place.
	ctx.Set("tenant.id", "beta")
	v, _ = ctx.Get("tenant.id")
	if v != "beta" {
		t.Errorf("Get() after overwrite = %v, want beta", v)
	}
}

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) = %v", s, err)
	}
	return id
}

func uuidNew(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.New()
}

func TestAppContext_CorrelationCausationDefaults(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangENG)
	if ctx.CorrelationID() != uuid.Nil {
		t.Errorf("expected zero CorrelationID, got %v", ctx.CorrelationID())
	}
	if ctx.CausationID() != uuid.Nil {
		t.Errorf("expected zero CausationID, got %v", ctx.CausationID())
	}
}

func TestAppContext_CorrelationCausationSetGet(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangENG)
	corr := uuid.New()
	caus := uuid.New()
	ctx.SetCorrelationID(corr)
	ctx.SetCausationID(caus)
	if ctx.CorrelationID() != corr {
		t.Errorf("CorrelationID mismatch: %v vs %v", ctx.CorrelationID(), corr)
	}
	if ctx.CausationID() != caus {
		t.Errorf("CausationID mismatch: %v vs %v", ctx.CausationID(), caus)
	}
}

func TestAppContext_SetParentNilRestoresBackground(t *testing.T) {
	ctx := NewAppContextWithRandomID(LangPTBR)
	parent, cancel := context.WithCancel(context.Background())
	ctx.SetParent(parent)
	cancel()
	if ctx.Err() == nil {
		t.Fatal("expected Err after cancel")
	}

	ctx.SetParent(nil)
	if ctx.Err() != nil {
		t.Errorf("expected nil Err after parent reset, got %v", ctx.Err())
	}
	if ctx.Done() != nil {
		t.Error("expected nil Done after parent reset")
	}
}
