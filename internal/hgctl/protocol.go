package hgctl

import (
	"crypto/rand"
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
)

// rawEvent is one captured intake item. The thin protocol has no schema, kinds,
// canonical identity, or validation: an event is a Markdown file with closed
// frontmatter (captured_at, client, machine[, hostname]) and a free-form body.
type rawEvent struct {
	CapturedAt time.Time
	Client     string
	Machine    string
	Hostname   string
	Body       string
}

// filename is timestamp-ordered and collision-free.
func (e rawEvent) filename() string {
	return e.CapturedAt.UTC().Format("20060102T150405Z") + "-" + randHex(4) + ".md"
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
	b.WriteString("---\n\n")
	b.WriteString(boundText(e.Body))
	b.WriteString("\n")
	return []byte(b.String())
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

func validRequiredString(value string, limit int) bool {
	return value != "" && validOptionalString(value, limit)
}

func validOptionalString(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit
}
