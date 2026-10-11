package hgctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/x2x3studio/hgctl/internal/fsx"

	"github.com/x2x3studio/hgctl/internal/config"

	"github.com/x2x3studio/hgctl/internal/event"

	"github.com/x2x3studio/hgctl/internal/ingest"
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
		_, _ = io.Copy(io.Discard, io.LimitReader(a.In, event.MaxEventBytes+1))
	case "sync":
		err = a.runSync(ctx, args[1:])
	case "ingest":
		err = a.runIngest(ctx, args[1:])
	case "update":
		err = a.update(ctx, true)
	case "mcp":
		if len(args) != 1 {
			err = errors.New("mcp accepts no arguments")
		} else {
			err = fsx.WithLockWait(ctx, a.Paths.LifecycleLock, func() error {
				if _, err := a.resolveBasicMemoryProject(ctx); err != nil {
					return err
				}
				return a.setupBasicMemoryMCP(ctx)
			})
		}
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
             machine's queue branch, install the sync scheduler. On a FIRST
             install it also backfills this machine's whole session history in
             one unbounded pass - the same work 'ingest' does - because the
             scheduled path parses only a handful of transcripts per run and
             would need hours of ticks to finish it. Expect that first run to
             take a while. Idempotent - re-running repairs a partial install,
             skips the backfill (the ledger is already populated), and never
             mints a new machine id.
  sync       One scheduled cycle: ingest new session turns, publish this queue,
             fast-forward shared, reindex the local recall mirror. Run by the
             scheduler about once a minute; safe to run by hand.
      --update   Also check for a newer release. THROTTLED to once an hour, so
                 an immediate second call is a silent no-op. To force an update
                 now, use the 'update' command instead.
  ingest     Operator-path ingest of session transcripts, unbounded. The same
             path 'install' runs for both products on a first connect, so it is
             rarely needed by hand: use it when a scheduled lane is behind.
      --client all|claude|codex|copilot
                                         Limit to one source (default all).
      --target memory|knowledge|all       Select the product ledger (default memory).
      --limit N                           Cap sessions per target. Candidates are ordered
                                  OLDEST first, so a small limit reports 0 when
                                  the oldest candidates have no new turns - it
                                  is a poor probe for "is ingest working".
  update     Force an update check now, ignoring the hourly throttle.
  mcp        Start or repair one loopback-only Basic Memory HTTP service and
             migrate installed clients from per-session stdio to its shared URL.
             Does not ingest, publish, or change the sync scheduler. Existing
             client sessions reconnect on their next restart.
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
	repo := fs.String("repo", envOr("HOURGLASS_REPO", config.DefaultRepoURL), "Hourglass Git remote")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("install accepts flags only")
	}
	return fsx.WithLockWait(ctx, a.Paths.LifecycleLock, func() error {
		return fsx.WithLockWait(ctx, a.Paths.UpdateLock, func() error {
			return a.install(ctx, *repo)
		})
	})
}

func (a *App) install(ctx context.Context, repo string) error {
	if err := a.installBinary(); err != nil {
		return err
	}
	id, err := config.LoadIdentity(a.Paths, a.Now)
	if err != nil {
		return err
	}
	state := config.State{RepoURL: repo, QueueBranch: "queue/" + id.ID}
	if err := fsx.WithLockWait(ctx, a.Paths.SyncLock, func() error {
		if previous, err := config.LoadState(a.Paths); err == nil {
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
		if err := config.SaveState(a.Paths, state); err != nil {
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
	if err := a.initialIntake(ctx, id); err != nil {
		_, _ = fmt.Fprintln(a.Err, "hgctl: initial intake deferred:", err)
	}
	if err := errors.Join(installErrs...); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "Hourglass initialized: %s (%s)\n", id.ID, id.Hostname)
	return err
}

// initialIntake gets a newly connected machine current, then hands it to the
// scheduler.
//
// On a FIRST install the whole of this machine's history is waiting and the
// scheduled path is the wrong tool for it: one sync parses at most
// syncIngestLimit transcripts, so a machine with a couple of thousand sessions
// would need hours of scheduler ticks to finish a backfill it can do in one
// pass. That is the operator path (`hgctl ingest`) - unbounded parse, bulk
// commits, one push - and there is no reason to make a person remember to run it
// on a machine that has just been connected and demonstrably has nothing
// ingested yet.
//
// A RE-RUN of install is repair, not onboarding: the ledger already has marks,
// the scheduler is already draining, and re-parsing every transcript would cost
// minutes to discover there is nothing new. Those get the ordinary bounded sync.
//
// Never fatal. A machine whose backfill fails is still installed and still
// scheduled; it just catches up over ticks instead of in one pass.
func (a *App) initialIntake(ctx context.Context, id config.Identity) error {
	lanes := a.allIngestLanes()
	clients, _ := ingest.Clients("all")
	total := 0
	backfilled := false
	var backfillErrs []error
	for _, lane := range lanes {
		marks, err := lane.ingester.LoadLedger()
		if err != nil {
			backfillErrs = append(backfillErrs, fmt.Errorf("%s ledger: %w", lane.name, err))
			continue
		}
		if len(marks) > 0 {
			continue
		}
		backfilled = true
		_, _ = fmt.Fprintf(a.Out, "Reading local session history for %s. This can take a while.\n", lane.name)
		// Empty ledger means this product has never seen the machine. Parse every
		// transcript once; later scheduler runs use the ordinary bounded delta path.
		enqueued, _, ingestErr := lane.ingester.Run(id, marks, clients, 0, 0, 0)
		total += enqueued
		if err := lane.ingester.SaveLedger(marks); err != nil {
			ingestErr = errors.Join(ingestErr, err)
		}
		if ingestErr != nil {
			backfillErrs = append(backfillErrs, fmt.Errorf("%s backfill: %w", lane.name, ingestErr))
		}
	}
	if !backfilled {
		return errors.Join(errors.Join(backfillErrs...), a.sync(ctx))
	}
	delivered, publishErr := a.bulkPublishQueue(ctx)
	var reportErr error
	if publishErr == nil {
		_, reportErr = fmt.Fprintf(a.Out, "Backfilled %d event(s) from local history; published %d.\n", total, delivered)
	}
	return errors.Join(errors.Join(backfillErrs...), publishErr, reportErr)
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
