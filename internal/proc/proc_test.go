package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Two properties, both about not trusting a subprocess. A command that floods
// stdout must cost a bounded amount of memory, and a command that FAILS must not
// put what it printed - or what it was asked - into an error string that lands
// in a log.
func TestOutputIsBoundedAndFailureErrorsAreRedacted(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$HGCTL_TEST_MODE" = "large" ]; then
  dd if=/dev/zero bs=1048576 count=5 2>/dev/null
  exit 0
fi
printf '%s\n' "$*" >&2
printf 'token-secret-value\n' >&2
exit 9
`
	if err := os.WriteFile(filepath.Join(bin, "basic-memory"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Setenv("HGCTL_TEST_MODE", "large")
	output, err := Run(testContext(t), "", "basic-memory")
	if err == nil || !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("oversized output error=%v, want ErrOutputLimit", err)
	}
	if len(output) != basicMemoryOutputLimit {
		t.Fatalf("captured %d bytes, want the class limit %d", len(output), basicMemoryOutputLimit)
	}

	t.Setenv("HGCTL_TEST_MODE", "failure")
	_, err = Run(testContext(t), "", "basic-memory", "tool", "search-notes", "private-query")
	if err == nil {
		t.Fatal("failing command returned success")
	}
	for _, secret := range []string{"private-query", "token-secret-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
	// It is still RETRIEVABLE - the caller has to ask, which is the whole point
	// of keeping it off Error().
	if !strings.Contains(FailureOutput(err), "token-secret-value") {
		t.Fatal("FailureOutput lost the output it exists to expose")
	}
}

// Each class gets its own ceiling; git is deliberately the most generous because
// one ls-tree over a queue branch legitimately runs to megabytes.
func TestPolicyPerCommandClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class string
		limit int
	}{
		{"git", "git", gitOutputLimit},
		{"basic-memory", "Basic Memory", basicMemoryOutputLimit},
		{"launchctl", "scheduler", schedulerOutputLimit},
		{"anything-else", "external", defaultOutputLimit},
	} {
		got := policyFor(tc.name)
		if got.class != tc.class || got.outputLimit != tc.limit {
			t.Errorf("policyFor(%q) = %+v, want class %q limit %d", tc.name, got, tc.class, tc.limit)
		}
	}
}

func TestBoundedWriterKeepsThePrefixAndReportsTruncation(t *testing.T) {
	w := NewBoundedWriter(4)
	if _, err := w.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if w.Truncated() {
		t.Fatal("reported truncation before the limit was reached")
	}
	// A short write must still report the FULL length, or an io.Copy into this
	// writer fails with ErrShortWrite instead of quietly dropping the excess.
	n, err := w.Write([]byte("cdef"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("Write returned %d, want the full 4 so io.Copy does not error", n)
	}
	if !w.Truncated() {
		t.Fatal("overflow was not reported")
	}
	if w.String() != "abcd" {
		t.Fatalf("kept %q, want the first 4 bytes", w.String())
	}
}

func TestExists(t *testing.T) {
	if !Exists("sh") {
		t.Fatal("sh should exist on any supported machine")
	}
	if Exists("hgctl-definitely-not-a-real-command") {
		t.Fatal("a missing command reported as present")
	}
}
