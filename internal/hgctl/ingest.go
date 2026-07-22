package hgctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxIngestBody     = 16 * 1024
	maxIngestTurn     = 1500
	minIngestUserText = 40
)

var ingestWrappers = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<local-command-[^>]*>.*?</local-command-[^>]*>`),
	regexp.MustCompile(`(?s)<command-[^>]*>.*?</command-[^>]*>`),
}

type ingestTurn struct{ role, text string }

type ingestSession struct {
	id, cwd, title, lastTS string
	turns                  []ingestTurn
}

// runIngest reads local agent session transcripts and enqueues each new one as a
// single bounded raw event. It is the one-time historical backlog counterpart to
// the automatic Stop-hook capture: intake only, never a Basic Memory write.
func (a *App) runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	client := fs.String("client", "claude", "session source to ingest (claude)")
	limit := fs.Int("limit", 20, "maximum new sessions to enqueue this run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *client != "claude" {
		return errors.New("usage: hgctl ingest [--client claude] [--limit N]")
	}
	id, err := a.loadIdentity()
	if err != nil {
		return err
	}
	seen, err := a.loadIngestedSessions()
	if err != nil {
		return err
	}
	files, err := claudeSessionFiles()
	if err != nil {
		return err
	}
	enqueued := 0
	for _, f := range files {
		if enqueued >= *limit {
			break
		}
		session, ok := extractClaudeSession(f)
		if !ok || seen[session.id] {
			continue
		}
		body := session.render()
		if body == "" {
			continue
		}
		captured := a.Now().UTC()
		if session.lastTS != "" {
			if parsed, perr := time.Parse(time.RFC3339, session.lastTS); perr == nil {
				captured = parsed.UTC()
			}
		}
		if err := a.enqueue(rawEvent{CapturedAt: captured, Client: "claude", Machine: id.ID, Hostname: id.Hostname, Body: body}); err != nil {
			return err
		}
		seen[session.id] = true
		enqueued++
	}
	if err := a.saveIngestedSessions(seen); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "ingested %d new session(s) into the outbox; run 'hgctl sync' to publish\n", enqueued)
	return err
}

func claudeSessionFiles() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	files := matches[:0]
	for _, f := range matches {
		if strings.Contains(f, "hgsmoke") || strings.Contains(f, "-private-tmp-") {
			continue
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool {
		fi, ei := os.Stat(files[i])
		fj, ej := os.Stat(files[j])
		if ei != nil || ej != nil {
			return files[i] > files[j]
		}
		return fi.ModTime().After(fj.ModTime())
	})
	return files, nil
}

func cleanIngestText(s string) string {
	for _, re := range ingestWrappers {
		s = re.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}

func ingestContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, block := range value {
			if object, ok := block.(map[string]any); ok && object["type"] == "text" {
				if text, ok := object["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func extractClaudeSession(path string) (ingestSession, bool) {
	f, err := os.Open(path)
	if err != nil {
		return ingestSession{}, false
	}
	defer f.Close()
	var session ingestSession
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		switch record["type"] {
		case "ai-title":
			if title, ok := record["aiTitle"].(string); ok {
				session.title = title
			}
			continue
		case "user", "assistant":
		default:
			continue
		}
		if session.id == "" {
			session.id, _ = record["sessionId"].(string)
		}
		if session.cwd == "" {
			session.cwd, _ = record["cwd"].(string)
		}
		if ts, ok := record["timestamp"].(string); ok {
			session.lastTS = ts
		}
		message, _ := record["message"].(map[string]any)
		text := cleanIngestText(ingestContentText(message["content"]))
		if text == "" {
			continue
		}
		if len(text) > maxIngestTurn {
			text = text[:maxIngestTurn] + " ...[truncated]"
		}
		role, _ := record["type"].(string)
		session.turns = append(session.turns, ingestTurn{role: role, text: text})
	}
	if session.id == "" {
		session.id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	userChars := 0
	for _, turn := range session.turns {
		if turn.role == "user" {
			userChars += len(turn.text)
		}
	}
	if userChars < minIngestUserText || len(session.turns) == 0 {
		return ingestSession{}, false
	}
	return session, true
}

func (s ingestSession) render() string {
	var b strings.Builder
	if s.title != "" {
		fmt.Fprintf(&b, "Session title: %s\n", s.title)
	}
	if s.cwd != "" {
		fmt.Fprintf(&b, "Project: %s\n", s.cwd)
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	for _, turn := range s.turns {
		label := "USER"
		if turn.role == "assistant" {
			label = "ASSISTANT"
		}
		entry := label + ": " + turn.text + "\n\n"
		if b.Len()+len(entry) > maxIngestBody {
			b.WriteString("...[session truncated for length]\n")
			break
		}
		b.WriteString(entry)
	}
	return strings.TrimSpace(b.String())
}

func (a *App) ingestedSessionsPath() string {
	return filepath.Join(a.Paths.Data, "ingested-sessions.json")
}

func (a *App) loadIngestedSessions() (map[string]bool, error) {
	var ids []string
	if err := readJSON(a.ingestedSessionsPath(), &ids); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}
	return seen, nil
}

func (a *App) saveIngestedSessions(seen map[string]bool) error {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return writeJSONAtomic(a.ingestedSessionsPath(), ids, 0o600)
}
