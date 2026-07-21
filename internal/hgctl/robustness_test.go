package hgctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookFailureIsSilentAndPersistedForDoctor(t *testing.T) {
	app := testApp(t)
	app.In = strings.NewReader(`{not-json`)
	if code := app.Run(testContext(t), []string{"hook", "--client", "codex", "--event", "stop"}); code != 0 {
		t.Fatalf("hook exit code=%d, want 0", code)
	}
	if output := app.Out.(*bytes.Buffer).String(); output != "" {
		t.Fatalf("failed hook wrote stdout: %q", output)
	}
	if output := app.Err.(*bytes.Buffer).String(); output != "" {
		t.Fatalf("failed hook wrote stderr: %q", output)
	}

	var diagnostic hookDiagnostic
	if err := readJSON(app.hookDiagnosticPath(), &diagnostic); err != nil {
		t.Fatal(err)
	}
	if diagnostic.SchemaVersion != hookDiagnosticSchemaVersion || diagnostic.Client != "codex" ||
		diagnostic.Event != "stop" || diagnostic.Message == "" || len(diagnostic.Message) > maxHookDiagnosticBytes {
		t.Fatalf("unexpected diagnostic: %+v", diagnostic)
	}
	check := app.hookDiagnosticDoctorCheck()
	if check.ok || !strings.Contains(check.note, "codex/stop") {
		t.Fatalf("doctor did not surface hook failure: %+v", check)
	}

	if err := app.recordHookDiagnostic("codex", "stop", errors.New(strings.Repeat("x", maxHookDiagnosticBytes*2))); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(app.hookDiagnosticPath(), &diagnostic); err != nil {
		t.Fatal(err)
	}
	if len(diagnostic.Message) != maxHookDiagnosticBytes {
		t.Fatalf("diagnostic message bytes=%d, want %d", len(diagnostic.Message), maxHookDiagnosticBytes)
	}

	app.In = strings.NewReader(`{}`)
	if code := app.Run(testContext(t), []string{"hook", "--client", "codex", "--event", "stop"}); code != 0 {
		t.Fatalf("recovered hook exit code=%d, want 0", code)
	}
	if _, err := os.Stat(app.hookDiagnosticPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful matching hook did not clear diagnostic: %v", err)
	}
}

