package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/x2x3studio/hgctl/internal/config"

	"strings"

	"sort"

	"fmt"

	"encoding/json"

	"strconv"
)

func testIngester(t *testing.T) *Ingester {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HGCTL_HOME", home)
	t.Setenv("HGCTL_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("HOURGLASS_VAULT", filepath.Join(home, "vault"))
	t.Setenv("HOME", home)
	paths, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return &Ingester{Paths: paths, Now: func() time.Time { return time.Date(2026, 7, 21, 1, 2, 3, 0, time.UTC) }}
}

func writeTranscript(t *testing.T, i *Ingester, rel string, lines ...string) string {
	t.Helper()
	path := filepath.Join(i.Paths.Home, ".claude", "projects", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const realTurn = `{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-07-25T00:00:00Z","message":{"content":"a genuine question long enough to clear the minimum user text bar"}}`

func writeSessionFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type outboxEvent struct {
	name string
	meta map[string]string
	body string
}

func readOutboxEvents(t *testing.T, ing *Ingester) []outboxEvent {
	t.Helper()
	entries, err := os.ReadDir(ing.Paths.Outbox)
	if err != nil {
		t.Fatal(err)
	}
	var evs []outboxEvent
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(ing.Paths.Outbox, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		meta, body := splitEventFrontmatter(t, string(content))
		evs = append(evs, outboxEvent{name: e.Name(), meta: meta, body: body})
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].name < evs[j].name })
	return evs
}

func splitEventFrontmatter(t *testing.T, content string) (map[string]string, string) {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		t.Fatal("event is missing opening frontmatter")
	}
	rest := content[len("---\n"):]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		t.Fatal("event has unterminated frontmatter")
	}
	meta := map[string]string{}
	for _, line := range strings.Split(rest[:idx], "\n") {
		if line == "" {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		meta[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return meta, strings.TrimSpace(rest[idx+len("\n---\n"):])
}

func repeatText(n int) string {
	return strings.Repeat("a", n)
}

func writeClaudeSession(t *testing.T, path, id, cwd, title string, texts []string) {
	t.Helper()
	var lines []string
	if title != "" {
		lines = append(lines, fmt.Sprintf(`{"type":"ai-title","aiTitle":%q}`, title))
	}
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		ts := fmt.Sprintf("2026-07-07T02:%02d:00.000Z", i)
		lines = append(lines, claudeLine(t, id, cwd, role, ts, text))
	}
	writeSessionFile(t, path, strings.Join(lines, "\n")+"\n")
}

func claudeLine(t *testing.T, id, cwd, role, ts, text string) string {
	t.Helper()
	var content any = text
	if role == "assistant" {
		content = []any{map[string]any{"type": "text", "text": text}}
	}
	rec := map[string]any{
		"type":      role,
		"sessionId": id,
		"timestamp": ts,
		"message":   map[string]any{"role": role, "content": content},
	}
	if cwd != "" {
		rec["cwd"] = cwd
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parseTurnsRange(t *testing.T, value string) (int, int) {
	t.Helper()
	lo, hi, ok := strings.Cut(value, "-")
	if !ok {
		t.Fatalf("turns range %q is not <start>-<end>", value)
	}
	start, err := strconv.Atoi(lo)
	if err != nil {
		t.Fatalf("turns start %q: %v", lo, err)
	}
	end, err := strconv.Atoi(hi)
	if err != nil {
		t.Fatalf("turns end %q: %v", hi, err)
	}
	return start, end
}
