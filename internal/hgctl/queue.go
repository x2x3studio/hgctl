package hgctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/gitx"
	"github.com/x2x3studio/hgctl/internal/proc"

	"github.com/x2x3studio/hgctl/internal/config"
)

func (a *App) syncQueueUnlocked(ctx context.Context, state config.State) error {
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
	if gitx.RefExists(ctx, a.Paths.Queue, "refs/remotes/origin/"+state.QueueBranch) {
		if err := reconcileQueueWithRemote(ctx, a.Paths.Queue, state.QueueBranch); err != nil {
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
		if _, err := proc.Run(ctx, a.Paths.Queue, "git", args...); err != nil {
			return err
		}
		if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, batch.EventPaths, true); err != nil {
			return err
		}
		staged, err := gitx.HasStagedChanges(ctx, a.Paths.Queue)
		if err != nil {
			return err
		}
		if staged {
			message := fmt.Sprintf("queue(%s): capture %d event(s)", shortMachine(state.QueueBranch), len(batch.EventPaths))
			if _, err := proc.Run(ctx, a.Paths.Queue, "git", "commit", "-m", message); err != nil {
				return err
			}
		}
	}
	// Identify the machine on its own branch. Upserted, so this commits only
	// when the metadata actually changed - a hostname edit, an OS upgrade, an
	// hgctl release - rather than on every scheduler tick.
	if id, err := config.LoadIdentity(a.Paths, a.Now); err == nil {
		staged, err := a.upsertMachineMeta(ctx, id)
		if err != nil {
			return err
		}
		if staged {
			if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, []string{machineMetaFile}, true); err != nil {
				return err
			}
			message := fmt.Sprintf("queue(%s): record machine metadata", shortMachine(state.QueueBranch))
			if _, err := proc.Run(ctx, a.Paths.Queue, "git", "commit", "-m", message); err != nil {
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
		if _, err := proc.Run(ctx, a.Paths.Queue, "git", "push", "origin", "HEAD:refs/heads/"+state.QueueBranch); err != nil {
			return err
		}
	}
	for _, path := range batch.OutboxPaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// reconcileQueueWithRemote fast-forwards the local queue branch onto its remote
// before new events are staged. The COMMON path (this machine was offline and
// made no local commits) is a clean fast-forward over the reflect Action's
// archive commits, which move consumed events/ into archive/<YYYY-MM>/. Those
// arrive as clean committed files, so the events/-scoped queue guards do not see
// them.
//
// The ONE divergence case is a local committed-but-unpushed append while origin
// advanced via an Action archive: the two branches share no fast-forward. This
// mirrors the shared self-heal (see syncSharedUnlocked): hard-reset onto the
// remote to adopt the archive commits, then let the caller's copyOutboxToQueue
// replay the un-pushed events from the OUTBOX, which is retained until a
// successful push - so no captured event is lost. Unlike shared (which is
// product-only and never legitimately ahead), a queue branch that is strictly
// AHEAD of its remote is a normal not-yet-pushed append; that is left untouched
// so the subsequent push publishes it rather than discarding it.
func reconcileQueueWithRemote(ctx context.Context, queue, branch string) error {
	remote := "origin/" + branch
	behind, err := gitx.IsAncestor(ctx, queue, "HEAD", remote)
	if err != nil {
		return err
	}
	if behind {
		// HEAD is an ancestor of the remote: a clean fast-forward (or already equal).
		_, err := proc.Run(ctx, queue, "git", "merge", "--ff-only", remote)
		return err
	}
	ahead, err := gitx.IsAncestor(ctx, queue, remote, "HEAD")
	if err != nil {
		return err
	}
	if ahead {
		// HEAD holds an un-pushed local append and the remote is contained in it;
		// leave it so the later push publishes the append.
		return nil
	}
	// Diverged: origin advanced via an Action archive while a local append was
	// committed but not pushed. Adopt the remote; the outbox replay re-stages the
	// un-pushed events on top.
	_, err = proc.Run(ctx, queue, "git", "reset", "--hard", remote)
	return err
}

func shortMachine(branch string) string {
	id := strings.TrimPrefix(branch, "queue/")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

type queueBatch struct {
	OutboxPaths []string
	EventPaths  []string
}

// copyOutboxToQueue moves up to a bounded steady-state batch of raw outbox events
// into the queue worktree. The MaxSyncEvents/MaxSyncBytes bounds exist only to
// keep a single commit and push finite - see protocol.go for why they are not a
// throttle. The operator-invoked bulk import removes them entirely via
// copyOutboxBatch (see drainOutboxToQueue).
func (a *App) copyOutboxToQueue() (queueBatch, error) {
	return a.copyOutboxBatch(MaxSyncEvents, MaxSyncBytes)
}

// copyOutboxBatch moves outbox events into the queue worktree under events/,
// oldest first (filenames are timestamp-ordered). Events are opaque Markdown;
// there is no decode, canonicalization, admission, or quarantine. maxEvents and
// maxBytes cap the batch; a non-positive bound disables that cap. The per-event
// MaxEventBytes ceiling always applies.
func (a *App) copyOutboxBatch(maxEvents, maxBytes int) (queueBatch, error) {
	entries, err := os.ReadDir(a.Paths.Outbox)
	if err != nil {
		return queueBatch{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var batch queueBatch
	selectedBytes := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		sourcePath := filepath.Join(a.Paths.Outbox, entry.Name())
		content, err := readOutboxFile(sourcePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return queueBatch{}, err
		}
		if (maxEvents > 0 && len(batch.EventPaths) == maxEvents) || (maxBytes > 0 && selectedBytes > 0 && selectedBytes+len(content) > maxBytes) {
			break
		}
		rel := filepath.ToSlash(filepath.Join("events", entry.Name()))
		if err := ensureQueueTargetDirectory(a.Paths.Queue, "events"); err != nil {
			return batch, err
		}
		target := filepath.Join(a.Paths.Queue, filepath.FromSlash(rel))
		info, statErr := os.Lstat(target)
		if statErr == nil && !info.Mode().IsRegular() {
			return batch, fmt.Errorf("queue event target is not a regular file: %s", target)
		}
		if existing, err := os.ReadFile(target); err == nil {
			if !bytes.Equal(existing, content) {
				return batch, fmt.Errorf("queue event collision: %s", target)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return batch, err
		} else if err := fsx.WriteAtomic(target, content, 0o600); err != nil {
			return batch, err
		}
		batch.OutboxPaths = append(batch.OutboxPaths, sourcePath)
		batch.EventPaths = append(batch.EventPaths, rel)
		selectedBytes += len(content)
	}
	return batch, nil
}

// bulkQueueCommitChunk bounds how many events land in one ingest commit so the
// git argument list stays sane for very large historical backlogs; the whole set
// is still published to origin in a single push.
const bulkQueueCommitChunk = 500

// bulkPublishQueue drains the entire outbox to the machine queue branch in large
// chronological commits and pushes once, so an operator-invoked historical import
// lands on origin/queue/<machine> before the command returns. It reuses every
// steady-state queue guard (orphan/append-only, events/ path, per-event byte
// ceiling) but deliberately bypasses the MaxSyncEvents/MaxSyncBytes capture bounds
// that keep automatic Stop-hook syncs small. It waits for, rather than skips, a
// held sync lock so the operator's import is never silently dropped.
func (a *App) bulkPublishQueue(ctx context.Context) (int, error) {
	state, err := config.LoadState(a.Paths)
	if err != nil {
		return 0, err
	}
	identity, err := config.LoadIdentity(a.Paths, a.Now)
	if err != nil {
		return 0, err
	}
	if expected := "queue/" + identity.ID; state.QueueBranch != expected {
		return 0, fmt.Errorf("configured queue %q does not match machine identity %q", state.QueueBranch, identity.ID)
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var delivered int
	err = fsx.WithLockWait(lockCtx, a.Paths.SyncLock, func() error {
		if err := a.verifyControlCheckout(lockCtx, state.RepoURL); err != nil {
			return err
		}
		if err := a.fetchEndpointRefs(lockCtx, state); err != nil {
			return err
		}
		delivered, err = a.drainOutboxToQueue(lockCtx, state)
		return err
	})
	return delivered, err
}

// drainOutboxToQueue commits every outbox event to the queue worktree in
// bulkQueueCommitChunk-sized commits and pushes once. It mirrors the proven
// syncQueueUnlocked preamble (temp cleanup, interrupted-batch recovery, clean
// check, fast-forward) and per-batch guards, differing only in that it loops
// until the outbox is empty instead of stopping at one bounded batch.
func (a *App) drainOutboxToQueue(ctx context.Context, state config.State) (int, error) {
	if err := cleanupQueueTemps(a.Paths.Queue); err != nil {
		return 0, err
	}
	recovered, err := a.recoverInterruptedQueueBatch(ctx)
	if err != nil {
		return 0, err
	}
	if err := requireQueueTrackedClean(ctx, a.Paths.Queue); err != nil {
		return 0, err
	}
	if gitx.RefExists(ctx, a.Paths.Queue, "refs/remotes/origin/"+state.QueueBranch) {
		if err := reconcileQueueWithRemote(ctx, a.Paths.Queue, state.QueueBranch); err != nil {
			return 0, err
		}
	}
	delivered := 0
	commit := func(batch queueBatch) error {
		if len(batch.EventPaths) == 0 {
			return nil
		}
		if err := a.stageAndCommitQueueBatch(ctx, state, batch); err != nil {
			return err
		}
		if err := removeOutboxPaths(batch.OutboxPaths); err != nil {
			return err
		}
		delivered += len(batch.EventPaths)
		return nil
	}
	// Fold any interrupted steady-state batch first; recoverInterruptedQueueBatch
	// already verified its worktree files against the outbox.
	if err := commit(recovered); err != nil {
		return delivered, err
	}
	for {
		batch, err := a.copyOutboxBatch(bulkQueueCommitChunk, 0)
		if err != nil {
			return delivered, err
		}
		if len(batch.EventPaths) == 0 {
			break
		}
		if err := commit(batch); err != nil {
			return delivered, err
		}
	}
	if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, nil, false); err != nil {
		return delivered, err
	}
	needsPush, err := queueNeedsPush(ctx, a.Paths.Queue, state.QueueBranch)
	if err != nil {
		return delivered, err
	}
	if needsPush {
		if _, err := proc.Run(ctx, a.Paths.Queue, "git", "push", "origin", "HEAD:refs/heads/"+state.QueueBranch); err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

// stageAndCommitQueueBatch stages and commits one already-copied batch, enforcing
// the events/ path guard before and after staging. A batch of only already-present
// duplicates stages nothing and is skipped.
func (a *App) stageAndCommitQueueBatch(ctx context.Context, state config.State, batch queueBatch) error {
	if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, batch.EventPaths, false); err != nil {
		return err
	}
	args := append([]string{"add", "--"}, batch.EventPaths...)
	if _, err := proc.Run(ctx, a.Paths.Queue, "git", args...); err != nil {
		return err
	}
	if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, batch.EventPaths, true); err != nil {
		return err
	}
	staged, err := gitx.HasStagedChanges(ctx, a.Paths.Queue)
	if err != nil || !staged {
		return err
	}
	message := fmt.Sprintf("queue(%s): ingest %d event(s)", shortMachine(state.QueueBranch), len(batch.EventPaths))
	_, err = proc.Run(ctx, a.Paths.Queue, "git", "commit", "-m", message)
	return err
}

func removeOutboxPaths(paths []string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
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
	status, err := proc.Run(ctx, queue, "git", "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("queue has staged or tracked worktree changes")
	}
	return nil
}

// recoverInterruptedQueueBatch re-derives a partially-staged batch after a crash.
// Events are opaque Markdown under events/; a staged file must byte-match its
// outbox source.
func (a *App) recoverInterruptedQueueBatch(ctx context.Context) (queueBatch, error) {
	status, err := proc.Run(ctx, a.Paths.Queue, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return queueBatch{}, err
	}
	if status == "" {
		return queueBatch{}, nil
	}
	var batch queueBatch
	var staged []string
	var resetMeta bool
	for _, record := range strings.Split(status, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return queueBatch{}, errors.New("queue has an unrecognized Git status entry")
		}
		code := record[:2]
		rel := filepath.ToSlash(record[3:])
		clean := filepath.ToSlash(filepath.Clean(rel))
		if clean == rel && clean == machineMetaFile {
			// Derived state, not evidence: re-deriving it costs nothing, so a
			// partial stage is reverted rather than refused. This path was
			// written when a brand-new event under events/ was the only thing
			// that could ever be staged, so a crash between the `git add` and
			// the `git commit` of a metadata update wedged EVERY later sync -
			// recovery refused the leftover, and recovery runs first.
			resetMeta = true
			continue
		}
		if code != "A " && code != "??" {
			return queueBatch{}, errors.New("queue has staged or tracked worktree changes")
		}
		if clean != rel || !strings.HasPrefix(clean, "events/") || !strings.HasSuffix(clean, ".md") {
			return queueBatch{}, fmt.Errorf("queue has unexpected change %q at %s", code, rel)
		}
		base := filepath.Base(clean)
		outboxPath := filepath.Join(a.Paths.Outbox, base)
		outbox, err := readOutboxFile(outboxPath)
		if err != nil {
			return queueBatch{}, fmt.Errorf("staged queue event has no matching outbox entry: %s", rel)
		}
		queued, err := readOutboxFile(filepath.Join(a.Paths.Queue, filepath.FromSlash(clean)))
		if err != nil || !bytes.Equal(queued, outbox) {
			return queueBatch{}, fmt.Errorf("staged queue event does not match its outbox entry: %s", rel)
		}
		batch.OutboxPaths = append(batch.OutboxPaths, outboxPath)
		batch.EventPaths = append(batch.EventPaths, clean)
		if code == "A " {
			staged = append(staged, clean)
		}
	}
	if resetMeta {
		if err := a.revertQueueMachineMeta(ctx); err != nil {
			return queueBatch{}, err
		}
	}
	if len(staged) > 0 {
		args := append([]string{"restore", "--staged", "--"}, staged...)
		if _, err := proc.Run(ctx, a.Paths.Queue, "git", args...); err != nil {
			return queueBatch{}, err
		}
	}
	// No MaxSyncEvents/MaxSyncBytes cap here: bulk ingest (drainOutboxToQueue)
	// stages up to bulkQueueCommitChunk events per commit, so an interrupted bulk
	// batch legitimately exceeds the steady-state capture bounds. The recovered
	// files are byte-validated against their outbox source and restricted to
	// events/*.md, so the caller commits the whole batch as one local commit.
	return batch, nil
}

func requireOnlyQueueTargets(ctx context.Context, queue string, targets []string, staged bool) error {
	allowed := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		clean := filepath.ToSlash(filepath.Clean(target))
		// events/ plus the single machine metadata file at the root; see
		// machine_meta.go for why that one exception is worth making.
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
			(!strings.HasPrefix(clean, "events/") && clean != machineMetaFile) {
			return fmt.Errorf("invalid queue target %q", target)
		}
		allowed[clean] = struct{}{}
	}
	status, err := proc.Run(ctx, queue, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
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
		if _, expected := allowed[path]; !expected {
			return fmt.Errorf("queue has unexpected change %q at %s", code, path)
		}
		// An event is only ever created, so "A " staged and "??" unstaged is the
		// entire vocabulary for one. The machine metadata file is the sole
		// target that is rewritten IN PLACE - a hostname edit, an OS upgrade, an
		// hgctl release - and so also arrives as a modification. Without this it
		// committed exactly once, when it was created, and every later change
		// failed the guard that exists to let it through.
		ok := code == "A "
		if !staged {
			ok = code == "??"
		}
		if !ok && path == machineMetaFile {
			ok = code == "M "
			if !staged {
				ok = code == " M"
			}
		}
		if !ok {
			return fmt.Errorf("queue has unexpected change %q at %s", code, path)
		}
	}
	return nil
}

func queueNeedsPush(ctx context.Context, queue, branch string) (bool, error) {
	remote := "refs/remotes/origin/" + branch
	if !gitx.RefExists(ctx, queue, remote) {
		return true, nil
	}
	headSHA, err := proc.Run(ctx, queue, "git", "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	remoteSHA, err := proc.Run(ctx, queue, "git", "rev-parse", remote)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(headSHA) == strings.TrimSpace(remoteSHA) {
		return false, nil
	}
	ancestor, err := gitx.IsAncestor(ctx, queue, remote, "HEAD")
	if err != nil {
		return false, err
	}
	if !ancestor {
		return false, errors.New("queue branch diverged from its remote")
	}
	return true, nil
}
