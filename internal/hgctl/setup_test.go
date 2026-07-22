package hgctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeManagedHookFile seeds a client config that carries one hgctl-managed hook
// command for client alongside a preserved unrelated hook, so prune and uninstall
// tests have something to prune now that hgctl installs no hooks itself.
func writeManagedHookFile(t *testing.T, path, binary, client string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "other-tool stop"},
					map[string]any{"type": "command", "command": binary + " hook --client " + client + " --event stop", "timeout": float64(5)},
				},
			}},
		},
	}
	if err := writeJSONAtomic(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPruneClientHookFileRemovesOnlyManagedCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	binary := "/Users/test/.local/bin/hgctl"
	original := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "other-tool stop"},
					map[string]any{"type": "command", "command": "/opt/other/hgctl hook --client claude --event stop"},
					map[string]any{"type": "command", "command": binary + " hook --client claude --event stop --user-owned"},
					map[string]any{"type": "command", "command": binary + " hook --client claude --event stop"},
					map[string]any{"type": "command", "command": binary + " hook --client claude --event user-prompt"},
				},
			}},
		},
	}
	if err := writeJSONAtomic(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneClientHookFile(path, binary, "claude"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The two hgctl-managed commands for this binary+client are gone.
	if got := countJSONText(b, binary+" hook --client claude --event stop\""); got != 0 {
		t.Fatalf("managed stop hook was not pruned (count=%d):\n%s", got, b)
	}
	if got := countJSONText(b, "user-prompt"); got != 0 {
		t.Fatalf("managed user-prompt hook was not pruned (count=%d):\n%s", got, b)
	}
	// Everything else is preserved: an unrelated tool, a same-prefix hook from a
	// different binary, and a look-alike carrying an extra argument.
	if got := countJSONText(b, "other-tool stop"); got != 1 {
		t.Fatalf("unrelated hook count=%d", got)
	}
	if got := countJSONText(b, "/opt/other/hgctl"); got != 1 {
		t.Fatalf("other-binary hgctl hook count=%d", got)
	}
	if got := countJSONText(b, "--user-owned"); got != 1 {
		t.Fatalf("look-alike hook count=%d", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["theme"] != "dark" {
		t.Fatal("unrelated setting changed")
	}
}

func TestPruneClientHookFileDropsEmptiedEventGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	binary := "/Users/test/.local/bin/hgctl"
	original := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": binary + " hook --client claude --event user-prompt", "timeout": float64(3)},
				},
			}},
			"Stop": []any{map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": binary + " hook --client claude --event stop --user-owned"},
				},
			}},
		},
	}
	if err := writeJSONAtomic(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneClientHookFile(path, binary, "claude"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root.Hooks["UserPromptSubmit"]; exists {
		t.Fatalf("emptied UserPromptSubmit event group was left behind:\n%s", b)
	}
	if _, exists := root.Hooks["Stop"]; !exists {
		t.Fatalf("user-owned Stop hook event was removed:\n%s", b)
	}
	if got := countJSONText(b, "--user-owned"); got != 1 {
		t.Fatalf("user-owned look-alike hook was not preserved (count=%d):\n%s", got, b)
	}
}

// A prune with nothing to remove must not rewrite the file, so the repair that
// runs on every sync never churns an untouched config.
func TestPruneClientHookFileIsNoOpWithoutManagedHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "command": "other-tool stop",
            "type": "command"
          }
        ]
      }
    ]
  },
  "theme": "dark"
}
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneClientHookFile(path, "/tmp/hgctl", "claude"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatalf("prune churned a file with no managed hooks:\nbefore:\n%s\nafter:\n%s", original, after)
	}
}

func TestPruneClientHooksPrunesPresentAndToleratesAbsent(t *testing.T) {
	app := testApp(t)
	stable := filepath.Join(app.Paths.Bin, "hgctl")
	// The Claude config carries a stale managed hook; the Codex config is absent.
	claudePath := filepath.Join(app.Paths.Home, ".claude", "settings.json")
	writeManagedHookFile(t, claudePath, stable, "claude")

	if err := app.pruneClientHooks(); err != nil {
		t.Fatal(err)
	}

	present, err := managedHooksPresent(claudePath, stable, "claude")
	if err != nil || present {
		t.Fatalf("claude managed hook was not pruned: present=%v err=%v", present, err)
	}
	// The preserved unrelated hook keeps the file present.
	if _, err := os.Stat(claudePath); err != nil {
		t.Fatalf("claude config was removed: %v", err)
	}
	// An absent client config is benign and is not created.
	if _, err := os.Stat(filepath.Join(app.Paths.Home, ".codex", "hooks.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent codex config was created: %v", err)
	}
}