func TestHookConfigPreservesRawRootValuesAndRetriesConcurrentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(`{
  "precise": 9007199254740993,
  "nested": { "integer": 18446744073709551615, "escaped": "\u0061" },
  "flags": [true, null, 1.2300],
  "hooks": {}
}
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var before map[string]json.RawMessage
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	if err := configureHookFile(path, "/tmp/hgctl", "claude", true); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(content, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"precise", "nested", "flags"} {
		if !bytes.Equal(compactJSON(t, before[key]), compactJSON(t, after[key])) {
			t.Fatalf("unrelated root value %q changed: before=%s after=%s", key, before[key], after[key])
		}
	}

	concurrent := []byte(`{"precise":9007199254740993,"sentinel":"concurrent","hooks":{}}` + "\n")
	if err := os.WriteFile(path, []byte(`{"sentinel":"initial","hooks":{}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutations := 0
	err = configureHookFileWithRetry(path, path, "/tmp/hgctl", "codex", true, func(attempt int) {
		if attempt == 0 {
			mutations++
			if err := os.WriteFile(path, concurrent, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutations != 1 || !hooksConfigured(path, "/tmp/hgctl", "codex") {
		t.Fatalf("concurrent retry did not install one exact hook set: mutations=%d", mutations)
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte(`"sentinel": "concurrent"`)) || !bytes.Contains(content, []byte(`9007199254740993`)) {
		t.Fatalf("concurrent root update was lost: %s", content)
	}
}

func TestRecallRequiresLiveProjectAndCurrentIndex(t *testing.T) {
	fixture := newGitFixture(t)
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	state := State{RepoURL: fixture.remote, QueueBranch: "queue/" + identity.ID}
	if err := app.initGit(testContext(t), state); err != nil {
		t.Fatal(err)
	}
	state.BasicMemoryProject = &BasicMemoryOwnership{
		ExternalID: "project-id",
		Path:       app.Paths.Vault,
		Managed:    true,
	}
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(app.Paths.Home, "fake-basic-memory-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	projectsPath := filepath.Join(app.Paths.Home, "projects.json")
	searchLog := filepath.Join(app.Paths.Home, "search.log")
	script := `#!/bin/sh
case "$1 $2" in
  "tool list-projects") cat "$HGCTL_TEST_PROJECTS"; exit 0 ;;
  "tool search-notes") printf x >> "$HGCTL_TEST_SEARCH_LOG"; printf '{"results":[],"current_page":1,"page_size":8,"total":0,"has_more":false}\n'; exit 0 ;;
  "reindex --search") exit 0 ;;
esac
exit 20
`
	if err := os.WriteFile(filepath.Join(bin, "basic-memory"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HGCTL_TEST_PROJECTS", projectsPath)
	t.Setenv("HGCTL_TEST_SEARCH_LOG", searchLog)

	writeProjects := func(projects []basicMemoryProject) {
		t.Helper()
		if err := writeJSONAtomic(projectsPath, map[string]any{"projects": projects}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProjects([]basicMemoryProject{{Name: ProjectName, ExternalID: "project-id", LocalPath: app.Paths.Vault}})
	head := strings.TrimSpace(runGitTest(t, app.Paths.Vault, "rev-parse", "HEAD"))
	if err := saveIndexReceiptForTest(app, head, "project-id"); err != nil {
		t.Fatal(err)
	}

	app.Out.(*bytes.Buffer).Reset()
	if err := app.runRecall(testContext(t), []string{"queue", "ownership", "--client", "codex"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(app.Out.(*bytes.Buffer).String(), "No verified Hourglass memory matched") || readSearchLog(t, searchLog) != "x" {
		t.Fatalf("ready recall did not query the exact project: output=%q log=%q", app.Out.(*bytes.Buffer).String(), readSearchLog(t, searchLog))
	}
	contextText := app.contextText(testContext(t), filepath.Join(app.Paths.Home, "project"), "codex")
	if strings.Contains(contextText, "Possible prior context") || readSearchLog(t, searchLog) != "xx" {
		t.Fatalf("ready context did not query the exact project: context=%q log=%q", contextText, readSearchLog(t, searchLog))
	}

	if err := saveIndexReceiptForTest(app, strings.Repeat("0", 40), "project-id"); err != nil {
		t.Fatal(err)
	}
	app.Out.(*bytes.Buffer).Reset()
	if err := app.runRecall(testContext(t), []string{"stale", "--client", "codex"}); err != nil {
		t.Fatalf("explicit recall did not repair a stale index: %v", err)
	}
	contextText = app.contextText(testContext(t), filepath.Join(app.Paths.Home, "project"), "codex")
	if strings.Contains(contextText, "Possible prior context") || readSearchLog(t, searchLog) != "xxxx" {
		t.Fatalf("repaired context did not query Basic Memory: context=%q log=%q", contextText, readSearchLog(t, searchLog))
	}

	if err := saveIndexReceiptForTest(app, head, "project-id"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		projects []basicMemoryProject
	}{
		{name: "wrong identity", projects: []basicMemoryProject{{Name: ProjectName, ExternalID: "other-id", LocalPath: app.Paths.Vault}}},
		{name: "wrong path", projects: []basicMemoryProject{{Name: ProjectName, ExternalID: "project-id", LocalPath: filepath.Join(app.Paths.Home, "other-vault")}}},
		{name: "empty path", projects: []basicMemoryProject{{Name: ProjectName, ExternalID: "project-id"}}},
		{name: "duplicate name", projects: []basicMemoryProject{
			{Name: ProjectName, ExternalID: "project-id", LocalPath: app.Paths.Vault},
			{Name: ProjectName, ExternalID: "other-id", LocalPath: app.Paths.Vault},
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			writeProjects(test.projects)
			app.Out.(*bytes.Buffer).Reset()
			if err := app.runRecall(testContext(t), []string{"identity", "--client", "codex"}); err == nil {
				t.Fatal("recall accepted a mismatched Basic Memory project")
			}
			if got := readSearchLog(t, searchLog); got != "xxxx" {
				t.Fatalf("mismatched project was queried: %q", got)
			}
		})
	}
}

func compactJSON(t *testing.T, content []byte) []byte {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

func saveIndexReceiptForTest(app *App, sharedSHA, projectID string) error {
	return app.saveBasicMemoryIndexReceipt(BasicMemoryIndexReceipt{
		SharedSHA: sharedSHA, ProjectExternalID: projectID,
	})
}

func readSearchLog(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
