package pipeline

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const finalizeCurrentID = "1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type finalizeFixture struct {
	controlRoot     string
	modelRoot       string
	publicationRoot string
	control         ControlManifest
	baseline        map[string][]byte
	prompt          []byte
	evidence        []byte
	newNotePath     string
	newNote         []byte
}

func TestFinalizeProducesSanitizedPublication(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	manifest, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot)
	if err != nil {
		t.Fatal(err)
	}

	wantPaths := []string{
		".hourglass/cursors/" + testMachine,
		".hourglass/seen/" + finalizeCurrentID[:2] + ".json",
		fixture.newNotePath,
	}
	if got := publicationPaths(manifest); strings.Join(got, "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("publication paths=%v, want %v", got, wantPaths)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(fixture.publicationRoot, PublicationManifestName))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePublication(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(publicationPaths(decoded), "\n") != strings.Join(wantPaths, "\n") {
		t.Fatalf("decoded publication paths=%v", publicationPaths(decoded))
	}
	note, err := os.ReadFile(filepath.Join(fixture.publicationRoot, PublicationFilesDirectory, filepath.FromSlash(fixture.newNotePath)))
	if err != nil || string(note) != string(fixture.newNote) {
		t.Fatalf("sanitized note=%q err=%v", note, err)
	}
	for _, forbidden := range []string{"prompt.md", "workspace", ".hourglass-runtime"} {
		if _, err := os.Lstat(filepath.Join(fixture.publicationRoot, forbidden)); !os.IsNotExist(err) {
			t.Fatalf("raw model material entered publication: %s", forbidden)
		}
	}
	assertPublicationModes(t, fixture.publicationRoot)
}

func TestFinalizeAllowsCanvasReferenceToNewMemory(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	canvas := `{"nodes":[{"id":"note","type":"file","file":"` + fixture.newNotePath + `","x":0,"y":0,"width":300,"height":200}],"edges":[]}`
	writeFinalizeFile(t, fixture.modelRoot, "workspace/Hourglass.canvas", canvas)

	manifest, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot)
	if err != nil {
		t.Fatal(err)
	}
	paths := publicationPaths(manifest)
	if !containsString(paths, "Hourglass.canvas") || !containsString(paths, fixture.newNotePath) {
		t.Fatalf("publication paths=%v", paths)
	}
}

func TestFinalizeAllowsCanvasOnlyTopologyRepair(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	if err := os.Remove(filepath.Join(fixture.modelRoot, "workspace", filepath.FromSlash(fixture.newNotePath))); err != nil {
		t.Fatal(err)
	}
	writeFinalizeFile(t, fixture.modelRoot, "workspace/Hourglass.canvas", `{"nodes":[{"id":"topology","type":"text","text":"Current topology","x":0,"y":0,"width":300,"height":200}],"edges":[]}`)

	manifest, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := publicationPaths(manifest); !containsString(got, "Hourglass.canvas") || containsString(got, fixture.newNotePath) {
		t.Fatalf("publication paths=%v", got)
	}
}

func TestFinalizeAllowsHomeWithCanvasTopologyRepair(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	if err := os.Remove(filepath.Join(fixture.modelRoot, "workspace", filepath.FromSlash(fixture.newNotePath))); err != nil {
		t.Fatal(err)
	}
	writeFinalizeFile(t, fixture.modelRoot, "workspace/Home.md", "# Current agent entry point\n")
	writeFinalizeFile(t, fixture.modelRoot, "workspace/Hourglass.canvas", `{"nodes":[{"id":"topology","type":"text","text":"Current topology","x":0,"y":0,"width":300,"height":200}],"edges":[]}`)

	manifest, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot)
	if err != nil {
		t.Fatal(err)
	}
	paths := publicationPaths(manifest)
	if !containsString(paths, "Home.md") || !containsString(paths, "Hourglass.canvas") || containsString(paths, fixture.newNotePath) {
		t.Fatalf("publication paths=%v", paths)
	}
}

func TestFinalizeAllowsHomeOnlyChange(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	if err := os.Remove(filepath.Join(fixture.modelRoot, "workspace", filepath.FromSlash(fixture.newNotePath))); err != nil {
		t.Fatal(err)
	}
	writeFinalizeFile(t, fixture.modelRoot, "workspace/Home.md", "# Changed navigation\n")

	manifest, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := publicationPaths(manifest); !containsString(got, "Home.md") || containsString(got, fixture.newNotePath) {
		t.Fatalf("publication paths=%v", got)
	}
}

