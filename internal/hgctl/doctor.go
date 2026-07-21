package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
		if indexed, readErr := a.loadBasicMemoryIndexReceipt(); readErr == nil {
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

func isGitWorktree(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}
