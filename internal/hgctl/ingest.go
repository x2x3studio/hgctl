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
	// defaultIngestMinInterval throttles the hot re-ingest of a still-growing
	// session: its full current content is re-snapshotted at most once per this
	// interval. A session that stops growing simply produces no new snapshot, so
	// its last snapshot is the final complete one - there is no idle/end handling.
	// Override with HG_INGEST_MIN_INTERVAL (a Go duration).
	defaultIngestMinInterval = 5 * time.Minute
	// syncIngestLimit bounds how many grown sessions one scheduler-driven sync
	// re-ingests, so a sync stays within its context budget; the remainder is
	// picked up by the next run.
	syncIngestLimit = 8
)

// ingestMinInterval is the smallest gap between two snapshots of the same growing
// session, overridable with HG_INGEST_MIN_INTERVAL (a Go duration such as "3m").
func ingestMinInterval() time.Duration {
	if raw := os.Getenv("HG_INGEST_MIN_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			return d
		}
	}
	return defaultIngestMinInterval
}

// ingestMark records what was last ingested for one session: the transcript byte
// size and when that snapshot was taken. Intake re-snapshots a session only once
// its transcript has grown past Size (and not more often than the min interval),
// so a session's knowledge flows in while it is live and a historical session
// that is not growing ingests exactly once.
type ingestMark struct {
	Size       int64     `json:"size"`
	IngestedAt time.Time `json:"ingested_at"`
}

var ingestWrappers = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<local-command-[^>]*>.*?</local-command-[^>]*>`),
	regexp.MustCompile(`(?s)<command-[^>]*>.*?</command-[^>]*>`),
	regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`),
}

type ingestTurn struct{ role, text string }

type ingestSession struct {
	id, cwd, title, firstTS, lastTS string
	turns                           []ingestTurn
}

// ingestCandidate is one extracted session snapshot ready to enqueue. It is
// tagged with the client so its dedup key never collides across sources, carries
// the session's latest-activity time so the event filename orders the backlog by
// when the work happened, and records the transcript byte size so the ledger can
// tell when the session next grows.
type ingestCandidate struct {
	client   string
	id       string
	captured time.Time
	body     string
	size     int64
}

// runIngest reads local agent session transcripts and publishes each new-or-grown
// one as a bounded raw event, oldest first, stamped with its latest-activity time.
// It is the operator/bulk entry point for per-session intake (live + historical):
// intake only, never a Basic Memory write. Unlike the steady-state sync, it drains
// the whole outbox to the machine queue branch in large batches and pushes once,
// so an operator-invoked import lands on origin before the command returns. It does
// not throttle (minInterval 0): an explicit run always snapshots the current state
// of any grown session, while the size marker keeps unchanged sessions idempotent.
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
	marks, err := a.loadIngestedSessions()
	if err != nil {
		return err
	}
	enqueued, ingestErr := a.ingestGrownSessions(id, marks, clients, *limit, 0, 0)
	if err := a.saveIngestedSessions(marks); err != nil {
		return errors.Join(ingestErr, err)
	}
	if ingestErr != nil {
		return ingestErr
	}
	delivered, derr := a.bulkPublishQueue(ctx)
	if errors.Is(derr, os.ErrNotExist) {
		_, err = fmt.Fprintf(a.Out, "ingested %d session snapshot(s) into the outbox; run 'hgctl sync' to publish\n", enqueued)
		return err
	}
	if derr != nil {
		return derr
	}
	_, err = fmt.Fprintf(a.Out, "ingested %d session snapshot(s); published %d queued event(s) to queue/%s\n", enqueued, delivered, id.ID)
	return err
}

