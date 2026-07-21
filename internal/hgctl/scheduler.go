package hgctl

import (
	"context"
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
