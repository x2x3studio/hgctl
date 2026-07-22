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
	// defaultIngestIdle gates live ingest: a transcript whose file has not been
	// modified for at least this long is treated as a completed session, so an
	// in-progress conversation is never ingested mid-flight. Historical sessions
	// are idle by definition. Override with HG_INGEST_IDLE (a Go duration).
	defaultIngestIdle = 15 * time.Minute
	// syncIngestLimit bounds how many newly-idle sessions one scheduler-driven
	// sync ingests, so a sync stays within its context budget; the remainder is
	// picked up by the next run.
	syncIngestLimit = 8
)

// ingestIdleThreshold is the minimum file-idle age before a session is eligible
// for ingest, overridable with HG_INGEST_IDLE (a Go duration such as "10m").
func ingestIdleThreshold() time.Duration {
	if raw := os.Getenv("HG_INGEST_IDLE"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			return d
		}
	}
	return defaultIngestIdle
}

var ingestWrappers = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<local-command-[^>]*>.*?</local-command-[^>]*>`),
	regexp.MustCompile(`(?s)<command-[^>]*>.*?</command-[^>]*>`),
	regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`),
}

type ingestTurn struct{ role, text string }

type ingestSession struct {
	id, cwd, title, firstTS string
	turns                   []ingestTurn
}

// ingestCandidate is one extracted session ready to enqueue. It is tagged with
// the client so its dedup key never collides across sources, and it carries the
// real historical session time so the event filename orders the backlog by when
// the work actually happened rather than by when ingest ran.
type ingestCandidate struct {
	client   string
	id       string
	captured time.Time
	body     string
}

// runIngest reads local agent session transcripts and publishes every new one as
// a bounded raw event, oldest first, stamped with its real historical time. It is
// the per-session intake path (live + historical): intake only, never a Basic
// Memory write. Unlike the steady-state sync, it drains the whole outbox to the
// machine queue branch in large batches and pushes once, so an operator-invoked
// import lands on origin before the command returns.
func (a *App) runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	client := fs.String("client", "all", "session source to ingest: all, claude, or codex")
	limit := fs.Int("limit", 0, "optional cap on sessions this run (0 = no cap, oldest first)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clients, ok := ingestClients(*client)
	if fs.NArg() != 0 || !ok || *limit < 0 {
		return errors.New("usage: hgctl ingest [--client all|claude|codex] [--limit N]")
	}
	id, err := a.loadIdentity()
	if err != nil {
		return err
	}
	seen, err := a.loadIngestedSessions()
	if err != nil {
		return err
	}
	enqueued, ingestErr := a.ingestIdleSessions(id, seen, clients, *limit, ingestIdleThreshold())
	if err := a.saveIngestedSessions(seen); err != nil {
		return errors.Join(ingestErr, err)
	}
	if ingestErr != nil {
		return ingestErr
	}
	delivered, derr := a.bulkPublishQueue(ctx)
	if errors.Is(derr, os.ErrNotExist) {
		_, err = fmt.Fprintf(a.Out, "ingested %d new session(s) into the outbox; run 'hgctl sync' to publish\n", enqueued)
		return err
	}
	if derr != nil {
		return derr
	}
	_, err = fmt.Fprintf(a.Out, "ingested %d new session(s); published %d queued event(s) to queue/%s\n", enqueued, delivered, id.ID)
	return err
}

// ingestIdleSessions gathers newly-idle, not-yet-ingested sessions across the
// given clients, enqueues up to limit of them into the outbox oldest first (0 =
// no cap), and records each in the dedup ledger. It only writes intake events;
// the caller publishes the outbox. seen is mutated in place so the caller can
// persist the ledger even on a partial error.
func (a *App) ingestIdleSessions(id Identity, seen map[string]bool, clients []string, limit int, idle time.Duration) (int, error) {
	var (
		candidates []ingestCandidate
		errs       []error
	)
	for _, c := range clients {
		got, err := a.gatherSessions(c, seen, idle)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		candidates = append(candidates, got...)
	}
	sortIngestCandidates(candidates)
	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}
	enqueued := 0
	for _, cand := range candidates {
		// Dedup keys the event filename to the session so an interrupted re-run
		// re-enqueues idempotently (overwrites the outbox file, collapses against
		// an already-published queue event) rather than duplicating reflect work.
		if err := a.enqueue(rawEvent{CapturedAt: cand.captured, Client: cand.client, Machine: id.ID, Hostname: id.Hostname, Body: cand.body, Dedup: ingestKey(cand.client, cand.id)}); err != nil {
			errs = append(errs, err)
			break
		}
		seen[ingestKey(cand.client, cand.id)] = true
		enqueued++
	}
	return enqueued, errors.Join(errs...)
}

