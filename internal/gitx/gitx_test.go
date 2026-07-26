package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x2x3studio/hgctl/internal/proc"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := testContext(t)
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.name", "gitx-test"},
		{"config", "user.email", "gitx-test@example.invalid"},
	} {
		if _, err := proc.Run(ctx, dir, "git", args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	return dir
}

func commit(t *testing.T, dir, name, body string) string {
	t.Helper()
	ctx := testContext(t)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--all"}, {"commit", "--quiet", "-m", name}} {
		if _, err := proc.Run(ctx, dir, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	out, err := proc.Run(ctx, dir, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestRefExists(t *testing.T) {
	dir := repo(t)
	ctx := testContext(t)
	if RefExists(ctx, dir, "refs/heads/main") {
		t.Fatal("an empty repository reported a branch ref")
	}
	commit(t, dir, "a.txt", "a")
	branch, err := proc.Run(ctx, dir, "git", "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	if !RefExists(ctx, dir, "refs/heads/"+strings.TrimSpace(branch)) {
		t.Fatal("the current branch's ref was reported missing")
	}
}

// git answers this through the EXIT STATUS: 1 means "no". Collapsing exit 1 into
// an error is how "not an ancestor" gets read as a repository failure.
func TestIsAncestorTreatsExitOneAsAnAnswer(t *testing.T) {
	dir := repo(t)
	ctx := testContext(t)
	first := commit(t, dir, "a.txt", "a")
	second := commit(t, dir, "b.txt", "b")

	yes, err := IsAncestor(ctx, dir, first, second)
	if err != nil || !yes {
		t.Fatalf("first should be an ancestor of second: %v %v", yes, err)
	}
	no, err := IsAncestor(ctx, dir, second, first)
	if err != nil {
		t.Fatalf("a negative answer must not be an error: %v", err)
	}
	if no {
		t.Fatal("second reported as an ancestor of first")
	}
	if _, err := IsAncestor(ctx, dir, "0000000000000000000000000000000000000000", second); err == nil {
		t.Fatal("an unknown commit must be an error, not a quiet false")
	}
}

// Same shape, opposite polarity: `diff --cached --quiet` exits 1 when there ARE
// changes, so exit 1 is the interesting answer rather than a failure.
func TestHasStagedChanges(t *testing.T) {
	dir := repo(t)
	ctx := testContext(t)
	commit(t, dir, "a.txt", "a")

	staged, err := HasStagedChanges(ctx, dir)
	if err != nil || staged {
		t.Fatalf("clean tree: staged=%v err=%v", staged, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Run(ctx, dir, "git", "add", "--all"); err != nil {
		t.Fatal(err)
	}
	staged, err = HasStagedChanges(ctx, dir)
	if err != nil || !staged {
		t.Fatalf("staged change: staged=%v err=%v", staged, err)
	}
}

func TestCommonDirIdentifiesTheOwningRepository(t *testing.T) {
	dir := repo(t)
	ctx := testContext(t)
	commit(t, dir, "a.txt", "a")
	other := repo(t)
	commit(t, other, "a.txt", "a")

	mine, err := CommonDir(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := CommonDir(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if mine == theirs {
		t.Fatal("two unrelated repositories share a common dir; worktree ownership cannot be checked")
	}
	if !strings.HasSuffix(mine, ".git") {
		t.Fatalf("common dir = %q, want the .git directory", mine)
	}
}

func TestIsWorktree(t *testing.T) {
	dir := repo(t)
	if !IsWorktree(dir) {
		t.Fatal("a real repository was not recognised")
	}
	if IsWorktree(t.TempDir()) {
		t.Fatal("a plain directory was reported as a worktree")
	}
	if IsWorktree(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("a missing path was reported as a worktree")
	}
}