func TestBackgroundHookRepairPrunesStaleAndIsIdempotent(t *testing.T) {
	prepareInstalledBinary := func(t *testing.T, app *App) {
		t.Helper()
		target := filepath.Join(app.Paths.Versions, "test", "hgctl")
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(app.Paths.Bin, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(app.Paths.Bin, "hgctl")); err != nil {
			t.Fatal(err)
		}
		if err := app.saveState(State{}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("uninstalled is a no-op", func(t *testing.T) {
		app := testApp(t)
		stable := filepath.Join(app.Paths.Bin, "hgctl")
		path := filepath.Join(app.Paths.Home, ".claude", "settings.json")
		writeManagedHookFile(t, path, stable, "claude")
		// No managed stable symlink and no state, so repair must touch nothing.
		app.repairClientHooks(testContext(t))
		if present, err := managedHooksPresent(path, stable, "claude"); err != nil || !present {
			t.Fatalf("repair pruned hooks on an uninstalled endpoint: present=%v err=%v", present, err)
		}
	})

	t.Run("lifecycle busy is deferred", func(t *testing.T) {
		app := testApp(t)
		prepareInstalledBinary(t, app)
		stable := filepath.Join(app.Paths.Bin, "hgctl")
		path := filepath.Join(app.Paths.Home, ".claude", "settings.json")
		writeManagedHookFile(t, path, stable, "claude")
		locked := make(chan struct{})
		release := make(chan struct{})
		done := make(chan error, 1)
		ctx := testContext(t)
		go func() {
			done <- withFileLockWait(ctx, app.Paths.LifecycleLock, func() error {
				close(locked)
				<-release
				return nil
			})
		}()
		<-locked
		app.repairClientHooks(ctx)
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if present, err := managedHooksPresent(path, stable, "claude"); err != nil || !present {
			t.Fatalf("background repair raced a lifecycle transaction: present=%v err=%v", present, err)
		}
	})

	t.Run("prunes then no-op", func(t *testing.T) {
		app := testApp(t)
		prepareInstalledBinary(t, app)
		stable := filepath.Join(app.Paths.Bin, "hgctl")
		path := filepath.Join(app.Paths.Home, ".claude", "settings.json")
		writeManagedHookFile(t, path, stable, "claude")
		app.repairClientHooks(testContext(t))
		if present, err := managedHooksPresent(path, stable, "claude"); err != nil || present {
			t.Fatalf("repair did not prune the stale managed hook: present=%v err=%v", present, err)
		}
		first, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		app.repairClientHooks(testContext(t))
		second, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("a second repair churned an already-pruned file:\n%s\n%s", first, second)
		}
	})
}

func TestPruneClientHookFilePreservesDotfileSymlink(t *testing.T) {
	root := t.TempDir()
	binary := "/tmp/hgctl"
	target := filepath.Join(root, "dotfiles", "claude-settings.json")
	link := filepath.Join(root, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	writeManagedHookFile(t, target, binary, "claude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if present, err := managedHooksPresent(link, binary, "claude"); err != nil || !present {
		t.Fatalf("managed hooks missing through symlink: present=%v err=%v", present, err)
	}
	if err := pruneClientHookFile(link, binary, "claude"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced during prune: mode=%v", info.Mode())
	}
	if present, err := managedHooksPresent(link, binary, "claude"); err != nil || present {
		t.Fatalf("managed hooks remain through symlink after prune: present=%v err=%v", present, err)
	}
}

func TestBasicMemoryOwnershipRequiresExactIDAndPath(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "vault")
	if err := os.MkdirAll(want, 0o700); err != nil {
		t.Fatal(err)
	}
	ownership := BasicMemoryOwnership{ExternalID: "owned-id", Path: want, Managed: true}
	projects := []basicMemoryProject{{Name: ProjectName, ExternalID: "owned-id", LocalPath: want}}
	if !basicMemoryOwnershipMatches(ownership, projects, want) {
		t.Fatal("exact project ownership did not match")
	}
	projects[0].ExternalID = "replacement-id"
	if basicMemoryOwnershipMatches(ownership, projects, want) {
		t.Fatal("replacement project inherited stale ownership")
	}
	projects[0].ExternalID = "owned-id"
	projects[0].LocalPath = filepath.Join(root, "other")
	if basicMemoryOwnershipMatches(ownership, projects, want) {
		t.Fatal("project at another path inherited ownership")
	}
}

func TestBasicMemorySetupAdoptsExistingProjectWithoutTakingOwnership(t *testing.T) {
	vault := t.TempDir()
	project := basicMemoryProject{Name: ProjectName, ExternalID: "project-id", LocalPath: vault}
	adopted := basicMemoryOwnershipForSetup(nil, project, false, vault)
	if adopted.Managed {
		t.Fatal("an existing Basic Memory project was marked as hgctl-managed")
	}
	created := basicMemoryOwnershipForSetup(nil, project, true, vault)
	if !created.Managed {
		t.Fatal("a project created by hgctl was not marked managed")
	}
	preserved := basicMemoryOwnershipForSetup(&created, project, false, vault)
	if !preserved.Managed {
		t.Fatal("idempotent setup lost ownership of the project hgctl created")
	}
	replacement := project
	replacement.ExternalID = "replacement-id"
	replaced := basicMemoryOwnershipForSetup(&created, replacement, false, vault)
	if replaced.Managed {
		t.Fatal("a replacement project inherited ownership")
	}
	pending := BasicMemoryOwnership{Path: vault, Managed: true, Pending: true}
	recovered := basicMemoryOwnershipForSetup(&pending, project, false, vault)
	if !recovered.Managed || recovered.Pending {
		t.Fatalf("interrupted project creation was not recovered: %+v", recovered)
	}
}

func TestBasicMemoryRemovalRequiresManagedExactIdentity(t *testing.T) {
	tests := []struct {
		name       string
		ownership  BasicMemoryOwnership
		wantRemove bool
	}{
		{name: "adopted project", ownership: BasicMemoryOwnership{ExternalID: "project-id", Managed: false}},
		{name: "replaced project", ownership: BasicMemoryOwnership{ExternalID: "old-project-id", Managed: true}},
		{name: "managed exact project", ownership: BasicMemoryOwnership{ExternalID: "project-id", Managed: true}, wantRemove: true},
		{name: "interrupted managed creation", ownership: BasicMemoryOwnership{Managed: true, Pending: true}, wantRemove: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t)
			if err := os.MkdirAll(app.Paths.Vault, 0o700); err != nil {
				t.Fatal(err)
			}
			test.ownership.Path = app.Paths.Vault
			if err := app.saveState(State{BasicMemoryProject: &test.ownership}); err != nil {
				t.Fatal(err)
			}
			if err := writeJSONAtomic(app.Paths.IndexedSHA, BasicMemoryIndexReceipt{
				SharedSHA:         strings.Repeat("a", 40),
				ProjectExternalID: test.ownership.ExternalID,
			}, 0o600); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(app.Paths.Home, "fake-bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(app.Paths.Home, "remove.log")
			script := `#!/bin/sh
if [ "$1" = "tool" ] && [ "$2" = "list-projects" ]; then
  printf '{"projects":[{"name":"hourglass","external_id":"%s","local_path":"%s"}]}\n' "$BM_ID" "$BM_PATH"
  exit 0
fi
if [ "$1" = "project" ] && [ "$2" = "remove" ]; then
  printf '%s\n' "$*" >> "$BM_REMOVE_LOG"
  exit 0
fi
exit 20
`
			if err := os.WriteFile(filepath.Join(bin, "basic-memory"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("BM_ID", "project-id")
			t.Setenv("BM_PATH", app.Paths.Vault)
			t.Setenv("BM_REMOVE_LOG", logPath)
			if err := app.removeManagedBasicMemoryProject(testContext(t)); err != nil {
				t.Fatal(err)
			}
			_, logErr := os.Stat(logPath)
			removed := logErr == nil
			if removed != test.wantRemove {
				t.Fatalf("removed=%v, want %v", removed, test.wantRemove)
			}
			state, err := app.loadState()
			if err != nil {
				t.Fatal(err)
			}
			if test.wantRemove {
				if state.BasicMemoryProject != nil {
					t.Fatalf("ownership remains after removal: %+v", state.BasicMemoryProject)
				}
				if _, err := os.Stat(app.Paths.IndexedSHA); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("index receipt remains: %v", err)
				}
			} else if state.BasicMemoryProject == nil {
				t.Fatal("unowned or replaced project state was cleared")
			}
		})
	}
}

func TestUserLingerIsEnabledWithoutSudoOrFailsWithMigrationCommand(t *testing.T) {
	t.Run("enables linger", func(t *testing.T) {
		root := t.TempDir()
		bin := filepath.Join(root, "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(root, "linger")
		logPath := filepath.Join(root, "calls")
		script := `#!/bin/sh
if [ "$1" = "--no-ask-password" ]; then shift; fi
if [ "$1" = "show-user" ]; then
  [ -f "$LINGER_STATE" ] && echo yes || echo no
  exit 0
fi
if [ "$1" = "enable-linger" ]; then
  printf '%s\n' "$*" >> "$LINGER_LOG"
  : > "$LINGER_STATE"
  exit 0
fi
exit 20
`
		if err := os.WriteFile(filepath.Join(bin, "loginctl"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("LINGER_STATE", statePath)
		t.Setenv("LINGER_LOG", logPath)
		if err := ensureUserLinger(testContext(t)); err != nil {
			t.Fatal(err)
		}
		calls, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(calls), "sudo") || !strings.Contains(string(calls), "enable-linger") {
			t.Fatalf("unexpected loginctl call: %q", calls)
		}
	})

	t.Run("reports one-time migration command", func(t *testing.T) {
		bin := t.TempDir()
		script := `#!/bin/sh
if [ "$1" = "--no-ask-password" ]; then shift; fi
if [ "$1" = "show-user" ]; then echo no; exit 0; fi
if [ "$1" = "enable-linger" ]; then echo denied >&2; exit 1; fi
exit 20
`
		if err := os.WriteFile(filepath.Join(bin, "loginctl"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		err := ensureUserLinger(testContext(t))
		if err == nil || !strings.Contains(err.Error(), "sudo loginctl enable-linger") {
			t.Fatalf("missing migration command: %v", err)
		}
	})
}

func TestUninstallRemovesManagedIntegrationAfterSchedulerStops(t *testing.T) {
	app := testApp(t)
	stable := prepareUninstallFixture(t, app, false)
	// Client removal must not strand hooks that were previously owned by hgctl.
	t.Setenv("PATH", filepath.Join(app.Paths.Home, "fake-bin"))
	if commandExists("claude") || commandExists("codex") {
		t.Fatal("client executables unexpectedly remain in the uninstall fixture")
	}
	writeManagedHookFile(t, filepath.Join(app.Paths.Home, ".claude", "settings.json"), stable, "claude")
	writeManagedHookFile(t, filepath.Join(app.Paths.Home, ".codex", "hooks.json"), stable, "codex")
	if err := app.uninstall(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stable); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stable binary was not removed: %v", err)
	}
	for _, item := range []struct{ path, client string }{
		{filepath.Join(app.Paths.Home, ".claude", "settings.json"), "claude"},
		{filepath.Join(app.Paths.Home, ".codex", "hooks.json"), "codex"},
	} {
		present, err := managedHooksPresent(item.path, stable, item.client)
		if err != nil || present {
			t.Fatalf("%s hook remains=%v err=%v", item.client, present, err)
		}
	}
	if present, err := app.schedulerFilesPresent(); err != nil || present {
		t.Fatalf("scheduler files remain=%v err=%v", present, err)
	}
}

func TestUninstallPreservesBinaryWhenHookOrSchedulerCleanupFails(t *testing.T) {
	t.Run("hook cleanup fails", func(t *testing.T) {
		app := testApp(t)
		stable := prepareUninstallFixture(t, app, false)
		path := filepath.Join(app.Paths.Home, ".claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := app.uninstall(testContext(t)); err == nil {
			t.Fatal("uninstall succeeded with an unremovable managed hook file")
		}
		if !managedStableSymlink(stable, app.Paths.Versions) {
			t.Fatal("binary was removed after hook cleanup failed")
		}
	})

	t.Run("scheduler cleanup fails", func(t *testing.T) {
		app := testApp(t)
		stable := prepareUninstallFixture(t, app, true)
		writeManagedHookFile(t, filepath.Join(app.Paths.Home, ".claude", "settings.json"), stable, "claude")
		if err := app.uninstall(testContext(t)); err == nil {
			t.Fatal("uninstall succeeded with an unremovable scheduler artifact")
		}
		if !managedStableSymlink(stable, app.Paths.Versions) {
			t.Fatal("binary was removed after scheduler cleanup failed")
		}
	})
}

func TestUninstallReportsIncompleteBasicMemoryCleanupAndPreservesRetryPath(t *testing.T) {
	for _, test := range []struct {
		name        string
		installTool bool
	}{
		{name: "command missing"},
		{name: "remove fails", installTool: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t)
			stable := prepareUninstallFixture(t, app, false)
			if err := app.saveState(State{BasicMemoryProject: &BasicMemoryOwnership{
				ExternalID: "project-id", Path: app.Paths.Vault, Managed: true,
			}}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", filepath.Join(app.Paths.Home, "fake-bin"))
			if test.installTool {
				script := `#!/bin/sh
if [ "$1 $2" = "tool list-projects" ]; then
  printf '{"projects":[{"name":"hourglass","external_id":"project-id","local_path":"%s"}]}\n' "$BM_PATH"
  exit 0
fi
if [ "$1 $2" = "project remove" ]; then exit 9; fi
exit 20
`
				if err := os.WriteFile(filepath.Join(app.Paths.Home, "fake-bin", "basic-memory"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("BM_PATH", app.Paths.Vault)
			}
			if err := app.uninstall(testContext(t)); err == nil {
				t.Fatal("uninstall succeeded without completing Basic Memory cleanup")
			}
			if !managedStableSymlink(stable, app.Paths.Versions) {
				t.Fatal("binary retry path was removed after Basic Memory cleanup failed")
			}
			output := app.Out.(*bytes.Buffer).String()
			if !strings.Contains(output, "removal is incomplete") || strings.Contains(output, "integration removed;") {
				t.Fatalf("untruthful uninstall output: %q", output)
			}
		})
	}
}

func prepareUninstallFixture(t *testing.T, app *App, blockSchedulerRemoval bool) string {
	t.Helper()
	target := filepath.Join(app.Paths.Versions, "test", "hgctl")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Bin, 0o700); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(app.Paths.Bin, "hgctl")
	if err := os.Symlink(target, stable); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(app.Paths.Home, "fake-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		script := `#!/bin/sh
if [ "$1" = "bootout" ]; then exit 0; fi
if [ "$1" = "print" ]; then echo 'not found' >&2; exit 1; fi
exit 20
`
		if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(app.Paths.Home, "Library", "LaunchAgents", LaunchLabel+".plist")
		writeSchedulerFixture(t, path, blockSchedulerRemoval, stable)
	case "linux":
		script := `#!/bin/sh
case "$*" in
  *" show "*) echo inactive; exit 0 ;;
  *" disable --now "*|*" stop "*|*" daemon-reload"*) exit 0 ;;
esac
exit 20
`
		if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(app.Paths.Home, ".config", "systemd", "user")
		writeSchedulerFixture(t, filepath.Join(dir, LaunchLabel+".service"), blockSchedulerRemoval, stable)
		writeSchedulerFixture(t, filepath.Join(dir, LaunchLabel+".timer"), false, stable)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stable
}

func writeSchedulerFixture(t *testing.T, path string, blockRemoval bool, stable string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if blockRemoval {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	body := "[Unit]\nDescription=Run Hourglass sync every minute\n\n[Timer]\nOnUnitActiveSec=60s\n"
	if strings.HasSuffix(path, ".plist") {
		body = launchAgent(stable, filepath.Dir(filepath.Dir(filepath.Dir(path))), filepath.Dir(filepath.Dir(filepath.Dir(path))), "vault")
	} else if strings.HasSuffix(path, ".service") {
		body = "[Unit]\nDescription=Hourglass sync\n\n[Service]\nExecStart=" + systemdEscape(stable) + " sync --update\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStableSymlinkNeverReplacesRegularFile(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "bin", "hgctl")
	target := filepath.Join(root, "versions", "1", "hgctl")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("user file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceStableSymlink(link, target); err == nil {
		t.Fatal("regular file was replaced")
	}
	b, err := os.ReadFile(link)
	if err != nil || string(b) != "user file" {
		t.Fatalf("regular file changed: %q err=%v", b, err)
	}
}

func TestSchedulerOwnershipRejectsUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, ".local", "bin", "hgctl")
	path := filepath.Join(root, "Library", "LaunchAgents", LaunchLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySchedulerFile(path, stable); err == nil {
		t.Fatal("unrelated scheduler file was treated as managed")
	}
	if err := os.WriteFile(path, []byte(launchAgent(stable, "data", root, "vault")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySchedulerFile(path, stable); err != nil {
		t.Fatalf("managed scheduler file was rejected: %v", err)
	}
	changedTemplate := strings.ReplaceAll(launchAgent(stable, "data", root, "vault"), "  <key>StartInterval</key><integer>60</integer>\n", "")
	if err := os.WriteFile(path, []byte(changedTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySchedulerFile(path, stable); err != nil {
		t.Fatalf("versioned ownership depended on the scheduler template: %v", err)
	}
	futureOwnership := strings.Replace(changedTemplate, schedulerOwnership, "x2x3studio.hgctl.scheduler/v2", 1)
	if err := os.WriteFile(path, []byte(futureOwnership), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySchedulerFile(path, stable); err == nil {
		t.Fatal("future scheduler ownership version was overwritten")
	}
}

func TestManagedStableSymlinkRequiresVersionTree(t *testing.T) {
	root := t.TempDir()
	versions := filepath.Join(root, "versions")
	managed := filepath.Join(versions, "1", "hgctl")
	other := filepath.Join(root, "other")
	for _, path := range []string{managed, other} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "hgctl")
	if err := os.Symlink(managed, link); err != nil {
		t.Fatal(err)
	}
	if !managedStableSymlink(link, versions) {
		t.Fatal("managed version symlink was not recognized")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if managedStableSymlink(link, versions) {
		t.Fatal("unrelated symlink was treated as managed")
	}
}

func TestBasicMemoryReindexIsReadOnlyAndReceiptBound(t *testing.T) {
	app := testApp(t)
	if err := os.MkdirAll(app.Paths.Shared, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "", "init", "-b", "shared", app.Paths.Shared)
	runGitTest(t, app.Paths.Shared, "config", "user.name", "test")
	runGitTest(t, app.Paths.Shared, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(app.Paths.Shared, "Home.md"), []byte("# Hourglass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, app.Paths.Shared, "add", "Home.md")
	runGitTest(t, app.Paths.Shared, "commit", "-m", "shared")
	bin := filepath.Join(app.Paths.Home, "fake-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "basic-memory")
	logPath := filepath.Join(app.Paths.Home, "reindex.log")
	script := `#!/bin/sh
[ "$1 $2" != "tool list-projects" ] || {
  printf '{"projects":[{"name":"hourglass","external_id":"%s","local_path":"%s"}]}\n' "$HGCTL_TEST_PROJECT_ID" "$HGCTL_TEST_PROJECT_PATH"
  exit 0
}
[ "$1" = "reindex" ] && [ "$2" = "--project" ] && [ "$3" = "hourglass" ] || exit 15
printf x >> "$HGCTL_TEST_REINDEX_LOG"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HGCTL_TEST_REINDEX_LOG", logPath)
	t.Setenv("HGCTL_TEST_PROJECT_ID", "project-id")
	t.Setenv("HGCTL_TEST_PROJECT_PATH", app.Paths.Vault)
	if err := app.saveState(State{BasicMemoryProject: &BasicMemoryOwnership{
		ExternalID: "project-id",
		Path:       app.Paths.Vault,
		Managed:    true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := app.reindexBasicMemory(testContext(t)); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(runGitTest(t, app.Paths.Shared, "rev-parse", "HEAD"))
	var receipt BasicMemoryIndexReceipt
	if err := readJSON(app.Paths.IndexedSHA, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.SharedSHA != head || receipt.ProjectExternalID != "project-id" {
		t.Fatalf("index receipt=%+v", receipt)
	}
	state, err := app.loadState()
	if err != nil {
		t.Fatal(err)
	}
	state.BasicMemoryProject.ExternalID = "replacement-project-id"
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HGCTL_TEST_PROJECT_ID", "replacement-project-id")
	if err := app.reindexBasicMemory(testContext(t)); err != nil {
		t.Fatal(err)
	}
	logBody, err := os.ReadFile(logPath)
	if err != nil || string(logBody) != "xx" {
		t.Fatalf("project replacement did not force reindex: log=%q err=%v", logBody, err)
	}
	if err := readJSON(app.Paths.IndexedSHA, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ProjectExternalID != "replacement-project-id" {
		t.Fatalf("replacement project was not bound in receipt: %+v", receipt)
	}
	if err := os.Remove(fake); err != nil {
		t.Fatal(err)
	}
	if err := app.reindexBasicMemory(testContext(t)); err != nil {
		t.Fatalf("matching receipt should skip external reindex: %v", err)
	}
}

func countJSONText(content []byte, needle string) int {
	return bytes.Count(content, []byte(needle))
}
