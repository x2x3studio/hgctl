package hgctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x2x3studio/hgctl/internal/config"
)

// The hook subcommand is a retired-capture no-op: whatever a stale client
// registration feeds it, it exits clean and captures nothing.
func TestHookCommandIsANoOp(t *testing.T) {
	for _, in := range []string{`{not-json`, `{}`, `{"prompt":"hi","last_assistant_message":"yo"}`, ""} {
		app := testApp(t)
		app.In = strings.NewReader(in)
		if code := app.Run(testContext(t), []string{"hook", "--client", "claude", "--event", "stop"}); code != 0 {
			t.Fatalf("hook exit code=%d for input %q, want 0", code, in)
		}
		if output := app.Out.(*bytes.Buffer).String(); output != "" {
			t.Fatalf("hook wrote stdout: %q", output)
		}
		if output := app.Err.(*bytes.Buffer).String(); output != "" {
			t.Fatalf("hook wrote stderr: %q", output)
		}
		if entries, err := os.ReadDir(app.Paths.Outbox); err == nil && len(entries) != 0 {
			t.Fatalf("hook captured %d outbox files, want 0", len(entries))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func TestPruneHookConfigPreservesRawRootValuesAndRetriesConcurrentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{
  "precise": 9007199254740993,
  "nested": { "integer": 18446744073709551615, "escaped": "\u0061" },
  "flags": [true, null, 1.2300],
  "hooks": { "Stop": [ { "hooks": [ { "type": "command", "command": "/tmp/hgctl hook --client claude --event stop" } ] } ] }
}
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var before map[string]json.RawMessage
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := pruneClientHookFile(path, "/tmp/hgctl", "claude"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(content, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"precise", "nested", "flags"} {
		if !bytes.Equal(compactJSON(t, before[key]), compactJSON(t, after[key])) {
			t.Fatalf("unrelated root value %q changed: before=%s after=%s", key, before[key], after[key])
		}
	}
	if present, err := managedHooksPresent(path, "/tmp/hgctl", "claude"); err != nil || present {
		t.Fatalf("managed hook was not pruned: present=%v err=%v", present, err)
	}

	seed := []byte(`{"sentinel":"initial","hooks":{"Stop":[{"hooks":[{"type":"command","command":"/tmp/hgctl hook --client codex --event stop"}]}]}}` + "\n")
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	concurrent := []byte(`{"sentinel":"concurrent","precise":9007199254740993,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/tmp/hgctl hook --client codex --event stop"}]}]}}` + "\n")
	mutations := 0
	err = pruneClientHookFileWithRetry(path, path, "/tmp/hgctl", "codex", func(attempt int) {
		if attempt == 0 {
			mutations++
			if err := os.WriteFile(path, concurrent, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutations != 1 {
		t.Fatalf("concurrent change was not retried once: mutations=%d", mutations)
	}
	if present, err := managedHooksPresent(path, "/tmp/hgctl", "codex"); err != nil || present {
		t.Fatalf("managed codex hook not pruned after retry: present=%v err=%v", present, err)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte(`"sentinel": "concurrent"`)) || !bytes.Contains(content, []byte(`9007199254740993`)) {
		t.Fatalf("concurrent root update was lost: %s", content)
	}
}

func compactJSON(t *testing.T, content []byte) []byte {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

// When a backlog replay rewrites origin/shared to a fresh orphan, an endpoint
// still on the old history is diverged (its HEAD is not an ancestor of the new
// origin/shared). Because shared is product-only and the vault is a disposable
// mirror, syncSharedUnlocked must hard-reset onto origin/shared and re-mirror
// rather than error forever.
func TestSyncSharedUnlockedRecoversFromDivergedOrigin(t *testing.T) {
	app := testApp(t)
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	runGitTest(t, "", "init", "--bare", origin)

	shared := app.Paths.Shared
	runGitTest(t, "", "init", "-b", "shared", shared)
	runGitTest(t, shared, "config", "user.name", "test")
	runGitTest(t, shared, "config", "user.email", "test@example.com")
	runGitTest(t, shared, "remote", "add", "origin", origin)
	if err := os.MkdirAll(filepath.Join(shared, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "memory", "old.md"), []byte("old product\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, shared, "add", ".")
	runGitTest(t, shared, "commit", "-m", "history A")
	runGitTest(t, shared, "push", "origin", "shared")
	runGitTest(t, shared, "fetch", "origin")

	// A backlog replay force-pushes a brand-new orphan history to origin/shared.
	rewrite := filepath.Join(dir, "rewrite")
	runGitTest(t, "", "init", "-b", "shared", rewrite)
	runGitTest(t, rewrite, "config", "user.name", "test")
	runGitTest(t, rewrite, "config", "user.email", "test@example.com")
	runGitTest(t, rewrite, "remote", "add", "origin", origin)
	if err := os.MkdirAll(filepath.Join(rewrite, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rewrite, "memory", "new.md"), []byte("new product\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rewrite, "Home.md"), []byte("# new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, rewrite, "add", ".")
	runGitTest(t, rewrite, "commit", "-m", "history B orphan")
	runGitTest(t, rewrite, "push", "-f", "origin", "shared")

	// The endpoint fetches the rewritten origin/shared; its HEAD (A) is now
	// diverged from origin/shared (B).
	runGitTest(t, shared, "fetch", "origin")
	if err := app.syncSharedUnlocked(testContext(t)); err != nil {
		t.Fatalf("diverged shared did not self-heal: %v", err)
	}

	head := strings.TrimSpace(runGitTest(t, shared, "rev-parse", "HEAD"))
	remote := strings.TrimSpace(runGitTest(t, shared, "rev-parse", "origin/shared"))
	if head != remote {
		t.Fatalf("shared not reset onto origin/shared: head=%s remote=%s", head, remote)
	}
	if _, err := os.Stat(filepath.Join(app.Paths.Vault, "memory", "new.md")); err != nil {
		t.Fatalf("vault missing new product after reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.Paths.Vault, "memory", "old.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault kept stale product after reset: %v", err)
	}
}

// initQueueTestRepo makes a standalone queue worktree at app.Paths.Queue on
// branch, wired to a bare origin, seeded with one committed+pushed event. It
// returns the origin path.
func initQueueTestRepo(t *testing.T, app *App, branch, seedEvent string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "queue-origin.git")
	runGitTest(t, "", "init", "--bare", origin)
	queue := app.Paths.Queue
	runGitTest(t, "", "init", "-b", branch, queue)
	runGitTest(t, queue, "config", "user.name", "chinaboard")
	runGitTest(t, queue, "config", "user.email", "chinaboard@gmail.com")
	runGitTest(t, queue, "remote", "add", "origin", origin)
	if err := os.MkdirAll(filepath.Join(queue, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queue, "events", seedEvent), []byte("seed "+seedEvent+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, queue, "add", ".")
	runGitTest(t, queue, "commit", "-m", "seed")
	runGitTest(t, queue, "push", "origin", "HEAD:refs/heads/"+branch)
	runGitTest(t, queue, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	return origin
}

// pushArchiveCommit clones origin at branch into a scratch worktree and pushes an
// Action-style archive commit: it moves consumedEvent out of events/ into
// archive/<month>/, fast-forward over the current tip, mirroring archive.sh.
func pushArchiveCommit(t *testing.T, origin, branch, consumedEvent, month string) {
	t.Helper()
	work := t.TempDir()
	runGitTest(t, "", "init", "-b", branch, work)
	runGitTest(t, work, "config", "user.name", "chinaboard")
	runGitTest(t, work, "config", "user.email", "chinaboard@gmail.com")
	runGitTest(t, work, "remote", "add", "origin", origin)
	runGitTest(t, work, "fetch", "origin", branch)
	runGitTest(t, work, "reset", "--hard", "FETCH_HEAD")
	if err := os.MkdirAll(filepath.Join(work, "archive", month), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(work, "events", consumedEvent), filepath.Join(work, "archive", month, consumedEvent)); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "add", "-A")
	runGitTest(t, work, "commit", "-m", "reflect: archive 1 consumed event(s)")
	runGitTest(t, work, "push", "origin", "HEAD:refs/heads/"+branch)
}

// The common offline path: the endpoint has no local commits and origin advanced
// only via an Action archive (consumed events moved to archive/<YYYY-MM>/). The
// queue must fast-forward cleanly through the archive commit, and the archive/
// files - clean committed additions outside events/ - must not trip the queue
// guards.
func TestSyncQueueFastForwardsThroughArchiveCommit(t *testing.T) {
	app := testApp(t)
	branch := "queue/testmachine"
	consumed := "20260301T120000Z-aaaaaaaa.md"
	origin := initQueueTestRepo(t, app, branch, consumed)
	if err := os.MkdirAll(app.Paths.Outbox, 0o700); err != nil {
		t.Fatal(err)
	}

	pushArchiveCommit(t, origin, branch, consumed, "2026-03")
	runGitTest(t, app.Paths.Queue, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)

	state := config.State{QueueBranch: branch, RepoURL: origin}
	if err := app.syncQueueUnlocked(testContext(t), state); err != nil {
		t.Fatalf("clean archive fast-forward failed: %v", err)
	}

	head := strings.TrimSpace(runGitTest(t, app.Paths.Queue, "rev-parse", "HEAD"))
	remote := strings.TrimSpace(runGitTest(t, app.Paths.Queue, "rev-parse", "origin/"+branch))
	if head != remote {
		t.Fatalf("queue did not fast-forward onto origin: head=%s remote=%s", head, remote)
	}
	tree := strings.Fields(runGitTest(t, app.Paths.Queue, "ls-tree", "-r", "--name-only", "HEAD"))
	if !contains(tree, "archive/2026-03/"+consumed) || contains(tree, "events/"+consumed) {
		t.Fatalf("archive commit not adopted: tree=%v", tree)
	}
}

// The divergence path: a local committed-but-unpushed append races an Action
// archive that advanced origin. merge --ff-only cannot reconcile the two, so the
// self-heal hard-resets onto origin (adopting the archive commit) and replays the
// un-pushed event from the retained outbox. No captured event is lost and the
// branch ends fast-forwardable (local == origin).
func TestSyncQueueSelfHealsDivergedRemoteViaOutboxReplay(t *testing.T) {
	app := testApp(t)
	branch := "queue/testmachine"
	consumed := "20260301T120000Z-aaaaaaaa.md"
	origin := initQueueTestRepo(t, app, branch, consumed)

	// A captured-but-unpushed local append: committed to the local queue AND still
	// present in the outbox (a real sync clears the outbox only after a push).
	appendEvent := "20260722T010203Z-deadbeef.md"
	appendBody := []byte("second event\n")
	if err := os.MkdirAll(app.Paths.Outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Outbox, appendEvent), appendBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.Paths.Queue, "events", appendEvent), appendBody, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, app.Paths.Queue, "add", ".")
	runGitTest(t, app.Paths.Queue, "commit", "-m", "local unpushed append")

	// Origin advances via an Action archive; the endpoint fetches, and now local
	// (seed<-append) and origin (seed<-archive) share no fast-forward.
	pushArchiveCommit(t, origin, branch, consumed, "2026-03")
	runGitTest(t, app.Paths.Queue, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)

	state := config.State{QueueBranch: branch, RepoURL: origin}
	if err := app.syncQueueUnlocked(testContext(t), state); err != nil {
		t.Fatalf("diverged queue did not self-heal: %v", err)
	}

	// Converged onto origin (fast-forwardable), the archive commit adopted, and the
	// un-pushed event replayed from the outbox with no data loss.
	head := strings.TrimSpace(runGitTest(t, app.Paths.Queue, "rev-parse", "HEAD"))
	remote := strings.TrimSpace(runGitTest(t, app.Paths.Queue, "rev-parse", "origin/"+branch))
	if head != remote {
		t.Fatalf("queue did not converge onto origin: head=%s remote=%s", head, remote)
	}
	tree := strings.Fields(runGitTest(t, app.Paths.Queue, "ls-tree", "-r", "--name-only", "HEAD"))
	if !contains(tree, "archive/2026-03/"+consumed) {
		t.Fatalf("archive commit was lost by the self-heal: tree=%v", tree)
	}
	if !contains(tree, "events/"+appendEvent) {
		t.Fatalf("un-pushed append was lost by the self-heal: tree=%v", tree)
	}
	if body := runGitTest(t, app.Paths.Queue, "show", "HEAD:events/"+appendEvent); body != string(appendBody) {
		t.Fatalf("replayed append body = %q, want %q", body, appendBody)
	}
	if entries, err := os.ReadDir(app.Paths.Outbox); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("outbox not drained after successful push: %d left", len(entries))
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
