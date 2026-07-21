package hgctl

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	sharedBootstrapWorkflow = "bootstrap.yml"
	sharedBootstrapTimeout  = 5 * time.Minute
	sharedBootstrapPoll     = 5 * time.Second
)

type sharedBootstrapOperations struct {
	branchExists func(context.Context) (bool, error)
	trigger      func(context.Context) error
	wait         func(context.Context, time.Duration) error
}

func (a *App) ensureSharedBranch(ctx context.Context, remote string) error {
	bootstrapCtx, cancel := context.WithTimeout(ctx, sharedBootstrapTimeout)
	defer cancel()
	operations := sharedBootstrapOperations{
		branchExists: func(checkCtx context.Context) (bool, error) {
			return remoteBranchExists(checkCtx, a.Paths.Control, "shared")
		},
		trigger: func(triggerCtx context.Context) error {
			return triggerSharedBootstrap(triggerCtx, remote)
		},
		wait: waitForSharedBootstrap,
	}
	err := ensureSharedBranchWith(bootstrapCtx, operations)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("shared bootstrap did not publish within %s", sharedBootstrapTimeout)
	}
	return err
}

func ensureSharedBranchWith(ctx context.Context, operations sharedBootstrapOperations) error {
	if operations.branchExists == nil || operations.trigger == nil || operations.wait == nil {
		return errors.New("shared bootstrap operations are incomplete")
	}
	exists, err := operations.branchExists(ctx)
	if err != nil {
		return fmt.Errorf("check shared branch: %w", err)
	}
	if exists {
		return nil
	}
	if err := operations.trigger(ctx); err != nil {
		return fmt.Errorf("trigger trusted shared bootstrap: %w", err)
	}
	for {
		if err := operations.wait(ctx, sharedBootstrapPoll); err != nil {
			return err
		}
		exists, err = operations.branchExists(ctx)
		if err != nil {
			return fmt.Errorf("poll shared branch: %w", err)
		}
		if exists {
			return nil
		}
	}
}

func triggerSharedBootstrap(ctx context.Context, remote string) error {
	repository, ok := githubRepoSlug(remote)
	if !ok {
		return errors.New("a missing shared branch can be bootstrapped only for a github.com repository")
	}
	if !commandExists("gh") {
		return errors.New("authenticated GitHub CLI is required to bootstrap shared")
	}
	if _, err := runCommand(ctx, "", "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		return fmt.Errorf("GitHub CLI authentication is not ready: %w", err)
	}
	if _, err := runCommand(ctx, "", "gh", "workflow", "run", sharedBootstrapWorkflow, "--repo", "github.com/"+repository); err != nil {
		return fmt.Errorf("dispatch %s from the default branch: %w", sharedBootstrapWorkflow, err)
	}
	return nil
}

func waitForSharedBootstrap(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
