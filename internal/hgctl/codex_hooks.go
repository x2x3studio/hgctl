package hgctl

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	codexRPCLineLimit    = 1 << 20
	codexRPCResponseWait = 30 * time.Second
	codexRPCSessionLimit = 90 * time.Second
	codexTrustInterval   = 6 * time.Hour
)

type codexHookMetadata struct {
	Command       *string `json:"command"`
	CurrentHash   string  `json:"currentHash"`
	DisplayOrder  int64   `json:"displayOrder"`
	Enabled       bool    `json:"enabled"`
	EventName     string  `json:"eventName"`
	HandlerType   string  `json:"handlerType"`
	IsManaged     bool    `json:"isManaged"`
	Key           string  `json:"key"`
	Matcher       *string `json:"matcher"`
	PluginID      *string `json:"pluginId"`
	Source        string  `json:"source"`
	SourcePath    string  `json:"sourcePath"`
	StatusMessage *string `json:"statusMessage"`
	TimeoutSec    uint64  `json:"timeoutSec"`
	TrustStatus   string  `json:"trustStatus"`
}

type codexHooksListResponse struct {
	Data []struct {
		CWD      string              `json:"cwd"`
		Errors   []codexHookError    `json:"errors"`
		Hooks    []codexHookMetadata `json:"hooks"`
		Warnings []string            `json:"warnings"`
	} `json:"data"`
}

type codexHookError struct {
	Message string `json:"message"`
	Path    string `json:"path"`
}

const codexCloudConfigTimeout = "timed out waiting for cloud config bundle after 15s"

type codexHookSpec struct {
	event      string
	command    string
	matcher    string
	hasMatcher bool
	timeout    uint64
}

type codexTrustCheck struct {
	CheckedAt   time.Time `json:"checked_at"`
	HooksSHA256 string    `json:"hooks_sha256,omitempty"`
}

func (a *App) trustCodexHooks(ctx context.Context) error {
	return a.checkCodexHooks(ctx, true)
}

func (a *App) attemptCodexTrust(ctx context.Context) error {
	return withFileLockWait(ctx, a.Paths.CodexLock, func() error {
		digest, err := codexHooksDigest(filepath.Join(a.Paths.Home, ".codex", "hooks.json"))
		if err != nil {
			return err
		}
		if err := writeJSONAtomic(a.Paths.CodexCheck, codexTrustCheck{CheckedAt: a.Now().UTC(), HooksSHA256: digest}, 0o600); err != nil {
			return err
		}
		return a.trustCodexHooks(ctx)
	})
}

