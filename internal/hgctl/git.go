package hgctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	if err := a.fetchEndpointRefs(ctx, state); err != nil {
		return err
	}
	if !gitRefExists(ctx, a.Paths.Control, "refs/remotes/origin/shared") {
		return errors.New("remote branch shared does not exist")
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
	return "origin/main"
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
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10")
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		return false, nil
	}
	return false, fmt.Errorf("git ls-remote %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
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
		a.retryCodexTrust(ctx)
	}
	return coreErr
}

func (a *App) syncQueueUnlocked(ctx context.Context, state State) error {
	if err := cleanupQueueTemps(a.Paths.Queue); err != nil {
		return err
	}
	recovered, err := a.recoverInterruptedQueueBatch(ctx)
	if err != nil {
		return err
	}
	if err := requireQueueTrackedClean(ctx, a.Paths.Queue); err != nil {
		return err
	}
	if gitRefExists(ctx, a.Paths.Queue, "refs/remotes/origin/"+state.QueueBranch) {
		if _, err := runCommand(ctx, a.Paths.Queue, "git", "merge", "--ff-only", "origin/"+state.QueueBranch); err != nil {
			return err
		}
	}
	batch := recovered
	if len(batch.EventPaths) == 0 {
		batch, err = a.copyOutboxToQueue()
		if err != nil {
			return err
		}
	}
	if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, batch.EventPaths, false); err != nil {
		return err
	}
	if len(batch.EventPaths) > 0 {
		args := append([]string{"add", "--"}, batch.EventPaths...)
		if _, err := runCommand(ctx, a.Paths.Queue, "git", args...); err != nil {
			return err
		}
		if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, batch.EventPaths, true); err != nil {
			return err
		}
		staged, err := gitHasStagedChanges(ctx, a.Paths.Queue)
		if err != nil {
			return err
		}
		if staged {
			message := fmt.Sprintf("queue(%s): capture %d event(s)", shortMachine(state.QueueBranch), len(batch.EventPaths))
			if _, err := runCommand(ctx, a.Paths.Queue, "git", "commit", "-m", message); err != nil {
				return err
			}
		}
	}
	if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, nil, false); err != nil {
		return err
	}
	needsPush, err := queueNeedsPush(ctx, a.Paths.Queue, state.QueueBranch)
	if err != nil {
		return err
	}
	if needsPush {
		if _, err := runCommand(ctx, a.Paths.Queue, "git", "push", "origin", "HEAD:refs/heads/"+state.QueueBranch); err != nil {
			return err
		}
	}
	if err := a.markDelivered(batch.OutboxPaths); err != nil {
		return err
	}
	for _, path := range batch.OutboxPaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if needsPush && len(batch.OutboxPaths) > 0 {
		if err := notifyDream(ctx, state.RepoURL); err != nil {
			_, _ = fmt.Fprintln(a.Err, "hgctl: Dream notification deferred:", err)
		}
	}
	return nil
}

func notifyDream(ctx context.Context, remote string) error {
	repo, ok := githubRepoSlug(remote)
	if !ok || !commandExists("gh") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := runCommand(ctx, "", "gh", "api", "--method", "POST", "repos/"+repo+"/dispatches", "-f", "event_type=hourglass_queue")
	return err
}

