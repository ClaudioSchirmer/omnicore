package bootstrap

import (
	"errors"
	"testing"
)

func TestSecretResolver_DefaultRejectsAllPaths(t *testing.T) {
	// Ensure the default is installed even if a prior test left a stub behind.
	RegisterSecretResolver(nil)

	got, err := currentSecretResolver().ResolveSecret("secret/anything#field")
	if !errors.Is(err, ErrUnsupportedResolver) {
		t.Fatalf("default resolver should return ErrUnsupportedResolver, got err=%v", err)
	}
	if got != "" {
		t.Errorf("default resolver should return empty string, got %q", got)
	}
}

type recordingResolver struct {
	value string
	last  string
}

func (r *recordingResolver) ResolveSecret(path string) (string, error) {
	r.last = path
	return r.value, nil
}

func TestRegisterSecretResolver_ReplacesDefault(t *testing.T) {
	defer RegisterSecretResolver(nil)
	rec := &recordingResolver{value: "from-stub"}
	RegisterSecretResolver(rec)

	got, err := currentSecretResolver().ResolveSecret("a/b#c")
	if err != nil {
		t.Fatalf("stub resolver error: %v", err)
	}
	if got != "from-stub" {
		t.Errorf("got %q, want %q", got, "from-stub")
	}
	if rec.last != "a/b#c" {
		t.Errorf("resolver received %q, want %q", rec.last, "a/b#c")
	}
}

func TestRegisterSecretResolver_NilRestoresDefault(t *testing.T) {
	RegisterSecretResolver(&recordingResolver{value: "x"})
	RegisterSecretResolver(nil)

	_, err := currentSecretResolver().ResolveSecret("anything")
	if !errors.Is(err, ErrUnsupportedResolver) {
		t.Fatalf("passing nil should restore the default; got err=%v", err)
	}
}
