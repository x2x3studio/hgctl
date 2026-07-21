package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type doctorCheck struct {
	name string
	ok   bool
	note string
}

func (a *App) hookDiagnosticDoctorCheck() doctorCheck {
	var diagnostic hookDiagnostic
	if err := readJSON(a.hookDiagnosticPath(), &diagnostic); errors.Is(err, os.ErrNotExist) {
		return doctorCheck{"hook diagnostics", true, "none"}
	} else if err != nil {
		return doctorCheck{"hook diagnostics", false, boundString(err.Error(), 512)}
	}
	if diagnostic.SchemaVersion != hookDiagnosticSchemaVersion || diagnostic.Client == "" ||
		diagnostic.Event == "" || diagnostic.Message == "" || diagnostic.OccurredAt.IsZero() {
		return doctorCheck{"hook diagnostics", false, "invalid persisted hook diagnostic"}
	}
	note := fmt.Sprintf("%s/%s at %s: %s", diagnostic.Client, diagnostic.Event,
		diagnostic.OccurredAt.UTC().Format(time.RFC3339), diagnostic.Message)
	return doctorCheck{"hook diagnostics", false, boundString(note, 512)}
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
	projectNote := a.Paths.Vault
	indexedOK := false
	indexNote := a.Paths.IndexedSHA
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	project, projectErr := a.resolveBasicMemoryProject(checkCtx)
	if projectErr == nil {
		projectOK = true
		if _, err := a.verifyBasicMemoryIndexReceipt(checkCtx, project); err == nil {
			indexedOK = true
		} else {
			indexNote = boundString(err.Error(), 512)
		}
	} else {
		projectNote = boundString(projectErr.Error(), 512)
	}
	cancel()
	quarantineEmpty := true
	if entries, err := os.ReadDir(a.Paths.Quarantine); err == nil {
		quarantineEmpty = len(entries) == 0
	} else if !errors.Is(err, os.ErrNotExist) {
		quarantineEmpty = false
	}
	checks := []doctorCheck{
		{"git", commandExists("git"), "required transport"},
		{"basic-memory", commandExists("basic-memory"), "required recall helper"},
		{"memory project", projectOK, projectNote},
		{"memory index", indexedOK, indexNote},
		{"stable binary", managedStableSymlink(filepath.Join(a.Paths.Bin, "hgctl"), a.Paths.Versions), filepath.Join(a.Paths.Bin, "hgctl")},
		{"control checkout", isGitWorktree(a.Paths.Control), a.Paths.Control},
		{"queue worktree", isGitWorktree(a.Paths.Queue), a.Paths.Queue},
		{"shared worktree", isGitWorktree(a.Paths.Vault), a.Paths.Vault},
	}
	checks = append(checks, a.clientDoctorChecks(ctx)...)
	checks = append(checks,
		doctorCheck{"scheduler", a.schedulerLoaded(ctx), LaunchLabel},
		doctorCheck{"quarantine", quarantineEmpty, a.Paths.Quarantine},
		a.hookDiagnosticDoctorCheck(),
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
