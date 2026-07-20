package hgctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (a *App) installBinary() error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(Version, "v")
	if version == "" || strings.ContainsAny(version, "/\\") {
		version = "dev"
	}
	target := filepath.Join(a.Paths.Versions, version, "hgctl")
	if err := writeFileAtomic(target, content, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Paths.Bin, 0o755); err != nil {
		return err
	}
	link := filepath.Join(a.Paths.Bin, "hgctl")
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink != 0 && !managedStableSymlink(link, a.Paths.Versions) {
		return fmt.Errorf("refusing to replace unmanaged symlink %s", link)
	}
	return replaceStableSymlink(link, target)
}

func (a *App) setupBasicMemory(ctx context.Context) error {
	if !commandExists("basic-memory") {
		return errors.New("basic-memory is not installed")
	}
	state, err := a.loadState()
	if err != nil {
		return err
	}
	project, created, err := a.ensureBasicMemoryProject(ctx, &state)
	if err != nil {
		return fmt.Errorf("Basic Memory project: %w", err)
	}
	ownership := basicMemoryOwnershipForSetup(state.BasicMemoryProject, project, created, a.Paths.Vault)
	state.BasicMemoryProject = &ownership
	if err := a.saveState(state); err != nil {
		return err
	}
	if err := a.reindexBasicMemory(ctx); err != nil {
		return fmt.Errorf("Basic Memory reindex: %w", err)
	}
	return nil
}

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

var basicMemoryReadOnlyEnv = []string{
	"BASIC_MEMORY_ENSURE_FRONTMATTER_ON_SYNC=false",
	"BASIC_MEMORY_DISABLE_PERMALINKS=true",
	"BASIC_MEMORY_SEMANTIC_SEARCH_ENABLED=false",
	"BASIC_MEMORY_DEFAULT_SEARCH_TYPE=text",
}

func (a *App) reindexBasicMemory(ctx context.Context) error {
	head, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	state, err := a.loadState()
	if err != nil {
		return err
	}
	if state.BasicMemoryProject == nil || state.BasicMemoryProject.ExternalID == "" ||
		canonicalPath(state.BasicMemoryProject.Path) != canonicalPath(a.Paths.Vault) {
		return errors.New("Basic Memory project identity is not configured")
	}
	projectID := state.BasicMemoryProject.ExternalID
	var indexed BasicMemoryIndexReceipt
	if err := readJSON(a.Paths.IndexedSHA, &indexed); err == nil &&
		indexed.SharedSHA == head && indexed.ProjectExternalID == projectID {
		return nil
	}
	if !commandExists("basic-memory") {
		return errors.New("basic-memory is not installed")
	}
	status, err := runCommand(ctx, a.Paths.Vault, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("shared worktree is dirty; refusing Basic Memory reindex")
	}
	if _, err := runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "reindex", "--search", "--project", ProjectName); err != nil {
		return err
	}
	status, err = runCommand(ctx, a.Paths.Vault, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("Basic Memory modified the shared worktree; index receipt withheld")
	}
	return writeJSONAtomic(a.Paths.IndexedSHA, BasicMemoryIndexReceipt{
		SharedSHA:         head,
		ProjectExternalID: projectID,
	}, 0o600)
}

type basicMemoryProject struct {
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
	LocalPath  string `json:"local_path"`
	Path       string `json:"path"`
}

func (p basicMemoryProject) CanonicalPath() string {
	path := p.LocalPath
	if path == "" {
		path = p.Path
	}
	return canonicalPath(path)
}

func listBasicMemoryProjects(ctx context.Context) ([]basicMemoryProject, error) {
	out, err := runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "tool", "list-projects", "--local")
	if err != nil {
		return nil, err
	}
	var listing struct {
		Projects []basicMemoryProject `json:"projects"`
	}
	if err := json.Unmarshal([]byte(out), &listing); err != nil {
		return nil, err
	}
	return listing.Projects, nil
}

