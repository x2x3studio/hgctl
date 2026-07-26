// Package proc runs external commands and bounds what comes back.
//
// It is a leaf: it depends on nothing else in this module, which is what lets
// every other package use it without a cycle. Everything hgctl does to the
// machine goes through here - git, basic-memory, the schedulers - so the two
// behaviours worth knowing are both about NOT trusting a subprocess:
//
//   - Output is capped per command class. A runaway `git log` or a basic-memory
//     reindex that decides to print the corpus must not become the caller's
//     memory footprint.
//   - A failure returns the class and the byte limit, never the raw output, so a
//     command failing inside an error message cannot dump a transcript into a
//     log. Callers that legitimately need the output ask for it with
//     FailureOutput.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Output limits by command class. git is the most generous because a single
// `ls-tree` over a queue branch legitimately runs to megabytes.
const (
	defaultOutputLimit     = 1 << 20
	gitOutputLimit         = 8 << 20
	basicMemoryOutputLimit = 4 << 20
	schedulerOutputLimit   = 256 << 10
)

// ErrOutputLimit reports that a command produced more than its class allows.
var ErrOutputLimit = errors.New("subprocess output limit exceeded")

// Exists reports whether a command is on PATH.
func Exists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// Run executes name in dir and returns its combined output.
func Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return RunEnv(ctx, dir, nil, name, args...)
}

// RunEnv is Run with extra environment entries appended to the caller's own.
//
// git gets fixed hardening on every invocation rather than at the call sites:
// hooks off (a repository must not be able to run code on this machine), signing
// off (an endpoint has no key and a configured signer would fail every commit),
// and no terminal prompting (a credential prompt on a scheduled run hangs
// forever instead of failing).
func RunEnv(ctx context.Context, dir string, environment []string, name string, args ...string) (string, error) {
	if name == "git" {
		args = append([]string{
			"-c", "core.hooksPath=/dev/null",
			"-c", "commit.gpgSign=false",
			"-c", "tag.gpgSign=false",
		}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), environment...)
	if name == "git" {
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
		if os.Getenv("GIT_SSH_COMMAND") == "" {
			cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10")
		}
	}
	p := policyFor(name)
	out := boundedOutput{limit: p.outputLimit}
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if out.truncated {
		return out.String(), &RunError{class: p.class, cause: errors.Join(err, ErrOutputLimit), output: out.String(), outputLimit: p.outputLimit}
	}
	if err != nil {
		return out.String(), &RunError{class: p.class, cause: err, output: out.String()}
	}
	return out.String(), nil
}

// FailureOutput returns what a failed command actually printed. RunError keeps
// it off the error string on purpose, so a caller has to ask.
func FailureOutput(err error) string {
	var runErr *RunError
	if errors.As(err, &runErr) {
		return runErr.output
	}
	return ""
}

type policy struct {
	class       string
	outputLimit int
}

func policyFor(name string) policy {
	switch name {
	case "git":
		return policy{class: "git", outputLimit: gitOutputLimit}
	case "basic-memory":
		return policy{class: "Basic Memory", outputLimit: basicMemoryOutputLimit}
	case "launchctl", "systemctl", "loginctl":
		return policy{class: "scheduler", outputLimit: schedulerOutputLimit}
	default:
		return policy{class: "external", outputLimit: defaultOutputLimit}
	}
}

// boundedOutput accepts every write but keeps only the first limit bytes, so a
// command that floods stdout costs a bounded amount of memory and still runs to
// completion (killing it mid-write would lose the exit status).
type boundedOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedOutput) Write(content []byte) (int, error) {
	written := len(content)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(content) > remaining {
			_, _ = b.buffer.Write(content[:remaining])
		} else {
			_, _ = b.buffer.Write(content)
		}
	}
	if len(content) > remaining {
		b.truncated = true
	}
	return written, nil
}

func (b *boundedOutput) String() string { return b.buffer.String() }

// BoundedWriter is an io.Writer that keeps at most limit bytes and reports
// whether more arrived. Exported for callers that drive a subprocess themselves
// (the updater pipes a release candidate's stdout) but still must not let its
// stderr become their memory footprint.
type BoundedWriter struct{ inner boundedOutput }

// NewBoundedWriter returns a writer capped at limit bytes.
func NewBoundedWriter(limit int) *BoundedWriter {
	return &BoundedWriter{inner: boundedOutput{limit: limit}}
}

func (w *BoundedWriter) Write(p []byte) (int, error) { return w.inner.Write(p) }

// String returns what was kept.
func (w *BoundedWriter) String() string { return w.inner.String() }

// Truncated reports whether anything was dropped.
func (w *BoundedWriter) Truncated() bool { return w.inner.truncated }

// RunError carries the command class and, separately from its message, the
// output. Error() never includes the output: these errors reach logs and a
// failing `git log` would otherwise print history into them.
type RunError struct {
	class       string
	cause       error
	output      string
	outputLimit int
}

func (e *RunError) Error() string {
	if e.outputLimit > 0 {
		return fmt.Sprintf("%s command exceeded its %d-byte output limit", e.class, e.outputLimit)
	}
	return fmt.Sprintf("%s command failed: %v (output suppressed)", e.class, e.cause)
}

func (e *RunError) Unwrap() error { return e.cause }
