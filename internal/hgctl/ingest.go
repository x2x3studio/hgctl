package hgctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/x2x3studio/hgctl/internal/config"
	"github.com/x2x3studio/hgctl/internal/ingest"
)

// ingester binds the intake package to this App's paths and clock.
func (a *App) ingester() *ingest.Ingester {
	return &ingest.Ingester{Paths: a.Paths, Now: a.Now}
}

// runIngest reads local agent session transcripts and publishes each session's new
// turns as complete, chunked delta events, oldest first, stamped with their turn
// times. It is the operator/bulk entry point for per-session intake (live +
// historical): intake only, never a Basic Memory write. Unlike the steady-state
// sync, it drains the whole outbox to the machine queue branch in large batches and
// pushes once, so an operator-invoked import lands on origin before the command
// returns. It does not throttle (minInterval 0): an explicit run always emits any
// pending delta, while the size+turn markers keep unchanged sessions idempotent.
func (a *App) runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	client := fs.String("client", "all", "session source to ingest: all, claude, codex, or copilot")
	limit := fs.Int("limit", 0, "optional cap on sessions this run (0 = no cap, oldest first)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clients, ok := ingest.Clients(*client)
	if fs.NArg() != 0 || !ok || *limit < 0 {
		return errors.New("usage: hgctl ingest [--client all|claude|codex|copilot] [--limit N]")
	}
	id, err := config.LoadIdentity(a.Paths, a.Now)
	if err != nil {
		return err
	}
	marks, err := a.ingester().LoadLedger()
	if err != nil {
		return err
	}
	enqueued, _, ingestErr := a.ingester().Run(id, marks, clients, *limit, 0, 0)
	if err := a.ingester().SaveLedger(marks); err != nil {
		return errors.Join(ingestErr, err)
	}
	if ingestErr != nil {
		return ingestErr
	}
	delivered, derr := a.bulkPublishQueue(ctx)
	if errors.Is(derr, os.ErrNotExist) {
		_, err = fmt.Fprintf(a.Out, "ingested %d session event(s) into the outbox; run 'hgctl sync' to publish\n", enqueued)
		return err
	}
	if derr != nil {
		return derr
	}
	_, err = fmt.Fprintf(a.Out, "ingested %d session event(s); published %d queued event(s) to queue/%s\n", enqueued, delivered, id.ID)
	return err
}