func (a *App) ensureBasicMemoryProject(ctx context.Context, state *State) (basicMemoryProject, bool, error) {
	projects, err := listBasicMemoryProjects(ctx)
	if err != nil {
		return basicMemoryProject{}, false, err
	}
	want := canonicalPath(a.Paths.Vault)
	for _, project := range projects {
		if project.Name != ProjectName {
			continue
		}
		if project.CanonicalPath() != want {
			return basicMemoryProject{}, false, fmt.Errorf("project %q already points to %s", ProjectName, project.CanonicalPath())
		}
		if project.ExternalID == "" {
			return basicMemoryProject{}, false, fmt.Errorf("project %q has no external identity", ProjectName)
		}
		return project, false, nil
	}
	previous := state.BasicMemoryProject
	state.BasicMemoryProject = &BasicMemoryOwnership{
		Path: canonicalPath(a.Paths.Vault), Managed: true, Pending: true,
	}
	if err := a.saveState(*state); err != nil {
		state.BasicMemoryProject = previous
		return basicMemoryProject{}, false, fmt.Errorf("record Basic Memory create intent: %w", err)
	}
	_, err = runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "project", "add", ProjectName, a.Paths.Vault, "--local")
	if err != nil {
		return basicMemoryProject{}, false, err
	}
	projects, err = listBasicMemoryProjects(ctx)
	if err != nil {
		return basicMemoryProject{}, false, err
	}
	for _, project := range projects {
		if project.Name == ProjectName && project.ExternalID != "" && project.CanonicalPath() == want {
			return project, true, nil
		}
	}
	return basicMemoryProject{}, false, errors.New("Basic Memory did not return the created project identity")
}

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func basicMemoryOwnershipForSetup(previous *BasicMemoryOwnership, project basicMemoryProject, created bool, vault string) BasicMemoryOwnership {
	managed := created
	if !created && previous != nil && previous.Managed {
		managed = basicMemoryOwnershipMatches(*previous, []basicMemoryProject{project}, vault) ||
			(previous.Pending && canonicalPath(previous.Path) == canonicalPath(vault))
	}
	return BasicMemoryOwnership{
		ExternalID: project.ExternalID,
		Path:       project.CanonicalPath(),
		Managed:    managed,
		Pending:    false,
	}
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

func (a *App) installScheduler(ctx context.Context) error {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	switch runtime.GOOS {
	case "darwin":
		dir := filepath.Join(a.Paths.Home, "Library", "LaunchAgents")
		path := filepath.Join(dir, LaunchLabel+".plist")
		plist := launchAgent(stable, a.Paths.Data, a.Paths.Home, a.Paths.Vault)
		if err := verifySchedulerFile(path, stable); err != nil {
			return err
		}
		if err := writeFileAtomic(path, []byte(plist), 0o644); err != nil {
			return err
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		_, _ = runCommand(ctx, "", "launchctl", "bootout", domain, path)
		_, err := runCommand(ctx, "", "launchctl", "bootstrap", domain, path)
		return err
	case "linux":
		if err := ensureUserLinger(ctx); err != nil {
			return err
		}
		dir := filepath.Join(a.Paths.Home, ".config", "systemd", "user")
		service := filepath.Join(dir, LaunchLabel+".service")
		timer := filepath.Join(dir, LaunchLabel+".timer")
		pathValue := a.Paths.Bin + ":" + filepath.Join(a.Paths.Home, ".local", "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
		serviceBody := fmt.Sprintf("[Unit]\nDescription=Hourglass sync\n\n[Service]\nType=oneshot\nTimeoutStartSec=180\nEnvironment=\"PATH=%s\"\nEnvironment=\"HGCTL_HOME=%s\"\nEnvironment=\"HGCTL_DATA_DIR=%s\"\nEnvironment=\"HOURGLASS_VAULT=%s\"\nExecStart=%s sync --update\n", systemdQuote(pathValue), systemdQuote(a.Paths.Home), systemdQuote(a.Paths.Data), systemdQuote(a.Paths.Vault), systemdEscape(stable))
		timerBody := "[Unit]\nDescription=Run Hourglass sync every minute\n\n[Timer]\nOnBootSec=30s\nOnUnitActiveSec=60s\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"
		for _, path := range []string{service, timer} {
			if err := verifySchedulerFile(path, stable); err != nil {
				return err
			}
		}
		if err := writeFileAtomic(service, []byte(serviceBody), 0o644); err != nil {
			return err
		}
		if err := writeFileAtomic(timer, []byte(timerBody), 0o644); err != nil {
			return err
		}
		if _, err := runCommand(ctx, "", "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		_, err := runCommand(ctx, "", "systemctl", "--user", "enable", "--now", LaunchLabel+".timer")
		return err
	default:
		return nil
	}
}

func ensureUserLinger(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	uid := strconv.Itoa(os.Getuid())
	if enabled, _ := userLingerEnabled(ctx, uid); enabled {
		return nil
	}
	attemptOutput, attemptErr := runCommand(ctx, "", "loginctl", "--no-ask-password", "enable-linger", uid)
	if enabled, verifyErr := userLingerEnabled(ctx, uid); enabled {
		return nil
	} else {
		detail := attemptErr
		if detail == nil {
			detail = verifyErr
		}
		if detail == nil {
			detail = errors.New("loginctl still reports Linger=no")
		}
		if strings.TrimSpace(attemptOutput) != "" {
			detail = fmt.Errorf("%w: %s", detail, strings.TrimSpace(attemptOutput))
		}
		return fmt.Errorf("persistent user services are required: %w; run once, then retry: sudo loginctl enable-linger %s", detail, uid)
	}
}

func userLingerEnabled(ctx context.Context, uid string) (bool, error) {
	out, err := runCommand(ctx, "", "loginctl", "show-user", uid, "--property=Linger", "--value")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(out), "yes"), nil
}

func launchAgent(binary, data, home, vault string) string {
	escape := html.EscapeString
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + LaunchLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + escape(binary) + `</string>
    <string>sync</string>
    <string>--update</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>StartInterval</key><integer>60</integer>
  <key>ProcessType</key><string>Background</string>
  <key>EnvironmentVariables</key>
  <dict>
	<key>PATH</key><string>` + escape(filepath.Dir(binary)) + `:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	<key>HGCTL_HOME</key><string>` + escape(home) + `</string>
	<key>HGCTL_DATA_DIR</key><string>` + escape(data) + `</string>
	<key>HOURGLASS_VAULT</key><string>` + escape(vault) + `</string>
  </dict>
  <key>StandardOutPath</key><string>` + escape(filepath.Join(data, "sync.log")) + `</string>
  <key>StandardErrorPath</key><string>` + escape(filepath.Join(data, "sync.err.log")) + `</string>
</dict>
</plist>
`
}

func systemdEscape(path string) string {
	path = strings.ReplaceAll(path, "%", "%%")
	return strings.ReplaceAll(path, " ", "\\x20")
}

func systemdQuote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return strings.ReplaceAll(value, "%", "%%")
}

func verifySchedulerFile(path, stable string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing unmanaged scheduler path %s", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(content)
	managed := false
	switch {
	case strings.HasSuffix(path, ".plist"):
		managed = strings.Contains(text, "<key>Label</key><string>"+LaunchLabel+"</string>") &&
			strings.Contains(text, "<string>"+html.EscapeString(stable)+"</string>")
	case strings.HasSuffix(path, ".service"):
		managed = strings.Contains(text, "\nDescription=Hourglass sync\n") &&
			strings.Contains(text, "\nExecStart="+systemdEscape(stable)+" sync --update\n")
	case strings.HasSuffix(path, ".timer"):
		managed = strings.Contains(text, "\nDescription=Run Hourglass sync every minute\n") &&
			strings.Contains(text, "\nOnUnitActiveSec=60s\n")
	}
	if !managed {
		return fmt.Errorf("refusing unmanaged scheduler file %s", path)
	}
	return nil
}

func (a *App) verifySchedulerOwnership() error {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	for _, path := range a.schedulerPaths() {
		if err := verifySchedulerFile(path, stable); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) schedulerPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(a.Paths.Home, "Library", "LaunchAgents", LaunchLabel+".plist")}
	case "linux":
		dir := filepath.Join(a.Paths.Home, ".config", "systemd", "user")
		return []string{filepath.Join(dir, LaunchLabel+".service"), filepath.Join(dir, LaunchLabel+".timer")}
	default:
		return nil
	}
}

