package hgctl

import (
	"context"
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

func (a *App) doctor(ctx context.Context) error {
	projectOK := false
	projectNote := a.Paths.Vault
	indexedOK := false
	indexNote := a.Paths.IndexedSHA
	// `basic-memory project list` takes ~15s on a real corpus, so a 5s probe
	// reported the binary as broken ("signal: killed") when it was merely slow -
	// a doctor line that lies is worse than one that is missing.
	checkCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
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
	checks := []doctorCheck{
		{"git", commandExists("git"), "required transport"},
		{"basic-memory", commandExists("basic-memory"), "required MCP-backed memory helper"},
		{"memory project", projectOK, projectNote},
		{"memory index", indexedOK, indexNote},
		{"stable binary", managedStableSymlink(filepath.Join(a.Paths.Bin, "hgctl"), a.Paths.Versions), filepath.Join(a.Paths.Bin, "hgctl")},
		{"control checkout", isGitWorktree(a.Paths.Control), a.Paths.Control},
		{"queue worktree", isGitWorktree(a.Paths.Queue), a.Paths.Queue},
		{"shared worktree", isGitWorktree(a.Paths.Shared), a.Paths.Shared},
	}
	checks = append(checks, a.basicMemoryMCPDoctorChecks(ctx)...)
	checks = append(checks,
		doctorCheck{"scheduler", a.schedulerLoaded(ctx), LaunchLabel},
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
