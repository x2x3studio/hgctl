package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/gitx"
	"github.com/x2x3studio/hgctl/internal/proc"

	"github.com/x2x3studio/hgctl/internal/config"
)

const (
	controlManagedKey = "hgctl.managed"
	controlOriginKey  = "hgctl.origin"
)

func (a *App) initGit(ctx context.Context, state config.State) error {
	if !proc.Exists("git") {
		return errors.New("git is required")
	}
	created := false
	if _, err := os.Stat(filepath.Join(a.Paths.Control, ".git")); errors.Is(err, os.ErrNotExist) {
		if exists, err := directoryNotEmpty(a.Paths.Control); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("control path is not an hgctl checkout: %s", a.Paths.Control)
		}
		if err := os.MkdirAll(filepath.Dir(a.Paths.Control), 0o700); err != nil {
			return err
		}
		if _, err := proc.Run(ctx, "", "git", "clone", "--branch", "main", "--single-branch", state.RepoURL, a.Paths.Control); err != nil {
			return err
		}
		created = true
	} else if err != nil {
		return err
	}
	if created {
		if _, err := proc.Run(ctx, a.Paths.Control, "git", "config", "--local", controlManagedKey, "true"); err != nil {
			return err
		}
		if _, err := proc.Run(ctx, a.Paths.Control, "git", "config", "--local", controlOriginKey, state.RepoURL); err != nil {
			return err
		}
	} else if err := a.verifyControlCheckout(ctx, state.RepoURL); err != nil {
		return err
	}
	for _, pair := range [][2]string{
		{"core.hooksPath", "/dev/null"},
		{"commit.gpgSign", "false"},
		{"tag.gpgSign", "false"},
		{"user.name", "chinaboard"},
		{"user.email", "chinaboard@gmail.com"},
	} {
		if _, err := proc.Run(ctx, a.Paths.Control, "git", "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	if err := a.ensureRepositoryBranches(ctx, state.RepoURL); err != nil {
		return err
	}
	if err := a.fetchEndpointRefs(ctx, state); err != nil {
		return err
	}
	if !gitx.RefExists(ctx, a.Paths.Control, "refs/remotes/origin/shared") {
		return errors.New("remote branch shared does not exist")
	}
	if err := a.ensureWorktree(ctx, a.Paths.Shared, "shared", "origin/shared"); err != nil {
		return err
	}
	if err := a.ensureQueueBranch(ctx, state.QueueBranch); err != nil {
		return err
	}
	if err := a.ensureWorktree(ctx, a.Paths.Queue, state.QueueBranch, "origin/"+state.QueueBranch); err != nil {
		return err
	}
	return a.syncSharedUnlocked(ctx)
}

func (a *App) verifyControlCheckout(ctx context.Context, repoURL string) error {
	top, err := proc.Run(ctx, a.Paths.Control, "git", "rev-parse", "--show-toplevel")
	if err != nil || fsx.Canonical(strings.TrimSpace(top)) != fsx.Canonical(a.Paths.Control) {
		return fmt.Errorf("control path is not an owned hgctl checkout: %s", a.Paths.Control)
	}
	managed, err := proc.Run(ctx, a.Paths.Control, "git", "config", "--local", "--get", controlManagedKey)
	if err != nil || strings.TrimSpace(managed) != "true" {
		return fmt.Errorf("control checkout has no hgctl ownership marker: %s", a.Paths.Control)
	}
	markedOrigin, err := proc.Run(ctx, a.Paths.Control, "git", "config", "--local", "--get", controlOriginKey)
	if err != nil || strings.TrimSpace(markedOrigin) != repoURL {
		return fmt.Errorf("control checkout origin marker does not match %s", repoURL)
	}
	origin, err := proc.Run(ctx, a.Paths.Control, "git", "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) != repoURL {
		return fmt.Errorf("control checkout origin does not match %s", repoURL)
	}
	return nil
}

func (a *App) ensureWorktree(ctx context.Context, path, branch, start string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		controlCommon, err := gitx.CommonDir(ctx, a.Paths.Control)
		if err != nil {
			return err
		}
		worktreeCommon, err := gitx.CommonDir(ctx, path)
		if err != nil {
			return err
		}
		if controlCommon != worktreeCommon {
			return fmt.Errorf("worktree %s belongs to another repository", path)
		}
		current, err := proc.Run(ctx, path, "git", "branch", "--show-current")
		if err != nil {
			return err
		}
		if strings.TrimSpace(current) != branch {
			return fmt.Errorf("worktree %s is on %q, expected %q", path, strings.TrimSpace(current), branch)
		}
		return nil
	}
	if exists, err := directoryNotEmpty(path); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("refusing to replace non-empty path %s", path)
	}
	if !gitx.RefExists(ctx, a.Paths.Control, "refs/heads/"+branch) {
		args := []string{"branch", "--no-track", branch, start}
		if _, err := proc.Run(ctx, a.Paths.Control, "git", args...); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, err := proc.Run(ctx, a.Paths.Control, "git", "worktree", "add", path, branch)
	return err
}