func TestFinalizeTerminalOperationsNeedNoModelArtifact(t *testing.T) {
	fixture := newFinalizeFixture(t, false)
	fixture.control.Cursors = []CursorOperation{{Machine: testMachine, Commit: testCommit}}
	fixture.control.Rejections = []RejectionOperation{{Machine: testMachine, Commit: testCommit, Reason: "merge-commit"}}
	writeControlManifest(t, fixture)

	manifest, err := Finalize("", fixture.controlRoot, fixture.publicationRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".hourglass/cursors/" + testMachine,
		".hourglass/rejected/" + testCommit[:2] + ".json",
	}
	if got := publicationPaths(manifest); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("publication paths=%v, want %v", got, want)
	}
	rejectionPath := filepath.Join(fixture.publicationRoot, PublicationFilesDirectory, ".hourglass", "rejected", testCommit[:2]+".json")
	rejection, err := os.ReadFile(rejectionPath)
	if err != nil {
		t.Fatalf("invalid rejection receipt %q: %v", rejection, err)
	}
	_, entries, err := decodeRejectionShard(".hourglass/rejected/"+testCommit[:2]+".json", rejection)
	if err != nil || entries[rejectionKey(testMachine, testCommit)].Reason != "merge-commit" {
		t.Fatalf("invalid rejection shard %q: %v", rejection, err)
	}
	assertPublicationModes(t, fixture.publicationRoot)
}

