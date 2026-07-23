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
	minIngestUserText = 40
	// ingestChunkBytes bounds one chunk-event body. A session's delta (its new,
	// complete turns) is split into ordered chunks so no single event is unbounded;
	// whole turns are never split, so a single turn larger than this is its own
	// chunk emitted whole. The bound sits well under the protocol's MaxTextBytes so
	// a normal multi-turn chunk is never clamped downstream.
	ingestChunkBytes = 48 * 1024
	// defaultIngestMinInterval throttles the hot re-ingest of a still-growing
	// session: its new turns are emitted at most once per this interval. A session
	// that stops growing simply produces no new delta, so its emitted turns are the
	// complete conversation - there is no idle/end handling. Override with
	// HG_INGEST_MIN_INTERVAL (a Go duration).
	defaultIngestMinInterval = 5 * time.Minute
	// syncIngestLimit bounds how many grown sessions one scheduler-driven sync
	// re-ingests, so a sync stays within its context budget; the remainder is
	// picked up by the next run.
	syncIngestLimit = 8
)

// ingestMinInterval is the smallest gap between two ingests of the same growing
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
// size, the number of turns already emitted (the delta cursor), and when that
// ingest ran. Intake re-parses a session only once its transcript has grown past
// Size (and not more often than the min interval), then emits only turns[Turns:]
// - the new, complete turns - and advances Turns to the parsed turn count. An old
// mark written before the delta model has no turns field, so Turns defaults to 0
// and the whole conversation is re-emitted once (a complete backfill).
type ingestMark struct {
	Size       int64     `json:"size"`
	Turns      int       `json:"turns"`
	IngestedAt time.Time `json:"ingested_at"`
}