func githubRepoSlug(remote string) (string, bool) {
	remote = strings.TrimSpace(strings.TrimSuffix(remote, ".git"))
	var path string
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		path = strings.TrimPrefix(remote, "git@github.com:")
	case strings.HasPrefix(remote, "ssh://git@github.com/"):
		path = strings.TrimPrefix(remote, "ssh://git@github.com/")
	case strings.HasPrefix(remote, "https://github.com/"):
		path = strings.TrimPrefix(remote, "https://github.com/")
	default:
		return "", false
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[1] == "." || parts[0] == ".." || parts[1] == ".." {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func shortMachine(branch string) string {
	id := strings.TrimPrefix(branch, "queue/")
	if len(id) > 8 {
		return id[:8]
	}
	return id
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

type queueBatch struct {
	OutboxPaths []string
	EventPaths  []string
}

func (a *App) copyOutboxToQueue() (queueBatch, error) {
	identity, err := a.loadIdentity()
	if err != nil {
		return queueBatch{}, err
	}
	entries, err := os.ReadDir(a.Paths.Outbox)
	if err != nil {
		return queueBatch{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var batch queueBatch
	queuedBytes := 0
	for _, entry := range entries {
		if len(batch.OutboxPaths) >= MaxSyncEvents {
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		sourcePath := filepath.Join(a.Paths.Outbox, entry.Name())
		content, err := readOutboxFile(sourcePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if quarantineErr := a.quarantineOutbox(sourcePath, err); quarantineErr != nil {
				return batch, quarantineErr
			}
			continue
		}
		event, canonical, err := decodeCanonicalOutboxEvent(content, entry.Name(), identity.ID)
		if err != nil {
			if quarantineErr := a.quarantineOutbox(sourcePath, err); quarantineErr != nil {
				return batch, quarantineErr
			}
			continue
		}
		if a.eventDelivered(event.ID) {
			batch.OutboxPaths = append(batch.OutboxPaths, sourcePath)
			continue
		}
		if queuedBytes > 0 && queuedBytes+len(canonical) > MaxSyncBytes {
			break
		}
		relTarget := filepath.Join("events", event.CapturedAt.UTC().Format("2006"), event.CapturedAt.UTC().Format("01"), entry.Name())
		target := filepath.Join(a.Paths.Queue, relTarget)
		if err := ensureQueueTargetDirectory(a.Paths.Queue, filepath.Dir(relTarget)); err != nil {
			return batch, err
		}
		info, statErr := os.Lstat(target)
		if statErr == nil && !info.Mode().IsRegular() {
			return batch, fmt.Errorf("queue event target is not a regular file: %s", target)
		}
		if existing, err := os.ReadFile(target); err == nil {
			if !bytes.Equal(existing, canonical) {
				return batch, fmt.Errorf("queue event collision: %s", target)
			}
		} else if !errors.Is(err, os.ErrNotExist) || (statErr != nil && !errors.Is(statErr, os.ErrNotExist)) {
			return batch, err
		} else if err := writeFileAtomic(target, canonical, 0o600); err != nil {
			return batch, err
		}
		batch.OutboxPaths = append(batch.OutboxPaths, sourcePath)
		batch.EventPaths = append(batch.EventPaths, filepath.ToSlash(relTarget))
		queuedBytes += len(canonical)
	}
	return batch, nil
}

func ensureQueueTargetDirectory(queue, relative string) error {
	current := queue
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid queue event directory %q", relative)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("queue event directory is not owned storage: %s", current)
		}
	}
	return nil
}

func readOutboxFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("outbox entry is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, MaxEventBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > MaxEventBytes {
		return nil, fmt.Errorf("event exceeds %d bytes", MaxEventBytes)
	}
	return content, nil
}

func (a *App) quarantineOutbox(path string, reason error) error {
	if err := os.MkdirAll(a.Paths.Quarantine, 0o700); err != nil {
		return err
	}
	target := filepath.Join(a.Paths.Quarantine, filepath.Base(path))
	if _, err := os.Stat(target); err == nil {
		target += fmt.Sprintf(".%d", a.Now().UnixNano())
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, target); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(a.Err, "hgctl: quarantined %s: %v\n", filepath.Base(path), reason)
	return nil
}

func gitHasStagedChanges(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet", "--exit-code")
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

func cleanupQueueTemps(queue string) error {
	root := filepath.Join(queue, "events")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".hgctl-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular queue temp %s", path)
		}
		return os.Remove(path)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func requireQueueTrackedClean(ctx context.Context, queue string) error {
	status, err := runCommand(ctx, queue, "git", "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("queue has staged or tracked worktree changes")
	}
	return nil
}

func (a *App) recoverInterruptedQueueBatch(ctx context.Context) (queueBatch, error) {
	status, err := runCommand(ctx, a.Paths.Queue, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return queueBatch{}, err
	}
	if status == "" {
		return queueBatch{}, nil
	}
	identity, err := a.loadIdentity()
	if err != nil {
		return queueBatch{}, err
	}
	var batch queueBatch
	var staged []string
	total := 0
	for _, record := range strings.Split(status, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return queueBatch{}, errors.New("queue has an unrecognized Git status entry")
		}
		code := record[:2]
		if code != "A " && code != "??" {
			return queueBatch{}, errors.New("queue has staged or tracked worktree changes")
		}
		rel := filepath.ToSlash(record[3:])
		clean := filepath.ToSlash(filepath.Clean(rel))
		if clean != rel || !strings.HasPrefix(clean, "events/") {
			return queueBatch{}, fmt.Errorf("queue has unexpected change %q at %s", code, rel)
		}
		base := filepath.Base(clean)
		digest := strings.TrimSuffix(base, ".json")
		if base == digest || !validLowerHex(digest, sha256.Size*2) {
			return queueBatch{}, fmt.Errorf("queue has unexpected change %q at %s", code, rel)
		}
		outboxPath := filepath.Join(a.Paths.Outbox, base)
		outbox, err := readOutboxFile(outboxPath)
		if err != nil {
			return queueBatch{}, fmt.Errorf("staged queue event has no matching outbox entry: %s", rel)
		}
		event, canonical, err := decodeCanonicalOutboxEvent(outbox, base, identity.ID)
		if err != nil {
			return queueBatch{}, fmt.Errorf("staged queue event has an invalid outbox entry: %s", rel)
		}
		expected := filepath.ToSlash(filepath.Join("events", event.CapturedAt.UTC().Format("2006"), event.CapturedAt.UTC().Format("01"), base))
		queued, err := readOutboxFile(filepath.Join(a.Paths.Queue, filepath.FromSlash(clean)))
		if err != nil || clean != expected || !bytes.Equal(queued, canonical) {
			return queueBatch{}, fmt.Errorf("staged queue event does not match its outbox entry: %s", rel)
		}
		batch.OutboxPaths = append(batch.OutboxPaths, outboxPath)
		batch.EventPaths = append(batch.EventPaths, clean)
		if code == "A " {
			staged = append(staged, clean)
		}
		total += len(canonical)
	}
	if len(batch.EventPaths) > MaxSyncEvents || total > MaxSyncBytes {
		return queueBatch{}, errors.New("interrupted queue stage exceeds endpoint commit limits")
	}
	if len(staged) > 0 {
		args := append([]string{"restore", "--staged", "--"}, staged...)
		if _, err := runCommand(ctx, a.Paths.Queue, "git", args...); err != nil {
			return queueBatch{}, err
		}
	}
	return batch, nil
}

func requireOnlyQueueTargets(ctx context.Context, queue string, targets []string, staged bool) error {
	allowed := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		clean := filepath.ToSlash(filepath.Clean(target))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || !strings.HasPrefix(clean, "events/") {
			return fmt.Errorf("invalid queue target %q", target)
		}
		allowed[clean] = struct{}{}
	}
	status, err := runCommand(ctx, queue, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	for _, record := range strings.Split(status, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return errors.New("queue has an unrecognized Git status entry")
		}
		code := record[:2]
		path := filepath.ToSlash(record[3:])
		_, expected := allowed[path]
		if !expected || (!staged && code != "??") || (staged && code != "A ") {
			return fmt.Errorf("queue has unexpected change %q at %s", code, path)
		}
	}
	return nil
}

func queueNeedsPush(ctx context.Context, queue, branch string) (bool, error) {
	remote := "refs/remotes/origin/" + branch
	if !gitRefExists(ctx, queue, remote) {
		return true, nil
	}
	headSHA, err := runCommand(ctx, queue, "git", "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	remoteSHA, err := runCommand(ctx, queue, "git", "rev-parse", remote)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(headSHA) == strings.TrimSpace(remoteSHA) {
		return false, nil
	}
	ancestor, err := gitIsAncestor(ctx, queue, remote, "HEAD")
	if err != nil {
		return false, err
	}
	if !ancestor {
		return false, errors.New("queue branch diverged from its remote")
	}
	return true, nil
}

func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git",
		"-c", "core.hooksPath=/dev/null",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"merge-base", "--is-ancestor", ancestor, descendant,
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w: %s", ancestor, descendant, err, strings.TrimSpace(string(out)))
}

func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func withFileLockWait(ctx context.Context, path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for lock %s: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
