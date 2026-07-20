package hgctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
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
	CheckedAt time.Time `json:"checked_at"`
}

func (a *App) trustCodexHooks(ctx context.Context) error {
	return a.checkCodexHooks(ctx, true)
}

func (a *App) attemptCodexTrust(ctx context.Context) error {
	return withFileLockWait(ctx, a.Paths.CodexLock, func() error {
		if err := writeJSONAtomic(a.Paths.CodexCheck, codexTrustCheck{CheckedAt: a.Now().UTC()}, 0o600); err != nil {
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
		var previous codexTrustCheck
		if err := readJSON(a.Paths.CodexCheck, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !previous.CheckedAt.IsZero() && now.Sub(previous.CheckedAt) < codexTrustInterval {
			return nil
		}
		if err := writeJSONAtomic(a.Paths.CodexCheck, codexTrustCheck{CheckedAt: now}, 0o600); err != nil {
			return err
		}
		return a.trustCodexHooks(ctx)
	})
	if err != nil {
		_, _ = fmt.Fprintln(a.Err, "hgctl: Codex hook trust deferred")
	}
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

type codexRPCLine struct {
	content []byte
	err     error
}

type codexRPC struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stdin    io.WriteCloser
	lines    chan codexRPCLine
	wait     chan error
	process  *os.Process
	closeOne sync.Once
}

func startCodexRPC(parent context.Context, codexHome string) (*codexRPC, error) {
	ctx, cancel := context.WithTimeout(parent, codexRPCSessionLimit)
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--stdio")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	rpc := &codexRPC{
		ctx: ctx, cancel: cancel, stdin: stdin, lines: make(chan codexRPCLine, 8),
		wait: make(chan error, 1), process: cmd.Process,
	}
	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), codexRPCLineLimit+2)
		for scanner.Scan() {
			if len(scanner.Bytes()) > codexRPCLineLimit {
				select {
				case rpc.lines <- codexRPCLine{err: errors.New("Codex app-server line exceeds the limit")}:
				case <-ctx.Done():
				}
				return
			}
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case rpc.lines <- codexRPCLine{content: line}:
			case <-ctx.Done():
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case rpc.lines <- codexRPCLine{err: err}:
		case <-ctx.Done():
		}
	}()
	go func() { rpc.wait <- cmd.Wait() }()
	return rpc, nil
}

func (rpc *codexRPC) Notify(message any) error {
	return rpc.send(message)
}

func (rpc *codexRPC) Call(id int, message any) (json.RawMessage, error) {
	if err := rpc.send(message); err != nil {
		return nil, err
	}
	timer := time.NewTimer(codexRPCResponseWait)
	defer timer.Stop()
	for {
		select {
		case line := <-rpc.lines:
			if line.err != nil {
				return nil, fmt.Errorf("Codex app-server stdout: %w", line.err)
			}
			result, matched, err := decodeCodexRPCResponse(line.content, id)
			if err != nil {
				return nil, err
			}
			if matched {
				return result, nil
			}
		case <-timer.C:
			return nil, fmt.Errorf("Codex app-server response %d timed out", id)
		case <-rpc.ctx.Done():
			return nil, fmt.Errorf("Codex app-server stopped: %w", rpc.ctx.Err())
		}
	}
}

func (rpc *codexRPC) send(message any) error {
	content, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(content) > codexRPCLineLimit {
		return errors.New("Codex app-server request exceeds the line limit")
	}
	content = append(content, '\n')
	_, err = rpc.stdin.Write(content)
	return err
}

func decodeCodexRPCResponse(content []byte, wantID int) (json.RawMessage, bool, error) {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := decodeExternalJSON(content, &message); err != nil {
		return nil, false, fmt.Errorf("malformed Codex app-server message: %w", err)
	}
	if len(message.ID) == 0 {
		if message.Method == "" {
			return nil, false, errors.New("Codex app-server emitted a message without id or method")
		}
		return nil, false, nil
	}
	var gotID int
	if err := json.Unmarshal(message.ID, &gotID); err != nil || gotID != wantID {
		return nil, false, errors.New("Codex app-server returned an unexpected response id")
	}
	if len(message.Error) != 0 && !bytes.Equal(message.Error, []byte("null")) {
		return nil, false, errors.New("Codex app-server returned an error")
	}
	if len(message.Result) == 0 || bytes.Equal(message.Result, []byte("null")) {
		return nil, false, errors.New("Codex app-server response has no result")
	}
	return message.Result, true, nil
}

func decodeExternalJSON(content []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (rpc *codexRPC) Close() {
	rpc.closeOne.Do(func() {
		_ = rpc.stdin.Close()
		rpc.cancel()
		select {
		case <-rpc.wait:
		case <-time.After(5 * time.Second):
			if rpc.process != nil {
				_ = rpc.process.Kill()
			}
			select {
			case <-rpc.wait:
			case <-time.After(5 * time.Second):
			}
		}
	})
}
