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

// ingester binds the intake package to this App's memory lane.
func (a *App) ingester() *ingest.Ingester {
	return &ingest.Ingester{Paths: a.Paths, Now: a.Now}
}

// knowledgeIngester reuses the same transcript parser with an independent
// ledger and outbox, so either product can replay and advance a session without
// making the other product skip it.
func (a *App) knowledgeIngester() *ingest.Ingester {
	paths := a.Paths
	paths.Outbox = a.Paths.KnowledgeOutbox
	return &ingest.Ingester{Paths: paths, Now: a.Now, LedgerPath: a.Paths.KnowledgeLedger}
}

type ingestLane struct {
	name     string
	ingester *ingest.Ingester
}

func (a *App) allIngestLanes() []ingestLane {
	return []ingestLane{
		{name: "memory", ingester: a.ingester()},
		{name: "knowledge", ingester: a.knowledgeIngester()},
	}
}

func (a *App) ingestLanes(target string) ([]ingestLane, bool) {
	lanes := a.allIngestLanes()
	if target == "all" {
		return lanes, true
	}
	for _, lane := range lanes {
		if lane.name == target {
			return []ingestLane{lane}, true
		}
	}
	return nil, false
}

// runIngest parses local session transcripts into one or both product lanes. Each
// lane owns an independent turn ledger and outbox, while both reuse the exact same
// Claude/Codex parsers and complete-turn chunker.
func (a *App) runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	client := fs.String("client", "all", "session source to ingest: all, claude, codex, or copilot")
	target := fs.String("target", "memory", "product target: memory, knowledge, or all")
	limit := fs.Int("limit", 0, "optional cap on sessions per target (0 = no cap, oldest first)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clients, clientsOK := ingest.Clients(*client)
	lanes, targetOK := a.ingestLanes(*target)
	if fs.NArg() != 0 || !clientsOK || !targetOK || *limit < 0 {
		return errors.New("usage: hgctl ingest [--client all|claude|codex|copilot] [--target memory|knowledge|all] [--limit N]")
	}
	id, err := config.LoadIdentity(a.Paths, a.Now)
	if err != nil {
		return err
	}
	total := 0
	var ingestErrs []error
	for _, lane := range lanes {
		marks, err := lane.ingester.LoadLedger()
		if err != nil {
			ingestErrs = append(ingestErrs, fmt.Errorf("%s ledger: %w", lane.name, err))
			continue
		}
		enqueued, _, runErr := lane.ingester.Run(id, marks, clients, *limit, 0, 0)
		total += enqueued
		if err := lane.ingester.SaveLedger(marks); err != nil {
			runErr = errors.Join(runErr, err)
		}
		if runErr != nil {
			ingestErrs = append(ingestErrs, fmt.Errorf("%s ingest: %w", lane.name, runErr))
		}
	}
	if err := errors.Join(ingestErrs...); err != nil {
		return err
	}
	delivered, derr := a.bulkPublishQueue(ctx)
	if errors.Is(derr, os.ErrNotExist) {
		_, err = fmt.Fprintf(a.Out, "ingested %d session event(s) into local outboxes; run 'hgctl sync' to publish\n", total)
		return err
	}
	if derr != nil {
		return derr
	}
	_, err = fmt.Fprintf(a.Out, "ingested %d session event(s); published %d queued event(s) to queue/%s\n", total, delivered, id.ID)
	return err
}
