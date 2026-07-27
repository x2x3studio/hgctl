package hgctl

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/x2x3studio/hgctl/internal/config"
)

// The usage text is hgctl's operating contract for the agents that run it: they
// reach for --help mid-task and act on whatever comes back. Two ways it rots - a
// command is added to the switch and never described, or the help flags stop
// working and the caller concludes the binary is broken. Both happened.
func TestHelpFlagsWorkAndUsageDescribesEveryCommand(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		app := testApp(t)
		if code := app.Run(context.Background(), []string{arg}); code != 0 {
			t.Fatalf("%q exited %d, want 0 - a nonzero exit reads as a broken binary", arg, code)
		}
		text := app.Out.(*bytes.Buffer).String() + app.Err.(*bytes.Buffer).String()
		for _, cmd := range []string{"install", "sync", "ingest", "update", "doctor", "version", "uninstall", "hook"} {
			if !strings.Contains(text, cmd) {
				t.Errorf("%q does not describe the %q command", arg, cmd)
			}
		}
		// The throttle is the specific surprise that cost a caller a manual
		// reinstall, so it has to be stated where they will actually read it.
		if !strings.Contains(text, "THROTTLED") {
			t.Errorf("%q does not warn that `sync --update` is throttled", arg)
		}
	}
}

// The metadata file is written on a path the scheduler walks about once a
// minute. If it were a plain write rather than an upsert, the queue's real
// history - the event captures - would be buried under identical commits.
func TestMachineMetaIsAnUpsert(t *testing.T) {
	id := config.Identity{ID: "a943c6d2-e7a3-48a4-a562-849aa8fa0560", Hostname: "Orange"}
	first, err := renderMachineMeta(id)
	if err != nil {
		t.Fatal(err)
	}
	again, err := renderMachineMeta(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(again) {
		t.Fatal("render is not deterministic; every sync would commit")
	}
	for _, want := range []string{id.ID, id.Hostname, `"os"`, `"arch"`, `"hgctl_version"`} {
		if !strings.Contains(string(first), want) {
			t.Errorf("metadata is missing %s:\n%s", want, first)
		}
	}
	changed, err := renderMachineMeta(config.Identity{ID: id.ID, Hostname: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) == string(first) {
		t.Fatal("a hostname change must produce different bytes, or the upsert never fires")
	}
	// No wall-clock field: a heartbeat here would commit every scheduler tick,
	// and git already records liveness in the event commits' own dates.
	for _, banned := range []string{"last_seen", "updated_at", "timestamp", "checked_at"} {
		if strings.Contains(string(first), banned) {
			t.Errorf("metadata carries %q, which would churn a commit every sync", banned)
		}
	}
}

// An unstamped build must announce itself as one. The release stamps a
// timestamp version through -ldflags, and Go SILENTLY IGNORES an -X whose
// package path does not resolve, so "what does an unstamped binary say" is a
// question that gets asked for real whenever this package moves.
//
// "dev" and not a plausible semver: versionIsNewer already treats it as older
// than every release, so an unstamped endpoint repairs itself on the next check
// instead of sitting at a version number nobody ever cut - which a reader
// comparing two machines would take at face value.
func TestUnstampedVersionIsDevAndAlwaysUpdates(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("built-in Version = %q; an unstamped build must say dev, not a version that looks released", Version)
	}
	newer, err := versionIsNewer(Version, "v0.20260726.67057")
	if err != nil {
		t.Fatalf("comparing a dev build against a timestamp release: %v", err)
	}
	if !newer {
		t.Fatal("a dev build did not consider a real release newer; it would never self-repair")
	}
	// The release scheme is v0.<YYYYMMDD>.<second-of-day>, which must keep
	// comparing correctly as dates advance.
	older, err := versionIsNewer("v0.20260726.67057", "v0.20260727.100")
	if err != nil || !older {
		t.Fatalf("a later timestamp release was not newer: %v %v", older, err)
	}
	same, err := versionIsNewer("v0.20260727.100", "v0.20260726.67057")
	if err != nil || same {
		t.Fatalf("an earlier release was treated as newer: %v %v", same, err)
	}
}
