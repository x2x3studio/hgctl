package hgctl

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFeedbackProducerIdentityExcludesAssessmentButBytesDoNot(t *testing.T) {
	identity := Identity{ID: "00000000-0000-4000-8000-000000000000", Hostname: "fixture-host"}
	issuedAt := time.Date(2026, 7, 21, 0, 59, 0, 0, time.UTC)
	surface := RecallSurface{
		Schema: SurfaceProtocol, Nonce: strings.Repeat("a", 32), IssuedAt: issuedAt,
		MachineID: identity.ID, Client: "codex", Origin: "explicit",
		Shared:  SharedRevision{Commit: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)},
		Results: []SurfaceResult{{Rank: 1, Path: "memory/project/decision.md", Blob: strings.Repeat("3", 40)}},
	}
	var err error
	surface.ID, err = recallSurfaceID(surface)
	if err != nil {
		t.Fatal(err)
	}
	rank := 1
	used, err := newFeedbackEvent(identity, surface, "used", &rank, issuedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	irrelevant, err := newFeedbackEvent(identity, surface, "irrelevant", &rank, issuedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	usedBytes, _ := canonicalEventBytes(used)
	irrelevantBytes, _ := canonicalEventBytes(irrelevant)
	if used.ID != irrelevant.ID || string(usedBytes) == string(irrelevantBytes) {
		t.Fatal("one surface did not converge on one event identity and distinct assessment bytes")
	}
}

func TestFeedbackProducerExactlyRecreatesPositiveCorpusBytes(t *testing.T) {
	identity := Identity{ID: "00000000-0000-4000-8000-000000000000", Hostname: "fixture-host"}
	issuedAt := time.Date(2026, 7, 21, 0, 59, 0, 0, time.UTC)
	capturedAt := time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC)
	cardRank := 1
	tests := []struct {
		name    string
		file    string
		results []SurfaceResult
		outcome string
		result  *int
	}{
		{
			name: "card feedback", file: "card-feedback.json", outcome: "used", result: &cardRank,
			results: []SurfaceResult{{Rank: 1, Path: "memory/project/decision.md", Blob: strings.Repeat("3", 40)}},
		},
		{name: "zero hit", file: "zero-hit.json", outcome: "zero_hit", results: []SurfaceResult{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			surface := RecallSurface{
				Schema: SurfaceProtocol, Nonce: strings.Repeat("a", 32), IssuedAt: issuedAt,
				MachineID: identity.ID, Client: "codex", Origin: "explicit",
				Shared:  SharedRevision{Commit: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)},
				Results: test.results,
			}
			var err error
			surface.ID, err = recallSurfaceID(surface)
			if err != nil {
				t.Fatal(err)
			}
			event, err := newFeedbackEvent(identity, surface, test.outcome, test.result, capturedAt)
			if err != nil {
				t.Fatal(err)
			}
			got, err := canonicalEventBytes(event)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "protocol", "event", "positive", test.file))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("producer bytes differ from frozen fixture:\ngot  %s\nwant %s", got, want)
			}
		})
	}
}

func TestFeedbackRejectsInstructionPathsAndExpiredReceipts(t *testing.T) {
	identity := Identity{ID: "00000000-0000-4000-8000-000000000000", Hostname: "host"}
	issuedAt := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"agents.md", "agents.override.md", "claude.md", "skill.md"} {
		t.Run(name, func(t *testing.T) {
			surface := testSurface(t, identity, issuedAt, "memory/project/"+name)
			rank := 1
			if _, err := newFeedbackEvent(identity, surface, "used", &rank, issuedAt.Add(time.Minute)); err == nil {
				t.Fatalf("instruction path %q was accepted", name)
			}
		})
	}
	surface := testSurface(t, identity, issuedAt, "memory/project/card.md")
	rank := 1
	event, err := newFeedbackEvent(identity, surface, "used", &rank, issuedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := canonicalEventBytes(event)
	name := strings.TrimPrefix(event.ID, "sha256:") + ".json"
	_, _, err = decodeCanonicalEvent(content, name, identity.ID, issuedAt.Add(SurfaceLifetime+time.Second))
	if !errors.Is(err, errFeedbackExpired) {
		t.Fatalf("expired feedback error=%v", err)
	}
}

func testSurface(t *testing.T, identity Identity, issuedAt time.Time, name string) RecallSurface {
	t.Helper()
	surface := RecallSurface{
		Schema: SurfaceProtocol, Nonce: strings.Repeat("a", 32), IssuedAt: issuedAt,
		MachineID: identity.ID, Client: "codex", Origin: "explicit",
		Shared:  SharedRevision{Commit: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)},
		Results: []SurfaceResult{{Rank: 1, Path: name, Blob: strings.Repeat("3", 40)}},
	}
	var err error
	surface.ID, err = recallSurfaceID(surface)
	if err != nil {
		t.Fatal(err)
	}
	return surface
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
