package hgctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
