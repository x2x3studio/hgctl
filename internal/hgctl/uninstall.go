package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/proc"

	"github.com/x2x3studio/hgctl/internal/config"
)

func (a *App) uninstall(ctx context.Context) error {
	return fsx.WithLockWait(ctx, a.Paths.LifecycleLock, func() error {
		return a.uninstallLocked(ctx)
	})
}

func (a *App) uninstallLocked(ctx context.Context) error {
	stable := filepath.Join(a.Paths.Bin, "hgctl")
	if err := a.verifySchedulerOwnership(); err != nil {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removal is incomplete; the binary was preserved because the scheduler is not owned by hgctl.")
		return err
	}
	if err := a.quiesceScheduler(ctx); err != nil {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removal is incomplete; the binary was preserved so the scheduler cannot break.")
		return fmt.Errorf("stop scheduler before uninstall: %w", err)
	}

	var errs []error
	safeToRemoveBinary := true
	cleanupErr := fsx.WithLockWait(ctx, a.Paths.SyncLock, func() error {
		return fsx.WithLockWait(ctx, a.Paths.UpdateLock, func() error {
			if err := a.removeManagedBasicMemoryMCP(ctx); err != nil {
				errs = append(errs, err)
				safeToRemoveBinary = false
			}
			for _, item := range a.clientAdapters() {
				present, err := managedHooksPresent(item.path, stable, item.client)
				if errors.Is(err, os.ErrNotExist) || (err == nil && !present) {
					continue
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("inspect %s hooks: %w", item.client, err))
					safeToRemoveBinary = false
					continue
				}
				if err := pruneClientHookFile(item.path, stable, item.client); err != nil {
					errs = append(errs, fmt.Errorf("remove %s hooks: %w", item.client, err))
					safeToRemoveBinary = false
					continue
				}
				remaining, err := managedHooksPresent(item.path, stable, item.client)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("verify %s hooks: %w", item.client, err))
					safeToRemoveBinary = false
				} else if remaining {
					errs = append(errs, fmt.Errorf("verify %s hooks: managed hooks remain", item.client))
					safeToRemoveBinary = false
				}
			}

			basicMemoryCleanup, err := a.managedBasicMemoryCleanupRequired()
			if err != nil {
				errs = append(errs, fmt.Errorf("inspect Basic Memory ownership: %w", err))
				safeToRemoveBinary = false
			} else if basicMemoryCleanup && !proc.Exists("basic-memory") {
				errs = append(errs, errors.New("managed Basic Memory project cleanup requires the basic-memory command"))
				safeToRemoveBinary = false
			} else if err := a.removeManagedBasicMemoryProject(ctx); err != nil {
				errs = append(errs, err)
				safeToRemoveBinary = false
			}

			if err := a.removeSchedulerFiles(ctx); err != nil {
				errs = append(errs, err)
				safeToRemoveBinary = false
			}
			if remaining, err := a.schedulerFilesPresent(); err != nil {
				errs = append(errs, fmt.Errorf("verify scheduler files: %w", err))
				safeToRemoveBinary = false
			} else if remaining {
				errs = append(errs, errors.New("verify scheduler files: managed scheduler files remain"))
				safeToRemoveBinary = false
			}

			if safeToRemoveBinary && managedStableSymlink(stable, a.Paths.Versions) {
				if err := os.Remove(stable); err != nil {
					errs = append(errs, err)
					safeToRemoveBinary = false
				}
			}
			return nil
		})
	})
	if cleanupErr != nil {
		errs = append(errs, cleanupErr)
		safeToRemoveBinary = false
	}
	if safeToRemoveBinary {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removed; vault, outbox, and machine identity were preserved.")
	} else {
		_, _ = fmt.Fprintln(a.Out, "Hourglass integration removal is incomplete; the binary was preserved so managed cleanup can be retried.")
	}
	return errors.Join(errs...)
}

func (a *App) managedBasicMemoryCleanupRequired() (bool, error) {
	state, err := config.LoadState(a.Paths)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state.BasicMemoryProject != nil && state.BasicMemoryProject.Managed, nil
}
