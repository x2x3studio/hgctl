package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x2x3studio/hgctl/internal/config"
	"github.com/x2x3studio/hgctl/internal/event"
	"github.com/x2x3studio/hgctl/internal/ingest"
)

func TestKnowledgeIngesterUsesIndependentLedgerAndOutbox(t *testing.T) {
	app := testApp(t)
	t.Setenv("HOME", app.Paths.Home)
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(app.Paths.Home, ".claude", "projects", "proj", "session.jsonl")
	writeSessionFile(t, path, strings.Join([]string{
		claudeLine(t, "session-id", "/workspace/project", "user", "2026-07-01T00:00:00Z", "Please document the queue architecture and why it uses one branch per machine."),
		claudeLine(t, "session-id", "/workspace/project", "assistant", "2026-07-01T00:00:01Z", "The queue keeps producers isolated and lets one central writer publish shared products."),
	}, "\n")+"\n")

	id := config.Identity{ID: "00000000-0000-4000-8000-000000000000", Hostname: "test"}
	clients, _ := ingest.Clients("claude")
	for _, lane := range []ingestLane{
		{name: "memory", ingester: app.ingester()},
		{name: "knowledge", ingester: app.knowledgeIngester()},
	} {
		marks, err := lane.ingester.LoadLedger()
		if err != nil {
			t.Fatal(err)
		}
		n, _, err := lane.ingester.Run(id, marks, clients, 0, 0, 0)
		if err != nil {
			t.Fatalf("%s ingest: %v", lane.name, err)
		}
		if n != 1 {
			t.Fatalf("%s emitted %d events, want 1", lane.name, n)
		}
		if err := lane.ingester.SaveLedger(marks); err != nil {
			t.Fatal(err)
		}
	}

	memoryFiles, err := filepath.Glob(filepath.Join(app.Paths.Outbox, "*.md"))
	if err != nil || len(memoryFiles) != 1 {
		t.Fatalf("memory outbox files=%v err=%v", memoryFiles, err)
	}
	knowledgeFiles, err := filepath.Glob(filepath.Join(app.Paths.KnowledgeOutbox, "*.md"))
	if err != nil || len(knowledgeFiles) != 1 {
		t.Fatalf("knowledge outbox files=%v err=%v", knowledgeFiles, err)
	}
	if filepath.Base(memoryFiles[0]) != filepath.Base(knowledgeFiles[0]) {
		t.Fatal("the same transcript delta should keep one stable filename across lanes")
	}

	memoryMarks, err := app.ingester().LoadLedger()
	if err != nil {
		t.Fatal(err)
	}
	knowledgeMarks, err := app.knowledgeIngester().LoadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if len(memoryMarks) == 0 || len(knowledgeMarks) == 0 {
		t.Fatal("both product ledgers must advance independently")
	}

	for _, lane := range []ingestLane{
		{name: "memory", ingester: app.ingester()},
		{name: "knowledge", ingester: app.knowledgeIngester()},
	} {
		marks, _ := lane.ingester.LoadLedger()
		n, _, err := lane.ingester.Run(id, marks, clients, 0, 0, 0)
		if err != nil || n != 0 {
			t.Fatalf("unchanged %s transcript emitted %d, err=%v", lane.name, n, err)
		}
	}
}

func TestQueuePublishesMemoryAndKnowledgeToSeparatePaths(t *testing.T) {
	app, origin, branch := setupBulkQueue(t)
	state := config.State{RepoURL: origin, QueueBranch: branch}
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	memory := event.Raw{CapturedAt: captured, Client: "claude", Machine: "m", Body: "memory lane event"}
	knowledge := event.Raw{CapturedAt: captured.Add(time.Second), Client: "claude", Machine: "m", Body: "knowledge lane event"}
	if err := event.Enqueue(app.Paths.Outbox, memory); err != nil {
		t.Fatal(err)
	}
	if err := event.Enqueue(app.Paths.KnowledgeOutbox, knowledge); err != nil {
		t.Fatal(err)
	}

	delivered, err := app.drainOutboxToQueue(testContext(t), state)
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 2 {
		t.Fatalf("delivered=%d, want 2", delivered)
	}
	tracked := strings.Fields(runGitTest(t, app.Paths.Queue, "ls-tree", "-r", "--name-only", "HEAD"))
	var memoryPath, knowledgePath bool
	for _, path := range tracked {
		memoryPath = memoryPath || strings.HasPrefix(path, "memory/events/") && strings.HasSuffix(path, ".md")
		knowledgePath = knowledgePath || strings.HasPrefix(path, "knowledge/events/") && strings.HasSuffix(path, ".md")
	}
	if !memoryPath || !knowledgePath {
		t.Fatalf("queue paths not isolated: %v", tracked)
	}
	for _, dir := range []string{app.Paths.Outbox, app.Paths.KnowledgeOutbox} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".md") {
				t.Fatalf("%s retained %s after successful push", dir, entry.Name())
			}
		}
	}
}