var ingestWrappers = []*regexp.Regexp{
	regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
	regexp.MustCompile(`(?s)<local-command-[^>]*>.*?</local-command-[^>]*>`),
	regexp.MustCompile(`(?s)<command-[^>]*>.*?</command-[^>]*>`),
	regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`),
}

// ingestTurn is one surviving conversation turn. ts is the turn's own timestamp so
// a chunk can be stamped with its last turn's time for backlog ordering.
type ingestTurn struct{ role, text, ts string }

type ingestSession struct {
	id, cwd, title, firstTS, lastTS string
	turns                           []ingestTurn
}

// ingestCandidate is one new-or-grown transcript FILE ready to chunk and enqueue.
// key is the file's ledger/dedup identity (see ingestUnitKey): every physical file
// - top-level session OR nested sub-agent transcript - is its own ingest unit with
// its own turn cursor, so a sub-agent (whose records carry the parent sessionId)
// never collides with its parent. It carries the fully parsed session, the delta
// cursor (prevTurns = how many turns were already emitted), the current transcript
// byte size for the ledger, and a session-level ordering time so the backlog sorts
// oldest session first.
type ingestCandidate struct {
	client    string
	key       string
	session   ingestSession
	prevTurns int
	size      int64
	captured  time.Time
}

// ingestChunk is one ordered slice of a session's delta: the half-open [start,end)
// turn range (0-based, absolute within the session), the rendered body, and the
// captured time derived from the chunk's last turn.
type ingestChunk struct {
	start, end int
	body       string
	lastTS     string
	captured   time.Time
}

// runIngest reads local agent session transcripts and publishes each session's new
// turns as complete, chunked delta events, oldest first, stamped with their turn
// times. It is the operator/bulk entry point for per-session intake (live +
// historical): intake only, never a Basic Memory write. Unlike the steady-state
// sync, it drains the whole outbox to the machine queue branch in large batches and
// pushes once, so an operator-invoked import lands on origin before the command
// returns. It does not throttle (minInterval 0): an explicit run always emits any
// pending delta, while the size+turn markers keep unchanged sessions idempotent.
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
	enqueued, _, ingestErr := a.ingestGrownSessions(id, marks, clients, *limit, 0, 0)
	if err := a.saveIngestedSessions(marks); err != nil {
		return errors.Join(ingestErr, err)
	}
	if ingestErr != nil {
		return ingestErr
	}
	delivered, derr := a.bulkPublishQueue(ctx)
	if errors.Is(derr, os.ErrNotExist) {
		_, err = fmt.Fprintf(a.Out, "ingested %d session event(s) into the outbox; run 'hgctl sync' to publish\n", enqueued)
		return err
	}
	if derr != nil {
		return derr
	}
	_, err = fmt.Fprintf(a.Out, "ingested %d session event(s); published %d queued event(s) to queue/%s\n", enqueued, delivered, id.ID)
	return err
}

// ingestGrownSessions gathers new-or-grown sessions across the given clients,
// processes up to limit of them oldest first (0 = no cap), emitting each session's
// delta (turns[Turns:]) as ordered chunk-events into the outbox and advancing that
// session's marker once. parseCap bounds how many eligible transcripts each client
// parses per run (0 = no cap); the scheduler path caps it so one sync stays within
// its context budget. minInterval throttles re-ingest of a still-growing session (0
// = no throttle). It only writes intake events; the caller publishes the outbox.
// marks is mutated in place so the caller can persist the ledger even on a partial
// error; the returned bool reports whether any marker changed.
func (a *App) ingestGrownSessions(id Identity, marks map[string]ingestMark, clients []string, limit, parseCap int, minInterval time.Duration) (int, bool, error) {
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
	changed := false
	for _, cand := range candidates {
		key := cand.key
		n := len(cand.session.turns)
		start := cand.prevTurns
		if start > n {
			// The transcript shrank/was rewritten below the cursor; resync down to
			// the current turn count rather than re-emitting from the top.
			start = n
		}
		if start == n {
			// Grew in bytes but produced no new complete turns (e.g. only tool
			// I/O). Emit nothing, but advance Size so it is not reparsed forever.
			marks[key] = ingestMark{Size: cand.size, Turns: start, IngestedAt: now}
			changed = true
			continue
		}
		header := sessionHeader(cand.session)
		chunks := chunkDeltaTurns(header, start, cand.session.turns[start:n], ingestChunkBytes)
		assignChunkCaptured(chunks, cand.captured)
		failed := false
		for _, ch := range chunks {
			ev := rawEvent{
				CapturedAt: ch.captured,
				Client:     cand.client,
				Machine:    id.ID,
				Hostname:   id.Hostname,
				Session:    cand.session.id,
				Project:    cand.session.cwd,
				Title:      cand.session.title,
				TurnStart:  ch.start,
				TurnEnd:    ch.end,
				Body:       ch.body,
				Dedup:      fmt.Sprintf("%s:%d-%d", key, ch.start, ch.end),
			}
			if err := a.enqueue(ev); err != nil {
				errs = append(errs, err)
				failed = true
				break
			}
			enqueued++
		}
		if failed {
			break
		}
		marks[key] = ingestMark{Size: cand.size, Turns: n, IngestedAt: now}
		changed = true
	}
	return enqueued, changed, errors.Join(errs...)
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
	_, changed, ingestErr := a.ingestGrownSessions(id, marks, clients, syncIngestLimit, syncIngestLimit, ingestMinInterval())
	if changed {
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

// sortIngestCandidates orders the backlog oldest session first, with a stable,
// source-deterministic tie-break so a fixed set of sessions always ingests in the
// same order regardless of filesystem enumeration.
func sortIngestCandidates(candidates []ingestCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if !candidates[i].captured.Equal(candidates[j].captured) {
			return candidates[i].captured.Before(candidates[j].captured)
		}
		if candidates[i].client != candidates[j].client {
			return candidates[i].client < candidates[j].client
		}
		return candidates[i].session.id < candidates[j].session.id
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
	// Cheapest filter first: stat each transcript and, using its per-file ledger key
	// (see ingestUnitKey - the path for Claude, the rollout id for Codex), skip any
	// that has not grown past its ledger marker (or was ingested within the min
	// interval) before opening and parsing the file, so a scheduler-driven sync only
	// parses genuinely new or grown files.
	type grownFile struct {
		path  string
		key   string
		size  int64
		mtime time.Time
	}
	var eligible []grownFile
	for _, f := range files {
		info, statErr := os.Stat(f)
		if statErr != nil {
			continue
		}
		key := ingestUnitKey(client, f)
		if !shouldReingest(marks[key], info.Size(), now, minInterval) {
			continue
		}
		eligible = append(eligible, grownFile{path: f, key: key, size: info.Size(), mtime: info.ModTime()})
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
		// The ledger unit is the file (ef.key), unchanged since the pre-filter above;
		// re-read its marker for the delta cursor and re-check that it still warrants
		// ingest (a size race between the stat and the parse is all that could flip
		// this). A Claude sub-agent transcript parses to session.id = the PARENT
		// sessionId, but its cursor lives under ef.key (its own path), so it never
		// collides with the parent session or a sibling sub-agent.
		mark := marks[ef.key]
		if !shouldReingest(mark, ef.size, now, minInterval) {
			continue
		}
		// Session-level ordering time: the latest turn's time when known, else the
		// first, else the ingest clock. Each chunk is stamped from its own last turn
		// (see assignChunkCaptured); this only orders the session in the backlog and
		// serves as the fallback base.
		captured := now
		if parsed, ok := parseIngestTime(session.lastTS); ok {
			captured = parsed
		} else if parsed, ok := parseIngestTime(session.firstTS); ok {
			captured = parsed
		}
		out = append(out, ingestCandidate{client: client, key: ef.key, session: session, prevTurns: mark.Turns, size: ef.size, captured: captured})
	}
	return out, nil
}

// shouldReingest reports whether a transcript now at size should be (re)ingested
// given its last marker. A session with no marker is new (ingest once); one whose
// transcript has grown past the marker is re-parsed for its delta, unless it was
// already ingested within minInterval (throttling a rapidly-growing live session).
// A session that has not grown is skipped.
func shouldReingest(mark ingestMark, size int64, now time.Time, minInterval time.Duration) bool {
	if mark.IngestedAt.IsZero() {
		return true
	}
	if size <= mark.Size {
		return false
	}
	return now.Sub(mark.IngestedAt) >= minInterval
}

// ingestUnitKey is the client-namespaced ledger/dedup identity of one transcript
// FILE. Intake keys each file's turn cursor and dedup on the physical file, not on
// the content sessionId, so a Claude sub-agent transcript - which carries its
// PARENT's sessionId - is its own ingest unit and never collides with the parent
// session or with a same-named sub-agent under a different parent.
//   - Claude: the transcript path relative to ~/.claude/projects/, which is unique
//     per file (it distinguishes .../<parentA>/subagents/foo.jsonl from
//     .../<parentB>/subagents/foo.jsonl where the bare basename would collide).
//   - Codex: the rollout id. Codex writes exactly one transcript per session, so the
//     id is already one-to-one with the file; keeping it avoids churning the Codex
//     ledger and matches the id derived from a well-formed rollout filename.
//
// It is derived from the path alone so the cheap pre-filter can key the ledger
// without parsing the file; the post-parse cursor lookup uses the same key.
func ingestUnitKey(client, path string) string {
	if client == "codex" {
		return ingestKey(client, codexIDFromPath(path))
	}
	return ingestKey(client, claudeFileKey(path))
}

// claudeFileKey is a stable, unique-per-file id: the transcript path relative to
// ~/.claude/projects/ (slash-separated for OS independence). It falls back to the
// base filename when the path lies outside that root, so the key stays deterministic
// in every case (e.g. a test transcript under a custom layout).
func claudeFileKey(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		root := filepath.Join(home, ".claude", "projects")
		if rel, err := filepath.Rel(root, path); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
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
	// Claude Code stores a top-level session at projects/<dash-encoded-cwd>/
	// <sessionId>.jsonl, but SUB-AGENT / workflow transcripts are nested deeper,
	// e.g. projects/<dash-encoded-cwd>/<parentSessionId>/subagents/workflows/
	// <name>.jsonl, and their records carry the PARENT's sessionId. On a heavy user
	// the sub-agent files vastly outnumber top-level ones, so a single-level glob
	// would miss most of the real work. Walk the whole tree for *.jsonl at any depth;
	// each physical file is keyed by its own path (see ingestUnitKey), so a nested
	// sub-agent never shares the parent session's ingest cursor.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".claude", "projects")
	var files []string
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		// Never abort the walk: an unreadable dir/file (or a missing root before the
		// first session is written) just yields fewer files, mirroring the old glob.
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if strings.Contains(path, "hgsmoke") || strings.Contains(path, "-private-tmp-") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	// Deterministic order so a fixed set of transcripts always enumerates the same.
	sort.Strings(files)
	return files, nil
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
		ts, _ := record["timestamp"].(string)
		session.turns = append(session.turns, ingestTurn{role: role, text: text, ts: ts})
		if ts != "" {
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
		session.turns = append(session.turns, ingestTurn{role: role, text: text, ts: record.Timestamp})
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

// ingestSessionQualifies gates a session on FIRST ingest: a session must carry at
// least minIngestUserText characters of real user text to ever enter intake, so a
// trivial session never does. A session's user text only grows, so an
// already-qualified session stays qualified and its later deltas always pass.
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

// sessionHeader renders the optional "Session title / Project" preamble that leads
// each chunk body. It is empty when the session has neither.
func sessionHeader(s ingestSession) string {
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
	return b.String()
}

// turnLabel is the body prefix for a turn.
func turnLabel(role string) string {
	if role == "assistant" {
		return "ASSISTANT"
	}
	return "USER"
}

// turnEntryLen is the byte length a turn contributes to a chunk body:
// "<LABEL>: <text>\n\n".
func turnEntryLen(t ingestTurn) int {
	return len(turnLabel(t.role)) + len(": ") + len(t.text) + len("\n\n")
}

// renderTurns renders a run of turns beneath header as the complete human body,
// with trailing whitespace trimmed. No turn text is ever truncated.
func renderTurns(header string, turns []ingestTurn) string {
	var b strings.Builder
	b.WriteString(header)
	for _, t := range turns {
		b.WriteString(turnLabel(t.role))
		b.WriteString(": ")
		b.WriteString(t.text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// render returns the complete session body (header + every turn, no truncation).
// It is the whole-session view; intake emits per-chunk bodies via chunkDeltaTurns.
func (s ingestSession) render() string {
	return renderTurns(sessionHeader(s), s.turns)
}

// chunkDeltaTurns splits delta (the session's new turns, whose first element is at
// absolute index base) into ordered chunks whose rendered body stays within bound
// bytes. Whole turns are accumulated into a chunk; when the next turn would push
// the body past bound, the chunk is flushed and a new one begins. A single turn
// larger than bound is its own chunk, emitted whole (never truncated).
func chunkDeltaTurns(header string, base int, delta []ingestTurn, bound int) []ingestChunk {
	var chunks []ingestChunk
	headerLen := len(header)
	start := 0
	size := headerLen
	flush := func(lo, hi int) {
		turns := delta[lo:hi]
		chunks = append(chunks, ingestChunk{
			start:  base + lo,
			end:    base + hi,
			body:   renderTurns(header, turns),
			lastTS: lastTurnTS(turns),
		})
	}
	for i, t := range delta {
		entry := turnEntryLen(t)
		if i > start && size+entry > bound {
			flush(start, i)
			start = i
			size = headerLen
		}
		size += entry
	}
	if start < len(delta) {
		flush(start, len(delta))
	}
	return chunks
}

// lastTurnTS returns the last non-empty turn timestamp in a chunk, or "" when none
// of its turns carry a parseable time.
func lastTurnTS(turns []ingestTurn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].ts != "" {
			return turns[i].ts
		}
	}
	return ""
}

// assignChunkCaptured stamps each chunk with a captured time so the chunk events of
// one delta sort in chunk order by their filename. A chunk takes its last turn's
// time; when that is missing it falls back to the session base. Because the event
// filename encodes time only to the second, later chunks are nudged to a strictly
// greater second whenever they would otherwise collide with or precede an earlier
// chunk, so the string sort of the filenames preserves chunk order.
func assignChunkCaptured(chunks []ingestChunk, sessionBase time.Time) {
	var prev time.Time
	for i := range chunks {
		c := sessionBase
		if ts, ok := parseIngestTime(chunks[i].lastTS); ok {
			c = ts
		}
		if i > 0 {
			floor := prev.Truncate(time.Second).Add(time.Second)
			if c.Before(floor) {
				c = floor
			}
		}
		chunks[i].captured = c
		prev = c
	}
}

func (a *App) ingestedSessionsPath() string {
	return filepath.Join(a.Paths.Data, "ingested-sessions.json")
}

// loadIngestedSessions reads the per-session marker ledger. It transparently
// migrates the legacy format (a JSON array of ingested keys) to the marker map by
// giving each key a zero marker. An old marker written before the delta model has
// no turns field, so Turns unmarshals to 0 and the session's whole conversation is
// re-emitted once as a complete backfill on the next ingest.
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
