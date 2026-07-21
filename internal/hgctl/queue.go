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
	"time"
)

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
	recovered, err = a.rebuildExpiredUnpublishedQueue(ctx, state, recovered)
	if err != nil {
		return err
	}
	if err := a.pruneExpiredFeedbackOutbox(a.Now().UTC()); err != nil {
		return err
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
	_, err := runCommand(ctx, "", "gh", "api", "--hostname", "github.com", "--method", "POST", "repos/"+repo+"/dispatches", "-f", "event_type=hourglass_queue")
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

type queueBatch struct {
	OutboxPaths []string
	EventPaths  []string
	Schema      string
}

type outboxCandidate struct {
	path  string
	name  string
	event queuedEvent
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
	var selected []outboxCandidate
	selectedBytes := 0
	hasV1 := false
	selectionFull := false
	for _, entry := range entries {
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
				return queueBatch{}, quarantineErr
			}
			continue
		}
		event, err := decodeCanonicalQueuedEvent(content, entry.Name(), identity.ID, a.Now().UTC())
		if err != nil {
			if errors.Is(err, errFeedbackExpired) {
				if removeErr := os.Remove(sourcePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return queueBatch{}, removeErr
				}
				continue
			}
			if quarantineErr := a.quarantineOutbox(sourcePath, err); quarantineErr != nil {
				return queueBatch{}, quarantineErr
			}
			continue
		}
		if a.eventDelivered(event.ID) {
			if err := os.Remove(sourcePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return queueBatch{}, err
			}
			continue
		}
		if event.Schema == Protocol && !hasV1 {
			hasV1 = true
			selected = nil
			selectedBytes = 0
			selectionFull = false
		}
		if (hasV1 && event.Schema != Protocol) || (!hasV1 && event.Schema != FeedbackProtocol) || selectionFull {
			continue
		}
		if len(selected) == MaxSyncEvents || (selectedBytes > 0 && selectedBytes+len(event.Canonical) > MaxSyncBytes) {
			selectionFull = true
			continue
		}
		selected = append(selected, outboxCandidate{path: sourcePath, name: entry.Name(), event: event})
		selectedBytes += len(event.Canonical)
	}
	var batch queueBatch
	for _, candidate := range selected {
		event := candidate.event
		relTarget := filepath.FromSlash(queuedEventPath(event, candidate.name))
		target := filepath.Join(a.Paths.Queue, relTarget)
		if err := ensureQueueTargetDirectory(a.Paths.Queue, filepath.Dir(relTarget)); err != nil {
			return batch, err
		}
		info, statErr := os.Lstat(target)
		if statErr == nil && !info.Mode().IsRegular() {
			return batch, fmt.Errorf("queue event target is not a regular file: %s", target)
		}
		if existing, err := os.ReadFile(target); err == nil {
			if !bytes.Equal(existing, event.Canonical) {
				return batch, fmt.Errorf("queue event collision: %s", target)
			}
		} else if !errors.Is(err, os.ErrNotExist) || (statErr != nil && !errors.Is(statErr, os.ErrNotExist)) {
			return batch, err
		} else if err := writeFileAtomic(target, event.Canonical, 0o600); err != nil {
			return batch, err
		}
		batch.OutboxPaths = append(batch.OutboxPaths, candidate.path)
		batch.EventPaths = append(batch.EventPaths, filepath.ToSlash(relTarget))
		batch.Schema = event.Schema
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
	type recoveryCandidate struct {
		path       string
		outboxPath string
		event      queuedEvent
		expired    bool
	}
	var candidates []recoveryCandidate
	var staged []string
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
		event, expired, err := decodeCanonicalQueuedEventForRecovery(outbox, base, identity.ID, a.Now().UTC())
		if err != nil {
			return queueBatch{}, fmt.Errorf("staged queue event has an invalid outbox entry: %s", rel)
		}
		expected := queuedEventPath(event, base)
		queued, err := readOutboxFile(filepath.Join(a.Paths.Queue, filepath.FromSlash(clean)))
		if err != nil || clean != expected || !bytes.Equal(queued, event.Canonical) {
			return queueBatch{}, fmt.Errorf("staged queue event does not match its outbox entry: %s", rel)
		}
		candidates = append(candidates, recoveryCandidate{
			path: clean, outboxPath: outboxPath, event: event, expired: expired,
		})
		if code == "A " {
			staged = append(staged, clean)
		}
	}
	if len(staged) > 0 {
		args := append([]string{"restore", "--staged", "--"}, staged...)
		if _, err := runCommand(ctx, a.Paths.Queue, "git", args...); err != nil {
			return queueBatch{}, err
		}
	}
	selectedSchema := FeedbackProtocol
	for _, candidate := range candidates {
		if !candidate.expired && candidate.event.Schema == Protocol {
			selectedSchema = Protocol
			break
		}
	}
	var batch queueBatch
	total := 0
	for _, candidate := range candidates {
		if candidate.expired || candidate.event.Schema != selectedSchema {
			if err := os.Remove(filepath.Join(a.Paths.Queue, filepath.FromSlash(candidate.path))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return queueBatch{}, err
			}
			if candidate.expired {
				if err := os.Remove(candidate.outboxPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return queueBatch{}, err
				}
			}
			continue
		}
		batch.OutboxPaths = append(batch.OutboxPaths, candidate.outboxPath)
		batch.EventPaths = append(batch.EventPaths, candidate.path)
		batch.Schema = candidate.event.Schema
		total += len(candidate.event.Canonical)
	}
	if len(batch.EventPaths) > MaxSyncEvents || total > MaxSyncBytes {
		return queueBatch{}, errors.New("interrupted queue stage exceeds endpoint commit limits")
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
