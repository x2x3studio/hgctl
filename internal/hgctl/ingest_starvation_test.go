package hgctl

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeClaudeTranscript puts a transcript at ~/.claude/projects/<rel>.
func writeClaudeTranscript(t *testing.T, app *App, rel string, lines ...string) string {
	t.Helper()
	path := filepath.Join(app.Paths.Home, ".claude", "projects", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const realTurn = `{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-07-25T00:00:00Z","message":{"content":"a genuine question long enough to clear the minimum user text bar"}}`

// A workflow journal.jsonl carries only started/result records - no conversation
// at all - so it can never produce a session. It used to be dropped WITHOUT a
// ledger marker, which left it permanently eligible. The parse budget takes the
// OLDEST eligible transcripts first, so a pile of them pinned the budget and no
// growing session was ever reached again: measured 81 journals against a budget
// of 8, four hours of sync exiting 0 having ingested nothing, while seven live
// sessions sat on megabytes of unread conversation.
func TestNonConversationTranscriptsCannotStarveIntake(t *testing.T) {
	app := testApp(t)
	t.Setenv("HOME", app.Paths.Home)

	const journals = 12
	for i := 0; i < journals; i++ {
		writeClaudeTranscript(t, app, fmt.Sprintf("p/%d/subagents/workflows/wf/journal.jsonl", i),
			`{"type":"started","id":"x"}`, `{"type":"result","id":"x"}`)
	}
	real := "p/live/session.jsonl"
	writeClaudeTranscript(t, app, real, realTurn)
	// The live session is written last, so it sorts NEWEST and the oldest-first
	// budget reaches it only once the journals stop coming back.
	marks := map[string]ingestMark{}
	parseCap := 4

	seen := false
	for round := 0; round < journals+2; round++ {
		got, skipped, err := app.gatherSessions("claude", marks, 0, parseCap)
		if err != nil {
			t.Fatal(err)
		}
		if len(got)+len(skipped) > parseCap {
			t.Fatalf("round %d parsed %d files, over the budget of %d", round, len(got)+len(skipped), parseCap)
		}
		for _, s := range skipped {
			marks[s.key] = ingestMark{Size: s.size, Turns: 0, IngestedAt: app.Now()}
		}
		for _, c := range got {
			marks[c.key] = ingestMark{Size: c.size, Turns: len(c.session.turns), IngestedAt: app.Now()}
			if c.key == ingestKey("claude", real) {
				seen = true
			}
		}
		if seen {
			return
		}
	}
	t.Fatalf("the live session was never reached in %d rounds; %d journals held the parse budget forever", journals+2, journals)
}

// The marker must not silently swallow a transcript that later becomes real: a
// session too thin to qualify today can grow into one, and it has to be emitted
// whole when it does.
func TestASkippedTranscriptIsIngestedOnceItBecomesReal(t *testing.T) {
	app := testApp(t)
	t.Setenv("HOME", app.Paths.Home)
	rel := "p/thin/session.jsonl"
	path := writeClaudeTranscript(t, app, rel, `{"type":"user","sessionId":"s1","message":{"content":"hi"}}`)
	key := ingestKey("claude", rel)
	marks := map[string]ingestMark{}

	got, skipped, err := app.gatherSessions("claude", marks, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(skipped) != 1 || skipped[0].key != key {
		t.Fatalf("a sub-minimum transcript should be skipped with a marker, got %d candidates / %d skips", len(got), len(skipped))
	}
	marks[key] = ingestMark{Size: skipped[0].size, Turns: 0, IngestedAt: app.Now()}

	// Unchanged: it must not come back and burn the budget again.
	if got, skipped, err = app.gatherSessions("claude", marks, 0, 0); err != nil {
		t.Fatal(err)
	} else if len(got)+len(skipped) != 0 {
		t.Fatal("a marked transcript was reconsidered while unchanged")
	}

	// It grows into a real conversation.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte(realTurn+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err = app.gatherSessions("claude", marks, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a grown transcript was not picked up, got %d candidates", len(got))
	}
	if got[0].prevTurns != 0 {
		t.Fatalf("cursor was %d, want 0 so the whole conversation is emitted", got[0].prevTurns)
	}
}
