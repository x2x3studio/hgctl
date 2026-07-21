package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyCopiesOnlyTheValidatedPublication(t *testing.T) {
	repository, publication, control, binding, paths := newApplyFixture(t)
	result, err := Apply(context.Background(), ApplyOptions{
		Publication: publication,
		Control:     control,
		Repository:  repository,
		Binding:     binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(result.Paths, paths) {
		t.Fatalf("unexpected applied paths: %v", result.Paths)
	}
	if result.FilePattern != ".hourglass/seen memory" {
		t.Fatalf("unexpected file pattern %q", result.FilePattern)
	}
	if !result.HasChanges {
		t.Fatal("initial apply reported no changes")
	}
	changed, err := changedRepositoryPaths(context.Background(), gitRepository{directory: repository})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(changed, paths) {
		t.Fatalf("unexpected repository diff: %v", changed)
	}
}

func TestApplyRecognizesAnAlreadyPublishedBundle(t *testing.T) {
	repository, publication, control, binding, _ := newApplyFixture(t)
	if _, err := Apply(context.Background(), ApplyOptions{Publication: publication, Control: control, Repository: repository, Binding: binding}); err != nil {
		t.Fatal(err)
	}
	commitTestRepository(t, repository)
	result, err := Apply(context.Background(), ApplyOptions{Publication: publication, Control: control, Repository: repository, Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	if result.HasChanges || result.FilePattern != "" || len(result.Paths) != 0 {
		t.Fatalf("already applied result = %#v", result)
	}
	assertCleanRepository(t, repository)
}

func TestApplyRejectsMutationBeforeTouchingTheCheckout(t *testing.T) {
	repository, publication, control, binding, _ := newApplyFixture(t)
	name := filepath.Join(publication, "files", "memory", "system", "queue.md")
	if err := os.WriteFile(name, []byte("mutated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), ApplyOptions{Publication: publication, Control: control, Repository: repository, Binding: binding}); err == nil {
		t.Fatal("accepted a publication mutated after finalization")
	}
	assertCleanRepository(t, repository)
}

func TestApplyRejectsAStaleSharedBaseline(t *testing.T) {
	repository, publication, control, binding, _ := newApplyFixture(t)
	writeTestFile(t, repository, "Home.md", "# Changed\n")
	commitTestRepository(t, repository)
	if _, err := Apply(context.Background(), ApplyOptions{Publication: publication, Control: control, Repository: repository, Binding: binding}); err == nil {
		t.Fatal("accepted a publication against a newer shared baseline")
	}
	assertCleanRepository(t, repository)
}

func TestApplyRejectsUnexpectedPublicationFiles(t *testing.T) {
	repository, publication, control, binding, _ := newApplyFixture(t)
	writeTestFile(t, publication, "files/extra.txt", "extra\n")
	if _, err := Apply(context.Background(), ApplyOptions{Publication: publication, Control: control, Repository: repository, Binding: binding}); err == nil {
		t.Fatal("accepted an unexpected publication file")
	}
	assertCleanRepository(t, repository)
}

func TestApplyRejectsForgedControlOperationsAndSemanticOutput(t *testing.T) {
	otherDigest := strings.Repeat("a", 64)
	tests := map[string]func(map[string][]byte){
		"cursor": func(files map[string][]byte) {
			files[".hourglass/cursors/"+testMachine] = []byte(testCommit + "\n")
		},
		"seen receipt": func(files map[string][]byte) {
			name := ".hourglass/seen/" + testDigest[:2] + ".json"
			content, err := encodeSeenShard(testDigest[:2], map[string]string{
				testDigest: "123e4567-e89b-42d3-b456-426614174001",
			})
			if err != nil {
				t.Fatal(err)
			}
			files[name] = content
		},
		"semantic source": func(files map[string][]byte) {
			name := "memory/system/queue.md"
			files[name] = []byte(strings.Replace(string(files[name]), testDigest, otherDigest, 1))
		},
	}
	for name, forge := range tests {
		t.Run(name, func(t *testing.T) {
			repository, publication, control, binding, _ := newApplyFixture(t)
			rewriteApplyPublication(t, publication, forge)
			if _, err := Apply(context.Background(), ApplyOptions{
				Publication: publication, Control: control, Repository: repository, Binding: binding,
			}); err == nil {
				t.Fatalf("accepted forged %s publication", name)
			}
			assertCleanRepository(t, repository)
		})
	}
}

func TestApplyRejectsMismatchedOrMalformedControlArtifacts(t *testing.T) {
	t.Run("different shared revision", func(t *testing.T) {
		repository, publication, control, binding, _ := newApplyFixture(t)
		rewriteApplyControl(t, control, func(manifest *ControlManifest) {
			manifest.Shared.Tree = strings.Repeat("a", 40)
		})
		if _, err := Apply(context.Background(), ApplyOptions{
			Publication: publication, Control: control, Repository: repository, Binding: binding,
		}); err == nil {
			t.Fatal("accepted a publication paired with a different control plan")
		}
		assertCleanRepository(t, repository)
	})

	t.Run("extra artifact file", func(t *testing.T) {
		repository, publication, control, binding, _ := newApplyFixture(t)
		writeTestFile(t, control, "extra.json", "{}\n")
		if _, err := Apply(context.Background(), ApplyOptions{
			Publication: publication, Control: control, Repository: repository, Binding: binding,
		}); err == nil {
			t.Fatal("accepted a control artifact with an extra file")
		}
		assertCleanRepository(t, repository)
	})

	t.Run("forged baseline record", func(t *testing.T) {
		repository, publication, control, binding, _ := newApplyFixture(t)
		rewriteApplyControl(t, control, func(manifest *ControlManifest) {
			for index := range manifest.Baseline {
				if manifest.Baseline[index].Path == "Home.md" {
					manifest.Baseline[index].SHA256 = strings.Repeat("b", 64)
				}
			}
		})
		if _, err := Apply(context.Background(), ApplyOptions{
			Publication: publication, Control: control, Repository: repository, Binding: binding,
		}); err == nil {
			t.Fatal("accepted forged control baseline records")
		}
		assertCleanRepository(t, repository)
	})
}

func newApplyFixture(t *testing.T) (string, string, string, RunBinding, []string) {
	t.Helper()
	repository := newTestRepository(t)
	writeTestFile(t, repository, ".gitignore", ".hourglass-runtime/\n")
	writeTestFile(t, repository, "Home.md", "# Hourglass\n")
	writeTestFile(t, repository, "Hourglass.canvas", `{"nodes":[],"edges":[]}`)
	commitTestRepository(t, repository)
	git := gitRepository{directory: repository}
	revision, err := git.revision(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	_, baselineFiles, _, err := readSharedTree(context.Background(), git, revision)
	if err != nil {
		t.Fatal(err)
	}
	binding := RunBinding{Repository: "x2x3studio/hourglass", ControlSHA: testCommit, RunID: "42", RunAttempt: 1}
	evidencePath := ".hourglass-runtime/incoming/" + testMachine + "/" + testDigest + ".json"
	evidence := fileRecord(evidencePath, []byte("evidence\n"))
	controlManifest := ControlManifest{
		Schema: ControlSchema, Repository: binding.Repository, ControlSHA: binding.ControlSHA,
		RunID: binding.RunID, RunAttempt: binding.RunAttempt, Shared: revision,
		QueueTips: []QueueTip{{Machine: testMachine, Commit: testCommit}},
		Events: []SelectedEvent{{
			Machine: testMachine, ID: testDigest, QueueCommit: testCommit,
			QueuePath: "events/2026/07/" + testDigest + ".json", Blob: testTree,
			ArtifactPath: evidencePath, SHA256: evidence.SHA256, Bytes: evidence.Bytes,
		}},
		Baseline: recordsForFiles(baselineFiles),
		Evidence: []FileRecord{evidence},
		Prompt:   fileRecord("prompt.md", []byte("prompt\n")),
	}
	controlContent, err := EncodeControl(controlManifest)
	if err != nil {
		t.Fatal(err)
	}
	control := t.TempDir()
	writeTestFile(t, control, ControlManifestName, string(controlContent))

	publication := t.TempDir()
	files := map[string][]byte{
		"memory/system/queue.md": []byte("---\ntitle: Queue branches remain endpoint-owned\ncreated: 2026-07-21\nupdated: 2026-07-21\nsources:\n  - sha256:" + testDigest + "\n---\n"),
	}
	for name, content := range seenLedgerFiles(t, map[string]string{testDigest: testMachine}) {
		files[name] = content
	}
	for name, content := range files {
		writeTestFile(t, publication, "files/"+name, string(content))
	}
	records := recordsForFiles(files)
	paths := make([]string, 0, len(records))
	for _, record := range records {
		paths = append(paths, record.Path)
	}
	manifest := PublicationManifest{
		Schema: PublicationSchema, Repository: binding.Repository, ControlSHA: binding.ControlSHA,
		RunID: binding.RunID, RunAttempt: binding.RunAttempt, Shared: revision, Files: records,
	}
	content, err := EncodePublication(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, publication, "publication.json", string(content))
	return repository, publication, control, binding, paths
}

func assertCleanRepository(t *testing.T, repository string) {
	t.Helper()
	status := runTestGit(t, repository, "status", "--porcelain", "--untracked-files=all")
	if status != "" {
		t.Fatalf("repository was modified on failed apply: %s", status)
	}
}

func rewriteApplyPublication(t *testing.T, root string, mutate func(map[string][]byte)) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, PublicationManifestName))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodePublication(content)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte, len(manifest.Files))
	for _, record := range manifest.Files {
		files[record.Path], err = os.ReadFile(filepath.Join(root, PublicationFilesDirectory, filepath.FromSlash(record.Path)))
		if err != nil {
			t.Fatal(err)
		}
	}
	mutate(files)
	for name, fileContent := range files {
		writeTestFile(t, root, PublicationFilesDirectory+"/"+name, string(fileContent))
	}
	manifest.Files = recordsForFiles(files)
	content, err = EncodePublication(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, PublicationManifestName, string(content))
}

func rewriteApplyControl(t *testing.T, root string, mutate func(*ControlManifest)) {
	t.Helper()
	name := filepath.Join(root, ControlManifestName)
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeControl(content)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&manifest)
	content, err = EncodeControl(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, ControlManifestName, string(content))
}