func TestLegacyMemoryQueueTargetUsesMemoryOutboxForCrashRecovery(t *testing.T) {
	app := testApp(t)
	lane, ok := app.queueLaneForTarget("events/legacy.md")
	if !ok {
		t.Fatal("legacy Memory queue target is not recoverable after upgrade")
	}
	if lane.outbox != app.Paths.Outbox || lane.queueDir != legacyMemoryQueueDir {
		t.Fatalf("legacy lane=%+v, want Memory outbox and %q", lane, legacyMemoryQueueDir)
	}
}

func TestSteadyQueueBudgetIsSharedAcrossProducts(t *testing.T) {
	app := testApp(t)
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Queue, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		captured := time.Date(2026, 7, 1, 0, 0, i, 0, time.UTC)
		if err := event.Enqueue(app.Paths.Outbox, event.Raw{CapturedAt: captured, Client: "claude", Machine: "m", Body: "memory"}); err != nil {
			t.Fatal(err)
		}
		if err := event.Enqueue(app.Paths.KnowledgeOutbox, event.Raw{CapturedAt: captured.Add(time.Minute), Client: "claude", Machine: "m", Body: "knowledge"}); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := app.copySteadyOutboxes(4, event.MaxSyncBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.EventPaths) != 4 {
		t.Fatalf("combined steady batch=%d, want global cap 4", len(batch.EventPaths))
	}
}

func TestBrokenKnowledgeOutboxDoesNotBlockMemoryCopy(t *testing.T) {
	app := testApp(t)
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Queue, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := event.Enqueue(app.Paths.Outbox, event.Raw{CapturedAt: time.Now(), Client: "claude", Machine: "m", Body: "memory"}); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(app.Paths.KnowledgeOutbox, "bad.md")
	if err := os.WriteFile(bad, make([]byte, event.MaxEventBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	batch, err := app.copySteadyOutboxes(event.MaxSyncEvents, event.MaxSyncBytes)
	if err == nil {
		t.Fatal("expected the broken Knowledge lane to be reported")
	}
	if len(batch.EventPaths) != 1 || !strings.HasPrefix(batch.EventPaths[0], "memory/events/") {
		t.Fatalf("valid Memory lane was not copied: %+v", batch)
	}
}

func TestBulkPushFailureRetainsBothProductOutboxes(t *testing.T) {
	app, origin, branch := setupBulkQueue(t)
	state := config.State{RepoURL: origin, QueueBranch: branch}
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := event.Enqueue(app.Paths.Outbox, event.Raw{CapturedAt: captured, Client: "claude", Machine: "m", Body: "memory"}); err != nil {
		t.Fatal(err)
	}
	if err := event.Enqueue(app.Paths.KnowledgeOutbox, event.Raw{CapturedAt: captured.Add(time.Second), Client: "claude", Machine: "m", Body: "knowledge"}); err != nil {
		t.Fatal(err)
	}
	// Make the final push fail after both lanes have been copied and committed.
	runGitTest(t, app.Paths.Queue, "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if _, err := app.drainOutboxToQueue(testContext(t), state); err == nil {
		t.Fatal("expected push failure")
	}
	for _, dir := range []string{app.Paths.Outbox, app.Paths.KnowledgeOutbox} {
		files, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil || len(files) != 1 {
			t.Fatalf("%s recovery source files=%v err=%v", dir, files, err)
		}
	}
}

func TestIngestTargetSelection(t *testing.T) {
	app := testApp(t)
	for target, want := range map[string]int{"memory": 1, "knowledge": 1, "all": 2} {
		lanes, ok := app.ingestLanes(target)
		if !ok || len(lanes) != want {
			t.Fatalf("target %s: ok=%v lanes=%d, want %d", target, ok, len(lanes), want)
		}
	}
	if _, ok := app.ingestLanes("other"); ok {
		t.Fatal("unknown target accepted")
	}
}