func (a *App) uninstall(ctx context.Context) error {
	return withFileLockWait(ctx, a.Paths.LifecycleLock, func() error {
		return a.uninstallLocked(ctx)
	})
}

func (a *App) uninstallLocked(ctx context.Context) error {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	if err := a.verifySchedulerOwnership(); err != nil {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removal is incomplete; the binary was preserved because the scheduler is not owned by hgctl.")
		return err
	}
	if err := a.quiesceScheduler(ctx); err != nil {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removal is incomplete; the binary was preserved so the scheduler cannot break.")
		return fmt.Errorf("stop scheduler before uninstall: %w", err)
	}

	var errs []error
	safeToRemoveBinary := true
	cleanupErr := withFileLockWait(ctx, a.Paths.SyncLock, func() error {
		return withFileLockWait(ctx, a.Paths.UpdateLock, func() error {
			return withFileLockWait(ctx, a.Paths.CodexLock, func() error {
				for _, item := range a.clientAdapters() {
					present, err := managedHooksPresent(item.path, stable, item.client)
					if errors.Is(err, os.ErrNotExist) || (err == nil && !present) {
						continue
					}
					if err != nil {
						errs = append(errs, fmt.Errorf("inspect %s hooks: %w", item.client, err))
						safeToRemoveBinary = false
						continue
					}
					if err := configureHookFile(item.path, stable, item.client, false); err != nil {
						errs = append(errs, fmt.Errorf("remove %s hooks: %w", item.client, err))
						safeToRemoveBinary = false
						continue
					}
					remaining, err := managedHooksPresent(item.path, stable, item.client)
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						errs = append(errs, fmt.Errorf("verify %s hooks: %w", item.client, err))
						safeToRemoveBinary = false
					} else if remaining {
						errs = append(errs, fmt.Errorf("verify %s hooks: managed hooks remain", item.client))
						safeToRemoveBinary = false
					}
				}

				if err := a.removeManagedBasicMemoryProject(ctx); err != nil {
					errs = append(errs, err)
				}

				if err := a.removeSchedulerFiles(ctx); err != nil {
					errs = append(errs, err)
					safeToRemoveBinary = false
				}
				if remaining, err := a.schedulerFilesPresent(); err != nil {
					errs = append(errs, fmt.Errorf("verify scheduler files: %w", err))
					safeToRemoveBinary = false
				} else if remaining {
					errs = append(errs, errors.New("verify scheduler files: managed scheduler files remain"))
					safeToRemoveBinary = false
				}

				if safeToRemoveBinary && managedStableSymlink(stable, a.Paths.Versions) {
					if err := os.Remove(stable); err != nil {
						errs = append(errs, err)
						safeToRemoveBinary = false
					}
				}
				return nil
			})
		})
	})
	if cleanupErr != nil {
		errs = append(errs, cleanupErr)
		safeToRemoveBinary = false
	}
	if safeToRemoveBinary {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removed; vault, outbox, and machine identity were preserved.")
	} else {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removal is incomplete; the binary was preserved so remaining hooks or the scheduler cannot break.")
	}
	return errors.Join(errs...)
}

