package hgctl

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	repositoryBootstrapWorkflow = "bootstrap.yml"
	repositoryBootstrapTimeout  = 5 * time.Minute
	repositoryBootstrapPoll     = 5 * time.Second
)

type repositoryBootstrapOperations struct {
	branchExists func(context.Context) (bool, error)
	trigger      func(context.Context) error
	wait         func(context.Context, time.Duration) error
}

func (a *App) ensureRepositoryBranches(ctx context.Context, remote string) error {
	bootstrapCtx, cancel := context.WithTimeout(ctx, repositoryBootstrapTimeout)
	defer cancel()
	operations := repositoryBootstrapOperations{
		branchExists: func(checkCtx context.Context) (bool, error) {
			for _, branch := range []string{"shared", "queue-template"} {
				exists, err := remoteBranchExists(checkCtx, a.Paths.Control, branch)
				if err != nil || !exists {
					return false, err
				}
			}
			return true, nil
		},
		trigger: func(triggerCtx context.Context) error {
			return triggerRepositoryBootstrap(triggerCtx, remote)
		},
		wait: waitForRepositoryBootstrap,
	}
	err := ensureRepositoryBranchesWith(bootstrapCtx, operations)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("repository bootstrap did not publish within %s", repositoryBootstrapTimeout)
	}
	return err
}

func ensureRepositoryBranchesWith(ctx context.Context, operations repositoryBootstrapOperations) error {
	if operations.branchExists == nil || operations.trigger == nil || operations.wait == nil {
		return errors.New("repository bootstrap operations are incomplete")
	}
	exists, err := operations.branchExists(ctx)
	if err != nil {
		return fmt.Errorf("check required branches: %w", err)
	}
	if exists {
		return nil
	}
	if err := operations.trigger(ctx); err != nil {
		return fmt.Errorf("trigger trusted repository bootstrap: %w", err)
	}
	for {
		if err := operations.wait(ctx, repositoryBootstrapPoll); err != nil {
			return err
		}
		exists, err = operations.branchExists(ctx)
		if err != nil {
			return fmt.Errorf("poll required branches: %w", err)
		}
		if exists {
			return nil
		}
	}
}

func triggerRepositoryBootstrap(ctx context.Context, remote string) error {
	repository, ok := githubRepoSlug(remote)
	if !ok {
		return errors.New("missing repository branches can be bootstrapped only for a github.com repository")
	}
	if !commandExists("gh") {
		return errors.New("authenticated GitHub CLI is required to bootstrap repository branches")
	}
	if _, err := runCommand(ctx, "", "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		return fmt.Errorf("GitHub CLI authentication is not ready: %w", err)
	}
	if _, err := runCommand(ctx, "", "gh", "workflow", "run", repositoryBootstrapWorkflow, "--repo", "github.com/"+repository); err != nil {
		return fmt.Errorf("dispatch %s from the default branch: %w", repositoryBootstrapWorkflow, err)
	}
	return nil
}

func waitForRepositoryBootstrap(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
