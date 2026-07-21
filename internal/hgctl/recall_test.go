package hgctl

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRecallBindsBasicMemoryPathsToExactGitContentAndReranks(t *testing.T) {
	fixture := newGitFixture(t)
	negativePath := "memory/test/negative.md"
	positivePath := "memory/test/positive.md"
	negativeContent := "# Exact negative card\n\nThis came from Git.\n"
	positiveContent := "# Exact positive card\n\nThis also came from Git.\n"
	publishRecallCards(t, fixture, map[string]string{
		negativePath: negativeContent,
		positivePath: positiveContent,
	}, map[string]feedbackAggregate{
		negativePath: {Irrelevant: 2},
		positivePath: {Used: 2},
	})
	search := map[string]any{
		"results": []map[string]any{
			{"type": "entity", "file_path": "Home.md", "content": "forged navigation"},
			{"type": "entity", "file_path": negativePath, "content": "forged Basic Memory content"},
			{"type": "entity", "file_path": negativePath, "content": "duplicate"},
			{"type": "entity", "file_path": positivePath, "content": "also forged"},
			{"type": "entity", "file_path": "Hourglass.canvas", "content": "forged canvas"},
		},
		"current_page": 1, "page_size": 8, "total": 2, "has_more": false,
	}
	searchLog := installFakeBasicMemory(t, fixture.app, search)
	query := "--vector"
	if err := fixture.app.runRecall(testContext(t), []string{query, "--client", "codex"}); err != nil {
		t.Fatal(err)
	}
	output := fixture.app.Out.(*bytes.Buffer).String()
	if !strings.Contains(output, positiveContent) || !strings.Contains(output, negativeContent) || strings.Contains(output, "forged Basic Memory") {
		t.Fatalf("recall did not render exact Git content:\n%s", output)
	}
	if strings.Index(output, positivePath) > strings.Index(output, negativePath) {
		t.Fatalf("verified feedback did not conservatively promote the positive card:\n%s", output)
	}
	logContent, err := os.ReadFile(searchLog)
	if err != nil {
		t.Fatal(err)
	}
	arguments := string(logContent)
	for _, required := range []string{"--project-id test-project-id", "--local", "--entity-type entity", "--page-size 8"} {
		if !strings.Contains(arguments, required) {
			t.Fatalf("Basic Memory search omitted %q: %s", required, arguments)
		}
	}
	if !strings.Contains(arguments, "--page-size 8 -- --vector") {
		t.Fatalf("option-like query was not isolated after --: %s", arguments)
	}
	if strings.Contains(arguments, "--project hourglass") {
		t.Fatal("recall used the mutable project name instead of its persisted external id")
	}
	receipts, err := os.ReadDir(fixture.app.Paths.Surfaces)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("surface receipts=%d err=%v", len(receipts), err)
	}
	receiptID := "sha256:" + strings.TrimSuffix(receipts[0].Name(), ".json")
	receipt, _, err := fixture.app.loadSurfaceReceipt(receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Surface.Results) != 2 || receipt.Surface.Results[0].Path != positivePath || receipt.Surface.Results[1].Path != negativePath {
		t.Fatalf("surface did not bind displayed order: %+v", receipt.Surface.Results)
	}
	receiptBytes, err := os.ReadFile(filepath.Join(fixture.app.Paths.Surfaces, receipts[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(receiptBytes, []byte(query)) || bytes.Contains(receiptBytes, []byte("forged")) {
		t.Fatal("local surface persisted query or Basic Memory prose")
	}
}

func TestExplicitZeroHitQueuesFeedbackButSessionStartDoesNot(t *testing.T) {
	fixture := newGitFixture(t)
	installFakeBasicMemory(t, fixture.app, map[string]any{
		"results": []any{}, "current_page": 1, "page_size": 8, "total": 0, "has_more": false,
	})
	if err := fixture.app.runRecall(testContext(t), []string{"nothing", "--client", "claude"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture.app.Paths.Outbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("explicit zero hit outbox entries=%d err=%v", len(entries), err)
	}
	content, err := os.ReadFile(filepath.Join(fixture.app.Paths.Outbox, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := decodeCanonicalEvent(content, entries[0].Name(), fixture.id.ID, fixture.app.Now().UTC())
	payload, payloadErr := feedbackPayload(event)
	if err != nil || payloadErr != nil || payload.Outcome != "zero_hit" || payload.Result != nil || payload.Surface.Origin != "explicit" {
		t.Fatalf("explicit zero hit changed: event=%#v err=%v", event, err)
	}
	before := len(entries)
	contextText := fixture.app.contextText(testContext(t), filepath.Join(fixture.app.Paths.Home, "nothing"), "claude")
	if strings.Contains(contextText, "Possible prior context") {
		t.Fatalf("empty session lookup rendered a result: %s", contextText)
	}
	entries, err = os.ReadDir(fixture.app.Paths.Outbox)
	if err != nil || len(entries) != before {
		t.Fatalf("session-start empty lookup queued zero-hit feedback: entries=%d err=%v", len(entries), err)
	}
}

func TestRecallRejectsUnverifiableBasicMemoryCandidatesWithoutReceipt(t *testing.T) {
	fixture := newGitFixture(t)
	installFakeBasicMemory(t, fixture.app, map[string]any{
		"results":      []map[string]any{{"type": "entity", "file_path": "AGENTS.md"}},
		"current_page": 1, "page_size": 8, "total": 1, "has_more": false,
	})
	if err := fixture.app.runRecall(testContext(t), []string{"instructions", "--client", "codex"}); err == nil {
		t.Fatal("recall accepted a non-memory candidate")
	}
	if entries, err := os.ReadDir(fixture.app.Paths.Surfaces); err != nil || len(entries) != 0 {
		t.Fatalf("unverifiable lookup issued a surface: entries=%d err=%v", len(entries), err)
	}
	if entries, err := os.ReadDir(fixture.app.Paths.Outbox); err != nil || len(entries) != 0 {
		t.Fatalf("unverifiable lookup queued feedback: entries=%d err=%v", len(entries), err)
	}
}

func TestBasicMemoryJSONMustContainBoundedEntityResults(t *testing.T) {
	for name, content := range map[string]string{
		"null results":    `{"results":null}`,
		"missing results": `{}`,
		"wrong type":      `{"results":[{"type":"observation","file_path":"memory/test/card.md"}]}`,
		"missing path":    `{"results":[{"type":"entity"}]}`,
		"trailing value":  `{"results":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeBasicMemoryEntityPaths([]byte(content)); err == nil {
				t.Fatalf("invalid Basic Memory JSON accepted: %s", content)
			}
		})
	}
	valid := []byte(`{"results":[{"type":"entity","file_path":"memory/test/card.md","score":0.5,"content":"ignored"}]}`)
	paths, err := decodeBasicMemoryEntityPaths(valid)
	if err != nil || len(paths) != 1 || paths[0] != "memory/test/card.md" {
		t.Fatalf("valid Basic Memory JSON changed: paths=%v err=%v", paths, err)
	}
}

func TestSessionStartKeepsRerankCorruptionOffHookStderr(t *testing.T) {
	fixture := newGitFixture(t)
	cardPath := "memory/test/card.md"
	publishRecallCards(t, fixture, map[string]string{cardPath: "# Exact card\n"}, nil)
	blob := strings.TrimSpace(runGitTest(t, fixture.seed, "hash-object", filepath.FromSlash(cardPath)))
	prefix := aggregateKey(cardPath, blob)[:2]
	shardPath := filepath.Join(fixture.seed, ".hourglass", "feedback", prefix+".json")
	if err := os.MkdirAll(filepath.Dir(shardPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shardPath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, fixture.seed, "add", "-A")
	runGitTest(t, fixture.seed, "commit", "-m", "corrupt feedback fixture")
	runGitTest(t, fixture.seed, "push", "origin", "shared")
	installFakeBasicMemory(t, fixture.app, map[string]any{
		"results":      []map[string]any{{"type": "entity", "file_path": cardPath}},
		"current_page": 1, "page_size": 8, "total": 1, "has_more": false,
	})
	fixture.app.Out.(*bytes.Buffer).Reset()
	fixture.app.Err.(*bytes.Buffer).Reset()
	fixture.app.In = strings.NewReader(`{"session_id":"s1","cwd":"/tmp/card"}`)
	if err := fixture.app.runHook(testContext(t), []string{"--client", "codex", "--event", "session-start"}); err != nil {
		t.Fatal(err)
	}
	if output := fixture.app.Err.(*bytes.Buffer).String(); output != "" {
		t.Fatalf("SessionStart leaked a rerank diagnostic to stderr: %q", output)
	}
	if output := fixture.app.Out.(*bytes.Buffer).String(); !strings.Contains(output, "Exact card") {
		t.Fatalf("corrupt optional feedback hid verified memory: %s", output)
	}
}

func publishRecallCards(t *testing.T, fixture gitFixture, cards map[string]string, counters map[string]feedbackAggregate) {
	t.Helper()
	for name, content := range cards {
		path := filepath.Join(fixture.seed, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.seed, "Hourglass.canvas"), []byte(`{"nodes":[],"edges":[]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shards := make(map[string][]feedbackAggregate)
	for name, counter := range counters {
		blob := strings.TrimSpace(runGitTest(t, fixture.seed, "hash-object", filepath.FromSlash(name)))
		key := aggregateKey(name, blob)
		counter.Key = key
		counter.Path = name
		counter.Blob = blob
		shards[key[:2]] = append(shards[key[:2]], counter)
	}
	for prefix, entries := range shards {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
		content, err := json.Marshal(feedbackShard{Schema: "hourglass.feedback-shard/v1", Shard: prefix, Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixture.seed, ".hourglass", "feedback", prefix+".json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(content, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGitTest(t, fixture.seed, "add", "-A")
	runGitTest(t, fixture.seed, "commit", "-m", "publish recall cards")
	runGitTest(t, fixture.seed, "push", "origin", "shared")
}

func installFakeBasicMemory(t *testing.T, app *App, search any) string {
	t.Helper()
	bin := filepath.Join(app.Paths.Home, "fake-basic-memory")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	projectsPath := filepath.Join(app.Paths.Home, "basic-memory-projects.json")
	searchPath := filepath.Join(app.Paths.Home, "basic-memory-search.json")
	searchLog := filepath.Join(app.Paths.Home, "basic-memory-search.log")
	if err := writeJSONAtomic(projectsPath, map[string]any{"projects": []basicMemoryProject{{
		Name: ProjectName, ExternalID: "test-project-id", LocalPath: app.Paths.Vault,
	}}}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(searchPath, search, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1 $2" in
  "tool list-projects") cat "$HGCTL_TEST_PROJECTS"; exit 0 ;;
  "tool search-notes") printf '%s\n' "$*" >> "$HGCTL_TEST_SEARCH_LOG"; cat "$HGCTL_TEST_SEARCH"; exit 0 ;;
  "reindex --search") exit 0 ;;
esac
exit 20
`
	if err := os.WriteFile(filepath.Join(bin, "basic-memory"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HGCTL_TEST_PROJECTS", projectsPath)
	t.Setenv("HGCTL_TEST_SEARCH", searchPath)
	t.Setenv("HGCTL_TEST_SEARCH_LOG", searchLog)
	return searchLog
}