// ensureQueueBranch guarantees a local queue branch exists before the queue
// worktree is created. It adopts this machine's existing queue when the remote
// already carries one; otherwise it self-seeds an orphan queue branch, so
// onboarding never depends on anything being present server-side.
func (a *App) ensureQueueBranch(ctx context.Context, branch string) error {
	if gitx.RefExists(ctx, a.Paths.Control, "refs/heads/"+branch) {
		return nil
	}
	if gitx.RefExists(ctx, a.Paths.Control, "refs/remotes/origin/"+branch) {
		return nil
	}
	return a.seedOrphanQueueBranch(ctx, branch)
}

// seedOrphanQueueBranch creates the machine queue branch as a parentless (orphan)
// root commit tracking an empty events/.gitkeep, then publishes it to origin. The
// commit is built through a temporary index so the control worktree, index, and
// HEAD stay untouched, and it preserves the queue orphan + append-only invariant:
// the only tracked path lives under events/.
func (a *App) seedOrphanQueueBranch(ctx context.Context, branch string) error {
	scratch, err := os.MkdirTemp("", "hgctl-queue-seed")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	placeholder := filepath.Join(scratch, "gitkeep")
	if err := os.WriteFile(placeholder, nil, 0o600); err != nil {
		return err
	}
	blob, err := proc.Run(ctx, a.Paths.Control, "git", "hash-object", "-w", placeholder)
	if err != nil {
		return err
	}
	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(scratch, "index")}
	if _, err := proc.RunEnv(ctx, a.Paths.Control, indexEnv, "git", "update-index", "--add",
		"--cacheinfo", "100644,"+strings.TrimSpace(blob)+",events/.gitkeep"); err != nil {
		return err
	}
	tree, err := proc.RunEnv(ctx, a.Paths.Control, indexEnv, "git", "write-tree")
	if err != nil {
		return err
	}
	commit, err := proc.Run(ctx, a.Paths.Control, "git", "commit-tree", strings.TrimSpace(tree), "-m", "Seed machine queue")
	if err != nil {
		return err
	}
	if _, err := proc.Run(ctx, a.Paths.Control, "git", "update-ref", "refs/heads/"+branch, strings.TrimSpace(commit)); err != nil {
		return err
	}
	_, err = proc.Run(ctx, a.Paths.Control, "git", "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

// fetchEndpointRefs updates the refs this endpoint reads: the control branch,
// the product, and this machine's own queue once it exists.
//
// There is deliberately no `queue-template` here. hgctl once probed for one with
// `git ls-remote` on every call - a full SSH round trip, measured at 3.7s - to
// decide whether to adopt a server-side queue seed. The control plane never grew
// that branch (ensureQueueBranch self-seeds an orphan instead, so onboarding
// depends on nothing server-side), so the probe asked a question with a fixed
// answer that no caller read, ~90 minutes of network a day.
func (a *App) fetchEndpointRefs(ctx context.Context, state config.State) error {
	refspecs := []string{
		"+refs/heads/main:refs/remotes/origin/main",
		"+refs/heads/shared:refs/remotes/origin/shared",
	}
	exists := gitx.RefExists(ctx, a.Paths.Control, "refs/remotes/origin/"+state.QueueBranch)
	if !exists {
		var err error
		exists, err = gitx.RemoteBranchExists(ctx, a.Paths.Control, state.QueueBranch)
		if err != nil {
			return err
		}
	}
	if exists {
		refspecs = append(refspecs, "+refs/heads/"+state.QueueBranch+":refs/remotes/origin/"+state.QueueBranch)
	}
	args := append([]string{"fetch", "--prune", "origin"}, refspecs...)
	_, err := proc.Run(ctx, a.Paths.Control, "git", args...)
	return err
}

func directoryNotEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// Budgets for the scheduled sync. They are split because the two halves differ
// by two orders of magnitude: everything but the reindex is git and transcript
// parsing, while the reindex embeds the entire product.
const (
	// syncTimeout covers fetch, queue sync, session ingest, and shared sync.
	// Session ingest parses multi-megabyte transcripts, so this has to leave room
	// for that rather than being sized against git alone.
	syncTimeout = 5 * time.Minute
	// basicMemoryReindexTimeout covers `basic-memory reindex`, measured at 399s
	// for a 291-note product and growing with it. Generous on purpose: a reindex
	// killed halfway writes no receipt, so a budget that is merely close to the
	// real cost fails permanently rather than occasionally.
	basicMemoryReindexTimeout = 30 * time.Minute
)

func (a *App) sync(ctx context.Context) error {
	// Everything except the reindex, which carries its own budget below. 50s was
	// too tight once transcripts reached several megabytes: parsing them is what
	// session ingest spends its time on, and a cancelled syncCtx cut that short
	// without surfacing anything - sync exited 0 having emitted nothing, for 46
	// hours, while the files kept growing.
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	coreErr := fsx.WithLock(a.Paths.SyncLock, func() error {
		state, err := config.LoadState(a.Paths)
		if err != nil {
			return err
		}
		if err := a.verifyControlCheckout(syncCtx, state.RepoURL); err != nil {
			return err
		}
		identity, err := config.LoadIdentity(a.Paths, a.Now)
		if err != nil {
			return err
		}
		if expected := "queue/" + identity.ID; state.QueueBranch != expected {
			return fmt.Errorf("configured queue %q does not match machine identity %q", state.QueueBranch, identity.ID)
		}
		var errs []error
		if err := a.fetchEndpointRefs(syncCtx, state); err != nil {
			errs = append(errs, err)
			return errors.Join(errs...)
		}
		// Per-session transcript ingest is the single intake path: fold a bounded
		// idle-complete ingest in before the queue drain so live sessions land in
		// the outbox per-session with no per-turn hooks. Non-fatal: still drain and
		// publish whatever is already queued.
		if err := a.ingestForSync(identity); err != nil {
			errs = append(errs, fmt.Errorf("session ingest: %w", err))
		}
		if err := a.syncQueueUnlocked(syncCtx, state); err != nil {
			errs = append(errs, fmt.Errorf("queue sync: %w", err))
		}
		sharedReady := true
		if err := a.syncSharedUnlocked(syncCtx); err != nil {
			errs = append(errs, fmt.Errorf("shared sync: %w", err))
			sharedReady = false
		}
		if sharedReady {
			// Reindex gets its OWN budget, derived from the caller's context
			// rather than syncCtx. Embedding the whole product is the long pole
			// by two orders of magnitude - measured at 399s for 291 notes, against
			// a 50s sync - and sharing the budget made that unrecoverable: the
			// reindex was killed every run, so its receipt was never written, so
			// the next run tried the same doomed reindex again. The local recall
			// mirror silently stopped advancing while sync kept exiting 0.
			//
			// It is also the one step that does not need to finish inside a sync:
			// the receipt records which shared SHA is indexed, so a reindex that
			// spans several scheduler ticks is simply skipped by the next one (the
			// file lock serialises them), and the mirror catches up when it lands.
			indexCtx, indexCancel := context.WithTimeout(ctx, basicMemoryReindexTimeout)
			if err := a.reindexBasicMemory(indexCtx); err != nil {
				errs = append(errs, fmt.Errorf("Basic Memory reindex: %w", err))
			}
			indexCancel()
		}
		return errors.Join(errs...)
	})
	cancel()
	if ctx.Err() == nil {
		a.repairClientHooks(ctx)
	}
	return coreErr
}

func (a *App) syncSharedUnlocked(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(a.Paths.Shared, ".git")); err != nil {
		return nil
	}
	status, err := proc.Run(ctx, a.Paths.Shared, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("shared worktree is dirty; refusing automatic merge")
	}
	remote, err := proc.Run(ctx, a.Paths.Shared, "git", "rev-parse", "origin/shared")
	if err != nil {
		return err
	}
	ancestor, err := gitx.IsAncestor(ctx, a.Paths.Shared, "HEAD", "origin/shared")
	if err != nil {
		return err
	}
	if ancestor {
		if _, err := proc.Run(ctx, a.Paths.Shared, "git", "merge", "--ff-only", "origin/shared"); err != nil {
			return err
		}
	} else {
		// origin/shared history was rewritten (e.g. a backlog replay reset it to a
		// new orphan), so local shared is ahead of or diverged from it. Shared is
		// product-only and the vault is a disposable mirror, so hard-reset onto
		// origin/shared instead of wedging every future sync (which would freeze
		// reindex and leave recall permanently stale). The dirty guard above still
		// protects any local uncommitted changes from being discarded.
		if _, err := proc.Run(ctx, a.Paths.Shared, "git", "reset", "--hard", "origin/shared"); err != nil {
			return err
		}
	}
	head, err := proc.Run(ctx, a.Paths.Shared, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != strings.TrimSpace(remote) {
		return errors.New("shared worktree did not converge exactly to origin/shared")
	}
	return a.mirrorProductToVault()
}

// mirrorProductToVault lives in vault_mirror.go.
