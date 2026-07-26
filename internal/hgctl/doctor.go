package hgctl

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/gitx"
	"github.com/x2x3studio/hgctl/internal/proc"
)

type doctorCheck struct {
	name string
	ok   bool
	note string
}

// checkBasicMemoryIndex answers "can this endpoint actually recall?" in two
// parts, because the receipt alone cannot. The receipt says a reindex ran to
// completion for a given shared revision - it says nothing about whether the
// database it wrote still exists. `basic-memory reset` drops every table and the
// receipt is untouched, so doctor read "ok" while every recall came back empty.
//
// The note carries both counts even when the check passes, because a PARTIAL
// index is real and there is no honest threshold for it: Basic Memory mints
// extra entities for forward-referenced wikilinks, so the index legitimately
// runs a little above the note count. Asserting a ratio would eventually report
// a healthy endpoint as broken; printing the numbers lets a reader see the
// difference without doctor having to claim something it cannot know.
func (a *App) checkBasicMemoryIndex(ctx context.Context) (bool, string) {
	project, err := a.resolveBasicMemoryProject(ctx)
	if err != nil {
		return false, fsx.Bound(err.Error(), 512)
	}
	if _, err := a.verifyBasicMemoryIndexReceipt(ctx, project); err != nil {
		return false, fsx.Bound(err.Error(), 512)
	}
	notes := productNoteCount(a.Paths.Vault)
	if notes == 0 {
		return true, "no product to index yet"
	}
	indexed, err := basicMemoryIndexedCount(ctx)
	if err != nil {
		return false, fsx.Bound("index probe failed: "+err.Error(), 512)
	}
	if indexed == 0 {
		return false, fmt.Sprintf("receipt is current but the index holds nothing for %d notes", notes)
	}
	return true, fmt.Sprintf("%d indexed / %d notes", indexed, notes)
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
	_, projectErr := a.resolveBasicMemoryProject(checkCtx)
	if projectErr == nil {
		projectOK = true
		indexedOK, indexNote = a.checkBasicMemoryIndex(checkCtx)
	} else {
		projectNote = fsx.Bound(projectErr.Error(), 512)
	}
	cancel()
	checks := []doctorCheck{
		{"git", proc.Exists("git"), "required transport"},
		{"basic-memory", proc.Exists("basic-memory"), "required MCP-backed memory helper"},
		{"memory project", projectOK, projectNote},
		{"memory index", indexedOK, indexNote},
		{"stable binary", managedStableSymlink(filepath.Join(a.Paths.Bin, "hgctl"), a.Paths.Versions), filepath.Join(a.Paths.Bin, "hgctl")},
		{"control checkout", gitx.IsWorktree(a.Paths.Control), a.Paths.Control},
		{"queue worktree", gitx.IsWorktree(a.Paths.Queue), a.Paths.Queue},
		{"shared worktree", gitx.IsWorktree(a.Paths.Shared), a.Paths.Shared},
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