// ingestGrownSessions gathers new-or-grown sessions across the given clients,
// enqueues up to limit of them into the outbox oldest first (0 = no cap), and
// updates each session's marker in the ledger. parseCap bounds how many eligible
// transcripts each client parses per run (0 = no cap); the scheduler path caps it
// so one sync stays within its context budget. minInterval throttles re-snapshots
// of a still-growing session (0 = no throttle). It only writes intake events; the
// caller publishes the outbox. marks is mutated in place so the caller can persist
// the ledger even on a partial error.
func (a *App) ingestGrownSessions(id Identity, marks map[string]ingestMark, clients []string, limit, parseCap int, minInterval time.Duration) (int, error) {
	var (
		candidates []ingestCandidate
		errs       []error
	)
	for _, c := range clients {
		got, err := a.gatherSessions(c, marks, minInterval, parseCap)
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
	now := a.Now().UTC()
	enqueued := 0
	for _, cand := range candidates {
		// Dedup keys the event filename to the session so an interrupted re-run of
		// the SAME snapshot (marker not yet saved) overwrites its outbox file and
		// collapses against an already-published queue event, while a later, grown
		// snapshot lands as a distinct event via its newer captured time.
		if err := a.enqueue(rawEvent{CapturedAt: cand.captured, Client: cand.client, Machine: id.ID, Hostname: id.Hostname, Body: cand.body, Dedup: ingestKey(cand.client, cand.id)}); err != nil {
			errs = append(errs, err)
			break
		}
		marks[ingestKey(cand.client, cand.id)] = ingestMark{Size: cand.size, IngestedAt: now}
		enqueued++
	}
	return enqueued, errors.Join(errs...)
}

// ingestForSync is the steady-state, scheduler-driven counterpart to runIngest:
// each sync re-ingests up to syncIngestLimit new-or-grown sessions into the outbox
// (the queue drain that follows publishes them). It is bounded so a sync stays
// within its context budget; the remainder is picked up next run. The cheap
// size-marker pre-filter in gatherSessions keeps this from reparsing unchanged
// sessions, and the min interval throttles a rapidly-growing live session.
func (a *App) ingestForSync(id Identity) error {
	clients, _ := ingestClients("all")
	marks, err := a.loadIngestedSessions()
	if err != nil {
		return err
	}
	enqueued, ingestErr := a.ingestGrownSessions(id, marks, clients, syncIngestLimit, syncIngestLimit, ingestMinInterval())
	if enqueued > 0 {
		if err := a.saveIngestedSessions(marks); err != nil {
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

func (a *App) gatherSessions(client string, marks map[string]ingestMark, minInterval time.Duration, parseCap int) ([]ingestCandidate, error) {
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
	// Cheapest filter first: stat each transcript and, using the filename-derived
	// session id, skip any that has not grown past its ledger marker (or was
	// snapshotted within the min interval) before opening and parsing the file, so
	// a scheduler-driven sync only parses genuinely new or grown sessions.
	type grownFile struct {
		path  string
		size  int64
		mtime time.Time
	}
	var eligible []grownFile
	for _, f := range files {
		info, statErr := os.Stat(f)
		if statErr != nil {
			continue
		}
		if !shouldReingest(marks[ingestKey(client, pathSessionID(client, f))], info.Size(), now, minInterval) {
			continue
		}
		eligible = append(eligible, grownFile{path: f, size: info.Size(), mtime: info.ModTime()})
	}
	// A bounded run (parseCap > 0, used by the scheduler-driven sync) parses only
	// the oldest few by file mtime so one sync stays within its context budget; the
	// remainder is picked up next run. The operator bulk ingest leaves parseCap 0
	// and parses the whole eligible set.
	if parseCap > 0 && len(eligible) > parseCap {
		sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].mtime.Before(eligible[j].mtime) })
		eligible = eligible[:parseCap]
	}
	var out []ingestCandidate
	for _, ef := range eligible {
		session, ok := extract(ef.path)
		if !ok {
			continue
		}
		// Re-check against the real session id (which the ledger is keyed on); this
		// also catches the rare transcript whose filename id differs from its
		// recorded id and slipped past the cheap pre-filter above.
		if !shouldReingest(marks[ingestKey(client, session.id)], ef.size, now, minInterval) {
			continue
		}
		body := session.render()
		// Never enqueue a zero-content event; render() is empty when a session has
		// no surviving turns after boilerplate stripping.
		if body == "" {
			continue
		}
		captured := now
		if parsed, ok := parseIngestTime(session.lastTS); ok {
			captured = parsed
		} else if parsed, ok := parseIngestTime(session.firstTS); ok {
			captured = parsed
		}
		out = append(out, ingestCandidate{client: client, id: session.id, captured: captured, body: body, size: ef.size})
	}
	return out, nil
}

// shouldReingest reports whether a transcript now at size should be (re)ingested
// given its last marker. A session with no marker is new (ingest once); one whose
// transcript has grown past the marker is re-snapshotted, unless it was already
// snapshotted within minInterval (throttling a rapidly-growing live session). A
// session that has not grown is skipped, so a historical transcript ingests once
// and its last snapshot is its final complete one.
func shouldReingest(mark ingestMark, size int64, now time.Time, minInterval time.Duration) bool {
	if mark.IngestedAt.IsZero() {
		return true
	}
	if size <= mark.Size {
		return false
	}
	return now.Sub(mark.IngestedAt) >= minInterval
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
		if ts, _ := record["timestamp"].(string); ts != "" {
			session.lastTS = ts
		}
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
		if record.Timestamp != "" {
			session.lastTS = record.Timestamp
		}
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

// loadIngestedSessions reads the per-session marker ledger. It transparently
// migrates the legacy format (a JSON array of ingested keys) to the marker map by
// giving each key a zero-size marker, so a previously ingested session is
// re-snapshotted once at its current size after upgrade.
func (a *App) loadIngestedSessions() (map[string]ingestMark, error) {
	content, err := os.ReadFile(a.ingestedSessionsPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]ingestMark{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(content))) == 0 {
		return map[string]ingestMark{}, nil
	}
	var marks map[string]ingestMark
	if err := json.Unmarshal(content, &marks); err == nil && marks != nil {
		normalized := make(map[string]ingestMark, len(marks))
		for key, mark := range marks {
			normalized[normalizeIngestKey(key)] = mark
		}
		return normalized, nil
	}
	var ids []string
	if err := json.Unmarshal(content, &ids); err != nil {
		return nil, fmt.Errorf("parse ingest ledger %s: %w", a.ingestedSessionsPath(), err)
	}
	marks = make(map[string]ingestMark, len(ids))
	for _, id := range ids {
		marks[normalizeIngestKey(id)] = ingestMark{}
	}
	return marks, nil
}

// normalizeIngestKey migrates the pre-namespace ledger: bare UUIDs written before
// Codex support were all Claude sessions, so they gain the claude: prefix on load.
func normalizeIngestKey(key string) string {
	if strings.HasPrefix(key, "claude:") || strings.HasPrefix(key, "codex:") {
		return key
	}
	return ingestKey("claude", key)
}

func (a *App) saveIngestedSessions(marks map[string]ingestMark) error {
	return writeJSONAtomic(a.ingestedSessionsPath(), marks, 0o600)
}
