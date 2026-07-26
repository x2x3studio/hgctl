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

	"github.com/x2x3studio/hgctl/internal/fsx"
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

// pruneClientHooks removes any stale hgctl-managed hook registration from every
// client config. Per-turn capture is retired: hgctl installs no hooks, so
// install, repair, and uninstall all converge on pruning whatever still points
// at the hgctl binary. Basic Memory MCP recall registration is separate and left
// untouched. A missing client config is benign (nothing to prune).
func (a *App) pruneClientHooks() error {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	var errs []error
	for _, item := range a.clientAdapters() {
		if err := pruneClientHookFile(item.path, stable, item.client); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%s hooks: %w", item.name, err))
		}
	}
	return errors.Join(errs...)
}

func (a *App) repairClientHooks(ctx context.Context) {
	err := fsx.WithLock(a.Paths.LifecycleLock, func() error {
		stable := filepath.Join(a.Paths.Bin, "hgctl")
		if !managedStableSymlink(stable, a.Paths.Versions) {
			return nil
		}
		if _, err := os.Stat(a.Paths.State); err != nil {
			return nil
		}
		return a.pruneClientHooks()
	})
	if err != nil {
		_, _ = fmt.Fprintln(a.Err, "hgctl: client hook prune deferred")
	}
}

const hookConfigWriteAttempts = 4

type hookConfigSnapshot struct {
	content []byte
	exists  bool
}

// pruneClientHookFile removes every hgctl-managed hook command for client from
// the config at path, preserving user hooks and every other key. A missing file
// is os.ErrNotExist (nothing to prune); callers treat that as benign.
func pruneClientHookFile(path, binary, client string) error {
	writePath, err := configFilePath(path)
	if err != nil {
		return err
	}
	return pruneClientHookFileWithRetry(writePath, path, binary, client, nil)
}

func pruneClientHookFileWithRetry(writePath, displayPath, binary, client string, beforeVerify func(int)) error {
	for attempt := 0; attempt < hookConfigWriteAttempts; attempt++ {
		snapshot, err := readHookConfigSnapshot(writePath)
		if err != nil {
			return err
		}
		if !snapshot.exists {
			return os.ErrNotExist
		}
		desired, changed, err := pruneHookConfig(snapshot.content, displayPath, binary, client)
		if err != nil {
			return err
		}
		if !changed {
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
		if err := fsx.WriteAtomic(writePath, desired, 0o600); err != nil {
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

// pruneHookConfig removes every hgctl-managed hook command for client from the
// parsed config, dropping any hook group and event that empties out, and reports
// whether anything changed. When nothing is pruned it reports changed=false so a
// repair that runs every sync never churns an untouched file. Unrelated keys and
// user hooks are preserved.
func pruneHookConfig(content []byte, displayPath, binary, client string) ([]byte, bool, error) {
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(content, &root); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", displayPath, err)
	}
	if root == nil {
		return nil, false, fmt.Errorf("parse %s: root must be an object", displayPath)
	}
	rawHooks, hasHooks := root["hooks"]
	if !hasHooks {
		return nil, false, nil
	}
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(rawHooks, &hooks); err != nil || hooks == nil {
		return nil, false, fmt.Errorf("parse %s: hooks must be an object", displayPath)
	}
	pruned := false
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
				if hgctlManagedHookCommand(command, binary, client) {
					pruned = true
					continue
				}
				kept = append(kept, rawHandler)
			}
			if len(kept) == 0 {
				continue
			}
			if len(kept) == len(handlers) {
				filtered = append(filtered, rawGroup)
				continue
			}
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
		if len(filtered) == 0 {
			delete(hooks, eventName)
			continue
		}
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return nil, false, err
		}
		hooks[eventName] = encoded
	}
	if !pruned {
		return nil, false, nil
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

// hgctlManagedHookCommand reports whether command is an hgctl-installed hook
// invocation for client, whatever event it targets. Prune uses this so a retired
// event (user-prompt, stop, session-start) is removed, while a look-alike that
// merely shares the prefix but carries extra arguments (a distinct, user-owned
// command) is left untouched.
func hgctlManagedHookCommand(command, binary, client string) bool {
	prefix := shellQuote(binary) + " hook --client " + client + " --event "
	rest, ok := strings.CutPrefix(command, prefix)
	return ok && rest != "" && !strings.ContainsAny(rest, " \t")
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
	if err := fsx.ReadJSON(readPath, &root); err != nil {
		return false, err
	}
	for _, groups := range root.Hooks {
		for _, group := range groups {
			for _, hook := range group.Hooks {
				if hgctlManagedHookCommand(hook.Command, binary, client) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
