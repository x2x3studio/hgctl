package pipeline

import (
	"bytes"
	"fmt"
	"testing"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	testTree    = "89abcdef0123456789abcdef0123456789abcdef"
	testDigest  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testMachine = "123e4567-e89b-42d3-a456-426614174000"
)

func TestControlManifestRoundTrip(t *testing.T) {
	evidencePath := ".hourglass-runtime/incoming/" + testMachine + "/" + testDigest + ".json"
	manifest := ControlManifest{
		Schema: ControlSchema, Repository: "x2x3studio/hourglass", ControlSHA: testCommit,
		RunID: "29795588883", RunAttempt: 1, Shared: Revision{Commit: testCommit, Tree: testTree},
		QueueTips: []QueueTip{{Machine: testMachine, Commit: testCommit}},
		Events: []SelectedEvent{{
			Machine: testMachine, ID: testDigest, QueueCommit: testCommit,
			QueuePath: "events/2026/07/" + testDigest + ".json", Blob: testTree,
			ArtifactPath: evidencePath, SHA256: testDigest, Bytes: 100,
		}},
		Cursors:  []CursorOperation{{Machine: testMachine, Commit: testCommit}},
		Baseline: []FileRecord{{Path: "Home.md", SHA256: testDigest, Bytes: 100}},
		Evidence: []FileRecord{{Path: evidencePath, SHA256: testDigest, Bytes: 100}},
		Prompt:   FileRecord{Path: "prompt.md", SHA256: testDigest, Bytes: 100},
	}
	content, err := EncodeControl(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeControl(content)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RunID != manifest.RunID || len(decoded.Events) != 1 {
		t.Fatalf("unexpected decoded manifest: %#v", decoded)
	}

	for name, mutation := range map[string][]byte{
		"unknown field": bytes.Replace(content, []byte(`"schema":`), []byte(`"extra":true,"schema":`), 1),
		"whitespace":    append([]byte(" "), content...),
		"trailing":      append(append([]byte(nil), content...), []byte("{}")...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeControl(mutation); err == nil {
				t.Fatalf("accepted noncanonical manifest: %s", mutation)
			}
		})
	}
}

func TestPublicationManifestRequiresSortedBoundedFiles(t *testing.T) {
	manifest := PublicationManifest{
		Schema: PublicationSchema, Repository: "x2x3studio/hourglass", ControlSHA: testCommit,
		RunID: "1", RunAttempt: 1, Shared: Revision{Commit: testCommit, Tree: testTree},
		Files: []FileRecord{{Path: "Home.md", SHA256: testDigest, Bytes: 1}},
	}
	if _, err := EncodePublication(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Files = append(manifest.Files, FileRecord{Path: ".hourglass/cursors/" + testMachine, SHA256: testDigest, Bytes: 41})
	if _, err := EncodePublication(manifest); err == nil {
		t.Fatal("accepted unsorted publication files")
	}
}

func TestPublicationManifestWorstCaseFileBoundary(t *testing.T) {
	manifest := PublicationManifest{
		Schema: PublicationSchema, Repository: "x2x3studio/hourglass", ControlSHA: testCommit,
		RunID: "1", RunAttempt: 1, Shared: Revision{Commit: testCommit, Tree: testTree},
		Files: make([]FileRecord, 0, maxPublicationFiles+1),
	}
	for index := 0; index < maxPublicationFiles; index++ {
		manifest.Files = append(manifest.Files, FileRecord{
			Path: fmt.Sprintf("generated/%03d", index), SHA256: testDigest, Bytes: 0,
		})
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("worst-case publication boundary was rejected: %v", err)
	}
	manifest.Files = append(manifest.Files, FileRecord{
		Path: fmt.Sprintf("generated/%03d", maxPublicationFiles), SHA256: testDigest, Bytes: 0,
	})
	if err := manifest.Validate(); err == nil {
		t.Fatal("publication accepted one file beyond the worst-case boundary")
	}
}

func TestControlManifestBindsOneMachinePerSemanticBatch(t *testing.T) {
	evidencePath := ".hourglass-runtime/incoming/" + testMachine + "/" + testDigest + ".json"
	manifest := ControlManifest{
		Schema: ControlSchema, Repository: "x2x3studio/hourglass", ControlSHA: testCommit,
		RunID: "1", RunAttempt: 1, Shared: Revision{Commit: testCommit, Tree: testTree},
		Events: []SelectedEvent{
			{Machine: testMachine, ID: testDigest, QueueCommit: testCommit, QueuePath: "events/2026/07/" + testDigest + ".json", Blob: testTree, ArtifactPath: evidencePath, SHA256: testDigest, Bytes: 1},
			{Machine: "123e4567-e89b-42d3-b456-426614174000", ID: "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", QueueCommit: testCommit, QueuePath: "events/2026/07/1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.json", Blob: testTree, ArtifactPath: ".hourglass-runtime/incoming/123e4567-e89b-42d3-b456-426614174000/1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.json", SHA256: testDigest, Bytes: 1},
		},
		Evidence: []FileRecord{{Path: evidencePath, SHA256: testDigest, Bytes: 1}, {Path: ".hourglass-runtime/incoming/123e4567-e89b-42d3-b456-426614174000/1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.json", SHA256: testDigest, Bytes: 1}},
		Prompt:   FileRecord{Path: "prompt.md", SHA256: testDigest, Bytes: 1},
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("accepted a cross-machine semantic batch")
	}
}
