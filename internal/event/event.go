// Package event owns the on-disk shape of one captured event and the outbox it
// lands in.
//
// An event is deliberately dumb: closed frontmatter plus a free-form body, with
// no kinds, no schema, and no validation. Intake is loose on purpose - every
// judgement about what deserves to be remembered happens later, in the one
// central reflect step - so the only rules here are the ones that keep a file
// parseable: bounded fields, no frontmatter injection, and a filename that sorts
// chronologically.
package event

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/x2x3studio/hgctl/internal/fsx"
)

const (
	MaxEventBytes = 512 * 1024
	MaxTextBytes  = 120 * 1024
	// Steady-state transport bounds: how much moves outbox -> queue in one sync.
	//
	// They protect nothing downstream. The queue is append-only storage and reflect
	// draws its own slice from it under HG_MAX_BYTES, so how fast the queue FILLS
	// cannot affect how much the model reads. The only justification was keeping a
	// per-turn Stop-hook sync small, and per-turn capture is retired - intake is
	// per-session ingest driven by the scheduler.
	//
	// At 4 events they were what starved catch-up: one sync parses up to
	// syncIngestLimit sessions and can emit dozens of chunk events while transport
	// moved four, so the outbox grew without bound (measured on another machine:
	// 635 events queued behind a 4-per-minute drain). Both have to rise together -
	// they are OR'd, first one wins - or raising the count alone just moves the
	// stall to the byte cap.
	//
	// Measured event sizes: median 4.8KB, mean 14KB, ceiling 120KB. So 1024 events
	// is ~5MB typical and ~14MB at the mean, and the count is what binds; the byte
	// cap only catches a batch that is unusually large end to end, which is what
	// keeps one commit and push finite.
	MaxSyncEvents      = 1024
	MaxSyncBytes       = 32 * 1024 * 1024
	MaxClient          = 64
	MaxMachineHostname = 255
	// MaxMetaField bounds the session/project/title frontmatter values so a long
	// cwd or title cannot bloat the closed frontmatter; the body keeps the full
	// header regardless.
	MaxMetaField = 1024
)

// Raw is one captured intake item. The thin protocol has no schema, kinds,
// canonical identity, or validation: an event is a Markdown file with closed
// frontmatter (captured_at, client, machine[, hostname]) and a free-form body.
// Session-intake chunks additionally carry the session identity (session, project,
// title) and the half-open turn range this chunk covers within the session (turns);
// steady-state captures leave those zero and emit only the base frontmatter.
type Raw struct {
	CapturedAt time.Time
	Client     string
	Machine    string
	Hostname   string
	Body       string
	// Session, Project, Title and TurnStart/TurnEnd describe a session-delta chunk
	// (see ingest.go). Session/Project/Title are omitted from the frontmatter when
	// empty; the turns line is emitted only for a real chunk (TurnEnd > TurnStart).
	Session   string
	Project   string
	Title     string
	TurnStart int
	TurnEnd   int
	// Dedup, when non-empty, makes filename() deterministic so re-enqueuing the
	// same logical event (e.g. an interrupted historical ingest re-run) overwrites
	// its outbox file and collapses against an already-published queue event via
	// byte-equality, instead of publishing a duplicate under a fresh random name.
	// Steady-state captures leave it empty and keep a random suffix, since they
	// have no stable identity and many can share one second.
	Dedup string
}

// filename is timestamp-ordered and collision-free. The suffix is random for
// steady-state captures but deterministic when Dedup is set (see Raw.Dedup).
// Filename sorts chronologically and is unique per event: the capture time to
// millisecond precision plus random bytes, because two events in one
// millisecond are ordinary during a bulk backfill.
func (e Raw) Filename() string {
	suffix := randHex(4)
	if e.Dedup != "" {
		sum := sha256.Sum256([]byte(e.Dedup))
		suffix = hex.EncodeToString(sum[:4])
	}
	return e.CapturedAt.UTC().Format("20060102T150405Z") + "-" + suffix + ".md"
}

// marshal renders the event as a Markdown file.
// Marshal renders the event as Markdown with closed frontmatter.
func (e Raw) Marshal() []byte {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "captured_at: %s\n", e.CapturedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "client: %s\n", fsx.Bound(e.Client, MaxClient))
	fmt.Fprintf(&b, "machine: %s\n", e.Machine)
	if e.Hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\n", fsx.Bound(e.Hostname, MaxMachineHostname))
	}
	if e.Session != "" {
		fmt.Fprintf(&b, "session: %s\n", frontmatterValue(e.Session, MaxMetaField))
	}
	if e.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", frontmatterValue(e.Project, MaxMetaField))
	}
	if e.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", frontmatterValue(e.Title, MaxMetaField))
	}
	if e.TurnEnd > e.TurnStart {
		fmt.Fprintf(&b, "turns: %d-%d\n", e.TurnStart, e.TurnEnd)
	}
	b.WriteString("---\n\n")
	b.WriteString(boundText(e.Body))
	b.WriteString("\n")
	return []byte(b.String())
}

// frontmatterValue keeps a value on a single line so it cannot break the closed
// frontmatter, then bounds its length.
func frontmatterValue(value string, limit int) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return fsx.Bound(strings.TrimSpace(value), limit)
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

func boundText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return '�'
		}
		return r
	}, value)
	if len(value) <= MaxTextBytes {
		return value
	}
	b := []byte(value)[:MaxTextBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// Enqueue atomically writes one event into the outbox.
//
// The outbox exists so intake never has to touch git: ingest writes here, and
// the next sync moves a batch into the queue worktree and pushes. Nothing is
// deleted until that push succeeds, so a failed push replays rather than losing
// the event - which is the whole of the delivery guarantee. There is no content
// identity or receipt; dedup is git's job downstream.
func Enqueue(outbox string, e Raw) error {
	return fsx.WriteAtomic(filepath.Join(outbox, e.Filename()), e.Marshal(), 0o600)
}
