package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written, so usage() (which writes to an *os.File) is observable.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

func TestUsage(t *testing.T) {
	out := captureStdout(t, func() { usage(os.Stdout) })
	for _, want := range []string{
		"omnicore-admin",
		"Usage:",
		"list-failures",
		"help",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q in:\n%s", want, out)
		}
	}
}

// withArgs swaps os.Args for the duration of fn and restores it after.
func withArgs(args []string, fn func()) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = args
	fn()
}

func TestRun_Dispatch(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
		errHas  string
	}{
		{"no-subcommand", []string{"omnicore-admin"}, true, "subcommand required"},
		{"unknown-subcommand", []string{"omnicore-admin", "bogus"}, true, "unknown subcommand"},
		{"help", []string{"omnicore-admin", "help"}, false, ""},
		{"dash-h", []string{"omnicore-admin", "-h"}, false, ""},
		{"dash-dash-help", []string{"omnicore-admin", "--help"}, false, ""},
		// Subcommand dispatch reaching the sub-Run's -h short-circuit (no DB).
		{"listfailures-help", []string{"omnicore-admin", "list-failures", "-h"}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			// Swallow stdout help text so it does not pollute test output.
			_ = captureStdout(t, func() {
				withArgs(tc.args, func() { err = run() })
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errHas != "" && !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("error %q missing %q", err.Error(), tc.errHas)
				}
			} else if err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}
