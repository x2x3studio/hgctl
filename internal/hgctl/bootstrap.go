package hgctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/x2x3studio/hgctl/internal/gitx"
)

func (a *App) ensureRepositoryBranches(ctx context.Context, remote string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	exists, err := gitx.RemoteBranchExists(checkCtx, a.Paths.Control, "shared")
	if err != nil {
		return fmt.Errorf("check shared branch: %w", err)
	}
	if !exists {
		return errors.New("remote branch shared does not exist; publish shared before installing an endpoint")
	}
	return nil
}
