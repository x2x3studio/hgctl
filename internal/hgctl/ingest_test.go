package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestExtractClaudeSessionUsesEarliestTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-sess-1.jsonl")
	writeSessionFile(t, path, strings.Join([]string{
		`{"type":"user","sessionId":"claude-sess-1","cwd":"/tmp/proj","timestamp":"2026-07-07T02:00:48.505Z","message":{"role":"user","content":"First real question about the parser design here."}}`,
		`{"type":"assistant","sessionId":"claude-sess-1","timestamp":"2026-07-07T02:01:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Here is the design."}]}}`,
		`{"type":"user","sessionId":"claude-sess-1","timestamp":"2026-07-07T02:05:00.000Z","message":{"role":"user","content":"Follow-up<system-reminder>ignore me</system-reminder> with enough length to keep it."}}`,
	}, "\n")+"\n")

	session, ok := extractClaudeSession(path)
	if !ok {
		t.Fatal("expected session to qualify")
	}
	if session.id != "claude-sess-1" {
		t.Fatalf("id = %q", session.id)
	}
	// The earliest message timestamp, not the last, so the backlog orders by
	// when the session began.
	if session.firstTS != "2026-07-07T02:00:48.505Z" {
		t.Fatalf("firstTS = %q, want the earliest message time", session.firstTS)
	}
	if len(session.turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(session.turns))
	}
	if strings.Contains(session.render(), "system-reminder") {
		t.Fatalf("system-reminder boilerplate not stripped:\n%s", session.render())
	}
}

func TestExtractCodexSessionMessageExtraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2026", "06", "05", "rollout-2026-06-05T11-41-12-019e95de-e099-7200-abb4-afc33f21aea3.jsonl")
	writeSessionFile(t, path, strings.Join([]string{
		`{"timestamp":"2026-06-05T03:41:12.298Z","type":"session_meta","payload":{"id":"019e95de-e099-7200-abb4-afc33f21aea3","cwd":"/tmp/x"}}`,
		`{"timestamp":"2026-06-05T03:41:12.394Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/tmp/x</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-06-05T03:41:20.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Please refactor the auth module and add tests for the login path."}]}}`,
		`{"timestamp":"2026-06-05T03:42:00.000Z","type":"response_item","payload":{"type":"reasoning","content":[{"type":"reasoning_text","text":"thinking about it"}]}}`,
		`{"timestamp":"2026-06-05T03:42:10.000Z","type":"token_count","payload":{"total":123}}`,
		`{"timestamp":"2026-06-05T03:42:20.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"ls"}}`,
		`{"timestamp":"2026-06-05T03:42:53.021Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Refactored auth and added login tests."}],"phase":"final_answer"}}`,
		`{"timestamp":"2026-06-05T03:42:55.000Z","type":"event_msg","payload":{"type":"agent_message","message":"noise"}}`,
	}, "\n")+"\n")

	session, ok := extractCodexSession(path)
	if !ok {
		t.Fatal("expected codex session to qualify")
	}
	if session.id != "019e95de-e099-7200-abb4-afc33f21aea3" {
		t.Fatalf("id = %q", session.id)
	}
	if session.firstTS != "2026-06-05T03:41:12.298Z" {
		t.Fatalf("firstTS = %q, want the first line timestamp", session.firstTS)
	}
	// The environment_context user turn is boilerplate and drops out; only the
	// real user input_text and the assistant output_text survive.
	if len(session.turns) != 2 {
		t.Fatalf("turns = %d (%v), want 2 (user + assistant)", len(session.turns), session.turns)
	}
	if session.turns[0].role != "user" || !strings.Contains(session.turns[0].text, "refactor the auth module") {
		t.Fatalf("first turn = %+v", session.turns[0])
	}
	if session.turns[1].role != "assistant" || !strings.Contains(session.turns[1].text, "Refactored auth") {
		t.Fatalf("second turn = %+v", session.turns[1])
	}
	body := session.render()
	for _, noise := range []string{"environment_context", "thinking about it", "agent_message", "function_call"} {
		if strings.Contains(body, noise) {
			t.Fatalf("noise %q leaked into body:\n%s", noise, body)
		}
	}
}

func TestCodexIDFromPathFallback(t *testing.T) {
	got := codexIDFromPath("/x/.codex/sessions/2026/06/05/rollout-2026-06-05T11-41-12-019e95de-e099-7200-abb4-afc33f21aea3.jsonl")
	if got != "019e95de-e099-7200-abb4-afc33f21aea3" {
		t.Fatalf("codexIDFromPath = %q", got)
	}
}