func (a *App) retryCodexTrust(ctx context.Context) {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	hooksPath := filepath.Join(a.Paths.Home, ".codex", "hooks.json")
	if !hooksConfigured(hooksPath, stable, "codex") {
		return
	}
	err := withFileLock(a.Paths.CodexLock, func() error {
		now := a.Now().UTC()
		digest, err := codexHooksDigest(hooksPath)
		if err != nil {
			return err
		}
		var previous codexTrustCheck
		if err := readJSON(a.Paths.CodexCheck, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if previous.HooksSHA256 == digest && !previous.CheckedAt.IsZero() && now.Sub(previous.CheckedAt) < codexTrustInterval {
			return nil
		}
		if err := writeJSONAtomic(a.Paths.CodexCheck, codexTrustCheck{CheckedAt: now, HooksSHA256: digest}, 0o600); err != nil {
			return err
		}
		return a.trustCodexHooks(ctx)
	})
	if err != nil {
		_, _ = fmt.Fprintln(a.Err, "hgctl: Codex hook trust deferred")
	}
}

func codexHooksDigest(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return fmt.Sprintf("%x", digest), nil
}

func (a *App) verifyCodexHooks(ctx context.Context) error {
	return a.checkCodexHooks(ctx, false)
}

func (a *App) checkCodexHooks(ctx context.Context, writeTrust bool) error {
	if !commandExists("codex") {
		return errors.New("codex is not installed")
	}
	rpc, err := startCodexRPC(ctx, filepath.Join(a.Paths.Home, ".codex"))
	if err != nil {
		return err
	}
	defer rpc.Close()

	initialize := map[string]any{
		"method": "initialize",
		"id":     1,
		"params": map[string]any{
			"clientInfo":   map[string]string{"name": "hgctl", "title": "hgctl", "version": Version},
			"capabilities": map[string]bool{"experimentalApi": true},
		},
	}
	result, err := rpc.Call(1, initialize)
	if err != nil {
		return fmt.Errorf("Codex initialize: %w", err)
	}
	var initialized map[string]any
	if err := json.Unmarshal(result, &initialized); err != nil || initialized == nil {
		return errors.New("Codex initialize returned a malformed result")
	}
	if err := rpc.Notify(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}

	cwd, err := filepath.Abs(a.Paths.Home)
	if err != nil {
		return err
	}
	hooksPath, err := filepath.Abs(filepath.Join(a.Paths.Home, ".codex", "hooks.json"))
	if err != nil {
		return err
	}
	selected, err := rpc.listHGCTLHooks(2, cwd, hooksPath, codexHookSpecs(filepath.Join(a.Paths.Bin, "hgctl")))
	if err != nil {
		return err
	}
	trustErr := requireCodexHooksTrusted(selected)
	if trustErr == nil || !writeTrust {
		return trustErr
	}

	states := make(map[string]any, len(selected))
	for _, hook := range selected {
		states[hook.Key] = map[string]any{"enabled": true, "trusted_hash": hook.CurrentHash}
	}
	writeResult, err := rpc.Call(3, map[string]any{
		"method": "config/batchWrite",
		"id":     3,
		"params": map[string]any{
			"edits": []any{map[string]any{
				"keyPath": "hooks.state", "mergeStrategy": "upsert", "value": states,
			}},
			"reloadUserConfig": true,
		},
	})
	if err != nil {
		return fmt.Errorf("Codex hook trust write: %w", err)
	}
	var writeResponse struct {
		FilePath           string          `json:"filePath"`
		OverriddenMetadata json.RawMessage `json:"overriddenMetadata"`
		Status             string          `json:"status"`
		Version            string          `json:"version"`
	}
	if err := decodeExternalJSON(writeResult, &writeResponse); err != nil || writeResponse.FilePath == "" || writeResponse.Version == "" || (writeResponse.Status != "ok" && writeResponse.Status != "okOverridden") {
		return errors.New("Codex hook trust write returned a malformed result")
	}
	expectedConfig := filepath.Join(a.Paths.Home, ".codex", "config.toml")
	if canonicalPath(writeResponse.FilePath) != canonicalPath(expectedConfig) {
		return errors.New("Codex wrote hook trust to an unexpected config")
	}

	selected, err = rpc.listHGCTLHooks(4, cwd, hooksPath, codexHookSpecs(filepath.Join(a.Paths.Bin, "hgctl")))
	if err != nil {
		return err
	}
	return requireCodexHooksTrusted(selected)
}

func codexHookSpecs(binary string) []codexHookSpec {
	prefix := shellQuote(binary) + " hook --client codex --event "
	return []codexHookSpec{
		{event: "sessionStart", command: prefix + "session-start", matcher: "startup|resume|clear|compact", hasMatcher: true, timeout: 10},
		{event: "userPromptSubmit", command: prefix + "user-prompt", timeout: 3},
		{event: "stop", command: prefix + "stop", timeout: 5},
	}
}

func (rpc *codexRPC) listHGCTLHooks(id int, cwd, hooksPath string, specs []codexHookSpec) ([]codexHookMetadata, error) {
	result, err := rpc.Call(id, map[string]any{
		"method": "hooks/list", "id": id, "params": map[string]any{"cwds": []string{cwd}},
	})
	if err != nil {
		return nil, fmt.Errorf("Codex hooks/list: %w", err)
	}
	var response codexHooksListResponse
	if err := decodeExternalJSON(result, &response); err != nil {
		return nil, fmt.Errorf("decode Codex hooks/list: %w", err)
	}
	if len(response.Data) != 1 || filepath.Clean(response.Data[0].CWD) != filepath.Clean(cwd) {
		return nil, errors.New("Codex hooks/list did not return the requested cwd exactly once")
	}
	entry := response.Data[0]
	for _, hookErr := range entry.Errors {
		if !benignCodexCloudConfigTimeout(hookErr, cwd, hooksPath) {
			return nil, errors.New("Codex reported hook discovery errors")
		}
	}

	selected := make([]codexHookMetadata, 0, len(specs))
	seenSpecs := make(map[int]bool, len(specs))
	seenKeys := make(map[string]bool, len(specs))
	for _, hook := range entry.Hooks {
		if hook.Source != "user" || hook.IsManaged || hook.HandlerType != "command" || hook.PluginID != nil || filepath.Clean(hook.SourcePath) != filepath.Clean(hooksPath) {
			continue
		}
		for index, spec := range specs {
			if !codexHookMatches(hook, spec) {
				continue
			}
			if seenSpecs[index] || hook.Key == "" || seenKeys[hook.Key] {
				return nil, errors.New("Codex returned duplicate hgctl hooks")
			}
			if _, ok := eventDigest(hook.CurrentHash); !ok {
				return nil, errors.New("Codex returned an invalid hook hash")
			}
			seenSpecs[index] = true
			seenKeys[hook.Key] = true
			selected = append(selected, hook)
		}
	}
	if len(selected) != len(specs) {
		return nil, fmt.Errorf("Codex discovered %d of %d hgctl hooks", len(selected), len(specs))
	}
	return selected, nil
}

func benignCodexCloudConfigTimeout(hookErr codexHookError, cwd, hooksPath string) bool {
	return hookErr.Message == codexCloudConfigTimeout &&
		filepath.Clean(hookErr.Path) == filepath.Clean(cwd) &&
		filepath.Clean(hookErr.Path) != filepath.Clean(hooksPath)
}

func codexHookMatches(hook codexHookMetadata, spec codexHookSpec) bool {
	if hook.Command == nil || *hook.Command != spec.command || hook.EventName != spec.event || hook.TimeoutSec != spec.timeout {
		return false
	}
	if spec.hasMatcher {
		return hook.Matcher != nil && *hook.Matcher == spec.matcher
	}
	return hook.Matcher == nil
}

func requireCodexHooksTrusted(hooks []codexHookMetadata) error {
	if len(hooks) != 3 {
		return errors.New("Codex hgctl hook set is incomplete")
	}
	for _, hook := range hooks {
		if !hook.Enabled || hook.TrustStatus != "trusted" {
			return errors.New("a Codex hgctl hook is not enabled and trusted")
		}
	}
	return nil
}
