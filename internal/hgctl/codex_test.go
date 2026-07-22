package hgctl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexHookTrustUsesOfficialAppServer(t *testing.T) {
	app := testApp(t)
	trustFile, writeLog := installFakeCodex(t, app, "success")
	if err := app.trustCodexHooks(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trustFile); err != nil {
		t.Fatalf("fake app-server did not persist trust: %v", err)
	}
	log, err := os.ReadFile(writeLog)
	if err != nil || string(log) != "write\n" {
		t.Fatalf("trust write log=%q err=%v", log, err)
	}
	if err := app.trustCodexHooks(testContext(t)); err != nil {
		t.Fatal(err)
	}
	log, err = os.ReadFile(writeLog)
	if err != nil || string(log) != "write\n" {
		t.Fatalf("already trusted hooks were rewritten: log=%q err=%v", log, err)
	}
	if err := app.verifyCodexHooks(testContext(t)); err != nil {
		t.Fatal(err)
	}
	log, err = os.ReadFile(writeLog)
	if err != nil || string(log) != "write\n" {
		t.Fatalf("doctor performed a config write: log=%q err=%v", log, err)
	}
}

func TestCodexHookTrustFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "rpc-error", "final-untrusted", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			app := testApp(t)
			installFakeCodex(t, app, mode)
			if err := app.trustCodexHooks(testContext(t)); err == nil {
				t.Fatal("Codex trust unexpectedly succeeded")
			}
		})
	}
}

