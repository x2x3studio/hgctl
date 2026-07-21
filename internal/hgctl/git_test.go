package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGitQueueAndSharedWorktrees(t *testing.T) {
	if !commandExists("git") {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "hourglass.git")
	seed := filepath.Join(root, "seed")
	runGitTest(t, "", "init", "--bare", remote)
	runGitTest(t, "", "init", "-b", "main", seed)
	runGitTest(t, seed, "config", "user.name", "test")
	runGitTest(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("control\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "README.md")
	runGitTest(t, seed, "commit", "-m", "main")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "origin", "main")
	runGitTest(t, seed, "checkout", "--orphan", "shared")
	if err := os.Remove(filepath.Join(seed, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "Home.md"), []byte("# Hourglass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "-A")
	runGitTest(t, seed, "commit", "-m", "shared")
	runGitTest(t, seed, "push", "origin", "shared")
	seedQueueTemplate(t, seed)
	runGitTest(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	app := testApp(t)
	id, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	state := State{RepoURL: remote, QueueBranch: "queue/" + id.ID}
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if err := app.initGit(ctx, state); err != nil {
		t.Fatal(err)
	}
	markCurrentIndexForTest(t, app)
	if _, err := os.Stat(filepath.Join(app.Paths.Vault, "Home.md")); err != nil {
		t.Fatalf("shared worktree not initialized: %v", err)
	}
	event, err := newObservation(id, "codex", "the hidden invariant matters", app.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueue(event); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "legacy")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "reason.md"), []byte("The reason must survive.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.importMarkdownTree(legacy, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := app.sync(ctx); err != nil {
		t.Fatal(err)
	}
	queueRef := "refs/heads/" + state.QueueBranch
	tree := runGitTest(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", queueRef)
	if !strings.Contains(tree, strings.TrimPrefix(event.ID, "sha256:")+".json") {
		t.Fatalf("queue event missing from remote tree:\n%s", tree)
	}
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outbox was not acknowledged: %d files", len(entries))
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Vault, "Home.md"), []byte("# locally edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondEvent, err := newObservation(id, "codex", "queue must not depend on a clean view", app.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueue(secondEvent); err != nil {
		t.Fatal(err)
	}
	if err := app.sync(ctx); err == nil || !strings.Contains(err.Error(), "shared worktree is dirty") {
		t.Fatalf("expected independent shared error, got %v", err)
	}
	entries, err = os.ReadDir(app.Paths.Outbox)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dirty shared blocked queue delivery: entries=%d err=%v", len(entries), err)
	}
	tree = runGitTest(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", queueRef)
	if !strings.Contains(tree, strings.TrimPrefix(secondEvent.ID, "sha256:")+".json") {
		t.Fatal("event captured while shared was dirty is missing remotely")
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Vault, "Home.md"), []byte("# Hourglass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.Now = func() time.Time { return time.Date(2026, 8, 21, 1, 2, 3, 0, time.UTC) }
	if _, err := app.importMarkdownTree(legacy, "legacy"); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(app.Paths.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("delivered import was queued again: %d files", len(entries))
	}
}

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

func TestFailedPushKeepsOutboxForRetry(t *testing.T) {
	if !commandExists("git") {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "hourglass.git")
	seed := filepath.Join(root, "seed")
	runGitTest(t, "", "init", "--bare", remote)
	runGitTest(t, "", "init", "-b", "main", seed)
	runGitTest(t, seed, "config", "user.name", "test")
	runGitTest(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("control\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "README.md")
	runGitTest(t, seed, "commit", "-m", "main")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "origin", "main")
	runGitTest(t, seed, "checkout", "--orphan", "shared")
	if err := os.Remove(filepath.Join(seed, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "Home.md"), []byte("# Hourglass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "-A")
	runGitTest(t, seed, "commit", "-m", "shared")
	runGitTest(t, seed, "push", "origin", "shared")
	seedQueueTemplate(t, seed)

	app := testApp(t)
	id, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	state := State{RepoURL: remote, QueueBranch: "queue/" + id.ID}
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if err := app.initGit(ctx, state); err != nil {
		t.Fatal(err)
	}
	markCurrentIndexForTest(t, app)
	event, err := newObservation(id, "codex", "retry this event", app.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueue(event); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, app.Paths.Control, "config", "remote.origin.pushurl", filepath.Join(root, "missing.git"))
	if err := app.sync(ctx); err == nil {
		t.Fatal("sync unexpectedly succeeded with broken push URL")
	}
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outbox entries=%d err=%v", len(entries), err)
	}
	runGitTest(t, app.Paths.Control, "config", "--unset", "remote.origin.pushurl")
	if err := app.sync(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(app.Paths.Outbox)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outbox entries after retry=%d err=%v", len(entries), err)
	}
	tree := runGitTest(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", "refs/heads/"+state.QueueBranch)
	if !strings.Contains(tree, strings.TrimPrefix(event.ID, "sha256:")+".json") {
		t.Fatalf("retried event missing from remote tree:\n%s", tree)
	}
}

func TestQueueNeverMergesMainAndSkipsNoopPush(t *testing.T) {
	fixture := newGitFixture(t)
	event, err := newObservation(fixture.id, "codex", "create the endpoint queue", fixture.app.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueue(event); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	queueRef := "refs/heads/" + fixture.state.QueueBranch
	before := strings.TrimSpace(runGitTest(t, "", "--git-dir", fixture.remote, "rev-parse", queueRef))

	runGitTest(t, fixture.seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(fixture.seed, "control-v2"), []byte("must stay on main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, fixture.seed, "add", "control-v2")
	runGitTest(t, fixture.seed, "commit", "-m", "advance main")
	runGitTest(t, fixture.seed, "push", "origin", "main")

	pushLog := filepath.Join(filepath.Dir(fixture.remote), "push.log")
	hook := "#!/bin/sh\nprintf 'push\\n' >> " + shellQuote(pushLog) + "\n"
	if err := os.WriteFile(filepath.Join(fixture.remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	after := strings.TrimSpace(runGitTest(t, "", "--git-dir", fixture.remote, "rev-parse", queueRef))
	if after != before {
		t.Fatalf("no-op sync changed queue: %s -> %s", before, after)
	}
	if body, err := os.ReadFile(pushLog); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op sync pushed: body=%q err=%v", body, err)
	}
	tree := runGitTest(t, "", "--git-dir", fixture.remote, "ls-tree", "-r", "--name-only", queueRef)
	if strings.Contains(tree, "README.md") || strings.Contains(tree, "control-v2") {
		t.Fatalf("queue branch inherited control-plane files:\n%s", tree)
	}
	if !strings.Contains(tree, ".hourglass-queue") {
		t.Fatal("queue branch does not descend from the queue template")
	}
}

func TestQueueRecoversOnlyItsOwnInterruptedStage(t *testing.T) {
	fixture := newGitFixture(t)
	event, err := newObservation(fixture.id, "codex", "recover the interrupted stage", fixture.app.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueue(event); err != nil {
		t.Fatal(err)
	}
	batch, err := fixture.app.copyOutboxToQueue()
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.EventPaths) != 1 {
		t.Fatalf("event paths=%v", batch.EventPaths)
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "--", batch.EventPaths[0])
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if status := runGitTest(t, fixture.app.Paths.Queue, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("recovered queue is dirty: %s", status)
	}
	if entries, err := os.ReadDir(fixture.app.Paths.Outbox); err != nil || len(entries) != 0 {
		t.Fatalf("recovered outbox entries=%d err=%v", len(entries), err)
	}

	unowned, err := newObservation(fixture.id, "codex", "do not adopt a staged lookalike", fixture.app.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(unowned.ID, "sha256:") + ".json"
	rel := filepath.ToSlash(filepath.Join("events", unowned.CapturedAt.Format("2006"), unowned.CapturedAt.Format("01"), name))
	target := filepath.Join(fixture.app.Paths.Queue, filepath.FromSlash(rel))
	content, err := canonicalEventBytes(unowned)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "--", rel)
	err = fixture.app.sync(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "no matching outbox") {
		t.Fatalf("unowned staged event was adopted: %v", err)
	}
	if staged := strings.TrimSpace(runGitTest(t, fixture.app.Paths.Queue, "diff", "--cached", "--name-only")); staged != rel {
		t.Fatalf("unowned stage was modified: %q", staged)
	}
}

func TestInterruptedQueueBatchPrecedesNewLexicalBacklog(t *testing.T) {
	fixture := newGitFixture(t)
	var events []Event
	for index := 0; index < 8; index++ {
		event, err := newObservation(fixture.id, "codex", fmt.Sprintf("recovery ordering %d", index), fixture.app.Now().Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	recovered := events[len(events)-1]
	if err := fixture.app.enqueue(recovered); err != nil {
		t.Fatal(err)
	}
	interrupted, err := fixture.app.copyOutboxToQueue()
	if err != nil || len(interrupted.EventPaths) != 1 {
		t.Fatalf("interrupted batch=%+v err=%v", interrupted, err)
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "--", interrupted.EventPaths[0])
	for _, event := range events[:MaxSyncEvents] {
		if event.ID >= recovered.ID {
			t.Fatal("test backlog is not lexically earlier than the recovered event")
		}
		if err := fixture.app.enqueue(event); err != nil {
			t.Fatal(err)
		}
	}

	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture.app.Paths.Outbox)
	if err != nil || len(entries) != MaxSyncEvents {
		t.Fatalf("first recovery left %d outbox entries, want %d: %v", len(entries), MaxSyncEvents, err)
	}
	queueRef := "refs/heads/" + fixture.state.QueueBranch
	tree := runGitTest(t, "", "--git-dir", fixture.remote, "ls-tree", "-r", "--name-only", queueRef)
	if !strings.Contains(tree, strings.TrimPrefix(recovered.ID, "sha256:")+".json") {
		t.Fatal("recovered event was starved behind newer lexical backlog")
	}
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(fixture.app.Paths.Outbox)
	if err != nil || len(entries) != 0 {
		t.Fatalf("backlog did not drain after recovery: entries=%d err=%v", len(entries), err)
	}
}

func TestQueueStagesOnlyValidatedTargetsAndRecoversOwnTemps(t *testing.T) {
	fixture := newGitFixture(t)
	event, err := newObservation(fixture.id, "codex", "stage only this event", fixture.app.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueue(event); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(fixture.app.Paths.Queue, "user-note.txt")
	if err := os.WriteFile(unrelated, []byte("do not stage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = fixture.app.sync(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "unexpected change") {
		t.Fatalf("unexpected queue file was not rejected: %v", err)
	}
	if gitRefExists(testContext(t), fixture.app.Paths.Queue, "refs/remotes/origin/"+fixture.state.QueueBranch) {
		t.Fatal("queue was pushed despite an unexpected file")
	}
	targetDir := filepath.Join(fixture.app.Paths.Queue, "events", "2026", "07")
	temp := filepath.Join(targetDir, ".hgctl-interrupted")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(unrelated); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned temp remains: %v", err)
	}
	queueRef := "refs/heads/" + fixture.state.QueueBranch
	changed := strings.Fields(runGitTest(t, "", "--git-dir", fixture.remote, "diff-tree", "--no-commit-id", "--name-only", "-r", queueRef))
	if len(changed) != 1 || !strings.HasSuffix(changed[0], strings.TrimPrefix(event.ID, "sha256:")+".json") {
		t.Fatalf("queue commit staged unexpected paths: %v", changed)
	}

	second, err := newObservation(fixture.id, "codex", "deliver after tracked repair", fixture.app.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueue(second); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.app.Paths.Queue, ".hourglass-queue")
	if err := os.WriteFile(marker, []byte("user edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = fixture.app.sync(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "tracked worktree changes") {
		t.Fatalf("tracked queue edit was not rejected: %v", err)
	}
	if err := os.WriteFile(marker, []byte("hourglass.queue-template/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

func TestQueueDivergenceFailsInsteadOfMerging(t *testing.T) {
	fixture := newGitFixture(t)
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.app.Paths.Queue, "local-only"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "local-only")
	runGitTest(t, fixture.app.Paths.Queue, "commit", "-m", "local divergence")

	other := filepath.Join(filepath.Dir(fixture.remote), "other")
	runGitTest(t, "", "clone", fixture.remote, other)
	runGitTest(t, other, "config", "user.name", "test")
	runGitTest(t, other, "config", "user.email", "test@example.com")
	runGitTest(t, other, "checkout", fixture.state.QueueBranch)
	if err := os.WriteFile(filepath.Join(other, "remote-only"), []byte("remote\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, other, "add", "remote-only")
	runGitTest(t, other, "commit", "-m", "remote divergence")
	runGitTest(t, other, "push", "origin", fixture.state.QueueBranch)

	err := fixture.app.sync(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "queue sync") {
		t.Fatalf("divergent queue did not fail: %v", err)
	}
	parents := strings.Fields(runGitTest(t, fixture.app.Paths.Queue, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 2 {
		t.Fatalf("queue gained a merge commit: %v", parents)
	}
}

func TestSharedMustEqualRemoteBeforeReindex(t *testing.T) {
	fixture := newGitFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.app.Paths.Vault, "local-only.md"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, fixture.app.Paths.Vault, "add", "local-only.md")
	runGitTest(t, fixture.app.Paths.Vault, "commit", "-m", "local shared commit")

	bin := filepath.Join(fixture.app.Paths.Home, "fake-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	reindexLog := filepath.Join(fixture.app.Paths.Home, "reindex.log")
	script := "#!/bin/sh\nprintf reindex >> " + shellQuote(reindexLog) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "basic-memory"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err := fixture.app.sync(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "ahead of or diverged") {
		t.Fatalf("locally advanced shared was not rejected: %v", err)
	}
	if body, err := os.ReadFile(reindexLog); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed shared sync triggered reindex: body=%q err=%v", body, err)
	}
}

func TestControlCheckoutRequiresOwnershipAndStableOrigin(t *testing.T) {
	fixture := newGitFixture(t)
	for key, want := range map[string]string{
		"core.hooksPath": "/dev/null", "commit.gpgSign": "false", "tag.gpgSign": "false",
	} {
		got := strings.TrimSpace(runGitTest(t, fixture.app.Paths.Control, "config", "--local", "--get", key))
		if got != want {
			t.Fatalf("%s=%q, want %q", key, got, want)
		}
	}
	wrong := filepath.Join(filepath.Dir(fixture.remote), "wrong.git")
	runGitTest(t, "", "init", "--bare", wrong)
	runGitTest(t, fixture.app.Paths.Control, "remote", "set-url", "origin", wrong)
	err := fixture.app.sync(testContext(t))
	if err == nil || !strings.Contains(err.Error(), "origin does not match") {
		t.Fatalf("changed control origin was not rejected: %v", err)
	}
	got := strings.TrimSpace(runGitTest(t, fixture.app.Paths.Control, "remote", "get-url", "origin"))
	if got != wrong {
		t.Fatalf("hgctl rewrote an unowned origin: %s", got)
	}

	app := testApp(t)
	runGitTest(t, "", "clone", "--branch", "main", fixture.remote, app.Paths.Control)
	if err := app.initGit(testContext(t), State{RepoURL: fixture.remote, QueueBranch: "queue/00000000-0000-4000-8000-000000000000"}); err == nil || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("unmarked checkout was adopted: %v", err)
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

func markCurrentIndexForTest(t *testing.T, app *App) {
	t.Helper()
	head := strings.TrimSpace(runGitTest(t, app.Paths.Vault, "rev-parse", "HEAD"))
	state, err := app.loadState()
	if err != nil {
		t.Fatal(err)
	}
	state.BasicMemoryProject = &BasicMemoryOwnership{
		ExternalID: "test-project-id",
		Path:       app.Paths.Vault,
		Managed:    false,
	}
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(app.Paths.IndexedSHA, BasicMemoryIndexReceipt{
		SharedSHA:         head,
		ProjectExternalID: state.BasicMemoryProject.ExternalID,
	}, 0o600); err != nil {
		t.Fatal(err)
	}
}

type gitFixture struct {
	app    *App
	id     Identity
	state  State
	remote string
	seed   string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "hourglass.git")
	seed := filepath.Join(root, "seed")
	runGitTest(t, "", "init", "--bare", remote)
	runGitTest(t, "", "init", "-b", "main", seed)
	runGitTest(t, seed, "config", "user.name", "test")
	runGitTest(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("control\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "README.md")
	runGitTest(t, seed, "commit", "-m", "main")
	runGitTest(t, seed, "remote", "add", "origin", remote)
	runGitTest(t, seed, "push", "origin", "main")
	runGitTest(t, seed, "checkout", "--orphan", "shared")
	if err := os.Remove(filepath.Join(seed, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "Home.md"), []byte("# Hourglass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", "-A")
	runGitTest(t, seed, "commit", "-m", "shared")
	runGitTest(t, seed, "push", "origin", "shared")
	seedQueueTemplate(t, seed)
	runGitTest(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	app := testApp(t)
	id, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	state := State{RepoURL: remote, QueueBranch: "queue/" + id.ID}
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}
	if err := app.initGit(testContext(t), state); err != nil {
		t.Fatal(err)
	}
	markCurrentIndexForTest(t, app)
	return gitFixture{app: app, id: id, state: state, remote: remote, seed: seed}
}

func seedQueueTemplate(t *testing.T, repository string) {
	t.Helper()
	runGitTest(t, repository, "checkout", "--orphan", "queue-template")
	runGitTest(t, repository, "rm", "-rf", "--ignore-unmatch", "--", ".")
	if err := os.WriteFile(filepath.Join(repository, ".hourglass-queue"), []byte("hourglass.queue-template/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repository, "add", ".hourglass-queue")
	runGitTest(t, repository, "commit", "-m", "queue template")
	runGitTest(t, repository, "push", "origin", "queue-template")
}
