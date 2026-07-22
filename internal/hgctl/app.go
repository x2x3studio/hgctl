package hgctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage()
		return 2
	}
	var err error
	switch args[0] {
	case "version", "--version", "-v":
		_, err = fmt.Fprintln(a.Out, Version)
	case "install":
		err = a.runInstall(ctx, args[1:])
	case "hook":
		client, eventName := hookDiagnosticScope(args[1:])
		err = a.runHook(ctx, args[1:])
		if err != nil {
			_ = a.recordHookDiagnostic(client, eventName, err)
			return 0
		}
		a.clearHookDiagnostic(client, eventName)
	case "sync":
		err = a.runSync(ctx, args[1:])
	case "ingest":
		err = a.runIngest(ctx, args[1:])
	case "update":
		err = a.update(ctx, true)
	case "doctor":
		err = a.doctor(ctx)
	case "uninstall":
		err = a.uninstall(ctx)
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		_, _ = fmt.Fprintln(a.Err, "hgctl:", err)
		return 1
	}
	return 0
}

func (a *App) usage() {
	_, _ = fmt.Fprintln(a.Err, "usage: hgctl <install|hook|sync|ingest|update|doctor|uninstall|version>")
}

func (a *App) runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", envOr("HOURGLASS_REPO", DefaultRepoURL), "Hourglass Git remote")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("install accepts flags only")
	}
	return withFileLockWait(ctx, a.Paths.LifecycleLock, func() error {
		return withFileLockWait(ctx, a.Paths.UpdateLock, func() error {
			return a.install(ctx, *repo)
		})
	})
}

func (a *App) install(ctx context.Context, repo string) error {
	if err := a.installBinary(); err != nil {
		return err
	}
	id, err := a.loadIdentity()
	if err != nil {
		return err
	}
	state := State{RepoURL: repo, QueueBranch: "queue/" + id.ID}
	if err := withFileLockWait(ctx, a.Paths.SyncLock, func() error {
		if previous, err := a.loadState(); err == nil {
			if previous.RepoURL != "" && previous.RepoURL != state.RepoURL {
				return fmt.Errorf("refusing to replace configured repository %s with %s", previous.RepoURL, state.RepoURL)
			}
			state.BasicMemoryProject = previous.BasicMemoryProject
			state.BasicMemoryMCP = previous.BasicMemoryMCP
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := a.initGit(ctx, state); err != nil {
			return err
		}
		if err := a.saveState(state); err != nil {
			return err
		}
		return a.setupBasicMemory(ctx)
	}); err != nil {
		return err
	}
	if err := a.installScheduler(ctx); err != nil {
		return err
	}
	var installErrs []error
	if err := a.setupBasicMemoryMCP(ctx); err != nil {
		installErrs = append(installErrs, err)
	}
	if err := a.setupClientHooks(ctx); err != nil {
		installErrs = append(installErrs, err)
	}
	if err := a.sync(ctx); err != nil {
		_, _ = fmt.Fprintln(a.Err, "hgctl: initial sync deferred:", err)
	}
	if err := errors.Join(installErrs...); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "Hourglass initialized: %s (%s)\n", id.ID, id.Hostname)
	return err
}

func (a *App) runSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	checkUpdate := fs.Bool("update", false, "check for a newer release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("sync accepts flags only")
	}
	syncErr := a.sync(ctx)
	if *checkUpdate {
		if err := a.update(ctx, false); err != nil {
			_, _ = fmt.Fprintln(a.Err, "hgctl: update deferred:", err)
		}
	}
	return syncErr
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
