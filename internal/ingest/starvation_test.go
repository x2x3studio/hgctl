package ingest

import (
	"fmt"
	"os"
	"testing"
)

// A workflow journal.jsonl carries only started/result records - no conversation
// at all - so it can never produce a session. It used to be dropped WITHOUT a
// ledger marker, which left it permanently eligible. The parse budget takes the
// OLDEST eligible transcripts first, so a pile of them pinned the budget and no
// growing session was ever reached again: measured 81 journals against a budget
// of 8, four hours of sync exiting 0 having ingested nothing, while seven live
// sessions sat on megabytes of unread conversation.
func TestNonConversationTranscriptsCannotStarveIntake(t *testing.T) {
	ing := testIngester(t)

	const journals = 12
	for i := 0; i < journals; i++ {
		writeTranscript(t, ing, fmt.Sprintf("p/%d/subagents/workflows/wf/journal.jsonl", i),
			`{"type":"started","id":"x"}`, `{"type":"result","id":"x"}`)
	}
	real := "p/live/session.jsonl"
	writeTranscript(t, ing, real, realTurn)
	// The live session is written last, so it sorts NEWEST and the oldest-first
	// budget reaches it only once the journals stop coming back.
	marks := map[string]Mark{}
	parseCap := 4

	seen := false
	for round := 0; round < journals+2; round++ {
		got, skipped, err := ing.gatherSessions("claude", marks, 0, parseCap)
		if err != nil {
			t.Fatal(err)
		}
		if len(got)+len(skipped) > parseCap {
			t.Fatalf("round %d parsed %d files, over the budget of %d", round, len(got)+len(skipped), parseCap)
		}
		for _, s := range skipped {
			marks[s.key] = Mark{Size: s.size, Turns: 0, IngestedAt: ing.Now()}
		}
		for _, c := range got {
			marks[c.key] = Mark{Size: c.size, Turns: len(c.session.turns), IngestedAt: ing.Now()}
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
	ing := testIngester(t)
	rel := "p/thin/session.jsonl"
	path := writeTranscript(t, ing, rel, `{"type":"user","sessionId":"s1","message":{"content":"hi"}}`)
	key := ingestKey("claude", rel)
	marks := map[string]Mark{}

	got, skipped, err := ing.gatherSessions("claude", marks, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(skipped) != 1 || skipped[0].key != key {
		t.Fatalf("a sub-minimum transcript should be skipped with a marker, got %d candidates / %d skips", len(got), len(skipped))
	}
	marks[key] = Mark{Size: skipped[0].size, Turns: 0, IngestedAt: ing.Now()}

	// Unchanged: it must not come back and burn the budget again.
	if got, skipped, err = ing.gatherSessions("claude", marks, 0, 0); err != nil {
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
	got, _, err = ing.gatherSessions("claude", marks, 0, 0)
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
