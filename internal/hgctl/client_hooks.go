package hgctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type clientAdapter struct {
	name       string
	executable string
	path       string
	client     string
}

func (a *App) clientAdapters() []clientAdapter {
	return []clientAdapter{
		{name: "Claude", executable: "claude", path: filepath.Join(a.Paths.Home, ".claude", "settings.json"), client: "claude"},
		{name: "Codex", executable: "codex", path: filepath.Join(a.Paths.Home, ".codex", "hooks.json"), client: "codex"},
	}
}

func (a *App) setupHookFiles() error {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	var errs []error
	for _, item := range a.clientAdapters() {
		if !commandExists(item.executable) {
			continue
		}
		if err := configureHookFile(item.path, stable, item.client, true); err != nil {
			errs = append(errs, fmt.Errorf("%s hooks: %w", item.name, err))
		} else if !hooksConfigured(item.path, stable, item.client) {
			errs = append(errs, fmt.Errorf("%s hooks: installed hook set is incomplete or malformed", item.name))
		}
	}
	return errors.Join(errs...)
}

func (a *App) setupClientHooks(ctx context.Context) error {
	var errs []error
	if err := a.setupHookFiles(); err != nil {
		errs = append(errs, err)
	}
	if commandExists("codex") {
		codexHooks := filepath.Join(a.Paths.Home, ".codex", "hooks.json")
		stable := filepath.Join(a.Paths.Bin, "hgctl")
		if hooksConfigured(codexHooks, stable, "codex") {
			if err := a.attemptCodexTrust(ctx); err != nil {
				errs = append(errs, fmt.Errorf("Codex hook trust: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

const hookConfigWriteAttempts = 4

type hookConfigSnapshot struct {
	content []byte
	exists  bool
}

func configureHookFile(path, binary, client string, install bool) error {
	writePath, err := configFilePath(path)
	if err != nil {
		return err
	}
	return configureHookFileWithRetry(writePath, path, binary, client, install, nil)
}

func configureHookFileWithRetry(writePath, displayPath, binary, client string, install bool, beforeVerify func(int)) error {
	for attempt := 0; attempt < hookConfigWriteAttempts; attempt++ {
		snapshot, err := readHookConfigSnapshot(writePath)
		if err != nil {
			return err
		}
		if !install && !snapshot.exists {
			return os.ErrNotExist
		}
		desired, write, err := mergeHookConfig(snapshot.content, snapshot.exists, displayPath, binary, client, install)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		if beforeVerify != nil {
			beforeVerify(attempt)
		}
		current, err := readHookConfigSnapshot(writePath)
		if err != nil {
			return err
		}
		if !sameHookConfigSnapshot(snapshot, current) {
			continue
		}
		if err := writeFileAtomic(writePath, desired, 0o600); err != nil {
			return err
		}
		persisted, err := readHookConfigSnapshot(writePath)
		if err != nil {
			return err
		}
		if persisted.exists && bytes.Equal(persisted.content, desired) {
			return nil
		}
	}
	return fmt.Errorf("configuration changed concurrently while updating %s", displayPath)
}

func readHookConfigSnapshot(path string) (hookConfigSnapshot, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hookConfigSnapshot{}, nil
	}
	if err != nil {
		return hookConfigSnapshot{}, err
	}
	return hookConfigSnapshot{content: content, exists: true}, nil
}

func sameHookConfigSnapshot(left, right hookConfigSnapshot) bool {
	return left.exists == right.exists && bytes.Equal(left.content, right.content)
}

func mergeHookConfig(content []byte, existed bool, displayPath, binary, client string, install bool) ([]byte, bool, error) {
	root := map[string]json.RawMessage{}
	if existed {
		if err := json.Unmarshal(content, &root); err != nil {
			return nil, false, fmt.Errorf("parse %s: %w", displayPath, err)
		}
		if root == nil {
			return nil, false, fmt.Errorf("parse %s: root must be an object", displayPath)
		}
	}
	rawHooks, hasHooks := root["hooks"]
	if !hasHooks {
		if !install {
			return nil, false, nil
		}
		rawHooks = json.RawMessage(`{}`)
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(rawHooks, &hooks); err != nil || hooks == nil {
		return nil, false, fmt.Errorf("parse %s: hooks must be an object", displayPath)
	}
	for eventName, rawGroups := range hooks {
		var groups []json.RawMessage
		if err := json.Unmarshal(rawGroups, &groups); err != nil || groups == nil {
			return nil, false, fmt.Errorf("parse %s: hooks.%s must be an array", displayPath, eventName)
		}
		filtered := make([]json.RawMessage, 0, len(groups))
		for _, rawGroup := range groups {
			var group map[string]json.RawMessage
			if err := json.Unmarshal(rawGroup, &group); err != nil || group == nil {
				filtered = append(filtered, rawGroup)
				continue
			}
			rawHandlers, hasHandlers := group["hooks"]
			if !hasHandlers {
				filtered = append(filtered, rawGroup)
				continue
			}
			var handlers []json.RawMessage
			if err := json.Unmarshal(rawHandlers, &handlers); err != nil || handlers == nil {
				return nil, false, fmt.Errorf("parse %s: hooks.%s group hooks must be an array", displayPath, eventName)
			}
			kept := make([]json.RawMessage, 0, len(handlers))
			for _, rawHandler := range handlers {
				var handler map[string]json.RawMessage
				if err := json.Unmarshal(rawHandler, &handler); err != nil || handler == nil {
					kept = append(kept, rawHandler)
					continue
				}
				var command string
				_ = json.Unmarshal(handler["command"], &command)
				if !managedHookCommand(command, binary, client) {
					kept = append(kept, rawHandler)
				}
			}
			if len(kept) > 0 {
				encoded, err := json.Marshal(kept)
				if err != nil {
					return nil, false, err
				}
				group["hooks"] = encoded
				encoded, err = json.Marshal(group)
				if err != nil {
					return nil, false, err
				}
				filtered = append(filtered, encoded)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, eventName)
		} else {
			encoded, err := json.Marshal(filtered)
			if err != nil {
				return nil, false, err
			}
			hooks[eventName] = encoded
		}
	}
	if install {
		for _, item := range hookFileSpecs() {
			command := shellQuote(binary) + " hook --client " + client + " --event " + item.name
			handler := map[string]any{"type": "command", "command": command, "timeout": item.timeout}
			group := map[string]any{"hooks": []any{handler}}
			if item.matcher != "" {
				group["matcher"] = item.matcher
			}
			encodedGroup, err := json.Marshal(group)
			if err != nil {
				return nil, false, err
			}
			var groups []json.RawMessage
			if rawGroups, ok := hooks[item.event]; ok {
				if err := json.Unmarshal(rawGroups, &groups); err != nil {
					return nil, false, err
				}
			}
			groups = append(groups, encodedGroup)
			encodedGroups, err := json.Marshal(groups)
			if err != nil {
				return nil, false, err
			}
			hooks[item.event] = encodedGroups
		}
	}
	encodedHooks, err := json.Marshal(hooks)
	if err != nil {
		return nil, false, err
	}
	root["hooks"] = encodedHooks
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(encoded, '\n'), true, nil
}

type hookFileSpec struct {
	event   string
	matcher string
	name    string
	timeout int
}

func hookFileSpecs() []hookFileSpec {
	return []hookFileSpec{
		{"SessionStart", "startup|resume|clear|compact", "session-start", 10},
		{"UserPromptSubmit", "", "user-prompt", 3},
		{"Stop", "", "stop", 5},
	}
}

func configFilePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("resolve config symlink %s: %w", path, err)
		}
		target, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !target.Mode().IsRegular() {
			return "", fmt.Errorf("config symlink target is not a regular file: %s", path)
		}
		return resolved, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("config path is not a regular file: %s", path)
	}
	return path, nil
}

func managedHookCommand(command, binary, client string) bool {
	prefix := shellQuote(binary) + " hook --client " + client + " --event "
	for _, spec := range hookFileSpecs() {
		if command == prefix+spec.name {
			return true
		}
	}
	return false
}

func shellQuote(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n'\"") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func managedHooksPresent(path, binary, client string) (bool, error) {
	readPath, err := configFilePath(path)
	if err != nil {
		return false, err
	}
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := readJSON(readPath, &root); err != nil {
		return false, err
	}
	for _, groups := range root.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if managedHookCommand(hook.Command, binary, client) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func hooksConfigured(path, binary, client string) bool {
	readPath, err := configFilePath(path)
	if err != nil {
		return false
	}
	var root struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := readJSON(readPath, &root); err != nil {
		return false
	}
	prefix := shellQuote(binary) + " hook --client " + client + " --event "
	specs := hookFileSpecs()
	counts := make([]int, len(specs))
	for eventName, rawGroups := range root.Hooks {
		if bytes.Equal(bytes.TrimSpace(rawGroups), []byte("null")) {
			return false
		}
		var groups []any
		if err := json.Unmarshal(rawGroups, &groups); err != nil {
			return false
		}
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				return false
			}
			rawHandlers, exists := group["hooks"]
			if !exists {
				return false
			}
			handlers, ok := rawHandlers.([]any)
			if !ok {
				return false
			}
			if matcher, exists := group["matcher"]; exists {
				if _, ok := matcher.(string); !ok {
					return false
				}
			}
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				if !ok {
					return false
				}
				handlerType, ok := handler["type"].(string)
				if !ok || handlerType == "" {
					return false
				}
				if handlerType == "command" {
					if _, ok := handler["command"].(string); !ok {
						return false
					}
				}
				command, _ := handler["command"].(string)
				matched := -1
				for index, spec := range specs {
					if command == prefix+spec.name {
						matched = index
						break
					}
				}
				if matched < 0 {
					continue
				}
				spec := specs[matched]
				matcher, hasMatcher := group["matcher"]
				matcherOK := !hasMatcher && spec.matcher == ""
				if spec.matcher != "" {
					value, ok := matcher.(string)
					matcherOK = hasMatcher && ok && value == spec.matcher
				}
				timeout, timeoutOK := handler["timeout"].(float64)
				if eventName != spec.event || !matcherOK || handlerType != "command" || !timeoutOK || timeout != float64(spec.timeout) {
					return false
				}
				counts[matched]++
				if counts[matched] != 1 {
					return false
				}
			}
		}
	}
	for _, count := range counts {
		if count != 1 {
			return false
		}
	}
	return true
}
