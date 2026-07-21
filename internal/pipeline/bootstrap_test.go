package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCreatesAValidOrphanSharedProduct(t *testing.T) {
	repository, remote, mainCommit := newBootstrapRepository(t)
	result, err := Bootstrap(context.Background(), BootstrapOptions{Checkout: repository, ControlSHA: mainCommit})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("bootstrap reported a NOOP for a missing shared branch")
	}
	if branch := strings.TrimSpace(runTestGit(t, repository, "branch", "--show-current")); branch != "shared" {
		t.Fatalf("bootstrap branch = %q", branch)
	}
	commitTestRepository(t, repository)
	parents := strings.Fields(runTestGit(t, repository, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 1 {
		t.Fatalf("shared bootstrap commit has parents: %v", parents[1:])
	}
	revision, err := (gitRepository{directory: repository}).revision(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, contents, _, err := readSharedTree(context.Background(), gitRepository{directory: repository}, revision); err != nil {
		t.Fatalf("generated shared product is invalid: %v", err)
	} else if len(contents) != 3 {
		t.Fatalf("generated shared files = %d, want 3", len(contents))
	}
	if mainTree := strings.TrimSpace(runTestGit(t, repository, "ls-tree", "-r", "--name-only", mainCommit)); !strings.Contains(mainTree, "README.md") {
		t.Fatal("bootstrap changed the control commit")
	}
	runTestGit(t, repository, "push", "origin", "shared")
	if branch := strings.TrimSpace(runTestGit(t, "", "--git-dir", remote, "rev-parse", "--verify", "refs/heads/shared")); branch == "" {
		t.Fatal("test did not publish shared")
	}
}

func TestBootstrapQueueCreatesExactOrphanTemplate(t *testing.T) {
	repository, remote, mainCommit := newBootstrapRepository(t)
	result, err := BootstrapQueue(context.Background(), BootstrapOptions{Checkout: repository, ControlSHA: mainCommit})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("queue bootstrap reported a NOOP for a missing template")
	}
	commitTestRepository(t, repository)
	commit := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
	if err := validateQueueTemplate(context.Background(), gitRepository{directory: repository}, commit); err != nil {
		t.Fatalf("generated queue template is invalid: %v", err)
	}
	if tree := strings.Fields(runTestGit(t, repository, "ls-tree", "-r", "--name-only", "HEAD")); len(tree) != 1 || tree[0] != queueTemplateMarker {
		t.Fatalf("queue template tree = %v", tree)
	}
	runTestGit(t, repository, "push", "origin", "queue-template")
	if branch := strings.TrimSpace(runTestGit(t, "", "--git-dir", remote, "rev-parse", "--verify", queueTemplateRef)); branch == "" {
		t.Fatal("test did not publish queue template")
	}
}

func TestBootstrapQueueNoopsWithoutMutatingWhenTemplateExists(t *testing.T) {
	repository, remote, mainCommit := newBootstrapRepository(t)
	if _, err := BootstrapQueue(context.Background(), BootstrapOptions{Checkout: repository, ControlSHA: mainCommit}); err != nil {
		t.Fatal(err)
	}
	commitTestRepository(t, repository)
	runTestGit(t, repository, "push", "origin", "queue-template")

	clone := filepath.Join(t.TempDir(), "clone")
	runTestGit(t, "", "clone", "--quiet", "--branch", "main", remote, clone)
	result, err := BootstrapQueue(context.Background(), BootstrapOptions{Checkout: clone, ControlSHA: mainCommit})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created {
		t.Fatal("bootstrap recreated an existing queue template")
	}
	if branch := strings.TrimSpace(runTestGit(t, clone, "branch", "--show-current")); branch != "main" {
		t.Fatalf("NOOP changed branch to %q", branch)
	}
}

func TestBootstrapNoopsWithoutMutatingWhenSharedExists(t *testing.T) {
	repository, remote, mainCommit := newBootstrapRepository(t)
	if _, err := Bootstrap(context.Background(), BootstrapOptions{Checkout: repository, ControlSHA: mainCommit}); err != nil {
		t.Fatal(err)
	}
	commitTestRepository(t, repository)
	runTestGit(t, repository, "push", "origin", "shared")

	clone := filepath.Join(t.TempDir(), "clone")
	runTestGit(t, "", "clone", "--quiet", "--branch", "main", remote, clone)
	result, err := Bootstrap(context.Background(), BootstrapOptions{Checkout: clone, ControlSHA: mainCommit})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created {
		t.Fatal("bootstrap recreated an existing shared branch")
	}
	if branch := strings.TrimSpace(runTestGit(t, clone, "branch", "--show-current")); branch != "main" {
		t.Fatalf("NOOP changed branch to %q", branch)
	}
	if status := runTestGit(t, clone, "status", "--porcelain"); status != "" {
		t.Fatalf("NOOP changed checkout: %q", status)
	}
}

func TestBootstrapRejectsDirtyOrUntrustedControlCheckout(t *testing.T) {
	for name, prepare := range map[string]func(*testing.T, string, string) string{
		"dirty": func(t *testing.T, repository, commit string) string {
			writeTestFile(t, repository, "untracked.txt", "untrusted\n")
			return commit
		},
		"wrong revision": func(_ *testing.T, _, _ string) string {
			return strings.Repeat("a", 40)
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository, _, mainCommit := newBootstrapRepository(t)
			controlSHA := prepare(t, repository, mainCommit)
			if _, err := Bootstrap(context.Background(), BootstrapOptions{Checkout: repository, ControlSHA: controlSHA}); err == nil {
				t.Fatal("bootstrap accepted an unsafe control checkout")
			}
			if branch := strings.TrimSpace(runTestGit(t, repository, "branch", "--show-current")); branch != "master" && branch != "main" {
				t.Fatalf("failed bootstrap changed branch to %q", branch)
			}
		})
	}
}

func newBootstrapRepository(t *testing.T) (string, string, string) {
	t.Helper()
	repository := newTestRepository(t)
	writeTestFile(t, repository, "README.md", "trusted control\n")
	commitTestRepository(t, repository)
	mainCommit := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
	remote := filepath.Join(t.TempDir(), "hourglass.git")
	runTestGit(t, "", "init", "--quiet", "--bare", remote)
	runTestGit(t, repository, "remote", "add", "origin", remote)
	runTestGit(t, repository, "push", "--quiet", "origin", "HEAD:refs/heads/main")
	runTestGit(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	if _, err := os.Stat(filepath.Join(repository, ".git")); err != nil {
		t.Fatal(err)
	}
	return repository, remote, mainCommit
}
