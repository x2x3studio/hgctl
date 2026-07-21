package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	controlManagedKey = "hgctl.managed"
	controlOriginKey  = "hgctl.origin"
)

func (a *App) initGit(ctx context.Context, state State) error {
	if !commandExists("git") {
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
		if _, err := runCommand(ctx, "", "git", "clone", "--branch", "main", "--single-branch", state.RepoURL, a.Paths.Control); err != nil {
			return err
		}
		created = true
	} else if err != nil {
		return err
	}
	if created {
		if _, err := runCommand(ctx, a.Paths.Control, "git", "config", "--local", controlManagedKey, "true"); err != nil {
			return err
		}
		if _, err := runCommand(ctx, a.Paths.Control, "git", "config", "--local", controlOriginKey, state.RepoURL); err != nil {
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
		if _, err := runCommand(ctx, a.Paths.Control, "git", "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	if err := a.ensureRepositoryBranches(ctx, state.RepoURL); err != nil {
		return err
	}
	if err := a.fetchEndpointRefs(ctx, state); err != nil {
		return err
	}
	if !gitRefExists(ctx, a.Paths.Control, "refs/remotes/origin/shared") {
		return errors.New("remote branch shared does not exist")
	}
	if !gitRefExists(ctx, a.Paths.Control, "refs/remotes/origin/queue-template") {
		return errors.New("remote branch queue-template does not exist")
	}
	if err := a.ensureWorktree(ctx, a.Paths.Vault, "shared", "origin/shared"); err != nil {
		return err
	}
	if err := a.ensureWorktree(ctx, a.Paths.Queue, state.QueueBranch, queueStartRef(ctx, a.Paths.Control, state.QueueBranch)); err != nil {
		return err
	}
	return a.syncSharedUnlocked(ctx)
}

func (a *App) verifyControlCheckout(ctx context.Context, repoURL string) error {
	top, err := runCommand(ctx, a.Paths.Control, "git", "rev-parse", "--show-toplevel")
	if err != nil || canonicalPath(strings.TrimSpace(top)) != canonicalPath(a.Paths.Control) {
		return fmt.Errorf("control path is not an owned hgctl checkout: %s", a.Paths.Control)
	}
	managed, err := runCommand(ctx, a.Paths.Control, "git", "config", "--local", "--get", controlManagedKey)
	if err != nil || strings.TrimSpace(managed) != "true" {
		return fmt.Errorf("control checkout has no hgctl ownership marker: %s", a.Paths.Control)
	}
	markedOrigin, err := runCommand(ctx, a.Paths.Control, "git", "config", "--local", "--get", controlOriginKey)
	if err != nil || strings.TrimSpace(markedOrigin) != repoURL {
		return fmt.Errorf("control checkout origin marker does not match %s", repoURL)
	}
	origin, err := runCommand(ctx, a.Paths.Control, "git", "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(origin) != repoURL {
		return fmt.Errorf("control checkout origin does not match %s", repoURL)
	}
	return nil
}

func (a *App) ensureWorktree(ctx context.Context, path, branch, start string) error {
	if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
		controlCommon, err := gitCommonDir(ctx, a.Paths.Control)
		if err != nil {
			return err
		}
		worktreeCommon, err := gitCommonDir(ctx, path)
		if err != nil {
			return err
		}
		if controlCommon != worktreeCommon {
			return fmt.Errorf("worktree %s belongs to another repository", path)
		}
		current, err := runCommand(ctx, path, "git", "branch", "--show-current")
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
	if !gitRefExists(ctx, a.Paths.Control, "refs/heads/"+branch) {
		args := []string{"branch", "--no-track", branch, start}
		if _, err := runCommand(ctx, a.Paths.Control, "git", args...); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, err := runCommand(ctx, a.Paths.Control, "git", "worktree", "add", path, branch)
	return err
}

func gitCommonDir(ctx context.Context, path string) (string, error) {
	out, err := runCommand(ctx, path, "git", "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	return filepath.Clean(common), nil
}

func queueStartRef(ctx context.Context, control, branch string) string {
	remote := "refs/remotes/origin/" + branch
	if gitRefExists(ctx, control, remote) {
		return "origin/" + branch
	}
	return "origin/queue-template"
}

func gitRefExists(ctx context.Context, dir, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}

func (a *App) fetchEndpointRefs(ctx context.Context, state State) error {
	refspecs := []string{
		"+refs/heads/main:refs/remotes/origin/main",
		"+refs/heads/shared:refs/remotes/origin/shared",
		"+refs/heads/queue-template:refs/remotes/origin/queue-template",
	}
	exists := gitRefExists(ctx, a.Paths.Control, "refs/remotes/origin/"+state.QueueBranch)
	if !exists {
		var err error
		exists, err = remoteBranchExists(ctx, a.Paths.Control, state.QueueBranch)
		if err != nil {
			return err
		}
	}
	if exists {
		refspecs = append(refspecs, "+refs/heads/"+state.QueueBranch+":refs/remotes/origin/"+state.QueueBranch)
	}
	args := append([]string{"fetch", "--prune", "origin"}, refspecs...)
	_, err := runCommand(ctx, a.Paths.Control, "git", args...)
	return err
}

func remoteBranchExists(ctx context.Context, dir, branch string) (bool, error) {
	_, err := runCommand(ctx, dir, "git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errCommandOutputLimit) {
		return false, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		return false, nil
	}
	return false, fmt.Errorf("git ls-remote: %w", err)
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

func (a *App) sync(ctx context.Context) error {
	syncCtx, cancel := context.WithTimeout(ctx, 50*time.Second)
	coreErr := withFileLock(a.Paths.SyncLock, func() error {
		state, err := a.loadState()
		if err != nil {
			return err
		}
		if err := a.verifyControlCheckout(syncCtx, state.RepoURL); err != nil {
			return err
		}
		identity, err := a.loadIdentity()
		if err != nil {
			return err
		}
		if expected := "queue/" + identity.ID; state.QueueBranch != expected {
			return fmt.Errorf("configured queue %q does not match machine identity %q", state.QueueBranch, identity.ID)
		}
		var errs []error
		if err := a.prunePending(7 * 24 * time.Hour); err != nil {
			errs = append(errs, fmt.Errorf("prune pending turns: %w", err))
		}
		if err := a.fetchEndpointRefs(syncCtx, state); err != nil {
			errs = append(errs, err)
			return errors.Join(errs...)
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
			if err := a.reindexBasicMemory(syncCtx); err != nil {
				errs = append(errs, fmt.Errorf("Basic Memory reindex: %w", err))
			}
		}
		return errors.Join(errs...)
	})
	cancel()
	if ctx.Err() == nil {
		a.repairClientHooks(ctx)
	}
	return coreErr
}

func (a *App) syncShared(ctx context.Context) error {
	state, err := a.loadState()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return withFileLock(a.Paths.SyncLock, func() error {
		if err := a.verifyControlCheckout(ctx, state.RepoURL); err != nil {
			return err
		}
		if _, err := runCommand(ctx, a.Paths.Control, "git", "fetch", "origin", "+refs/heads/shared:refs/remotes/origin/shared"); err != nil {
			return err
		}
		return a.syncSharedUnlocked(ctx)
	})
}

func (a *App) syncSharedUnlocked(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(a.Paths.Vault, ".git")); err != nil {
		return nil
	}
	status, err := runCommand(ctx, a.Paths.Vault, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("shared worktree is dirty; refusing automatic merge")
	}
	remote, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "origin/shared")
	if err != nil {
		return err
	}
	ancestor, err := gitIsAncestor(ctx, a.Paths.Vault, "HEAD", "origin/shared")
	if err != nil {
		return err
	}
	if !ancestor {
		return errors.New("shared worktree is ahead of or diverged from origin/shared")
	}
	if _, err := runCommand(ctx, a.Paths.Vault, "git", "merge", "--ff-only", "origin/shared"); err != nil {
		return err
	}
	head, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) != strings.TrimSpace(remote) {
		return errors.New("shared worktree did not converge exactly to origin/shared")
	}
	return nil
}

func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	_, err := runCommand(ctx, dir, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errCommandOutputLimit) {
		return false, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}
