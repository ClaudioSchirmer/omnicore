package listfailures

import (
	"context"
	"flag"
	"strings"
	"testing"
)

func TestNewUsage(t *testing.T) {
	fs := flag.NewFlagSet("list-failures", flag.ContinueOnError)
	var buf strings.Builder
	fs.SetOutput(&buf)
	fs.String("format", formatText, "fmt")
	newUsage(fs)()
	out := buf.String()
	for _, want := range []string{
		"list-failures",
		"Usage:",
		"-format",
		"Read-only",
		"APP_PROFILE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q in:\n%s", want, out)
		}
	}
}

// Run's argument-validation branches return before any config load / DB dial.
func TestRun_ArgValidation(t *testing.T) {
	t.Run("bad-format", func(t *testing.T) {
		err := Run(context.Background(), []string{"--format", "xml"})
		if err == nil || !strings.Contains(err.Error(), "--format must be text or json") {
			t.Fatalf("expected format error, got %v", err)
		}
	})
	t.Run("negative-limit", func(t *testing.T) {
		err := Run(context.Background(), []string{"--limit", "-1"})
		if err == nil || !strings.Contains(err.Error(), "--limit must be >= 0") {
			t.Fatalf("expected limit error, got %v", err)
		}
	})
	t.Run("help-returns-nil", func(t *testing.T) {
		if err := Run(context.Background(), []string{"-h"}); err != nil {
			t.Fatalf("help must return nil, got %v", err)
		}
	})
	t.Run("unknown-flag", func(t *testing.T) {
		if err := Run(context.Background(), []string{"--nope"}); err == nil {
			t.Fatal("expected parse error for unknown flag")
		}
	})
}