func (a *App) removeManagedBasicMemoryProject(ctx context.Context) error {
	state, err := a.loadState()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !commandExists("basic-memory") || state.BasicMemoryProject == nil || !state.BasicMemoryProject.Managed {
		return nil
	}
	projects, err := listBasicMemoryProjects(ctx)
	if err != nil {
		return fmt.Errorf("Basic Memory project check: %w", err)
	}
	owned := basicMemoryOwnershipMatches(*state.BasicMemoryProject, projects, a.Paths.Vault)
	if !owned && state.BasicMemoryProject.Pending && canonicalPath(state.BasicMemoryProject.Path) == canonicalPath(a.Paths.Vault) {
		matches := 0
		for _, project := range projects {
			if project.Name == ProjectName && project.ExternalID != "" && project.CanonicalPath() == canonicalPath(a.Paths.Vault) {
				matches++
			}
		}
		owned = matches == 1
	}
	if !owned {
		return nil
	}
	if _, err := runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "project", "remove", ProjectName, "--local"); err != nil {
		return fmt.Errorf("Basic Memory project remove: %w", err)
	}
	state.BasicMemoryProject = nil
	if err := a.saveState(state); err != nil {
		return fmt.Errorf("clear Basic Memory ownership: %w", err)
	}
	if err := os.Remove(a.Paths.IndexedSHA); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Basic Memory index receipt: %w", err)
	}
	return nil
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

