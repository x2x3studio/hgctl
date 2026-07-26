package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x2x3studio/hgctl/internal/proc"

	"github.com/x2x3studio/hgctl/internal/config"
)

// seedQueueRepo makes a real queue repository with machine.json committed, which
// is the state every machine reaches after its first sync.
func seedQueueRepo(t *testing.T, app *App) {
	t.Helper()
	ctx := testContext(t)
	if err := os.MkdirAll(filepath.Join(app.Paths.Queue, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := proc.Run(ctx, app.Paths.Queue, "git", args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	git("init", "--quiet")
	git("config", "user.name", "hgctl-test")
	git("config", "user.email", "hgctl-test@example.invalid")
	meta, err := renderMachineMeta(config.Identity{ID: "a943c6d2", Hostname: "Orange"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Queue, machineMetaFile), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "--", machineMetaFile)
	git("commit", "--quiet", "-m", "record machine metadata")
}

func queueStatus(t *testing.T, app *App) string {
	t.Helper()
	out, err := proc.Run(testContext(t), app.Paths.Queue, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// machine.json is the only tracked file in a queue that is rewritten in place,
// and both guards were written when a brand-new event under events/ was the only
// thing that could ever be staged. So it passed on the commit that CREATED it
// and failed on every change after - an hgctl release bump was enough.
func TestQueueGuardAcceptsAModifiedMachineMeta(t *testing.T) {
	app := testApp(t)
	seedQueueRepo(t, app)
	ctx := testContext(t)

	meta, err := renderMachineMeta(config.Identity{ID: "a943c6d2", Hostname: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Queue, machineMetaFile), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireOnlyQueueTargets(ctx, app.Paths.Queue, []string{machineMetaFile}, false); err != nil {
		t.Fatalf("unstaged metadata edit rejected: %v", err)
	}
	if _, err := proc.Run(ctx, app.Paths.Queue, "git", "add", "--", machineMetaFile); err != nil {
		t.Fatal(err)
	}
	if err := requireOnlyQueueTargets(ctx, app.Paths.Queue, []string{machineMetaFile}, true); err != nil {
		t.Fatalf("staged metadata edit rejected, so the file can never be updated: %v", err)
	}
}

// The exemption is for machine.json alone. An event that turns up MODIFIED means
// captured evidence was rewritten, which must still be refused.
func TestQueueGuardStillRefusesAModifiedEvent(t *testing.T) {
	app := testApp(t)
	seedQueueRepo(t, app)
	ctx := testContext(t)

	event := filepath.Join("events", "2026-07-25T00-00-00Z-aaaa.md")
	full := filepath.Join(app.Paths.Queue, event)
	if err := os.WriteFile(full, []byte("captured\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--", event}, {"commit", "--quiet", "-m", "capture"}} {
		if _, err := proc.Run(ctx, app.Paths.Queue, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(full, []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Run(ctx, app.Paths.Queue, "git", "add", "--", event); err != nil {
		t.Fatal(err)
	}
	if err := requireOnlyQueueTargets(ctx, app.Paths.Queue, []string{filepath.ToSlash(event)}, true); err == nil {
		t.Fatal("a rewritten event was accepted; the queue is append-only")
	}
}

// A crash between `git add` and `git commit` of a metadata update used to wedge
// the queue PERMANENTLY: recovery runs first on every sync and refused the
// leftover, so no later sync could get past it. Recovery must revert it instead.
func TestInterruptedMachineMetaStageDoesNotWedgeTheQueue(t *testing.T) {
	app := testApp(t)
	seedQueueRepo(t, app)
	ctx := testContext(t)

	bumped, err := renderMachineMeta(config.Identity{ID: "a943c6d2", Hostname: "Orange-v2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Queue, machineMetaFile), bumped, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Run(ctx, app.Paths.Queue, "git", "add", "--", machineMetaFile); err != nil {
		t.Fatal(err)
	}
	if got := queueStatus(t, app); got != "M  "+machineMetaFile {
		t.Fatalf("test did not reproduce the interrupted stage, got %q", got)
	}

	batch, err := app.recoverInterruptedQueueBatch(ctx)
	if err != nil {
		t.Fatalf("recovery refused a leftover metadata stage, wedging every later sync: %v", err)
	}
	if len(batch.EventPaths) != 0 {
		t.Fatalf("metadata was mistaken for captured evidence: %v", batch.EventPaths)
	}
	if got := queueStatus(t, app); got != "" {
		t.Fatalf("recovery left the queue dirty, so the clean check fails next: %q", got)
	}
	// Reverted to HEAD, not to the half-written update. upsertMachineMeta
	// re-derives it from the live identity later in the same sync.
	body, err := os.ReadFile(filepath.Join(app.Paths.Queue, machineMetaFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Orange-v2") {
		t.Fatal("worktree kept the uncommitted update, which leaves the queue dirty")
	}
}

// Same crash, but before machine.json ever landed in HEAD: there is no version
// to restore, so the leftover is dropped entirely.
func TestInterruptedFirstMachineMetaStageIsDropped(t *testing.T) {
	app := testApp(t)
	ctx := testContext(t)
	if err := os.MkdirAll(filepath.Join(app.Paths.Queue, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.name", "hgctl-test"},
		{"config", "user.email", "hgctl-test@example.invalid"},
		{"commit", "--quiet", "--allow-empty", "-m", "root"},
	} {
		if _, err := proc.Run(ctx, app.Paths.Queue, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := renderMachineMeta(config.Identity{ID: "a943c6d2", Hostname: "Orange"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Queue, machineMetaFile), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Run(ctx, app.Paths.Queue, "git", "add", "--", machineMetaFile); err != nil {
		t.Fatal(err)
	}

	if _, err := app.recoverInterruptedQueueBatch(ctx); err != nil {
		t.Fatalf("recovery refused a first-ever metadata stage: %v", err)
	}
	if got := queueStatus(t, app); got != "" {
		t.Fatalf("recovery left the queue dirty: %q", got)
	}
}