// ingestForSync is the steady-state, scheduler-driven counterpart to runIngest:
// each sync ingests up to syncIngestLimit newly-idle, not-yet-ingested sessions
// into the outbox (the queue drain that follows publishes them). It is bounded so
// a sync stays within its context budget; the remainder is picked up next run.
// The cheap ledger + idle pre-filters in gatherSessions keep this from reparsing
// the whole history every run.
func (a *App) ingestForSync(id Identity) error {
	clients, _ := ingestClients("all")
	seen, err := a.loadIngestedSessions()
	if err != nil {
		return err
	}
	enqueued, ingestErr := a.ingestIdleSessions(id, seen, clients, syncIngestLimit, ingestIdleThreshold())
	if enqueued > 0 {
		if err := a.saveIngestedSessions(seen); err != nil {
			return errors.Join(ingestErr, err)
		}
	}
	return ingestErr
}

func ingestClients(client string) ([]string, bool) {
	switch client {
	case "all":
		return []string{"claude", "codex"}, true
	case "claude", "codex":
		return []string{client}, true
	}
	return nil, false
}

func ingestKey(client, id string) string {
	return client + ":" + id
}

// sortIngestCandidates orders the backlog oldest first, with a stable, source
// deterministic tie-break so a fixed set of sessions always ingests in the same
// order regardless of filesystem enumeration.
func sortIngestCandidates(candidates []ingestCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].captured.Equal(candidates[j].captured) {
			return candidates[i].captured.Before(candidates[j].captured)
		}
		if candidates[i].client != candidates[j].client {
			return candidates[i].client < candidates[j].client
		}
		return candidates[i].id < candidates[j].id
	})
}