func replaceStableSymlink(link, target string) error {
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-symlink %s", link)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := link + ".new"
	if info, err := os.Lstat(tmp); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-symlink %s", tmp)
		}
		if err := os.Remove(tmp); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func managedStableSymlink(link, versions string) bool {
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	rel, err := filepath.Rel(canonicalPath(versions), canonicalPath(target))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func basicMemoryOwnershipMatches(ownership BasicMemoryOwnership, projects []basicMemoryProject, expectedPath string) bool {
	want := canonicalPath(expectedPath)
	if ownership.ExternalID == "" || canonicalPath(ownership.Path) != want {
		return false
	}
	for _, project := range projects {
		if project.Name == ProjectName && project.ExternalID == ownership.ExternalID && project.CanonicalPath() == want {
			return true
		}
	}
	return false
}

func (a *App) quiesceScheduler(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid())
		target := domain + "/" + LaunchLabel
		if _, err := runCommand(ctx, "", "launchctl", "bootout", target); err != nil && !ignorableSchedulerStopError(err) {
			return err
		}
		if _, err := runCommand(ctx, "", "launchctl", "print", target); err == nil {
			return fmt.Errorf("LaunchAgent %s is still loaded", LaunchLabel)
		} else if !ignorableSchedulerStopError(err) {
			return fmt.Errorf("verify LaunchAgent stopped: %w", err)
		}
	case "linux":
		if _, err := runCommand(ctx, "", "systemctl", "--user", "disable", "--now", LaunchLabel+".timer"); err != nil && !ignorableSchedulerStopError(err) {
			return err
		}
		if _, err := runCommand(ctx, "", "systemctl", "--user", "stop", LaunchLabel+".service"); err != nil && !ignorableSchedulerStopError(err) {
			return err
		}
		for _, name := range []string{LaunchLabel + ".timer", LaunchLabel + ".service"} {
			inactive, err := systemdUnitInactive(ctx, name)
			if err != nil {
				return err
			}
			if !inactive {
				return fmt.Errorf("systemd user unit %s is still active", name)
			}
		}
	}
	return nil
}

func systemdUnitInactive(ctx context.Context, name string) (bool, error) {
	out, err := runCommand(ctx, "", "systemctl", "--user", "show", name, "--property=ActiveState", "--value")
	if err != nil {
		if ignorableSchedulerStopError(err) {
			return true, nil
		}
		return false, fmt.Errorf("verify systemd user unit %s stopped: %w", name, err)
	}
	switch strings.TrimSpace(out) {
	case "inactive", "failed":
		return true, nil
	default:
		return false, nil
	}
}

