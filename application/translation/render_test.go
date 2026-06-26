package translation

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

func TestRender_NoVarsNoPlaceholders_IdenticalToGet(t *testing.T) {
	tr := New()
	tr.Import(CoreENG())

	got := tr.Render(configuration.LangENG, "RecordNotFoundNotification", nil)
	if got != "Record not found." {
		t.Errorf("Render = %q, want %q", got, "Record not found.")
	}
}

func TestRender_WithVarsAndPlaceholder_Substitutes(t *testing.T) {
	tr := New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"NameMaxLengthExceededNotification": "Name exceeds {maxLength} characters.",
		},
	})

	got := tr.Render(configuration.LangENG, "NameMaxLengthExceededNotification", map[string]string{"maxLength": "100"})
	if got != "Name exceeds 100 characters." {
		t.Errorf("Render = %q, want substitution applied", got)
	}
}

func TestRender_MultipleOccurrences_AllSubstituted(t *testing.T) {
	tr := New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"K": "{x} and again {x} and {y}.",
		},
	})

	got := tr.Render(configuration.LangENG, "K", map[string]string{"x": "A", "y": "B"})
	if got != "A and again A and B." {
		t.Errorf("Render = %q, want repeated occurrences substituted", got)
	}
}

func TestRender_MissingKey_ReturnsKeyAndWarnsOnce(t *testing.T) {
	ResetWarnOnceForTesting()
	t.Cleanup(ResetWarnOnceForTesting)

	tr := New()
	tr.Import(CoreENG())

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := tr.Render(configuration.LangENG, "NoSuchKey", nil)
	if got != "NoSuchKey" {
		t.Errorf("missing key fallback = %q, want %q", got, "NoSuchKey")
	}

	if !strings.Contains(buf.String(), "translation.key.missing") {
		t.Errorf("expected warn-once for missing key, log = %q", buf.String())
	}

	// Second call with the same (lang, key) — warn must NOT fire again.
	buf.Reset()
	_ = tr.Render(configuration.LangENG, "NoSuchKey", nil)
	if strings.Contains(buf.String(), "translation.key.missing") {
		t.Errorf("second emission should be silent, log = %q", buf.String())
	}
}

func TestRender_MissingPlaceholder_LeavesLiteralAndWarnsOnce(t *testing.T) {
	ResetWarnOnceForTesting()
	t.Cleanup(ResetWarnOnceForTesting)

	tr := New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"K": "Has {present} and {missing} placeholders.",
		},
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := tr.Render(configuration.LangENG, "K", map[string]string{"present": "X"})
	want := "Has X and {missing} placeholders."
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
	if !strings.Contains(buf.String(), "translation.var.missing") {
		t.Errorf("expected warn-once for missing placeholder, log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), `placeholder=missing`) {
		t.Errorf("warn should name the placeholder, log = %q", buf.String())
	}

	// Second call — silent.
	buf.Reset()
	_ = tr.Render(configuration.LangENG, "K", map[string]string{"present": "Y"})
	if strings.Contains(buf.String(), "translation.var.missing") {
		t.Errorf("second emission should be silent, log = %q", buf.String())
	}
}

func TestRender_NilVarsWithPlaceholdersInString_NoWarn(t *testing.T) {
	ResetWarnOnceForTesting()
	t.Cleanup(ResetWarnOnceForTesting)

	tr := New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"K": "Has {placeholder} inside.",
		},
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := tr.Render(configuration.LangENG, "K", nil)
	if got != "Has {placeholder} inside." {
		t.Errorf("nil vars should return raw string, got %q", got)
	}
	if strings.Contains(buf.String(), "translation.var.missing") {
		t.Errorf("nil vars must not produce missing-placeholder warns, log = %q", buf.String())
	}
}

func TestRender_EmptyVarsMap_NoWarn(t *testing.T) {
	ResetWarnOnceForTesting()
	t.Cleanup(ResetWarnOnceForTesting)

	tr := New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"K": "Has {placeholder} inside.",
		},
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := tr.Render(configuration.LangENG, "K", map[string]string{})
	if got != "Has {placeholder} inside." {
		t.Errorf("empty vars should return raw string, got %q", got)
	}
	if strings.Contains(buf.String(), "translation.var.missing") {
		t.Errorf("empty vars must not produce missing-placeholder warns, log = %q", buf.String())
	}
}

func TestInterpolate_BasicAndScannerEdgeCases(t *testing.T) {
	tests := []struct {
		name string
		in   string
		vars map[string]string
		want string
	}{
		{
			name: "nil vars passthrough",
			in:   "Has {x}.",
			vars: nil,
			want: "Has {x}.",
		},
		{
			name: "empty vars passthrough",
			in:   "Has {x}.",
			vars: map[string]string{},
			want: "Has {x}.",
		},
		{
			name: "single placeholder",
			in:   "Has {x}.",
			vars: map[string]string{"x": "value"},
			want: "Has value.",
		},
		{
			name: "invalid placeholder head digit is literal",
			in:   "Has {1bad}.",
			vars: map[string]string{"1bad": "value"},
			want: "Has {1bad}.",
		},
		{
			name: "unclosed brace is literal",
			in:   "Has {x and rest",
			vars: map[string]string{"x": "v"},
			want: "Has {x and rest",
		},
		{
			name: "underscore prefix allowed",
			in:   "Has {_x}.",
			vars: map[string]string{"_x": "value"},
			want: "Has value.",
		},
		{
			name: "alphanumeric tail allowed",
			in:   "Has {x9_y}.",
			vars: map[string]string{"x9_y": "value"},
			want: "Has value.",
		},
		{
			name: "dash inside braces is invalid placeholder, kept literal",
			in:   "Has {x-y}.",
			vars: map[string]string{"x-y": "value"},
			want: "Has {x-y}.",
		},
		{
			name: "missing placeholder kept literal",
			in:   "Has {present} and {missing}.",
			vars: map[string]string{"present": "P"},
			want: "Has P and {missing}.",
		},
		{
			name: "no braces in string short-circuits",
			in:   "no placeholders here",
			vars: map[string]string{"x": "value"},
			want: "no placeholders here",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Interpolate(tc.in, tc.vars)
			if got != tc.want {
				t.Errorf("Interpolate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRender_WarnOnce_DistinctPlaceholders_FireIndependently(t *testing.T) {
	ResetWarnOnceForTesting()
	t.Cleanup(ResetWarnOnceForTesting)

	tr := New()
	tr.Import(stubModule{
		lang: configuration.LangENG,
		entries: map[string]string{
			"K": "Has {a} and {b}.",
		},
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_ = tr.Render(configuration.LangENG, "K", map[string]string{"a": "1"})

	got := buf.String()
	if !strings.Contains(got, `placeholder=b`) {
		t.Errorf("expected warn for {b}, log = %q", got)
	}

	buf.Reset()
	_ = tr.Render(configuration.LangENG, "K", map[string]string{"b": "2"})
	got = buf.String()
	if !strings.Contains(got, `placeholder=a`) {
		t.Errorf("expected warn for {a} after {b} answered, log = %q", got)
	}
}