func TestCodexHookTrustToleratesUnrelatedWarningsAndFields(t *testing.T) {
	for _, mode := range []string{"warning", "external-fields"} {
		t.Run(mode, func(t *testing.T) {
			app := testApp(t)
			installFakeCodex(t, app, mode)
			if err := app.trustCodexHooks(testContext(t)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexHookTrustToleratesKnownCloudConfigTimeout(t *testing.T) {
	app := testApp(t)
	installFakeCodex(t, app, "cloud-config-timeout")
	if err := app.trustCodexHooks(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHookTrustRejectsOtherDiscoveryErrors(t *testing.T) {
	for _, mode := range []string{
		"hook-discovery-error",
		"cloud-config-timeout-hook-path",
		"cloud-config-timeout-drift",
		"mixed-discovery-errors",
	} {
		t.Run(mode, func(t *testing.T) {
			app := testApp(t)
			installFakeCodex(t, app, mode)
			if err := app.trustCodexHooks(testContext(t)); err == nil {
				t.Fatal("Codex trust unexpectedly ignored a discovery error")
			}
		})
	}
}

func TestCodexTrustRetryIsLowFrequencyAndNonBlocking(t *testing.T) {
	app := testApp(t)
	trustFile, writeLog := installFakeCodex(t, app, "rpc-error")
	stable := filepath.Join(app.Paths.Bin, "hgctl")
	if err := configureHookFile(filepath.Join(app.Paths.Home, ".codex", "hooks.json"), stable, "codex", true); err != nil {
		t.Fatal(err)
	}
	if err := app.saveState(State{RepoURL: DefaultRepoURL, QueueBranch: "queue/00000000-0000-4000-8000-000000000000"}); err != nil {
		t.Fatal(err)
	}
	now := app.Now()
	app.Now = func() time.Time { return now }
	app.retryCodexTrust(testContext(t))
	var check codexTrustCheck
	err := readJSON(app.Paths.CodexCheck, &check)
	if err != nil || !check.CheckedAt.Equal(now) || len(check.HooksSHA256) != 64 {
		t.Fatalf("trust retry timestamp=%v err=%v", check.CheckedAt, err)
	}
	output := app.Err.(*bytes.Buffer).String()
	if !strings.Contains(output, "Codex hook trust deferred") || strings.Contains(output, "bundle timeout") {
		t.Fatalf("unsafe deferred diagnostic: %q", output)
	}

	t.Setenv("HGCTL_FAKE_CODEX_MODE", "success")
	app.retryCodexTrust(testContext(t))
	if _, err := os.Stat(trustFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trust retried before interval: %v", err)
	}
	app.Now = func() time.Time { return now.Add(codexTrustInterval + time.Second) }
	app.retryCodexTrust(testContext(t))
	if _, err := os.Stat(trustFile); err != nil {
		t.Fatalf("due trust retry did not recover: %v", err)
	}
	log, err := os.ReadFile(writeLog)
	if err != nil || string(log) != "write\n" {
		t.Fatalf("recovery writes=%q err=%v", log, err)
	}
}

func TestCodexTrustRetryDoesNotThrottleAChangedHookFile(t *testing.T) {
	app := testApp(t)
	trustFile, _ := installFakeCodex(t, app, "rpc-error")
	stable := filepath.Join(app.Paths.Bin, "hgctl")
	hooksPath := filepath.Join(app.Paths.Home, ".codex", "hooks.json")
	if err := configureHookFile(hooksPath, stable, "codex", true); err != nil {
		t.Fatal(err)
	}
	now := app.Now()
	app.Now = func() time.Time { return now }
	app.retryCodexTrust(testContext(t))
	if _, err := os.Stat(trustFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failing trust attempt unexpectedly succeeded: %v", err)
	}
	content, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HGCTL_FAKE_CODEX_MODE", "success")
	app.retryCodexTrust(testContext(t))
	if _, err := os.Stat(trustFile); err != nil {
		t.Fatalf("changed hook file remained throttled by the old receipt: %v", err)
	}
}

func TestCodexAppServerHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_APP_SERVER_HELPER") != "1" {
		return
	}
	if err := runCodexAppServerHelper(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(20)
	}
	os.Exit(0)
}

func installFakeCodex(t *testing.T, app *App, mode string) (string, string) {
	t.Helper()
	bin := filepath.Join(app.Paths.Home, "fake-codex-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexec \"$HGCTL_TEST_BINARY\" -test.run '^TestCodexAppServerHelper$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	trustFile := filepath.Join(app.Paths.Home, "codex-trusted")
	writeLog := filepath.Join(app.Paths.Home, "codex-writes")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GO_WANT_CODEX_APP_SERVER_HELPER", "1")
	t.Setenv("HGCTL_TEST_BINARY", executable)
	t.Setenv("HGCTL_FAKE_CODEX_HOME", app.Paths.Home)
	t.Setenv("HGCTL_FAKE_CODEX_STABLE", filepath.Join(app.Paths.Bin, "hgctl"))
	t.Setenv("HGCTL_FAKE_CODEX_MODE", mode)
	t.Setenv("HGCTL_FAKE_CODEX_TRUST", trustFile)
	t.Setenv("HGCTL_FAKE_CODEX_WRITES", writeLog)
	return trustFile, writeLog
}

func runCodexAppServerHelper() error {
	mode := os.Getenv("HGCTL_FAKE_CODEX_MODE")
	if mode == "oversized" {
		_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("x", codexRPCLineLimit+1))
		return nil
	}
	home := os.Getenv("HGCTL_FAKE_CODEX_HOME")
	stable := os.Getenv("HGCTL_FAKE_CODEX_STABLE")
	trustFile := os.Getenv("HGCTL_FAKE_CODEX_TRUST")
	trusted := fileExists(trustFile)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), codexRPCLineLimit)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return err
		}
		switch request.Method {
		case "initialize":
			if mode == "rpc-error" {
				writeFakeCodexMessage(map[string]any{"id": 1, "error": map[string]any{"code": -32000, "message": "bundle timeout"}})
				continue
			}
			writeFakeCodexMessage(map[string]any{"id": 1, "result": map[string]any{"serverInfo": map[string]string{"name": "fake"}}})
		case "initialized":
			continue
		case "hooks/list":
			var id int
			if err := json.Unmarshal(request.ID, &id); err != nil {
				return err
			}
			hooks := fakeCodexHooks(stable, filepath.Join(home, ".codex", "hooks.json"), trusted)
			if mode == "missing" {
				hooks = hooks[:1]
			}
			if mode == "duplicate" {
				hooks = append(hooks, hooks[0])
			}
			warnings := []string{}
			if mode == "warning" {
				warnings = append(warnings, "ambiguous hook source")
			}
			errors := []any{}
			switch mode {
			case "cloud-config-timeout":
				errors = append(errors, map[string]any{"path": home, "message": codexCloudConfigTimeout})
			case "hook-discovery-error":
				errors = append(errors, map[string]any{"path": filepath.Join(home, ".codex", "hooks.json"), "message": "invalid hook definition"})
			case "cloud-config-timeout-hook-path":
				errors = append(errors, map[string]any{"path": filepath.Join(home, ".codex", "hooks.json"), "message": codexCloudConfigTimeout})
			case "cloud-config-timeout-drift":
				errors = append(errors, map[string]any{"path": home, "message": "timed out waiting for cloud config bundle after 16s"})
			case "mixed-discovery-errors":
				errors = append(errors,
					map[string]any{"path": home, "message": codexCloudConfigTimeout},
					map[string]any{"path": filepath.Join(home, ".codex", "hooks.json"), "message": "invalid hook definition"},
				)
			}
			writeFakeCodexMessage(map[string]any{
				"id": id,
				"result": map[string]any{"data": []any{map[string]any{
					"cwd": home, "errors": errors, "hooks": hooks, "warnings": warnings,
				}}},
			})
		case "config/batchWrite":
			if err := validateFakeCodexTrustWrite(request.Params); err != nil {
				return err
			}
			log, err := os.OpenFile(os.Getenv("HGCTL_FAKE_CODEX_WRITES"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return err
			}
			if _, err := log.WriteString("write\n"); err != nil {
				_ = log.Close()
				return err
			}
			if err := log.Close(); err != nil {
				return err
			}
			if mode != "final-untrusted" {
				if err := os.WriteFile(trustFile, []byte("trusted\n"), 0o600); err != nil {
					return err
				}
				trusted = true
			}
			result := map[string]any{
				"filePath":           filepath.Join(home, ".codex", "config.toml"),
				"overriddenMetadata": nil, "status": "ok", "version": "fake-v1",
			}
			if mode == "external-fields" {
				result["future"] = true
			}
			writeFakeCodexMessage(map[string]any{"id": 3, "result": result})
		default:
			return fmt.Errorf("unexpected method %q", request.Method)
		}
	}
	return scanner.Err()
}

func fakeCodexHooks(stable, sourcePath string, trusted bool) []any {
	trustStatus := "untrusted"
	if trusted {
		trustStatus = "trusted"
	}
	hooks := make([]any, 0, 3)
	for index, spec := range codexHookSpecs(stable) {
		var matcher any
		if spec.hasMatcher {
			matcher = spec.matcher
		}
		hook := map[string]any{
			"command": spec.command, "currentHash": "sha256:" + strings.Repeat(string(rune('a'+index)), 64),
			"displayOrder": index, "enabled": trusted, "eventName": spec.event,
			"handlerType": "command", "isManaged": false, "key": fmt.Sprintf("hgctl-%d", index),
			"matcher": matcher, "pluginId": nil, "source": "user", "sourcePath": sourcePath,
			"statusMessage": nil, "timeoutSec": spec.timeout, "trustStatus": trustStatus,
		}
		if os.Getenv("HGCTL_FAKE_CODEX_MODE") == "external-fields" {
			hook["future"] = true
		}
		hooks = append(hooks, hook)
	}
	return hooks
}

func validateFakeCodexTrustWrite(content json.RawMessage) error {
	var params struct {
		Edits []struct {
			KeyPath       string `json:"keyPath"`
			MergeStrategy string `json:"mergeStrategy"`
			Value         map[string]struct {
				Enabled     bool   `json:"enabled"`
				TrustedHash string `json:"trusted_hash"`
			} `json:"value"`
		} `json:"edits"`
		ReloadUserConfig bool `json:"reloadUserConfig"`
	}
	if err := json.Unmarshal(content, &params); err != nil {
		return err
	}
	if len(params.Edits) != 1 || params.Edits[0].KeyPath != "hooks.state" || params.Edits[0].MergeStrategy != "upsert" || !params.ReloadUserConfig || len(params.Edits[0].Value) != 2 {
		return errors.New("unexpected config/batchWrite shape")
	}
	for key, state := range params.Edits[0].Value {
		if !state.Enabled || !strings.HasPrefix(key, "hgctl-") {
			return errors.New("hook state is not enabled or has an unexpected key")
		}
		hash := state.TrustedHash
		if len(hash) == 71 && hash[:7] == "sha256:" {
			hash = hash[7:]
		}
		if !validLowerHex(hash, 64) {
			return errors.New("hook state has an invalid trusted hash")
		}
	}
	return nil
}

func writeFakeCodexMessage(message any) {
	if os.Getenv("HGCTL_FAKE_CODEX_MODE") == "external-fields" {
		if object, ok := message.(map[string]any); ok {
			object["future"] = true
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(message)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