func TestFinalizeMergesExistingControlLedgerShards(t *testing.T) {
	t.Run("seen", func(t *testing.T) {
		fixture := newFinalizeFixture(t, true)
		existing := "11" + strings.Repeat("a", 62)
		for name := range fixture.baseline {
			if _, ok := seenShardName(name); ok {
				delete(fixture.baseline, name)
			}
		}
		for name, content := range seenLedgerFiles(t, map[string]string{
			testDigest: testMachine,
			existing:   "123e4567-e89b-42d3-b456-426614174001",
		}) {
			fixture.baseline[name] = content
		}
		fixture.control.Baseline = recordsForFiles(fixture.baseline)
		if err := os.RemoveAll(filepath.Join(fixture.controlRoot, ".hourglass")); err != nil {
			t.Fatal(err)
		}
		writeControlManifest(t, fixture)

		if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err != nil {
			t.Fatal(err)
		}
		name := ".hourglass/seen/" + finalizeCurrentID[:2] + ".json"
		content, err := os.ReadFile(filepath.Join(fixture.publicationRoot, PublicationFilesDirectory, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		_, entries, err := decodeSeenShard(name, content)
		if err != nil || entries[existing] == "" || entries[finalizeCurrentID] != testMachine {
			t.Fatalf("merged seen shard = %#v, err=%v", entries, err)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		fixture := newFinalizeFixture(t, false)
		existingCommit := "01" + strings.Repeat("f", 38)
		existing := rejectionEntry{Machine: testMachine, Commit: existingCommit, Reason: "invalid-event"}
		content, err := encodeRejectionShard("01", map[string]rejectionEntry{
			rejectionKey(existing.Machine, existing.Commit): existing,
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.baseline[".hourglass/rejected/01.json"] = content
		fixture.control.Baseline = recordsForFiles(fixture.baseline)
		fixture.control.Rejections = []RejectionOperation{{Machine: testMachine, Commit: testCommit, Reason: "merge-commit"}}
		writeControlManifest(t, fixture)

		if _, err := Finalize("", fixture.controlRoot, fixture.publicationRoot); err != nil {
			t.Fatal(err)
		}
		name := ".hourglass/rejected/01.json"
		published, err := os.ReadFile(filepath.Join(fixture.publicationRoot, PublicationFilesDirectory, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		_, entries, err := decodeRejectionShard(name, published)
		if err != nil || entries[rejectionKey(existing.Machine, existing.Commit)].Reason != "invalid-event" ||
			entries[rejectionKey(testMachine, testCommit)].Reason != "merge-commit" {
			t.Fatalf("merged rejection shard = %#v, err=%v", entries, err)
		}
	})
}

func TestFinalizeRejectsUntrustedModelArtifact(t *testing.T) {
	tests := map[string]func(*testing.T, *finalizeFixture){
		"changed prompt": func(t *testing.T, fixture *finalizeFixture) {
			writeFinalizeFile(t, fixture.modelRoot, "prompt.md", "changed\n")
		},
		"changed evidence": func(t *testing.T, fixture *finalizeFixture) {
			writeFinalizeFile(t, fixture.modelRoot, "workspace/"+fixture.control.Events[0].ArtifactPath, "changed\n")
		},
		"deleted baseline": func(t *testing.T, fixture *finalizeFixture) {
			if err := os.Remove(filepath.Join(fixture.modelRoot, "workspace", "memory", "system", "existing.md")); err != nil {
				t.Fatal(err)
			}
		},
		"extra file": func(t *testing.T, fixture *finalizeFixture) {
			writeFinalizeFile(t, fixture.modelRoot, "workspace/notes.txt", "extra\n")
		},
		"empty directory": func(t *testing.T, fixture *finalizeFixture) {
			if err := os.MkdirAll(filepath.Join(fixture.modelRoot, "workspace", "memory", "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, fixture *finalizeFixture) {
			name := filepath.Join(fixture.modelRoot, "workspace", "memory", "link.md")
			if err := os.Symlink("../Home.md", name); err != nil {
				t.Fatal(err)
			}
		},
		"special file": func(t *testing.T, fixture *finalizeFixture) {
			name := filepath.Join(fixture.modelRoot, "workspace", "memory", "pipe.md")
			if err := syscall.Mkfifo(name, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newFinalizeFixture(t, true)
			mutate(t, fixture)
			if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil {
				t.Fatal("accepted an untrusted model artifact")
			}
			if _, err := os.Lstat(fixture.publicationRoot); !os.IsNotExist(err) {
				t.Fatalf("failed finalization left a publication bundle: %v", err)
			}
		})
	}
}

func TestFinalizeRejectsInvalidSemanticChanges(t *testing.T) {
	tests := map[string]func(*testing.T, *finalizeFixture){
		"missing current provenance": func(t *testing.T, fixture *finalizeFixture) {
			content := strings.ReplaceAll(string(fixture.newNote), finalizeCurrentID, testDigest)
			writeFinalizeFile(t, fixture.modelRoot, "workspace/"+fixture.newNotePath, content)
		},
		"unknown provenance": func(t *testing.T, fixture *finalizeFixture) {
			content := strings.Replace(string(fixture.newNote), "---\n\n", "  - sha256:"+strings.Repeat("f", 64)+"\n---\n\n", 1)
			writeFinalizeFile(t, fixture.modelRoot, "workspace/"+fixture.newNotePath, content)
		},
		"invalid frontmatter": func(t *testing.T, fixture *finalizeFixture) {
			content := strings.Replace(string(fixture.newNote), "sources:\n", "tags: [unsafe]\nsources:\n", 1)
			writeFinalizeFile(t, fixture.modelRoot, "workspace/"+fixture.newNotePath, content)
		},
		"invalid Canvas": func(t *testing.T, fixture *finalizeFixture) {
			writeFinalizeFile(t, fixture.modelRoot, "workspace/Hourglass.canvas", `{"nodes":[],"edges":[{"id":"bad","fromNode":"missing","toNode":"missing"}]}`)
		},
		"missing Canvas file": func(t *testing.T, fixture *finalizeFixture) {
			writeFinalizeFile(t, fixture.modelRoot, "workspace/Hourglass.canvas", `{"nodes":[{"id":"missing","type":"file","file":"memory/missing.md","x":0,"y":0,"width":300,"height":200}],"edges":[]}`)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newFinalizeFixture(t, true)
			mutate(t, fixture)
			if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil {
				t.Fatal("accepted invalid semantic output")
			}
		})
	}
}

func TestFinalizeForbidsSemanticChangesWithoutEvents(t *testing.T) {
	fixture := newFinalizeFixture(t, false)
	writeFinalizeFile(t, fixture.modelRoot, "workspace/memory/new.md", string(fixture.newNote))
	if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil || !strings.Contains(err.Error(), "require selected events") {
		t.Fatalf("got %v", err)
	}
}

func TestFinalizeRejectsNonPortableMemoryPath(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	writeFinalizeFile(t, fixture.modelRoot, "workspace/memory/people/Alice.md", string(fixture.newNote))
	if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil || !strings.Contains(err.Error(), "forbidden workspace path") {
		t.Fatalf("got %v", err)
	}
}

func TestFinalizeEnforcesSemanticChangeFileLimit(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	if err := os.Remove(filepath.Join(fixture.modelRoot, "workspace", filepath.FromSlash(fixture.newNotePath))); err != nil {
		t.Fatal(err)
	}
	baselineNote := strings.ReplaceAll(string(fixture.newNote), finalizeCurrentID, testDigest)
	for index := 0; index <= maxChangedSemanticFiles; index++ {
		name := "memory/generated/note-" + leftPad(index) + ".md"
		fixture.baseline[name] = []byte(baselineNote)
		writeFinalizeFile(t, fixture.modelRoot, "workspace/"+name, string(fixture.newNote))
	}
	fixture.control.Baseline = recordsForFiles(fixture.baseline)
	writeControlManifest(t, fixture)

	if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil || !strings.Contains(err.Error(), "change limit") {
		t.Fatalf("got %v", err)
	}
}

func TestFinalizeEnforcesDreamEventLimit(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	for index := 2; index <= maxEventsPerDream+1; index++ {
		id := strings.Repeat(strconv.Itoa(index), 64)
		artifactPath := ".hourglass-runtime/incoming/" + testMachine + "/" + id + ".json"
		record := fileRecord(artifactPath, fixture.evidence)
		fixture.control.Events = append(fixture.control.Events, SelectedEvent{
			Machine: testMachine, ID: id, QueueCommit: testCommit,
			QueuePath: "events/2026/07/" + id + ".json", Blob: testTree,
			ArtifactPath: artifactPath, SHA256: record.SHA256, Bytes: record.Bytes,
		})
		fixture.control.Evidence = append(fixture.control.Evidence, record)
		writeFinalizeFile(t, fixture.modelRoot, "workspace/"+artifactPath, string(fixture.evidence))
	}
	writeControlManifest(t, fixture)

	if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil || !strings.Contains(err.Error(), "Dream event limit") {
		t.Fatalf("got %v", err)
	}
}

func TestFinalizeEnforcesSemanticChangeByteLimit(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	for index := 0; index < 5; index++ {
		name := "memory/generated/large-" + leftPad(index) + ".md"
		fixture.baseline[name] = largeFinalizeNote(testDigest, "old")
		writeFinalizeFile(t, fixture.modelRoot, "workspace/"+name, string(largeFinalizeNote(finalizeCurrentID, "new")))
	}
	fixture.control.Baseline = recordsForFiles(fixture.baseline)
	writeControlManifest(t, fixture)

	if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil || !strings.Contains(err.Error(), "change limit") {
		t.Fatalf("got %v", err)
	}
}

func TestFinalizeRejectsOverlappingArtifactRoots(t *testing.T) {
	tests := map[string]func(*finalizeFixture) string{
		"publication inside model": func(fixture *finalizeFixture) string {
			return filepath.Join(fixture.modelRoot, "publication")
		},
		"publication inside control": func(fixture *finalizeFixture) string {
			return filepath.Join(fixture.controlRoot, "publication")
		},
		"model inside publication": func(fixture *finalizeFixture) string {
			return filepath.Dir(fixture.modelRoot)
		},
	}
	for name, publicationRoot := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newFinalizeFixture(t, true)
			if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, publicationRoot(fixture)); err == nil || !strings.Contains(err.Error(), "roots overlap") {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("publication through model symlink", func(t *testing.T) {
		fixture := newFinalizeFixture(t, true)
		alias := filepath.Join(filepath.Dir(fixture.modelRoot), "model-alias")
		if err := os.Symlink(fixture.modelRoot, alias); err != nil {
			t.Fatal(err)
		}
		publicationRoot := filepath.Join(alias, "publication")
		if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, publicationRoot); err == nil || !strings.Contains(err.Error(), "roots overlap") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestFinalizeRequiresFreshPublicationDestination(t *testing.T) {
	fixture := newFinalizeFixture(t, true)
	if err := os.MkdirAll(fixture.publicationRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture.publicationRoot, "owner-data")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Finalize(fixture.modelRoot, fixture.controlRoot, fixture.publicationRoot); err == nil {
		t.Fatal("accepted an existing publication destination")
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "keep" {
		t.Fatalf("existing publication data changed: %q %v", content, err)
	}
}

func newFinalizeFixture(t *testing.T, withEvent bool) *finalizeFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &finalizeFixture{
		controlRoot: filepath.Join(root, "control"), modelRoot: filepath.Join(root, "model"),
		publicationRoot: filepath.Join(root, "publication"), newNotePath: "memory/system/new-decision.md",
		prompt:   []byte("# Dream prompt\n"),
		evidence: []byte("{\"evidence\":true}\n"),
		newNote: []byte("---\ntitle: Durable decisions retain their reasons\ncreated: 2026-07-21\nupdated: 2026-07-21\nsources:\n" +
			"  - sha256:" + finalizeCurrentID + "\n---\n\nThe retained reason remains useful.\n"),
	}
	fixture.baseline = map[string][]byte{
		".gitignore":       []byte(".hourglass-runtime/\n"),
		"Home.md":          []byte("# Hourglass\n"),
		"Hourglass.canvas": []byte(`{"nodes":[],"edges":[]}`),
		"memory/system/existing.md": []byte("---\ntitle: Existing memory remains durable\ncreated: 2026-07-20\nupdated: 2026-07-20\nsources:\n" +
			"  - sha256:" + testDigest + "\n---\n\nExisting reason.\n"),
	}
	for name, content := range seenLedgerFiles(t, map[string]string{testDigest: testMachine}) {
		fixture.baseline[name] = content
	}
	fixture.control = ControlManifest{
		Schema: ControlSchema, Repository: "x2x3studio/hourglass", ControlSHA: testCommit,
		RunID: "29795588883", RunAttempt: 1, Shared: Revision{Commit: testCommit, Tree: testTree},
		QueueTips: []QueueTip{{Machine: testMachine, Commit: testCommit}},
		Baseline:  recordsForFiles(fixture.baseline),
		Prompt:    fileRecord("prompt.md", fixture.prompt),
	}
	if withEvent {
		evidencePath := ".hourglass-runtime/incoming/" + testMachine + "/" + finalizeCurrentID + ".json"
		evidenceRecord := fileRecord(evidencePath, fixture.evidence)
		fixture.control.Events = []SelectedEvent{{
			Machine: testMachine, ID: finalizeCurrentID, QueueCommit: testCommit,
			QueuePath: "events/2026/07/" + finalizeCurrentID + ".json", Blob: testTree,
			ArtifactPath: evidencePath, SHA256: evidenceRecord.SHA256, Bytes: evidenceRecord.Bytes,
		}}
		fixture.control.Evidence = []FileRecord{evidenceRecord}
		fixture.control.Cursors = []CursorOperation{{Machine: testMachine, Commit: testCommit}}
	}
	writeControlManifest(t, fixture)
	writeFinalizeFile(t, fixture.modelRoot, "prompt.md", string(fixture.prompt))
	for name, content := range fixture.baseline {
		if name == "Home.md" || name == "Hourglass.canvas" || strings.HasPrefix(name, "memory/") {
			writeFinalizeFile(t, fixture.modelRoot, "workspace/"+name, string(content))
		}
	}
	if withEvent {
		writeFinalizeFile(t, fixture.modelRoot, "workspace/"+fixture.control.Events[0].ArtifactPath, string(fixture.evidence))
		writeFinalizeFile(t, fixture.modelRoot, "workspace/"+fixture.newNotePath, string(fixture.newNote))
	}
	return fixture
}

func writeControlManifest(t *testing.T, fixture *finalizeFixture) {
	t.Helper()
	content, err := EncodeControl(fixture.control)
	if err != nil {
		t.Fatal(err)
	}
	writeFinalizeFile(t, fixture.controlRoot, ControlManifestName, string(content))
	for name, fileContent := range fixture.baseline {
		_, seen := seenShardName(name)
		_, rejected := rejectionShardName(name)
		if seen || rejected {
			writeFinalizeFile(t, fixture.controlRoot, name, string(fileContent))
		}
	}
}

func writeFinalizeFile(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func publicationPaths(manifest PublicationManifest) []string {
	paths := make([]string, 0, len(manifest.Files))
	for _, record := range manifest.Files {
		paths = append(paths, record.Path)
	}
	return paths
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func assertPublicationModes(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
			t.Errorf("publication file mode for %s is %s", name, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func leftPad(value int) string {
	text := strconv.Itoa(value)
	return strings.Repeat("0", 3-len(text)) + text
}

func largeFinalizeNote(source, marker string) []byte {
	header := "---\ntitle: Large retained reason " + marker + "\ncreated: 2026-07-21\nupdated: 2026-07-21\nsources:\n  - sha256:" + source + "\n---\n\n"
	return []byte(header + strings.Repeat(marker, 140*1024) + "\n")
}
