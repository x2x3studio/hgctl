package hgctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage()
		return 2
	}
	var err error
	switch args[0] {
	// `--help`, `-h`, and `help` are what a caller reaches for first. All three
	// used to error out with "unknown command", which reads as a broken binary
	// and sends the reader looking somewhere else - or, worse, guessing.
	case "help", "--help", "-h":
		a.usage()
		return 0
	case "version", "--version", "-v":
		_, err = fmt.Fprintln(a.Out, Version)
	case "install":
		err = a.runInstall(ctx, args[1:])
	case "hook":
		// Per-turn capture is retired; intake is per-session transcript ingest
		// driven by the scheduler. A stale client hook registration may still
		// invoke this during the prune window, so drain stdin and exit clean so
		// no client session is disrupted.
		_, _ = io.Copy(io.Discard, io.LimitReader(a.In, MaxEventBytes+1))
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

// usage lists every command with what it is FOR, and with the behaviour that
// surprises a first-time caller. hgctl is operated by agents as much as by
// people, and an agent reaches for `--help` mid-task and acts on whatever comes
// back - so this text, not the README, is where the operating contract has to
// live. It cannot drift from the binary the way a separate document can.
//
// The specific failure this replaces: a caller who wanted to force an update ran
// `hgctl sync --update`, got a silent no-op (it is throttled), concluded
// self-update was broken, and reinstalled from the release tarball by hand. The
// `update` command was one line away in the old usage output, but that output
// listed command NAMES only, and `--help` and `help` both errored out.
func (a *App) usage() {
	_, _ = fmt.Fprint(a.Err, `hgctl - Hourglass endpoint runtime

usage: hgctl <command> [flags]

  install    Connect this machine: register the read-only recall MCP, seed this
             machine's queue branch, install the sync scheduler. Idempotent -
             re-running repairs a partial install and never mints a new machine
             id.
  sync       One scheduled cycle: ingest new session turns, publish this queue,
             fast-forward shared, reindex the local recall mirror. Run by the
             scheduler about once a minute; safe to run by hand.
      --update   Also check for a newer release. THROTTLED to once an hour, so
                 an immediate second call is a silent no-op. To force an update
                 now, use the 'update' command instead.
  ingest     Operator-path ingest of session transcripts, unbounded. Use when
             the scheduled path is behind and you want everything pending
             emitted in one go.
      --client all|claude|codex   Limit to one source (default all).
      --limit N                   Cap sessions this run. Candidates are ordered
                                  OLDEST first, so a small limit reports 0 when
                                  the oldest candidates have no new turns - it
                                  is a poor probe for "is ingest working".
  update     Force an update check now, ignoring the hourly throttle.
  doctor     Check every endpoint invariant and print one line each. A failing
             line names what is wrong, not what to do about it.
  version    Print the installed version.
  uninstall  Remove the scheduler, MCP registrations, and local state.
  hook       Retired per-turn capture entry point. Kept only so a stale client
             hook registration exits cleanly during the prune window.
`)
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
	if err := a.pruneClientHooks(); err != nil {
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