func (a *App) removeSchedulerFiles(ctx context.Context) error {
	if err := a.verifySchedulerOwnership(); err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(a.Paths.Home, "Library", "LaunchAgents", LaunchLabel+".plist")
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	case "linux":
		dir := filepath.Join(a.Paths.Home, ".config", "systemd", "user")
		for _, name := range []string{LaunchLabel + ".service", LaunchLabel + ".timer"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if _, err := runCommand(ctx, "", "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) schedulerFilesPresent() (bool, error) {
	for _, path := range a.schedulerPaths() {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func ignorableSchedulerStopError(err error) bool {
	message := strings.ToLower(err.Error())
	for _, text := range []string{"could not find specified service", "could not find service", "could not be found", "no such process", "not loaded", "not found", "does not exist"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

type doctorCheck struct {
	name string
	ok   bool
	note string
}

func (a *App) clientDoctorChecks(ctx context.Context) []doctorCheck {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	var checks []doctorCheck
	for _, item := range a.clientAdapters() {
		if !commandExists(item.executable) {
			continue
		}
		ok := hooksConfigured(item.path, stable, item.client)
		note := "user settings"
		if item.client == "codex" {
			note = "user hooks + app-server trust"
			if ok {
				if err := a.verifyCodexHooks(ctx); err != nil {
					ok = false
					note = boundString(err.Error(), 512)
				}
			}
		}
		checks = append(checks, doctorCheck{item.name + " hooks", ok, note})
	}
	return checks
}

func (a *App) doctor(ctx context.Context) error {
	projectOK := false
	projectID := ""
	if commandExists("basic-memory") {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		projects, err := listBasicMemoryProjects(checkCtx)
		cancel()
		if err == nil {
			want := canonicalPath(a.Paths.Vault)
			for _, project := range projects {
				if project.Name == ProjectName && project.ExternalID != "" && project.CanonicalPath() == want {
					projectOK = true
					projectID = project.ExternalID
				}
			}
		}
	}
	indexedOK := false
	if head, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "HEAD"); err == nil {
		var indexed BasicMemoryIndexReceipt
		if readErr := readJSON(a.Paths.IndexedSHA, &indexed); readErr == nil {
			indexedOK = indexed.SharedSHA == strings.TrimSpace(head) && indexed.ProjectExternalID == projectID
		}
	}
	quarantineEmpty := true
	if entries, err := os.ReadDir(a.Paths.Quarantine); err == nil {
		quarantineEmpty = len(entries) == 0
	} else if !errors.Is(err, os.ErrNotExist) {
		quarantineEmpty = false
	}
	checks := []doctorCheck{
		{"git", commandExists("git"), "required transport"},
		{"basic-memory", commandExists("basic-memory"), "required recall helper"},
		{"memory project", projectOK, a.Paths.Vault},
		{"memory index", indexedOK, a.Paths.IndexedSHA},
		{"stable binary", managedStableSymlink(filepath.Join(a.Paths.Bin, "hgctl"), a.Paths.Versions), filepath.Join(a.Paths.Bin, "hgctl")},
		{"control checkout", isGitWorktree(a.Paths.Control), a.Paths.Control},
		{"queue worktree", isGitWorktree(a.Paths.Queue), a.Paths.Queue},
		{"shared worktree", isGitWorktree(a.Paths.Vault), a.Paths.Vault},
	}
	checks = append(checks, a.clientDoctorChecks(ctx)...)
	checks = append(checks,
		doctorCheck{"scheduler", a.schedulerLoaded(ctx), LaunchLabel},
		doctorCheck{"quarantine", quarantineEmpty, a.Paths.Quarantine},
	)
	failed := 0
	for _, item := range checks {
		status := "ok"
		if !item.ok {
			status = "missing"
			failed++
		}
		_, _ = fmt.Fprintf(a.Out, "%-7s %-18s %s\n", status, item.name, item.note)
	}
	if id, err := a.loadIdentity(); err == nil {
		_, _ = fmt.Fprintf(a.Out, "machine %-36s hostname=%s\n", id.ID, id.Hostname)
	} else {
		failed++
	}
	if failed > 0 {
		return fmt.Errorf("%d doctor check(s) failed", failed)
	}
	return nil
}

func (a *App) schedulerLoaded(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		domain := "gui/" + strconv.Itoa(os.Getuid()) + "/" + LaunchLabel
		_, err := runCommand(ctx, "", "launchctl", "print", domain)
		return err == nil
	case "linux":
		if _, err := runCommand(ctx, "", "systemctl", "--user", "is-active", "--quiet", LaunchLabel+".timer"); err != nil {
			return false
		}
		out, err := runCommand(ctx, "", "loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value")
		return err == nil && strings.TrimSpace(out) == "yes"
	default:
		return true
	}
}

func isGitWorktree(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
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
