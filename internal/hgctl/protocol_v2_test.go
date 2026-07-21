package hgctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const feedbackConformanceManifestSHA256 = "d6ebb24cfdf9fe3d0154ffab4aeb2c0932e034f0c08e58bbbc645e06034fb4ae"

func TestFeedbackV2ConformanceCorpusIsPinnedAndIndependentlyDecoded(t *testing.T) {
	type conformanceCase struct {
		Name    string `json:"name"`
		File    string `json:"file"`
		SHA256  string `json:"sha256"`
		Machine string `json:"machine"`
		Path    string `json:"path"`
		Expect  string `json:"expect"`
		Outcome string `json:"outcome"`
		EventID string `json:"event_id"`
	}
	var manifest struct {
		Protocol     string            `json:"protocol"`
		SourceFile   string            `json:"source_file"`
		SourceSHA256 string            `json:"source_sha256"`
		RerankFile   string            `json:"rerank_file"`
		RerankSHA256 string            `json:"rerank_sha256"`
		Cases        []conformanceCase `json:"cases"`
	}
	root := filepath.Join("testdata", "protocol", "v2")
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if digestBytes(manifestBytes) != feedbackConformanceManifestSHA256 {
		t.Fatal("feedback conformance manifest bytes changed without a protocol update")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Protocol != FeedbackProtocol || len(manifest.Cases) == 0 {
		t.Fatalf("invalid feedback conformance manifest: %#v", manifest)
	}
	for name, want := range map[string]string{
		manifest.SourceFile: manifest.SourceSHA256,
		manifest.RerankFile: manifest.RerankSHA256,
	} {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || digestBytes(content) != want {
			t.Fatalf("fixed corpus file %s changed: err=%v", name, err)
		}
	}
	now := time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC)
	for _, test := range manifest.Cases {
		t.Run(test.Name, func(t *testing.T) {
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(test.File)))
			if err != nil {
				t.Fatal(err)
			}
			if digestBytes(content) != test.SHA256 {
				t.Fatal("fixture bytes do not match their manifest digest")
			}
			event, canonical, decodeErr := decodeCanonicalFeedbackEvent(content, filepath.Base(test.Path), test.Machine, now)
			switch test.Expect {
			case "valid":
				if decodeErr != nil || event.ID != test.EventID || event.Payload.Outcome != test.Outcome || string(canonical) != string(content) {
					t.Fatalf("valid fixture changed: event=%#v err=%v", event, decodeErr)
				}
			case "invalid", "defer":
				if decodeErr == nil {
					t.Fatalf("non-producer fixture %q was accepted", test.Expect)
				}
			default:
				t.Fatalf("unknown fixture expectation %q", test.Expect)
			}
		})
	}
}

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
	usedBytes, _ := canonicalFeedbackEventBytes(used)
	irrelevantBytes, _ := canonicalFeedbackEventBytes(irrelevant)
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
			got, err := canonicalFeedbackEventBytes(event)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "protocol", "v2", "positive", test.file))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("producer bytes differ from frozen fixture:\ngot  %s\nwant %s", got, want)
			}
		})
	}
}

func TestFeedbackV2RejectsInstructionPathsAndExpiredReceipts(t *testing.T) {
	identity := Identity{ID: "00000000-0000-4000-8000-000000000000", Hostname: "host"}
	issuedAt := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"agents.md", "agents.override.md", "claude.md", "skill.md"} {
		t.Run(name, func(t *testing.T) {
			surface := testSurfaceV2(t, identity, issuedAt, "memory/project/"+name)
			rank := 1
			if _, err := newFeedbackEvent(identity, surface, "used", &rank, issuedAt.Add(time.Minute)); err == nil {
				t.Fatalf("instruction path %q was accepted", name)
			}
		})
	}
	surface := testSurfaceV2(t, identity, issuedAt, "memory/project/card.md")
	rank := 1
	event, err := newFeedbackEvent(identity, surface, "used", &rank, issuedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := canonicalFeedbackEventBytes(event)
	name := strings.TrimPrefix(event.ID, "sha256:") + ".json"
	_, _, err = decodeCanonicalFeedbackEvent(content, name, identity.ID, issuedAt.Add(SurfaceLifetime+time.Second))
	if !errors.Is(err, errFeedbackExpired) {
		t.Fatalf("expired feedback error=%v", err)
	}
}

func testSurfaceV2(t *testing.T, identity Identity, issuedAt time.Time, name string) RecallSurface {
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
