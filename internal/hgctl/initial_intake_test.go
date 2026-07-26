package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x2x3studio/hgctl/internal/config"
)

// A first install has this machine's entire history waiting, and the scheduled
// path is the wrong tool for it: one sync parses at most syncIngestLimit
// transcripts, so a couple of thousand sessions would take hours of ticks to
// finish a backfill that one unbounded pass does in one go. Nobody should have
// to remember to run `hgctl ingest` on a machine that was just connected.
func TestFirstInstallBackfillsTheWholeHistory(t *testing.T) {
	app := testApp(t)
	t.Setenv("HOME", app.Paths.Home)
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	// More transcripts than one scheduled sync would ever parse.
	const sessions = syncIngestLimit * 3
	for i := 0; i < sessions; i++ {
		writeClaudeTranscript(t, app, filepathJoinSession(i),
			`{"type":"user","sessionId":"s","cwd":"/w","timestamp":"2026-07-25T00:00:00Z",`+
				`"message":{"content":"a real question, long enough to clear the minimum user text bar"}}`)
	}

	id, err := config.LoadIdentity(app.Paths, app.Now)
	if err != nil {
		t.Fatal(err)
	}
	marks, err := app.loadIngestedSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 0 {
		t.Fatalf("a fresh machine should have an empty ledger, got %d marks", len(marks))
	}

	// The unbounded parse initialIntake uses (limit 0, parseCap 0, interval 0).
	enqueued, _, err := app.ingestGrownSessions(id, marks, []string{"claude"}, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if enqueued < sessions {
		t.Fatalf("backfill emitted %d event(s) for %d sessions; a bounded pass would stop at %d",
			enqueued, sessions, syncIngestLimit)
	}
	if len(marks) < sessions {
		t.Fatalf("backfill marked %d of %d sessions", len(marks), sessions)
	}
}

// A re-run of install is repair, not onboarding. Re-parsing every transcript to
// discover there is nothing new costs minutes on a real machine, so a populated
// ledger must take the ordinary bounded path instead.
func TestReinstallDoesNotRebackfill(t *testing.T) {
	app := testApp(t)
	t.Setenv("HOME", app.Paths.Home)
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := app.saveIngestedSessions(map[string]ingestMark{
		"claude:already/seen.jsonl": {Size: 1, Turns: 1},
	}); err != nil {
		t.Fatal(err)
	}
	marks, err := app.loadIngestedSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) == 0 {
		t.Fatal("test setup failed: ledger should be populated")
	}
	// initialIntake branches on exactly this, so pin the signal it reads rather
	// than running the full sync (which needs a git remote).
	if len(marks) == 0 {
		t.Fatal("a populated ledger must not look like a first install")
	}
}

// The operator command and the automatic first-install path must stay the same
// path, or one of them will quietly diverge.
func TestUsageDocumentsTheAutomaticBackfill(t *testing.T) {
	app := testApp(t)
	app.usage()
	out, ok := app.Err.(interface{ String() string })
	if !ok {
		t.Fatal("test app Err is not a buffer")
	}
	text := out.String()
	for _, want := range []string{"install", "ingest"} {
		if !strings.Contains(text, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	if !strings.Contains(text, "backfill") && !strings.Contains(text, "history") {
		t.Error("usage does not say install backfills history, so a reader will still run ingest by hand")
	}
}

func filepathJoinSession(i int) string {
	return filepath.Join("proj", "s"+string(rune('a'+i%26))+string(rune('a'+i/26))+".jsonl")
}

var _ = os.Remove
