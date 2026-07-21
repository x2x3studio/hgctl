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

func TestHookConfigIsIdempotentAndPreservesUnrelatedHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"Stop": []any{map[string]any{
				"hooks": []any{
					map[string]any{"type": "command", "command": "other-tool stop"},
					map[string]any{"type": "command", "command": "/opt/other/hgctl hook --client claude --event stop"},
					map[string]any{"type": "command", "command": "/Users/test/.local/bin/hgctl hook --client claude --event stop --user-owned"},
				},
			}},
		},
	}
	if err := writeJSONAtomic(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := "/Users/test/.local/bin/hgctl"
	if err := configureHookFile(path, binary, "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := configureHookFile(path, binary, "claude", true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := countJSONText(b, binary+" hook --client claude"); got != 4 {
		t.Fatalf("got %d matching command prefixes, want 4", got)
	}
	if got := countJSONText(b, "other-tool stop"); got != 1 {
		t.Fatalf("unrelated hook count=%d", got)
	}
	if got := countJSONText(b, "/opt/other/hgctl"); got != 1 {
		t.Fatalf("other hgctl hook count=%d", got)
	}
	if got := countJSONText(b, "--user-owned"); got != 1 {
		t.Fatalf("lookalike hook count=%d", got)
	}
	if err := configureHookFile(path, binary, "claude", false); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if got := countJSONText(b, binary+" hook --client claude"); got != 1 {
		t.Fatalf("unexpected matching command prefixes remain: %d", got)
	}
	if got := countJSONText(b, "other-tool stop"); got != 1 {
		t.Fatalf("unrelated hook was removed")
	}
	if got := countJSONText(b, "/opt/other/hgctl"); got != 1 {
		t.Fatalf("other hgctl hook was removed")
	}
	if got := countJSONText(b, "--user-owned"); got != 1 {
		t.Fatalf("lookalike hook was removed")
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["theme"] != "dark" {
		t.Fatal("unrelated setting changed")
	}
}

func TestSessionStartHookLeavesFailOpenHeadroom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	if err := configureHookFile(path, "/tmp/hgctl", "codex", true); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := readJSON(path, &decoded); err != nil {
		t.Fatal(err)
	}
	groups := decoded.Hooks["SessionStart"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Timeout != 10 {
		t.Fatalf("unexpected SessionStart hook: %#v", groups)
	}
}

func TestSetupHookFilesConfiguresOnlyInstalledClients(t *testing.T) {
	tests := []struct {
		name       string
		claude     bool
		codex      bool
		seedAbsent bool
	}{
		{name: "Claude only", claude: true},
		{name: "Codex only", codex: true, seedAbsent: true},
		{name: "both", claude: true, codex: true},
		{name: "neither", seedAbsent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t)
			bin := filepath.Join(app.Paths.Home, "client-bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			if test.claude {
				writeFakeExecutable(t, bin, "claude")
			}
			if test.codex {
				writeFakeExecutable(t, bin, "codex")
			}
			t.Setenv("PATH", bin)

			stable := filepath.Join(app.Paths.Bin, "hgctl")
			for _, item := range app.clientAdapters() {
				installed := (item.client == "claude" && test.claude) || (item.client == "codex" && test.codex)
				if installed || test.seedAbsent {
					if err := os.MkdirAll(filepath.Dir(item.path), 0o700); err != nil {
						t.Fatal(err)
					}
					body := []byte("{\"sentinel\":\"" + item.client + "\"}\n")
					if err := os.WriteFile(item.path, body, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := app.setupHookFiles(); err != nil {
				t.Fatal(err)
			}
			for _, item := range app.clientAdapters() {
				installed := (item.client == "claude" && test.claude) || (item.client == "codex" && test.codex)
				if installed {
					if !hooksConfigured(item.path, stable, item.client) {
						t.Fatalf("%s hooks were not configured", item.name)
					}
					content, err := os.ReadFile(item.path)
					if err != nil || !bytes.Contains(content, []byte(`"sentinel"`)) {
						t.Fatalf("%s unrelated config was not preserved: %q err=%v", item.name, content, err)
					}
					continue
				}
				content, err := os.ReadFile(item.path)
				if test.seedAbsent {
					want := "{\"sentinel\":\"" + item.client + "\"}\n"
					if err != nil || string(content) != want {
						t.Fatalf("absent %s config changed: %q err=%v", item.name, content, err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("absent %s config was created: %q err=%v", item.name, content, err)
				}
			}
		})
	}
}

func TestBackgroundHookRepairIsIdempotentAndDefersCodexTrust(t *testing.T) {
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
	t.Run("uninstalled", func(t *testing.T) {
		app := testApp(t)
		bin := filepath.Join(app.Paths.Home, "client-bin")
		writeFakeExecutable(t, bin, "claude")
		t.Setenv("PATH", bin)
		app.repairClientHooks(testContext(t))
		if _, err := os.Stat(filepath.Join(app.Paths.Home, ".claude", "settings.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("background repair configured an uninstalled endpoint: %v", err)
		}
	})
	t.Run("lifecycle busy", func(t *testing.T) {
		app := testApp(t)
		prepareInstalledBinary(t, app)
		bin := filepath.Join(app.Paths.Home, "client-bin")
		writeFakeExecutable(t, bin, "claude")
		t.Setenv("PATH", bin)
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
		if _, err := os.Stat(filepath.Join(app.Paths.Home, ".claude", "settings.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("background repair raced a lifecycle transaction: %v", err)
		}
	})

	t.Run("Claude", func(t *testing.T) {
		app := testApp(t)
		prepareInstalledBinary(t, app)
		bin := filepath.Join(app.Paths.Home, "client-bin")
		writeFakeExecutable(t, bin, "claude")
		t.Setenv("PATH", bin)
		app.repairClientHooks(testContext(t))
		path := filepath.Join(app.Paths.Home, ".claude", "settings.json")
		first, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		app.repairClientHooks(testContext(t))
		second, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) || !hooksConfigured(path, filepath.Join(app.Paths.Bin, "hgctl"), "claude") {
			t.Fatal("background repair changed an already-correct Claude hook set")
		}
	})

	t.Run("Codex", func(t *testing.T) {
		app := testApp(t)
		prepareInstalledBinary(t, app)
		t.Setenv("PATH", t.TempDir())
		installFakeCodex(t, app, "rpc-error")
		app.repairClientHooks(testContext(t))
		path := filepath.Join(app.Paths.Home, ".codex", "hooks.json")
		if !hooksConfigured(path, filepath.Join(app.Paths.Bin, "hgctl"), "codex") {
			t.Fatal("background repair did not restore Codex hooks")
		}
		if output := app.Err.(*bytes.Buffer).String(); !strings.Contains(output, "Codex hook trust deferred") {
			t.Fatalf("temporary Codex trust failure was not deferred: %q", output)
		}
	})
}

func TestSetupClientHooksSupportsClaudeOnlyAndCodexOnly(t *testing.T) {
	t.Run("Claude only", func(t *testing.T) {
		app := testApp(t)
		bin := filepath.Join(app.Paths.Home, "client-bin")
		writeFakeExecutable(t, bin, "claude")
		t.Setenv("PATH", bin)
		if err := app.setupClientHooks(testContext(t)); err != nil {
			t.Fatal(err)
		}
		if !hooksConfigured(filepath.Join(app.Paths.Home, ".claude", "settings.json"), filepath.Join(app.Paths.Bin, "hgctl"), "claude") {
			t.Fatal("Claude hooks were not configured")
		}
		if _, err := os.Stat(filepath.Join(app.Paths.Home, ".codex", "hooks.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Codex config was touched on a Claude-only endpoint: %v", err)
		}
	})

	t.Run("Codex only", func(t *testing.T) {
		app := testApp(t)
		t.Setenv("PATH", t.TempDir())
		trustFile, _ := installFakeCodex(t, app, "success")
		if err := app.setupClientHooks(testContext(t)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(trustFile); err != nil {
			t.Fatalf("Codex hooks were not trusted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(app.Paths.Home, ".claude", "settings.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Claude config was touched on a Codex-only endpoint: %v", err)
		}
	})
}

func TestClientDoctorChecksSkipAbsentAndRequireInstalledClients(t *testing.T) {
	t.Run("absent clients", func(t *testing.T) {
		app := testApp(t)
		t.Setenv("PATH", t.TempDir())
		for _, item := range app.clientAdapters() {
			if err := os.MkdirAll(filepath.Dir(item.path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(item.path, []byte("not json\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if checks := app.clientDoctorChecks(testContext(t)); len(checks) != 0 {
			t.Fatalf("doctor checked absent clients: %+v", checks)
		}
		for _, item := range app.clientAdapters() {
			content, err := os.ReadFile(item.path)
			if err != nil || string(content) != "not json\n" {
				t.Fatalf("doctor modified absent %s config: %q err=%v", item.name, content, err)
			}
		}
	})

	t.Run("Claude strict", func(t *testing.T) {
		app := testApp(t)
		bin := filepath.Join(app.Paths.Home, "client-bin")
		writeFakeExecutable(t, bin, "claude")
		t.Setenv("PATH", bin)
		checks := app.clientDoctorChecks(testContext(t))
		if len(checks) != 1 || checks[0].name != "Claude hooks" || checks[0].ok {
			t.Fatalf("missing Claude hooks were not unhealthy: %+v", checks)
		}
		if err := app.setupHookFiles(); err != nil {
			t.Fatal(err)
		}
		checks = app.clientDoctorChecks(testContext(t))
		if len(checks) != 1 || !checks[0].ok {
			t.Fatalf("configured Claude hooks were not healthy: %+v", checks)
		}
	})

	t.Run("Codex strict", func(t *testing.T) {
		app := testApp(t)
		t.Setenv("PATH", t.TempDir())
		installFakeCodex(t, app, "success")
		if err := app.setupClientHooks(testContext(t)); err != nil {
			t.Fatal(err)
		}
		checks := app.clientDoctorChecks(testContext(t))
		if len(checks) != 1 || checks[0].name != "Codex hooks" || !checks[0].ok {
			t.Fatalf("trusted Codex hooks were not healthy: %+v", checks)
		}
	})
}

func TestHooksConfiguredRequiresOneExactCompleteSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	binary := "/Users/test/.local/bin/hgctl"
	if err := configureHookFile(path, binary, "codex", true); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hooksConfigured(path, binary, "codex") {
		t.Fatal("freshly configured hook set was not recognized")
	}

	mutations := map[string]func(map[string]any){
		"partial": func(root map[string]any) {
			delete(root["hooks"].(map[string]any), "Stop")
		},
		"duplicate": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			groups := hooks["Stop"].([]any)
			hooks["Stop"] = append(groups, groups[0])
		},
		"wrong timeout": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			group := hooks["Stop"].([]any)[0].(map[string]any)
			group["hooks"].([]any)[0].(map[string]any)["timeout"] = float64(30)
		},
		"wrong matcher": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			hooks["Stop"].([]any)[0].(map[string]any)["matcher"] = "unexpected"
		},
		"wrong handler type": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			group := hooks["Stop"].([]any)[0].(map[string]any)
			group["hooks"].([]any)[0].(map[string]any)["type"] = "prompt"
		},
		"wrong event": func(root map[string]any) {
			hooks := root["hooks"].(map[string]any)
			hooks["Other"] = hooks["Stop"]
			delete(hooks, "Stop")
		},
		"unrelated non-object group": func(root map[string]any) {
			root["hooks"].(map[string]any)["Other"] = []any{"invalid"}
		},
		"unrelated group missing hooks": func(root map[string]any) {
			root["hooks"].(map[string]any)["Other"] = []any{map[string]any{"matcher": "value"}}
		},
		"unrelated non-object handler": func(root map[string]any) {
			root["hooks"].(map[string]any)["Other"] = []any{map[string]any{"hooks": []any{"invalid"}}}
		},
		"unrelated handler missing type": func(root map[string]any) {
			root["hooks"].(map[string]any)["Other"] = []any{map[string]any{"hooks": []any{map[string]any{"command": "other-tool"}}}}
		},
		"unrelated null event": func(root map[string]any) {
			root["hooks"].(map[string]any)["Other"] = nil
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal(base, &root); err != nil {
				t.Fatal(err)
			}
			mutate(root)
			if err := writeJSONAtomic(path, root, 0o600); err != nil {
				t.Fatal(err)
			}
			if hooksConfigured(path, binary, "codex") {
				t.Fatal("invalid hook set was accepted")
			}
		})
	}

	var root map[string]any
	if err := json.Unmarshal(base, &root); err != nil {
		t.Fatal(err)
	}
	hooks := root["hooks"].(map[string]any)
	hooks["Stop"] = append(hooks["Stop"].([]any), map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "other-tool stop", "timeout": float64(1)}},
	})
	if err := writeJSONAtomic(path, root, 0o600); err != nil {
		t.Fatal(err)
	}
	if !hooksConfigured(path, binary, "codex") {
		t.Fatal("unrelated hook made the exact managed set unhealthy")
	}
}

func TestHookConfigPreservesDotfileSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "dotfiles", "claude-settings.json")
	link := filepath.Join(root, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{\"theme\":\"dark\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := configureHookFile(link, "/tmp/hgctl", "claude", true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced: mode=%v", info.Mode())
	}
	present, err := managedHooksPresent(link, "/tmp/hgctl", "claude")
	if err != nil || !present {
		t.Fatalf("managed hooks missing through symlink: present=%v err=%v", present, err)
	}
	if err := configureHookFile(link, "/tmp/hgctl", "claude", false); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config symlink was replaced during uninstall: mode=%v", info.Mode())
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
	if err := configureHookFile(filepath.Join(app.Paths.Home, ".claude", "settings.json"), stable, "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := configureHookFile(filepath.Join(app.Paths.Home, ".codex", "hooks.json"), stable, "codex", true); err != nil {
		t.Fatal(err)
	}
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
		if err := configureHookFile(filepath.Join(app.Paths.Home, ".claude", "settings.json"), stable, "claude", true); err != nil {
			t.Fatal(err)
		}
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
	if err := os.MkdirAll(app.Paths.Vault, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "", "init", "-b", "shared", app.Paths.Vault)
	runGitTest(t, app.Paths.Vault, "config", "user.name", "test")
	runGitTest(t, app.Paths.Vault, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(app.Paths.Vault, "Home.md"), []byte("# Hourglass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, app.Paths.Vault, "add", "Home.md")
	runGitTest(t, app.Paths.Vault, "commit", "-m", "shared")
	bin := filepath.Join(app.Paths.Home, "fake-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "basic-memory")
	logPath := filepath.Join(app.Paths.Home, "reindex.log")
	script := `#!/bin/sh
[ "$BASIC_MEMORY_ENSURE_FRONTMATTER_ON_SYNC" = "false" ] || exit 11
[ "$BASIC_MEMORY_DISABLE_PERMALINKS" = "true" ] || exit 12
[ "$BASIC_MEMORY_SEMANTIC_SEARCH_ENABLED" = "false" ] || exit 13
[ "$BASIC_MEMORY_DEFAULT_SEARCH_TYPE" = "text" ] || exit 14
[ "$1 $2" != "tool list-projects" ] || {
  printf '{"projects":[{"name":"hourglass","external_id":"%s","local_path":"%s"}]}\n' "$HGCTL_TEST_PROJECT_ID" "$HGCTL_TEST_PROJECT_PATH"
  exit 0
}
[ "$1" = "reindex" ] && [ "$2" = "--search" ] && [ "$3" = "--project" ] && [ "$4" = "hourglass" ] || exit 15
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
	head := strings.TrimSpace(runGitTest(t, app.Paths.Vault, "rev-parse", "HEAD"))
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

func TestInstallContinuesImportAndSyncWhenCodexTrustIsDeferred(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("scheduler contract is only defined for macOS and Linux")
	}
	fixture := newGitFixture(t)
	app := testApp(t)
	installFakeCodex(t, app, "rpc-error")
	bin := filepath.Join(app.Paths.Home, "fake-codex-bin")
	writeFakeExecutable(t, bin, "claude")
	basicMemory := `#!/bin/sh
case "$1 $2" in
  "tool list-projects")
    if [ -f "$BM_STATE" ]; then
      printf '{"projects":[{"name":"hourglass","external_id":"project-id","local_path":"%s"}]}\n' "$BM_PATH"
    else
      printf '{"projects":[]}\n'
    fi
    exit 0 ;;
  "project add") : > "$BM_STATE"; exit 0 ;;
  "reindex --search") exit 0 ;;
esac
exit 20
`
	if err := os.WriteFile(filepath.Join(bin, "basic-memory"), []byte(basicMemory), 0o700); err != nil {
		t.Fatal(err)
	}
	switch runtime.GOOS {
	case "darwin":
		launchctl := "#!/bin/sh\ncase \"$1\" in bootout|bootstrap) exit 0 ;; esac\nexit 20\n"
		if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(launchctl), 0o700); err != nil {
			t.Fatal(err)
		}
	case "linux":
		loginctl := "#!/bin/sh\nif [ \"$1\" = \"show-user\" ]; then echo yes; exit 0; fi\nexit 20\n"
		if err := os.WriteFile(filepath.Join(bin, "loginctl"), []byte(loginctl), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("BM_STATE", filepath.Join(app.Paths.Home, "basic-memory-created"))
	t.Setenv("BM_PATH", app.Paths.Vault)

	legacy := filepath.Join(app.Paths.Home, "legacy")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "why.md"), []byte("The reason survives.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := app.runInstall(testContext(t), []string{"--repo", fixture.remote, "--import", legacy})
	if err == nil || !strings.Contains(err.Error(), "Codex hook trust") || strings.Contains(err.Error(), "bundle timeout") {
		t.Fatalf("install did not fail safely on Codex trust: %v", err)
	}
	if strings.Contains(app.Out.(*bytes.Buffer).String(), "Hourglass initialized") {
		t.Fatal("install printed success after deferred Codex trust")
	}
	if present, err := app.schedulerFilesPresent(); err != nil || !present {
		t.Fatalf("scheduler was not installed before deferred integration: present=%v err=%v", present, err)
	}
	stable := filepath.Join(app.Paths.Bin, "hgctl")
	if !hooksConfigured(filepath.Join(app.Paths.Home, ".claude", "settings.json"), stable, "claude") {
		t.Fatal("Claude hooks were not installed")
	}
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	queueRef := "refs/heads/queue/" + identity.ID
	tree := runGitTest(t, "", "--git-dir", fixture.remote, "ls-tree", "-r", "--name-only", queueRef)
	if !strings.Contains(tree, "events/") {
		t.Fatalf("explicit import did not reach the queue after trust failure:\n%s", tree)
	}
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil || len(entries) != 0 {
		t.Fatalf("initial sync did not acknowledge import: entries=%d err=%v", len(entries), err)
	}
}

func writeFakeExecutable(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func countJSONText(content []byte, needle string) int {
	return bytes.Count(content, []byte(needle))
}
