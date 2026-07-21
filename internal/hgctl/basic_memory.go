package hgctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) setupBasicMemory(ctx context.Context) error {
	if !commandExists("basic-memory") {
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

var basicMemoryReadOnlyEnv = []string{
	"BASIC_MEMORY_ENSURE_FRONTMATTER_ON_SYNC=false",
	"BASIC_MEMORY_DISABLE_PERMALINKS=true",
	"BASIC_MEMORY_SEMANTIC_SEARCH_ENABLED=false",
	"BASIC_MEMORY_DEFAULT_SEARCH_TYPE=text",
}

func (a *App) reindexBasicMemory(ctx context.Context) error {
	head, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	head = strings.TrimSpace(head)
	state, err := a.loadState()
	if err != nil {
		return err
	}
	if state.BasicMemoryProject == nil || state.BasicMemoryProject.ExternalID == "" || state.BasicMemoryProject.Path == "" ||
		canonicalPath(state.BasicMemoryProject.Path) != canonicalPath(a.Paths.Vault) {
		return errors.New("Basic Memory project identity is not configured")
	}
	projectID := state.BasicMemoryProject.ExternalID
	indexed, indexedErr := a.loadBasicMemoryIndexReceipt()
	if indexedErr == nil &&
		indexed.SharedSHA == head && indexed.ProjectExternalID == projectID {
		return nil
	}
	if errors.Is(indexedErr, errUnsupportedSchemaVersion) {
		return indexedErr
	}
	if !commandExists("basic-memory") {
		return errors.New("basic-memory is not installed")
	}
	project, err := a.resolveBasicMemoryProject(ctx)
	if err != nil {
		return err
	}
	projectID = project.ExternalID
	status, err := runCommand(ctx, a.Paths.Vault, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("shared worktree is dirty; refusing Basic Memory reindex")
	}
	if _, err := runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "reindex", "--search", "--project", ProjectName); err != nil {
		return err
	}
	status, err = runCommand(ctx, a.Paths.Vault, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("Basic Memory modified the shared worktree; index receipt withheld")
	}
	return a.saveBasicMemoryIndexReceipt(BasicMemoryIndexReceipt{
		SharedSHA:         head,
		ProjectExternalID: projectID,
	})
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
	return canonicalPath(path)
}

func listBasicMemoryProjects(ctx context.Context) ([]basicMemoryProject, error) {
	out, err := runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "tool", "list-projects", "--local")
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
	if !commandExists("basic-memory") {
		return basicMemoryProject{}, errors.New("basic-memory is not installed")
	}
	state, err := a.loadState()
	if err != nil {
		return basicMemoryProject{}, fmt.Errorf("load Basic Memory identity: %w", err)
	}
	if state.BasicMemoryProject == nil || state.BasicMemoryProject.ExternalID == "" ||
		canonicalPath(state.BasicMemoryProject.Path) != canonicalPath(a.Paths.Vault) {
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
		project.CanonicalPath() != canonicalPath(a.Paths.Vault) {
		return basicMemoryProject{}, errors.New("Basic Memory project identity or shared path changed")
	}
	return project, nil
}

func (a *App) verifyBasicMemoryIndexReceipt(ctx context.Context, project basicMemoryProject) (string, error) {
	head, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "HEAD")
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
	want := canonicalPath(a.Paths.Vault)
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
		Path: canonicalPath(a.Paths.Vault), Managed: true, Pending: true,
	}
	if err := a.saveState(*state); err != nil {
		state.BasicMemoryProject = previous
		return basicMemoryProject{}, false, fmt.Errorf("record Basic Memory create intent: %w", err)
	}
	_, err = runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "project", "add", ProjectName, a.Paths.Vault, "--local")
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

func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func basicMemoryOwnershipForSetup(previous *BasicMemoryOwnership, project basicMemoryProject, created bool, vault string) BasicMemoryOwnership {
	managed := created
	if !created && previous != nil && previous.Managed {
		managed = basicMemoryOwnershipMatches(*previous, []basicMemoryProject{project}, vault) ||
			(previous.Pending && canonicalPath(previous.Path) == canonicalPath(vault))
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
	if !commandExists("basic-memory") || state.BasicMemoryProject == nil || !state.BasicMemoryProject.Managed {
		return nil
	}
	projects, err := listBasicMemoryProjects(ctx)
	if err != nil {
		return fmt.Errorf("Basic Memory project check: %w", err)
	}
	owned := basicMemoryOwnershipMatches(*state.BasicMemoryProject, projects, a.Paths.Vault)
	if !owned && state.BasicMemoryProject.Pending && canonicalPath(state.BasicMemoryProject.Path) == canonicalPath(a.Paths.Vault) {
		matches := 0
		for _, project := range projects {
			if project.Name == ProjectName && project.ExternalID != "" && project.CanonicalPath() == canonicalPath(a.Paths.Vault) {
				matches++
			}
		}
		owned = matches == 1
	}
	if !owned {
		return nil
	}
	if _, err := runCommandEnv(ctx, "", basicMemoryReadOnlyEnv, "basic-memory", "project", "remove", ProjectName, "--local"); err != nil {
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
	want := canonicalPath(expectedPath)
	if ownership.ExternalID == "" || canonicalPath(ownership.Path) != want {
		return false
	}
	for _, project := range projects {
		if project.Name == ProjectName && project.ExternalID == ownership.ExternalID && project.CanonicalPath() == want {
			return true
		}
	}
	return false
}