func TestSortIngestCandidatesOldestFirst(t *testing.T) {
	early := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	candidates := []ingestCandidate{
		{client: "claude", id: "late", captured: late},
		{client: "codex", id: "tie-b", captured: mid},
		{client: "claude", id: "tie-a", captured: mid},
		{client: "codex", id: "early", captured: early},
	}
	sortIngestCandidates(candidates)
	order := make([]string, len(candidates))
	for i, c := range candidates {
		order[i] = c.id
	}
	// Oldest first; equal times break deterministically by client then id
	// (claude < codex).
	want := []string{"early", "tie-a", "tie-b", "late"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestCapturedTimeEncodedInEventFilename(t *testing.T) {
	captured, ok := parseIngestTime("2026-06-05T03:41:12.298Z")
	if !ok {
		t.Fatal("parseIngestTime failed on fractional-second RFC3339")
	}
	name := rawEvent{CapturedAt: captured}.filename()
	// The reflect backlog orders by this filename timestamp, so it must encode
	// the real session time, not the ingest run time.
	if !strings.HasPrefix(name, "20260605T034112Z-") {
		t.Fatalf("filename = %q, want the real session time prefix", name)
	}
}

func TestIngestKeyIsClientNamespaced(t *testing.T) {
	if ingestKey("claude", "abc") == ingestKey("codex", "abc") {
		t.Fatal("claude and codex keys collided for the same id")
	}
	if got := normalizeIngestKey("bare-legacy-id"); got != "claude:bare-legacy-id" {
		t.Fatalf("legacy migration = %q, want claude-namespaced", got)
	}
	if got := normalizeIngestKey("codex:x"); got != "codex:x" {
		t.Fatalf("already-namespaced key mutated to %q", got)
	}
}

// A mid-loop interruption in runIngest re-processes already-enqueued sessions on
// the next run. With a session-derived deterministic filename, re-enqueue must
// overwrite the same outbox file rather than pile up duplicates under fresh
// random names (which would waste reflect compute downstream).
func TestReenqueueWithDedupCollapsesInsteadOfDuplicating(t *testing.T) {
	app := testApp(t)
	if err := os.MkdirAll(app.Paths.Outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2026, 6, 5, 3, 41, 12, 0, time.UTC)
	ev := rawEvent{CapturedAt: captured, Client: "claude", Machine: "m", Body: "session body", Dedup: ingestKey("claude", "sess-1")}
	for i := 0; i < 3; i++ {
		if err := app.enqueue(ev); err != nil {
			t.Fatal(err)
		}
	}
	md := 0
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			md++
		}
	}
	if md != 1 {
		t.Fatalf("re-enqueue left %d outbox files, want 1 (deterministic dedup)", md)
	}

	// Same session id + client at the same second is stable; a different session
	// at that second gets a distinct suffix; steady-state (no Dedup) stays random.
	if ev.filename() != (rawEvent{CapturedAt: captured, Body: "grown body", Dedup: ingestKey("claude", "sess-1")}).filename() {
		t.Fatal("same session produced different filenames")
	}
	if ev.filename() == (rawEvent{CapturedAt: captured, Dedup: ingestKey("claude", "sess-2")}).filename() {
		t.Fatal("distinct sessions collided on filename")
	}
	if (rawEvent{CapturedAt: captured}).filename() == (rawEvent{CapturedAt: captured}).filename() {
		t.Fatal("expected a random suffix for a capture without Dedup")
	}
}

func TestLoadIngestedSessionsMigratesLegacyLedger(t *testing.T) {
	app := testApp(t)
	if err := writeJSONAtomic(app.ingestedSessionsPath(), []string{"legacy-claude", "codex:known"}, 0o600); err != nil {
		t.Fatal(err)
	}
	seen, err := app.loadIngestedSessions()
	if err != nil {
		t.Fatal(err)
	}
	if !seen["claude:legacy-claude"] {
		t.Fatal("legacy bare id was not migrated to the claude namespace")
	}
	if !seen["codex:known"] {
		t.Fatal("namespaced codex id was lost on load")
	}
}

func TestGatherSessionsIsClientNamespaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A Claude and a Codex session that happen to share the same id string.
	writeSessionFile(t, filepath.Join(home, ".claude", "projects", "proj", "shared.jsonl"),
		`{"type":"user","sessionId":"shared-id","timestamp":"2026-07-07T02:00:00.000Z","message":{"role":"user","content":"A Claude question long enough to qualify for ingest here."}}`+"\n")
	writeSessionFile(t, filepath.Join(home, ".codex", "sessions", "2026", "07", "07", "rollout-2026-07-07T10-00-00-shared.jsonl"),
		strings.Join([]string{
			`{"timestamp":"2026-07-07T02:30:00.000Z","type":"session_meta","payload":{"id":"shared-id"}}`,
			`{"timestamp":"2026-07-07T02:30:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"A Codex question long enough to qualify for ingest here."}]}}`,
		}, "\n")+"\n")

	app := testApp(t)
	// Claude id already ingested; the codex sibling must not be treated as seen.
	seen := map[string]bool{ingestKey("claude", "shared-id"): true}

	claude, err := app.gatherSessions("claude", seen)
	if err != nil {
		t.Fatal(err)
	}
	if len(claude) != 0 {
		t.Fatalf("claude sessions = %d, want 0 (already ingested)", len(claude))
	}
	codex, err := app.gatherSessions("codex", seen)
	if err != nil {
		t.Fatal(err)
	}
	if len(codex) != 1 || codex[0].client != "codex" || codex[0].id != "shared-id" {
		t.Fatalf("codex sessions = %+v, want one un-deduped codex candidate", codex)
	}
	// Its captured time comes from the source line, not the ingest clock.
	if !codex[0].captured.Equal(time.Date(2026, 7, 7, 2, 30, 0, 0, time.UTC)) {
		t.Fatalf("codex captured = %s, want the session_meta timestamp", codex[0].captured)
	}
}

func TestCopyOutboxBatchBoundsAreOptIn(t *testing.T) {
	app := testApp(t)
	if err := os.MkdirAll(filepath.Join(app.Paths.Queue, "events"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		captured := time.Date(2026, 6, 5, 0, 0, i, 0, time.UTC)
		if err := app.enqueue(rawEvent{CapturedAt: captured, Client: "codex", Machine: "m", Body: "event body long enough"}); err != nil {
			t.Fatal(err)
		}
	}
	bounded, err := app.copyOutboxBatch(MaxSyncEvents, MaxSyncBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded.EventPaths) != MaxSyncEvents {
		t.Fatalf("steady-state batch = %d, want the MaxSyncEvents bound %d", len(bounded.EventPaths), MaxSyncEvents)
	}
	unbounded, err := app.copyOutboxBatch(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded.EventPaths) != 9 {
		t.Fatalf("bulk batch = %d, want all 9 events", len(unbounded.EventPaths))
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

	id, err := app.loadIdentity()
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
	state := State{RepoURL: origin, QueueBranch: branch}

	const events = 9 // more than MaxSyncEvents, so a steady-state sync could not do this in one push
	for i := 0; i < events; i++ {
		captured := time.Date(2026, 6, 5, 0, 0, i, 0, time.UTC)
		if err := app.enqueue(rawEvent{CapturedAt: captured, Client: "codex", Machine: "m", Body: "backlog event body"}); err != nil {
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
	if err := app.saveState(State{RepoURL: "x", QueueBranch: "queue/not-this-machine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.bulkPublishQueue(testContext(t)); err == nil {
		t.Fatal("expected a branch/identity mismatch error")
	}
}

// A bulk ingest stages up to bulkQueueCommitChunk events before committing, so an
// interruption after copy/stage but before commit leaves more than MaxSyncEvents
// events in the queue worktree. Recovery must fold the whole validated batch, not
// wedge on the steady-state caps.
func TestRecoverInterruptedQueueBatchExceedsSteadyStateCaps(t *testing.T) {
	app, _, _ := setupBulkQueue(t)

	const events = 9 // more than MaxSyncEvents
	for i := 0; i < events; i++ {
		captured := time.Date(2026, 6, 5, 0, 0, i, 0, time.UTC)
		if err := app.enqueue(rawEvent{CapturedAt: captured, Client: "codex", Machine: "m", Body: "interrupted batch body"}); err != nil {
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
		t.Fatalf("recovery of a >MaxSyncEvents interrupted batch failed: %v", err)
	}
	if len(recovered.EventPaths) != events {
		t.Fatalf("recovered %d events, want the full %d", len(recovered.EventPaths), events)
	}
	if len(recovered.OutboxPaths) != events {
		t.Fatalf("recovered %d outbox paths, want %d", len(recovered.OutboxPaths), events)
	}
}
