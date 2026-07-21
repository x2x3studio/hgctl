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

func configureHookFile(path, binary, client string, install bool) error {
	writePath, err := configFilePath(path)
	if err != nil {
		return err
	}
	root := map[string]any{}
	existed := false
	if content, err := os.ReadFile(writePath); err == nil {
		existed = true
		if err := json.Unmarshal(content, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if root == nil {
			return fmt.Errorf("parse %s: root must be an object", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !install && !existed {
		return os.ErrNotExist
	}
	rawHooks, hasHooks := root["hooks"]
	if !hasHooks {
		if !install {
			return nil
		}
		hooks := map[string]any{}
		root["hooks"] = hooks
		rawHooks = hooks
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return fmt.Errorf("parse %s: hooks must be an object", path)
	}
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	for eventName, raw := range hooks {
		groups, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("parse %s: hooks.%s must be an array", path, eventName)
		}
		filtered := make([]any, 0, len(groups))
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				filtered = append(filtered, rawGroup)
				continue
			}
			rawHandlers, hasHandlers := group["hooks"]
			if !hasHandlers {
				filtered = append(filtered, rawGroup)
				continue
			}
			handlers, ok := rawHandlers.([]any)
			if !ok {
				return fmt.Errorf("parse %s: hooks.%s group hooks must be an array", path, eventName)
			}
			kept := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				command, _ := handler["command"].(string)
				if !ok || !managedHookCommand(command, binary, client) {
					kept = append(kept, rawHandler)
				}
			}
			if len(kept) > 0 {
				group["hooks"] = kept
				filtered = append(filtered, group)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, eventName)
		} else {
			hooks[eventName] = filtered
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
			groups, _ := hooks[item.event].([]any)
			hooks[item.event] = append(groups, group)
		}
	}
	return writeJSONAtomic(writePath, root, 0o600)
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
