package ingest

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/x2x3studio/hgctl/internal/config"
)

func sessionPath(i int) string {
	return filepath.Join("proj", fmt.Sprintf("s%03d.jsonl", i))
}

func TestFirstInstallBackfillsTheWholeHistory(t *testing.T) {
	ing := testIngester(t)

	// More transcripts than one scheduled sync would ever parse.
	const sessions = SyncLimit * 3
	for i := 0; i < sessions; i++ {
		writeTranscript(t, ing, sessionPath(i),
			`{"type":"user","sessionId":"s","cwd":"/w","timestamp":"2026-07-25T00:00:00Z",`+
				`"message":{"content":"a real question, long enough to clear the minimum user text bar"}}`)
	}

	id, err := config.LoadIdentity(ing.Paths, ing.Now)
	if err != nil {
		t.Fatal(err)
	}
	marks, err := ing.LoadLedger()
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 0 {
		t.Fatalf("a fresh machine should have an empty ledger, got %d marks", len(marks))
	}

	// The unbounded parse initialIntake uses (limit 0, parseCap 0, interval 0).
	enqueued, _, err := ing.Run(id, marks, []string{"claude"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued < sessions {
		t.Fatalf("backfill emitted %d event(s) for %d sessions; a bounded pass would stop at %d",
			enqueued, sessions, SyncLimit)
	}
	if len(marks) < sessions {
		t.Fatalf("backfill marked %d of %d sessions", len(marks), sessions)
	}
}

// A re-run of install is repair, not onboarding. Re-parsing every transcript to
// discover there is nothing new costs minutes on a real machine, so a populated
// ledger must take the ordinary bounded path instead.