func (a *App) gatherSessions(client string, seen map[string]bool, idle time.Duration) ([]ingestCandidate, error) {
	var (
		files   []string
		extract func(string) (ingestSession, bool)
		err     error
	)
	switch client {
	case "claude":
		files, err = claudeSessionFiles()
		extract = extractClaudeSession
	case "codex":
		files, err = codexSessionFiles()
		extract = extractCodexSession
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := a.Now().UTC()
	var out []ingestCandidate
	for _, f := range files {
		// Cheapest filters first: skip sessions already in the ledger (by the
		// filename-derived id) and sessions still being written (idle gate) before
		// opening and parsing the file, so a scheduler-driven sync only parses
		// genuinely new, completed sessions.
		if seen[ingestKey(client, pathSessionID(client, f))] {
			continue
		}
		info, statErr := os.Stat(f)
		if statErr != nil {
			continue
		}
		if now.Sub(info.ModTime()) < idle {
			continue
		}
		session, ok := extract(f)
		// The post-parse dedup uses the real session id (which the ledger is keyed
		// on); it also catches the rare transcript whose filename id differs from
		// its recorded id and slipped past the cheap pre-filter above.
		if !ok || seen[ingestKey(client, session.id)] {
			continue
		}
		body := session.render()
		// Never enqueue a zero-content event; render() is empty when a session has
		// no surviving turns after boilerplate stripping.
		if body == "" {
			continue
		}
		captured := now
		if parsed, ok := parseIngestTime(session.firstTS); ok {
			captured = parsed
		}
		out = append(out, ingestCandidate{client: client, id: session.id, captured: captured, body: body})
	}
	return out, nil
}

// pathSessionID recovers a session id from the transcript filename alone, so the
// dedup ledger can skip an already-ingested session without parsing it. It equals
// the parsed session id for well-formed transcripts (Claude names files by
// sessionId; Codex names them rollout-<ts>-<uuid>); the post-parse dedup check
// still uses the real id, so a rare filename/id mismatch only costs one reparse.
func pathSessionID(client, path string) string {
	if client == "codex" {
		return codexIDFromPath(path)
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

func parseIngestTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func claudeSessionFiles() ([]string, error) {
	return globSessionFiles(filepath.Join(".claude", "projects", "*", "*.jsonl"))
}

func codexSessionFiles() ([]string, error) {
	return globSessionFiles(filepath.Join(".codex", "sessions", "*", "*", "*", "rollout-*.jsonl"))
}

func globSessionFiles(pattern string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(home, pattern))
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
		if session.firstTS == "" {
			session.firstTS, _ = record["timestamp"].(string)
		}
		message, _ := record["message"].(map[string]any)
		text := cleanIngestText(ingestContentText(message["content"]))
		if text == "" {
			continue
		}
		role, _ := record["type"].(string)
		session.turns = append(session.turns, ingestTurn{role: role, text: boundTurn(text)})
	}
	if session.id == "" {
		session.id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if !ingestSessionQualifies(session) {
		return ingestSession{}, false
	}
	return session, true
}

// codexRecord is one line of a Codex rollout transcript. Only response_item lines
// carrying a message payload become turns; every other record type (reasoning,
// token_count, function_call/_output, event_msg, world_state, ...) is dropped.
type codexRecord struct {
	Timestamp string       `json:"timestamp"`
	Type      string       `json:"type"`
	Payload   codexPayload `json:"payload"`
}

type codexPayload struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	ID      string             `json:"id"`
	CWD     string             `json:"cwd"`
	Content []codexContentPart `json:"content"`
}

type codexContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func extractCodexSession(path string) (ingestSession, bool) {
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
		var record codexRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if session.firstTS == "" {
			session.firstTS = record.Timestamp
		}
		if record.Type == "session_meta" {
			if session.id == "" {
				session.id = record.Payload.ID
			}
			if session.cwd == "" {
				session.cwd = record.Payload.CWD
			}
			continue
		}
		if record.Type != "response_item" || record.Payload.Type != "message" {
			continue
		}
		role := record.Payload.Role
		want := ""
		switch role {
		case "user":
			want = "input_text"
		case "assistant":
			want = "output_text"
		default:
			continue
		}
		var parts []string
		for _, block := range record.Payload.Content {
			if block.Type == want && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		text := cleanIngestText(strings.Join(parts, "\n"))
		if text == "" {
			continue
		}
		session.turns = append(session.turns, ingestTurn{role: role, text: boundTurn(text)})
	}
	if session.id == "" {
		session.id = codexIDFromPath(path)
	}
	if !ingestSessionQualifies(session) {
		return ingestSession{}, false
	}
	return session, true
}

// codexIDFromPath recovers the rollout UUID from the filename when a session has
// no session_meta line. Names are rollout-<ISO8601>-<uuid>.jsonl and the UUID is
// always the trailing 36 characters.
func codexIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	base = strings.TrimPrefix(base, "rollout-")
	if len(base) >= 36 {
		return base[len(base)-36:]
	}
	return base
}

func boundTurn(text string) string {
	if len(text) > maxIngestTurn {
		return text[:maxIngestTurn] + " ...[truncated]"
	}
	return text
}

func ingestSessionQualifies(session ingestSession) bool {
	if len(session.turns) == 0 {
		return false
	}
	userChars := 0
	for _, turn := range session.turns {
		if turn.role == "user" {
			userChars += len(turn.text)
		}
	}
	return userChars >= minIngestUserText
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
		seen[normalizeIngestKey(id)] = true
	}
	return seen, nil
}

// normalizeIngestKey migrates the pre-namespace ledger: bare UUIDs written before
// Codex support were all Claude sessions, so they gain the claude: prefix on load.
func normalizeIngestKey(key string) string {
	if strings.HasPrefix(key, "claude:") || strings.HasPrefix(key, "codex:") {
		return key
	}
	return ingestKey("claude", key)
}

func (a *App) saveIngestedSessions(seen map[string]bool) error {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return writeJSONAtomic(a.ingestedSessionsPath(), ids, 0o600)
}
