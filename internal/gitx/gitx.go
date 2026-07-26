// Package gitx wraps the handful of git plumbing questions hgctl asks. It sits
// on proc and depends on nothing else in this module.
//
// The common thread is that git answers most of these through the EXIT STATUS
// rather than through output: `merge-base --is-ancestor` exits 1 for "no",
// `diff --cached --quiet` exits 1 for "there are changes". Exit 1 is therefore
// an answer, and any other nonzero is a real failure. Getting that backwards is
// how a repository problem gets read as a clean tree, so each of these
// distinguishes the two rather than collapsing them into a bool.
package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/x2x3studio/hgctl/internal/proc"
)

// RefExists reports whether a ref resolves in dir. A ref that does not exist is
// an ordinary answer, not an error, so this returns a plain bool.
func RefExists(ctx context.Context, dir, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// IsAncestor reports whether ancestor is reachable from descendant.
func IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	_, err := proc.Run(ctx, dir, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, proc.ErrOutputLimit) {
		return false, err
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

// HasStagedChanges reports whether dir's index differs from HEAD.
func HasStagedChanges(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet", "--exit-code")
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	if exitCode(err) == 1 {
		return true, nil
	}
	return false, err
}

// RemoteBranchExists asks the REMOTE, which costs a network round trip. Callers
// on a scheduled path should prefer a cached remote-tracking ref via RefExists;
// this one measured 3.7s against GitHub.
func RemoteBranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := proc.Run(ctx, dir, "git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, proc.ErrOutputLimit) {
		return false, err
	}
	if exitCode(err) == 2 {
		return false, nil
	}
	return false, fmt.Errorf("git ls-remote --heads origin %s: %w", branch, err)
}

// CommonDir returns dir's shared .git directory, resolved. Two worktrees of the
// same repository share it, which is how ownership of a worktree is checked.
func CommonDir(ctx context.Context, dir string) (string, error) {
	out, err := proc.Run(ctx, dir, "git", "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	return filepath.Clean(common), nil
}

// IsWorktree reports whether path looks like a checkout. Deliberately a cheap
// stat rather than a git call: doctor asks it for several paths in a row and a
// missing directory is the answer, not a failure.
func IsWorktree(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
