package hgctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExistingWorktreeMustBelongToControlRepository(t *testing.T) {
	app := testApp(t)
	control := app.Paths.Control
	other := app.Paths.Vault
	runGitTest(t, "", "init", "-b", "main", control)
	runGitTest(t, control, "config", "user.name", "test")
	runGitTest(t, control, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(control, "main"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, control, "add", "main")
	runGitTest(t, control, "commit", "-m", "main")
	runGitTest(t, "", "init", "-b", "shared", other)
	if err := app.ensureWorktree(testContext(t), other, "shared", "origin/shared"); err == nil || !strings.Contains(err.Error(), "another repository") {
		t.Fatalf("expected repository ownership error, got %v", err)
	}
}

func TestFileLockReleasesAfterError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")
	want := errors.New("boom")
	if err := withFileLock(path, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	called := false
	if err := withFileLock(path, func() error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("second lock acquisition did not run")
	}
}

func TestFileLockWaitDoesNotRunCleanupWithoutTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	called := false
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = withFileLockWait(ctx, path, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
	if called {
		t.Fatal("cleanup ran without acquiring the lock")
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := withFileLockWait(context.Background(), path, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("cleanup did not run after the lock was released")
	}
}

func TestLifecycleLockSerializesInstallAndUninstall(t *testing.T) {
	app := testApp(t)
	holdFileLockForTest(t, app.Paths.LifecycleLock)
	for name, operation := range map[string]func(context.Context) error{
		"install": func(ctx context.Context) error {
			return app.runInstall(ctx, []string{"--repo", "git@github.com:x2x3studio/hourglass.git"})
		},
		"uninstall": app.uninstall,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := operation(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v, want lifecycle lock timeout", err)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(app.Paths.Bin, "hgctl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked lifecycle mutated the stable binary: %v", err)
	}
}

func TestUpdateLockModesAndInstallerSerialization(t *testing.T) {
	app := testApp(t)
	holdFileLockForTest(t, app.Paths.UpdateLock)
	if err := app.update(context.Background(), false); err != nil {
		t.Fatalf("background update did not skip a busy lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := app.update(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("explicit update got %v, want lock timeout", err)
	}
	cancel()
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := app.runInstall(ctx, []string{"--repo", "git@github.com:x2x3studio/hourglass.git"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("installer got %v, want update lock timeout", err)
	}
	if _, err := os.Lstat(filepath.Join(app.Paths.Bin, "hgctl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked installer mutated the stable binary: %v", err)
	}
}

func holdFileLockForTest(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	})
	return file
}

func TestGitHubRepoSlug(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:x2x3studio/hourglass.git",
		"ssh://git@github.com/x2x3studio/hourglass.git",
		"https://github.com/x2x3studio/hourglass.git",
	} {
		if got, ok := githubRepoSlug(remote); !ok || got != "x2x3studio/hourglass" {
			t.Fatalf("githubRepoSlug(%q)=(%q,%v)", remote, got, ok)
		}
	}
	for _, remote := range []string{"git@example.com:x/y.git", "https://github.com/x", "https://github.com/x/y/z"} {
		if got, ok := githubRepoSlug(remote); ok {
			t.Fatalf("githubRepoSlug(%q) unexpectedly returned %q", remote, got)
		}
	}
}

func TestNotifyDreamPinsGitHubDotCom(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "gh.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$HGCTL_GH_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HGCTL_GH_LOG", logPath)
	if err := notifyDream(context.Background(), "git@github.com:x2x3studio/hourglass.git"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "api --hostname github.com --method POST repos/x2x3studio/hourglass/dispatches -f event_type=hourglass_queue\n"
	if string(content) != want {
		t.Fatalf("gh call = %q, want %q", content, want)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestSeedOrphanQueueBranchWithoutTemplate(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	runGitTest(t, "", "init", "--bare", origin)

	app := testApp(t)
	control := app.Paths.Control
	runGitTest(t, "", "init", "-b", "main", control)
	runGitTest(t, control, "config", "user.name", "chinaboard")
	runGitTest(t, control, "config", "user.email", "chinaboard@gmail.com")
	runGitTest(t, control, "remote", "add", "origin", origin)

	branch := "queue/seed-machine"
	if err := app.seedOrphanQueueBranch(testContext(t), branch); err != nil {
		t.Fatalf("seed orphan queue: %v", err)
	}

	if !gitRefExists(testContext(t), control, "refs/heads/"+branch) {
		t.Fatal("local queue branch was not created")
	}
	if parents := strings.Fields(runGitTest(t, control, "rev-list", "--parents", "-n", "1", branch)); len(parents) != 1 {
		t.Fatalf("queue seed is not a parentless root commit: %v", parents)
	}
	if tracked := strings.TrimSpace(runGitTest(t, control, "ls-tree", "-r", "--name-only", branch)); tracked != "events/.gitkeep" {
		t.Fatalf("orphan seed tracks %q, want only events/.gitkeep", tracked)
	}
	local := strings.TrimSpace(runGitTest(t, control, "rev-parse", "refs/heads/"+branch))
	remote := strings.TrimSpace(runGitTest(t, origin, "rev-parse", "refs/heads/"+branch))
	if local != remote {
		t.Fatalf("origin did not receive the seeded branch: local=%s remote=%s", local, remote)
	}
}
