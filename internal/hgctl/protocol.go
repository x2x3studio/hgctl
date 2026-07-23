package hgctl

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxEventBytes      = 512 * 1024
	MaxTextBytes       = 120 * 1024
	MaxSyncEvents      = 4
	MaxSyncBytes       = 512 * 1024
	MaxClient          = 64
	MaxMachineHostname = 255
	// MaxMetaField bounds the session/project/title frontmatter values so a long
	// cwd or title cannot bloat the closed frontmatter; the body keeps the full
	// header regardless.
	MaxMetaField = 1024
)

// rawEvent is one captured intake item. The thin protocol has no schema, kinds,
// canonical identity, or validation: an event is a Markdown file with closed
// frontmatter (captured_at, client, machine[, hostname]) and a free-form body.
// Session-intake chunks additionally carry the session identity (session, project,
// title) and the half-open turn range this chunk covers within the session (turns);
// steady-state captures leave those zero and emit only the base frontmatter.
type rawEvent struct {
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
// steady-state captures but deterministic when Dedup is set (see rawEvent.Dedup).
func (e rawEvent) filename() string {
	suffix := randHex(4)
	if e.Dedup != "" {
		sum := sha256.Sum256([]byte(e.Dedup))
		suffix = hex.EncodeToString(sum[:4])
	}
	return e.CapturedAt.UTC().Format("20060102T150405Z") + "-" + suffix + ".md"
}

// marshal renders the event as a Markdown file.
func (e rawEvent) marshal() []byte {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "captured_at: %s\n", e.CapturedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "client: %s\n", boundString(e.Client, MaxClient))
	fmt.Fprintf(&b, "machine: %s\n", e.Machine)
	if e.Hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\n", boundString(e.Hostname, MaxMachineHostname))
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
	return boundString(strings.TrimSpace(value), limit)
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

func boundString(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	b := []byte(value)[:limit]
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

func validMachineID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	return validLowerHex(value[:8]+value[9:13]+value[14:18]+value[19:23]+value[24:], 32)
}
