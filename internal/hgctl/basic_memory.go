package hgctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/proc"
)

func (a *App) setupBasicMemory(ctx context.Context) error {
	if !proc.Exists("basic-memory") {
		return errors.New("basic-memory is not installed")
	}
	state, err := a.loadState()
	if err != nil {
		return err
	}
	project, created, err := a.ensureBasicMemoryProject(ctx, &state)
	if err != nil {
		return fmt.Errorf("Basic Memory project: %w", err)
	}
	ownership := basicMemoryOwnershipForSetup(state.BasicMemoryProject, project, created, a.Paths.Vault)
	state.BasicMemoryProject = &ownership
	if err := a.saveState(state); err != nil {
		return err
	}
	if err := a.reindexBasicMemory(ctx); err != nil {
		return fmt.Errorf("Basic Memory reindex: %w", err)
	}
	return nil
}

// basicMemoryReadOnlyEnv intentionally sets no crippling flags. Permalinks,
// semantic search, and hybrid retrieval stay at Basic Memory's defaults so the
// wikilink graph and semantic recall work. The vault is a disposable copy that
// is decoupled from the git worktree, so permalink frontmatter writes there are
// harmless and never dirty tracked history.
var basicMemoryReadOnlyEnv []string

func (a *App) reindexBasicMemory(ctx context.Context) error {
	head, err := proc.Run(ctx, a.Paths.Shared, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	state, err := a.loadState()
	if err != nil {
		return err
	}
	if state.BasicMemoryProject == nil || state.BasicMemoryProject.ExternalID == "" || state.BasicMemoryProject.Path == "" ||
		fsx.Canonical(state.BasicMemoryProject.Path) != fsx.Canonical(a.Paths.Vault) {
		return errors.New("Basic Memory project identity is not configured")
	}
	projectID := state.BasicMemoryProject.ExternalID
	indexed, indexedErr := a.loadBasicMemoryIndexReceipt()
	if indexedErr == nil &&
		indexed.SharedSHA == head && indexed.ProjectExternalID == projectID {
		return nil
	}
	if errors.Is(indexedErr, fsx.ErrUnsupportedSchema) {
		return indexedErr
	}
	// shared moved but the PRODUCT did not. reflect advances its cursor past a
	// noop slice with an empty commit, and consolidate carries watermark trailers
	// forward the same way; both change the SHA this receipt keys on while every
	// indexed file stays byte-identical. Re-point the receipt and skip the
	// subprocess: `basic-memory reindex` loads a local embedding model on every
	// invocation - measured at ~45s of CPU here - and there is nothing for it to
	// index. Measured 4 of the last 40 shared commits, and a far larger share
	// while a backlog drains, which is exactly when noop slices are common and
	// the machine is least able to spare the cores.
	if indexedErr == nil && indexed.ProjectExternalID == projectID &&
		productUnchangedBetween(ctx, a.Paths.Shared, indexed.SharedSHA, head) {
		return a.saveBasicMemoryIndexReceipt(BasicMemoryIndexReceipt{
			SharedSHA:         head,
			ProjectExternalID: projectID,
		})
	}
	if !proc.Exists("basic-memory") {
		return errors.New("basic-memory is not installed")
	}
	project, err := a.resolveBasicMemoryProject(ctx)
	if err != nil {
		return err
	}
	projectID = project.ExternalID
	if _, err := proc.RunEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "reindex", "--project", ProjectName); err != nil {
		return err
	}
	return a.saveBasicMemoryIndexReceipt(BasicMemoryIndexReceipt{
		SharedSHA:         head,
		ProjectExternalID: projectID,
	})
}

