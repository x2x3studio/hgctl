package hgctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

func (a *App) runRecall(ctx context.Context, args []string) error {
	client, rest, err := extractOption(args, "--client", "")
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(rest, " "))
	if !validEndpointClient(client) || query == "" {
		return errors.New("usage: hgctl recall <query> --client <claude|codex>")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	surface, cards, err := a.lookupRecall(ctx, query, client, "explicit", maxRenderedCardBytes)
	if err != nil {
		return err
	}
	if len(cards) == 0 {
		if _, err := a.assessSurface(ctx, surface.ID, client, "zero_hit", nil); err != nil {
			return fmt.Errorf("queue zero-hit feedback: %w", err)
		}
	}
	_, err = io.WriteString(a.Out, renderRecallSurface(surface, cards, client))
	return err
}

func (a *App) lookupRecall(ctx context.Context, query, client, origin string, contentLimit int) (RecallSurface, []recallCard, error) {
	if !validEndpointClient(client) || (origin != "explicit" && origin != "session_start") || strings.TrimSpace(query) == "" {
		return RecallSurface{}, nil, errors.New("invalid recall lookup")
	}
	var surface RecallSurface
	var cards []recallCard
	err := withFileLockWait(ctx, a.Paths.SyncLock, func() error {
		state, err := a.loadState()
		if err != nil {
			return err
		}
		if err := a.verifyControlCheckout(ctx, state.RepoURL); err != nil {
			return err
		}
		if _, err := runCommand(ctx, a.Paths.Control, "git", "fetch", "origin", "+refs/heads/shared:refs/remotes/origin/shared"); err != nil {
			return fmt.Errorf("refresh shared memory: %w", err)
		}
		if err := a.syncSharedUnlocked(ctx); err != nil {
			return fmt.Errorf("refresh shared memory: %w", err)
		}
		if err := ensureManagedVault(a.Paths.Vault); err != nil {
			return err
		}
		if err := a.reindexBasicMemory(ctx); err != nil {
			return fmt.Errorf("refresh recall index: %w", err)
		}
		project, err := a.requireBasicMemoryIndexReady(ctx)
		if err != nil {
			return fmt.Errorf("recall is not ready: %w", err)
		}
		revision, err := a.currentSharedRevision(ctx)
		if err != nil {
			return err
		}
		indexedCommit, err := a.verifyBasicMemoryIndexReceipt(ctx, project)
		if err != nil || indexedCommit != revision.Commit {
			return errors.New("Basic Memory index does not bind the exact shared commit")
		}
		candidatePaths, err := searchBasicMemoryEntities(ctx, project.ExternalID, query)
		if err != nil {
			return err
		}
		if current, err := a.currentSharedRevision(ctx); err != nil || current != revision {
			return errors.New("shared revision changed during Basic Memory lookup")
		}
		eligible, err := eligibleRecallPaths(candidatePaths)
		if err != nil {
			return err
		}
		blobs, err := resolveTreeBlobs(ctx, a.Paths.Vault, revision.Tree, eligible)
		if err != nil {
			return err
		}
		cards = make([]recallCard, 0, len(eligible))
		for _, name := range eligible {
			blob, exists := blobs[name]
			if !exists {
				return fmt.Errorf("Basic Memory result %q is not a regular blob in shared", name)
			}
			cards = append(cards, recallCard{SurfaceResult: SurfaceResult{Path: name, Blob: blob}})
		}
		surfaceResults := make([]SurfaceResult, len(cards))
		for index := range cards {
			surfaceResults[index] = cards[index].SurfaceResult
		}
		aggregates, err := loadFeedbackAggregates(ctx, a.Paths.Vault, revision.Tree, surfaceResults)
		if err != nil {
			if origin == "explicit" {
				_, _ = fmt.Fprintln(a.Err, "hgctl: feedback rerank disabled:", err)
			}
		} else {
			rerankRecallCards(cards, aggregates)
		}
		for index := range cards {
			cards[index].Rank = index + 1
			content, truncated, err := readGitBlob(ctx, a.Paths.Vault, cards[index].Blob, contentLimit)
			if err != nil {
				return fmt.Errorf("read exact shared card %q: %w", cards[index].Path, err)
			}
			cards[index].Content = content
			cards[index].Truncated = truncated
		}
		if current, err := a.currentSharedRevision(ctx); err != nil || current != revision {
			return errors.New("shared revision changed before recall receipt issuance")
		}
		identity, err := a.loadIdentity()
		if err != nil {
			return err
		}
		results := make([]SurfaceResult, len(cards))
		for index := range cards {
			results[index] = cards[index].SurfaceResult
		}
		surface, err = newRecallSurface(identity, client, origin, revision, results, a.Now().UTC())
		if err != nil {
			return err
		}
		return a.saveSurface(ctx, surface)
	})
	if err != nil {
		return RecallSurface{}, nil, err
	}
	return surface, cards, nil
}

func eligibleRecallPaths(candidatePaths []string) ([]string, error) {
	seen := make(map[string]struct{}, len(candidatePaths))
	eligible := make([]string, 0, len(candidatePaths))
	for _, name := range candidatePaths {
		if name == "Home.md" || name == "Hourglass.canvas" {
			continue
		}
		if !validMemoryPath(name) {
			return nil, fmt.Errorf("Basic Memory returned an invalid Hourglass path %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		eligible = append(eligible, name)
	}
	return eligible, nil
}

func renderRecallSurface(surface RecallSurface, cards []recallCard, client string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "Hourglass surface: %s\nShared commit: %s\n", surface.ID, surface.Shared.Commit)
	if len(cards) == 0 {
		output.WriteString("No verified Hourglass memory matched.\n")
		return output.String()
	}
	for _, card := range cards {
		fmt.Fprintf(&output, "\n[%d] %s (%s)\n", card.Rank, card.Path, card.Blob)
		output.WriteString(card.Content)
		if !strings.HasSuffix(card.Content, "\n") {
			output.WriteByte('\n')
		}
		if card.Truncated {
			output.WriteString("[content truncated by hgctl]\n")
		}
	}
	fmt.Fprintf(&output, "\nRecord consumption with: hgctl feedback %s --client %s --outcome <used|irrelevant|stale|contradicted> --result <rank>\n", surface.ID, client)
	return output.String()
}
