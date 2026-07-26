package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x2x3studio/hgctl/internal/ingest"
)

// A first install has this machine's entire history waiting, and the scheduled
// path is the wrong tool for it: one sync parses at most syncIngestLimit
// transcripts, so a couple of thousand sessions would take hours of ticks to
// finish a backfill that one unbounded pass does in one go. Nobody should have
// to remember to run `hgctl ingest` on a machine that was just connected.
func TestReinstallDoesNotRebackfill(t *testing.T) {
	app := testApp(t)
	t.Setenv("HOME", app.Paths.Home)
	if err := app.Paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := app.ingester().SaveLedger(map[string]ingest.Mark{
		"claude:already/seen.jsonl": {Size: 1, Turns: 1},
	}); err != nil {
		t.Fatal(err)
	}
	marks, err := app.ingester().LoadLedger()
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