// basicMemoryIndexedCount asks the index how many entries it actually holds.
//
// The receipt proves a reindex once ran to completion for a given shared sha; it
// proves nothing about whether that result is still there. A dropped or reset
// database leaves the receipt untouched, so `doctor` reported a current index
// while recall returned nothing - the exact shape of failure this codebase calls
// worse than no check at all, only inverted.
//
// The permalink wildcard is the one filter that matches the whole corpus without
// depending on any single note's text. A title probe would ride on FTS escaping
// of a title chosen at random from the vault, so a note with a quote or an
// operator in it would fail the probe and make doctor report a healthy index as
// broken.
func basicMemoryIndexedCount(ctx context.Context) (int, error) {
	out, err := proc.RunEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "tool", "search-notes",
		"--permalink", "*", "--project", ProjectName)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Total *int `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return 0, fmt.Errorf("parse index probe response: %w", err)
	}
	if payload.Total == nil {
		return 0, errors.New("index probe response carried no total")
	}
	return *payload.Total, nil
}

type basicMemoryProject struct {
	Name       string `json:"name"`
	ExternalID string `json:"external_id"`
	LocalPath  string `json:"local_path"`
	Path       string `json:"path"`
}

func (p basicMemoryProject) CanonicalPath() string {
	path := p.LocalPath
	if path == "" {
		path = p.Path
	}
	return fsx.Canonical(path)
}

func listBasicMemoryProjects(ctx context.Context) ([]basicMemoryProject, error) {
	out, err := proc.RunEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "tool", "list-projects", "--local")
	if err != nil {
		return nil, err
	}
	var listing struct {
		Projects []basicMemoryProject `json:"projects"`
	}
	if err := json.Unmarshal([]byte(out), &listing); err != nil {
		return nil, err
	}
	return listing.Projects, nil
}

func (a *App) resolveBasicMemoryProject(ctx context.Context) (basicMemoryProject, error) {
	if !proc.Exists("basic-memory") {
		return basicMemoryProject{}, errors.New("basic-memory is not installed")
	}
	state, err := a.loadState()
	if err != nil {
		return basicMemoryProject{}, fmt.Errorf("load Basic Memory identity: %w", err)
	}
	if state.BasicMemoryProject == nil || state.BasicMemoryProject.ExternalID == "" ||
		fsx.Canonical(state.BasicMemoryProject.Path) != fsx.Canonical(a.Paths.Vault) {
		return basicMemoryProject{}, errors.New("Basic Memory project identity is not configured for the shared vault")
	}
	projects, err := listBasicMemoryProjects(ctx)
	if err != nil {
		return basicMemoryProject{}, fmt.Errorf("list Basic Memory projects: %w", err)
	}
	var named []basicMemoryProject
	for _, project := range projects {
		if project.Name == ProjectName {
			named = append(named, project)
		}
	}
	if len(named) != 1 {
		return basicMemoryProject{}, fmt.Errorf("Basic Memory project %q resolved %d times", ProjectName, len(named))
	}
	project := named[0]
	if (project.LocalPath == "" && project.Path == "") || project.ExternalID != state.BasicMemoryProject.ExternalID ||
		project.CanonicalPath() != fsx.Canonical(a.Paths.Vault) {
		return basicMemoryProject{}, errors.New("Basic Memory project identity or shared path changed")
	}
	return project, nil
}

func (a *App) verifyBasicMemoryIndexReceipt(ctx context.Context, project basicMemoryProject) (string, error) {
	head, err := proc.Run(ctx, a.Paths.Shared, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read shared revision: %w", err)
	}
	head = strings.TrimSpace(head)
	receipt, err := a.loadBasicMemoryIndexReceipt()
	if err != nil {
		return "", fmt.Errorf("load Basic Memory index receipt: %w", err)
	}
	if receipt.SharedSHA != head || receipt.ProjectExternalID != project.ExternalID {
		return "", errors.New("Basic Memory index is not current for the shared revision")
	}
	return head, nil
}

func (a *App) ensureBasicMemoryProject(ctx context.Context, state *State) (basicMemoryProject, bool, error) {
	projects, err := listBasicMemoryProjects(ctx)
	if err != nil {
		return basicMemoryProject{}, false, err
	}
	want := fsx.Canonical(a.Paths.Vault)
	for _, project := range projects {
		if project.Name != ProjectName {
			continue
		}
		if project.CanonicalPath() != want {
			return basicMemoryProject{}, false, fmt.Errorf("project %q already points to %s", ProjectName, project.CanonicalPath())
		}
		if project.ExternalID == "" {
			return basicMemoryProject{}, false, fmt.Errorf("project %q has no external identity", ProjectName)
		}
		return project, false, nil
	}
	previous := state.BasicMemoryProject
	state.BasicMemoryProject = &BasicMemoryOwnership{
		Path: fsx.Canonical(a.Paths.Vault), Managed: true, Pending: true,
	}
	if err := a.saveState(*state); err != nil {
		state.BasicMemoryProject = previous
		return basicMemoryProject{}, false, fmt.Errorf("record Basic Memory create intent: %w", err)
	}
	_, err = proc.RunEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "project", "add", ProjectName, a.Paths.Vault, "--local")
	if err != nil {
		return basicMemoryProject{}, false, err
	}
	projects, err = listBasicMemoryProjects(ctx)
	if err != nil {
		return basicMemoryProject{}, false, err
	}
	for _, project := range projects {
		if project.Name == ProjectName && project.ExternalID != "" && project.CanonicalPath() == want {
			return project, true, nil
		}
	}
	return basicMemoryProject{}, false, errors.New("Basic Memory did not return the created project identity")
}

func basicMemoryOwnershipForSetup(previous *BasicMemoryOwnership, project basicMemoryProject, created bool, vault string) BasicMemoryOwnership {
	managed := created
	if !created && previous != nil && previous.Managed {
		managed = basicMemoryOwnershipMatches(*previous, []basicMemoryProject{project}, vault) ||
			(previous.Pending && fsx.Canonical(previous.Path) == fsx.Canonical(vault))
	}
	return BasicMemoryOwnership{
		ExternalID: project.ExternalID,
		Path:       project.CanonicalPath(),
		Managed:    managed,
		Pending:    false,
	}
}

func (a *App) removeManagedBasicMemoryProject(ctx context.Context) error {
	state, err := a.loadState()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !proc.Exists("basic-memory") || state.BasicMemoryProject == nil || !state.BasicMemoryProject.Managed {
		return nil
	}
	projects, err := listBasicMemoryProjects(ctx)
	if err != nil {
		return fmt.Errorf("Basic Memory project check: %w", err)
	}
	owned := basicMemoryOwnershipMatches(*state.BasicMemoryProject, projects, a.Paths.Vault)
	if !owned && state.BasicMemoryProject.Pending && fsx.Canonical(state.BasicMemoryProject.Path) == fsx.Canonical(a.Paths.Vault) {
		matches := 0
		for _, project := range projects {
			if project.Name == ProjectName && project.ExternalID != "" && project.CanonicalPath() == fsx.Canonical(a.Paths.Vault) {
				matches++
			}
		}
		owned = matches == 1
	}
	if !owned {
		return nil
	}
	if _, err := proc.RunEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "project", "remove", ProjectName, "--local"); err != nil {
		return fmt.Errorf("Basic Memory project remove: %w", err)
	}
	state.BasicMemoryProject = nil
	if err := a.saveState(state); err != nil {
		return fmt.Errorf("clear Basic Memory ownership: %w", err)
	}
	if err := os.Remove(a.Paths.IndexedSHA); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Basic Memory index receipt: %w", err)
	}
	return nil
}

func basicMemoryOwnershipMatches(ownership BasicMemoryOwnership, projects []basicMemoryProject, expectedPath string) bool {
	want := fsx.Canonical(expectedPath)
	if ownership.ExternalID == "" || fsx.Canonical(ownership.Path) != want {
		return false
	}
	for _, project := range projects {
		if project.Name == ProjectName && project.ExternalID == ownership.ExternalID && project.CanonicalPath() == want {
			return true
		}
	}
	return false
}
