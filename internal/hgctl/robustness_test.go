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

func TestUnknownHookEventNoOpsAndRecordsNoDiagnostic(t *testing.T) {
	app := testApp(t)
	app.In = strings.NewReader(`{"session_id":"s1","prompt":"hi","last_assistant_message":"yo"}`)
	if code := app.Run(testContext(t), []string{"hook", "--client", "claude", "--event", "session-start"}); code != 0 {
		t.Fatalf("unknown hook event exit code=%d, want 0", code)
	}
	if _, err := os.Stat(app.hookDiagnosticPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown hook event recorded a diagnostic: %v", err)
	}
	for _, dir := range []string{app.Paths.Pending, app.Paths.Outbox} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("unknown hook event captured %d files under %s, want 0", len(entries), dir)
		}
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

func compactJSON(t *testing.T, content []byte) []byte {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, content); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}
