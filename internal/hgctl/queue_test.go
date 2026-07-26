package hgctl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/x2x3studio/hgctl/internal/config"

	"github.com/x2x3studio/hgctl/internal/event"

	"github.com/x2x3studio/hgctl/internal/ingest"
)

func writeSessionFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCopyOutboxBatchBoundsAreOptIn(t *testing.T) {
	app := testApp(t)
	if err := os.MkdirAll(filepath.Join(app.Paths.Queue, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		captured := time.Date(2026, 6, 5, 0, 0, i, 0, time.UTC)
		if err := event.Enqueue(app.Paths.Outbox, event.Raw{CapturedAt: captured, Client: "codex", Machine: "m", Body: "event body long enough"}); err != nil {
			t.Fatal(err)
		}
	}
	// An explicit bound is honoured. Written with a literal rather than
	// event.MaxSyncEvents so the test keeps checking the MECHANISM when the constant
	// moves - it used to assert batch == event.MaxSyncEvents, which silently stopped
	// testing anything the moment the bound rose above the events seeded.
	bounded, err := app.copyOutboxBatch(4, event.MaxSyncBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.EventPaths) != 4 {
		t.Fatalf("bounded batch = %d, want the explicit bound 4", len(bounded.EventPaths))
	}
	// A byte bound binds before the event bound when it is the smaller of the two.
	byteBounded, err := app.copyOutboxBatch(9, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(byteBounded.EventPaths) != 1 {
		t.Fatalf("byte-bounded batch = %d, want 1 (the first event always goes)", len(byteBounded.EventPaths))
	}
	unbounded, err := app.copyOutboxBatch(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded.EventPaths) != 9 {
		t.Fatalf("bulk batch = %d, want all 9 events", len(unbounded.EventPaths))
	}
}

// The steady-state bound has to outpace what one sync can INGEST, or the outbox
// grows without bound and the machine never catches up on its own. It sat at 4
// events while a single sync parses up to ingest.SyncLimit sessions, each able to
// emit many chunk events; the outbox on one machine reached 635 behind that
// drain. This is a floor on the constant, not on the mechanism.
func TestSteadyStateTransportOutpacesIngest(t *testing.T) {
	worstCaseEventsPerSync := ingest.SyncLimit * 32 // 32 chunk events per session is already generous
	if event.MaxSyncEvents < worstCaseEventsPerSync {
		t.Fatalf("event.MaxSyncEvents = %d cannot drain %d events one sync can produce; the outbox will grow",
			event.MaxSyncEvents, worstCaseEventsPerSync)
	}
	// And the byte cap must not quietly become the binding term at the measured
	// mean event size (~14KB), or raising the count achieved nothing.
	if want := event.MaxSyncEvents * 14 * 1024; event.MaxSyncBytes < want {
		t.Fatalf("event.MaxSyncBytes = %d binds before %d events at the measured mean size; want >= %d",
			event.MaxSyncBytes, event.MaxSyncEvents, want)
	}
}

func setupBulkQueue(t *testing.T) (*App, string, string) {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	runGitTest(t, "", "init", "--bare", origin)

	app := testApp(t)
	control := app.Paths.Control
	runGitTest(t, "", "init", "-b", "main", control)
	runGitTest(t, control, "config", "user.name", "chinaboard")
	runGitTest(t, control, "config", "user.email", "chinaboard@gmail.com")
	runGitTest(t, control, "remote", "add", "origin", origin)

	id, err := config.LoadIdentity(app.Paths, app.Now)
	if err != nil {
		t.Fatal(err)
	}
	branch := "queue/" + id.ID
	if err := app.seedOrphanQueueBranch(testContext(t), branch); err != nil {
		t.Fatal(err)
	}
	if err := app.ensureWorktree(testContext(t), app.Paths.Queue, branch, branch); err != nil {
		t.Fatal(err)
	}
	return app, origin, branch
}

func TestDrainOutboxToQueuePublishesEntireBacklog(t *testing.T) {
	app, origin, branch := setupBulkQueue(t)
	state := config.State{RepoURL: origin, QueueBranch: branch}

	const events = 9 // more than event.MaxSyncEvents, so a steady-state sync could not do this in one push
	for i := 0; i < events; i++ {
		captured := time.Date(2026, 6, 5, 0, 0, i, 0, time.UTC)
		if err := event.Enqueue(app.Paths.Outbox, event.Raw{CapturedAt: captured, Client: "codex", Machine: "m", Body: "backlog event body"}); err != nil {
			t.Fatal(err)
		}
	}

	delivered, err := app.drainOutboxToQueue(testContext(t), state)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if delivered != events {
		t.Fatalf("delivered = %d, want %d", delivered, events)
	}

	// Every event committed under events/ alongside the orphan .gitkeep seed.
	tracked := strings.Fields(runGitTest(t, app.Paths.Queue, "ls-tree", "-r", "--name-only", "HEAD"))
	md := 0
	for _, name := range tracked {
		if strings.HasPrefix(name, "events/") && strings.HasSuffix(name, ".md") {
			md++
		}
	}
	if md != events {
		t.Fatalf("committed %d event files, want %d (tracked: %v)", md, events, tracked)
	}

	// The whole backlog landed on origin, not just a bounded batch.
	local := strings.TrimSpace(runGitTest(t, app.Paths.Queue, "rev-parse", "HEAD"))
	remote := strings.TrimSpace(runGitTest(t, origin, "rev-parse", "refs/heads/"+branch))
	if local != remote {
		t.Fatalf("origin not updated: local=%s remote=%s", local, remote)
	}

	// It fit in a few commits (seed + bulk), not one commit per event.
	commits := strings.TrimSpace(runGitTest(t, app.Paths.Queue, "rev-list", "--count", "HEAD"))
	if commits != "2" {
		t.Fatalf("commit count = %s, want 2 (orphan seed + one bulk commit)", commits)
	}

	// Outbox drained.
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			t.Fatalf("outbox still holds %s after bulk delivery", e.Name())
		}
	}
}

