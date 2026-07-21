package hgctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
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
	case "observe":
		err = a.runObserve(args[1:])
	case "import":
		err = a.runImport(args[1:])
	case "sync":
		err = a.runSync(ctx, args[1:])
	case "context":
		err = a.runContext(args[1:])
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
	_, _ = fmt.Fprintln(a.Err, "usage: hgctl <install|hook|observe|import|sync|context|update|doctor|uninstall|version>")
}

func (a *App) runInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", envOr("HOURGLASS_REPO", DefaultRepoURL), "Hourglass Git remote")
	importPath := fs.String("import", "", "existing Markdown tree to bootstrap")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("install accepts flags only")
	}
	return withFileLockWait(ctx, a.Paths.LifecycleLock, func() error {
		return withFileLockWait(ctx, a.Paths.UpdateLock, func() error {
			return a.install(ctx, *repo, *importPath)
		})
	})
}

func (a *App) install(ctx context.Context, repo, importPath string) error {
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
	if err := a.importDurableAgentMemory(); err != nil {
		installErrs = append(installErrs, err)
	}
	if importPath != "" {
		if _, err := a.importMarkdownTree(importPath, "bootstrap"); err != nil {
			installErrs = append(installErrs, err)
		}
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

func (a *App) runObserve(args []string) error {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	client := fs.String("client", "", "capturing client")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !validEndpointClient(*client) {
		return errors.New("observe requires --client claude|codex")
	}
	b, err := io.ReadAll(io.LimitReader(a.In, MaxTextBytes+1))
	if err != nil {
		return err
	}
	text := strings.TrimSpace(boundText(string(b)))
	if text == "" {
		return errors.New("empty observation")
	}
	id, err := a.loadIdentity()
	if err != nil {
		return err
	}
	event, err := newObservation(id, *client, text, a.Now().UTC())
	if err != nil {
		return err
	}
	return a.enqueue(event)
}

func (a *App) runImport(args []string) error {
	source, rest, err := extractOption(args, "--source", "manual")
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: hgctl import [--source name] <path>")
	}
	n, err := a.importMarkdownTree(rest[0], source)
	if err == nil {
		_, err = fmt.Fprintf(a.Out, "queued %d deterministic import batches\n", n)
	}
	return err
}

func (a *App) runContext(args []string) error {
	client, rest, err := extractOption(args, "--client", "")
	if err != nil {
		return err
	}
	if !validEndpointClient(client) {
		return errors.New("context requires --client claude|codex")
	}
	if len(rest) > 1 {
		return errors.New("context accepts at most one path")
	}
	text := a.contextText(client)
	_, err = fmt.Fprintln(a.Out, text)
	return err
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func validEndpointClient(client string) bool {
	return client == "claude" || client == "codex"
}

func extractOption(args []string, name, fallback string) (string, []string, error) {
	value := fallback
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] != name {
			rest = append(rest, args[i])
			continue
		}
		i++
		if i >= len(args) {
			return "", nil, fmt.Errorf("%s requires a value", name)
		}
		value = args[i]
	}
	return value, rest, nil
}
