package hgctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type unpublishedQueueEvent struct {
	outboxPath string
	expired    bool
}

func (a *App) rebuildExpiredUnpublishedQueue(ctx context.Context, state State, recovered queueBatch) (queueBatch, error) {
	base, head, err := unpublishedQueueRange(ctx, a.Paths.Queue, state.QueueBranch)
	if err != nil || base == head {
		return recovered, err
	}
	events, hasExpired, err := a.inspectUnpublishedQueue(ctx, base, head)
	if err != nil || !hasExpired {
		return recovered, err
	}
	if err := requireOnlyQueueTargets(ctx, a.Paths.Queue, recovered.EventPaths, false); err != nil {
		return queueBatch{}, err
	}
	for _, name := range recovered.EventPaths {
		if err := os.Remove(filepath.Join(a.Paths.Queue, filepath.FromSlash(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return queueBatch{}, err
		}
	}
	if _, err := runCommand(ctx, a.Paths.Queue, "git", "reset", "--hard", base); err != nil {
		return queueBatch{}, fmt.Errorf("rebuild unpublished queue commits: %w", err)
	}
	current, err := runCommand(ctx, a.Paths.Queue, "git", "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(current) != base {
		return queueBatch{}, errors.New("unpublished queue rewind did not reach its verified base")
	}
	for _, event := range events {
		if !event.expired {
			continue
		}
		if err := os.Remove(event.outboxPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return queueBatch{}, err
		}
	}
	return queueBatch{}, nil
}

func unpublishedQueueRange(ctx context.Context, queue, branch string) (string, string, error) {
	headText, err := runCommand(ctx, queue, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	head := strings.TrimSpace(headText)
	remote := "refs/remotes/origin/" + branch
	var base string
	if gitRefExists(ctx, queue, remote) {
		baseText, err := runCommand(ctx, queue, "git", "rev-parse", remote)
		if err != nil {
			return "", "", err
		}
		base = strings.TrimSpace(baseText)
		ancestor, err := gitIsAncestor(ctx, queue, base, head)
		if err != nil || !ancestor {
			return "", "", errors.New("local queue does not append to its remote before recovery")
		}
	} else {
		baseText, err := runCommand(ctx, queue, "git", "merge-base", "HEAD", "origin/main")
		if err != nil {
			return "", "", err
		}
		base = strings.TrimSpace(baseText)
	}
	if !validObjectID(base) || !validObjectID(head) {
		return "", "", errors.New("queue recovery range is not a full Git object id")
	}
	return base, head, nil
}

func (a *App) inspectUnpublishedQueue(ctx context.Context, base, head string) ([]unpublishedQueueEvent, bool, error) {
	commitsText, err := runCommand(ctx, a.Paths.Queue, "git", "rev-list", "--first-parent", "--reverse", base+".."+head)
	if err != nil {
		return nil, false, err
	}
	commits := strings.Fields(commitsText)
	previous := base
	var events []unpublishedQueueEvent
	hasExpired := false
	identity, err := a.loadIdentity()
	if err != nil {
		return nil, false, err
	}
	for _, commit := range commits {
		parentsText, err := runCommand(ctx, a.Paths.Queue, "git", "rev-list", "--parents", "-n", "1", commit)
		if err != nil {
			return nil, false, err
		}
		parents := strings.Fields(parentsText)
		if len(parents) != 2 || parents[0] != commit || parents[1] != previous {
			return nil, false, errors.New("unpublished queue history is not a linear first-parent append")
		}
		names, err := addedPathsInCommit(ctx, a.Paths.Queue, previous, commit)
		if err != nil || len(names) == 0 || len(names) > MaxSyncEvents {
			return nil, false, errors.New("unpublished queue commit has invalid event additions")
		}
		commitBytes := 0
		for _, name := range names {
			baseName := filepath.Base(name)
			outboxPath := filepath.Join(a.Paths.Outbox, baseName)
			outbox, err := readOutboxFile(outboxPath)
			if err != nil {
				return nil, false, fmt.Errorf("unpublished queue event has no outbox twin: %s", name)
			}
			event, canonical, expired, err := decodeCanonicalEventForRecovery(outbox, baseName, identity.ID, a.Now().UTC())
			if err != nil || queuedEventPath(event, baseName) != name {
				return nil, false, fmt.Errorf("unpublished queue event has an invalid outbox twin: %s", name)
			}
			committed, err := runCommand(ctx, a.Paths.Queue, "git", "show", commit+":"+name)
			if err != nil || !bytes.Equal([]byte(committed), canonical) {
				return nil, false, fmt.Errorf("unpublished queue event differs from its outbox twin: %s", name)
			}
			commitBytes += len(canonical)
			if commitBytes > MaxSyncBytes {
				return nil, false, errors.New("unpublished queue commit exceeds its byte limit")
			}
			events = append(events, unpublishedQueueEvent{outboxPath: outboxPath, expired: expired})
			hasExpired = hasExpired || expired
		}
		previous = commit
	}
	return events, hasExpired, nil
}

func addedPathsInCommit(ctx context.Context, repository, parent, commit string) ([]string, error) {
	output, err := runCommand(ctx, repository, "git", "diff-tree", "--no-commit-id", "--name-status", "-z", "-r", parent, commit, "--")
	if err != nil {
		return nil, err
	}
	records := strings.Split(output, "\x00")
	var names []string
	for index := 0; index < len(records); {
		if records[index] == "" {
			index++
			continue
		}
		status := records[index]
		index++
		if status != "A" || index >= len(records) || records[index] == "" {
			return nil, errors.New("unpublished queue commit changes a non-event path")
		}
		name := filepath.ToSlash(filepath.Clean(records[index]))
		index++
		if name != records[index-1] || !strings.HasPrefix(name, "events/") {
			return nil, errors.New("unpublished queue commit has an invalid event path")
		}
		names = append(names, name)
	}
	return names, nil
}

func queuedEventPath(event Event, filename string) string {
	return filepath.ToSlash(filepath.Join(
		"events", event.CapturedAt.UTC().Format("2006"), event.CapturedAt.UTC().Format("01"), filename,
	))
}