func TestBulkPublishQueueRejectsForeignBranch(t *testing.T) {
	app := testApp(t)
	if err := config.SaveState(app.Paths, config.State{RepoURL: "x", QueueBranch: "queue/not-this-machine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.bulkPublishQueue(testContext(t)); err == nil {
		t.Fatal("expected a branch/identity mismatch error")
	}
}

// A bulk ingest stages up to bulkQueueCommitChunk events before committing, so an
// interruption after copy/stage but before commit leaves more than event.MaxSyncEvents
// events in the queue worktree. Recovery must fold the whole validated batch, not
// wedge on the steady-state caps.
func TestRecoverInterruptedQueueBatchExceedsSteadyStateCaps(t *testing.T) {
	app, _, _ := setupBulkQueue(t)

	const events = 9 // more than event.MaxSyncEvents
	for i := 0; i < events; i++ {
		captured := time.Date(2026, 6, 5, 0, 0, i, 0, time.UTC)
		if err := event.Enqueue(app.Paths.Outbox, event.Raw{CapturedAt: captured, Client: "codex", Machine: "m", Body: "interrupted batch body"}); err != nil {
			t.Fatal(err)
		}
	}

	// Copy and stage the batch into the worktree, then stop short of the commit to
	// simulate a crash mid-bulk-ingest.
	batch, err := app.copyOutboxBatch(bulkQueueCommitChunk, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.EventPaths) != events {
		t.Fatalf("staged %d events, want %d", len(batch.EventPaths), events)
	}
	runGitTest(t, app.Paths.Queue, append([]string{"add", "--"}, batch.EventPaths...)...)

	recovered, err := app.recoverInterruptedQueueBatch(testContext(t))
	if err != nil {
		t.Fatalf("recovery of a >event.MaxSyncEvents interrupted batch failed: %v", err)
	}
	if len(recovered.EventPaths) != events {
		t.Fatalf("recovered %d events, want the full %d", len(recovered.EventPaths), events)
	}
	if len(recovered.OutboxPaths) != events {
		t.Fatalf("recovered %d outbox paths, want %d", len(recovered.OutboxPaths), events)
	}
}

// ---- delta + complete + chunked model ----

func repeatText(n int) string {
	return strings.Repeat("a", n)
}

// claudeLine renders one Claude transcript line. User content is a plain string;
// assistant content is a text-block array, matching real transcripts.
func claudeLine(t *testing.T, id, cwd, role, ts, text string) string {
	t.Helper()
	var content any = text
	if role == "assistant" {
		content = []any{map[string]any{"type": "text", "text": text}}
	}
	rec := map[string]any{
		"type":      role,
		"sessionId": id,
		"timestamp": ts,
		"message":   map[string]any{"role": role, "content": content},
	}
	if cwd != "" {
		rec["cwd"] = cwd
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeClaudeSession writes a Claude transcript of alternating user/assistant turns
// (turn i is user when i is even) with the given per-turn texts, optionally led by
// an ai-title line.
func writeClaudeSession(t *testing.T, path, id, cwd, title string, texts []string) {
	t.Helper()
	var lines []string
	if title != "" {
		lines = append(lines, fmt.Sprintf(`{"type":"ai-title","aiTitle":%q}`, title))
	}
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ts := fmt.Sprintf("2026-07-07T02:%02d:00.000Z", i)
		lines = append(lines, claudeLine(t, id, cwd, role, ts, text))
	}
	writeSessionFile(t, path, strings.Join(lines, "\n")+"\n")
}

type outboxEvent struct {
	name string
	meta map[string]string
	body string
}

// readOutboxEvents reads every outbox .md event, parses its closed frontmatter and
// body, and returns them sorted by filename (the reflect backlog order).
func readOutboxEvents(t *testing.T, app *App) []outboxEvent {
	t.Helper()
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	var evs []outboxEvent
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(app.Paths.Outbox, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		meta, body := splitEventFrontmatter(t, string(content))
		evs = append(evs, outboxEvent{name: e.Name(), meta: meta, body: body})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].name < evs[j].name })
	return evs
}

func splitEventFrontmatter(t *testing.T, content string) (map[string]string, string) {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		t.Fatal("event is missing opening frontmatter")
	}
	rest := content[len("---\n"):]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		t.Fatal("event has unterminated frontmatter")
	}
	meta := map[string]string{}
	for _, line := range strings.Split(rest[:idx], "\n") {
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		meta[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return meta, strings.TrimSpace(rest[idx+len("\n---\n"):])
}

func parseTurnsRange(t *testing.T, value string) (int, int) {
	t.Helper()
	lo, hi, ok := strings.Cut(value, "-")
	if !ok {
		t.Fatalf("turns range %q is not <start>-<end>", value)
	}
	start, err := strconv.Atoi(lo)
	if err != nil {
		t.Fatalf("turns start %q: %v", lo, err)
	}
	end, err := strconv.Atoi(hi)
	if err != nil {
		t.Fatalf("turns end %q: %v", hi, err)
	}
	return start, end
}

// A delta is split into ordered chunks whose bodies stay within the bound, cover
// the whole delta contiguously, and never truncate a turn.
