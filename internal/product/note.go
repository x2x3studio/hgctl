package product

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	numberPattern = regexp.MustCompile(`^[0-9][0-9_,]*(\.[0-9_]*)?([eE][+-]?[0-9]+)?$`)
)

type Note struct {
	Title   string
	Created time.Time
	Updated time.Time
	Sources []string
}

func ParseNote(content []byte) (Note, error) {
	if !utf8.Valid(content) || bytes.IndexByte(content, '\r') >= 0 {
		return Note{}, errors.New("note must be valid UTF-8 with LF line endings")
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if !scanner.Scan() || scanner.Text() != "---" {
		return Note{}, errors.New("note must begin with frontmatter")
	}

	var note Note
	seen := make(map[string]bool, 4)
	inSources := false
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			closed = true
			break
		}
		if inSources && strings.HasPrefix(line, "  - sha256:") {
			digest := strings.TrimPrefix(line, "  - sha256:")
			if !digestPattern.MatchString(digest) {
				return Note{}, errors.New("note contains an invalid source")
			}
			for _, existing := range note.Sources {
				if existing == digest {
					return Note{}, errors.New("note contains a duplicate source")
				}
			}
			note.Sources = append(note.Sources, digest)
			continue
		}
		inSources = false
		key, value, ok := strings.Cut(line, ":")
		if !ok || seen[key] {
			return Note{}, fmt.Errorf("invalid or duplicate frontmatter field %q", line)
		}
		seen[key] = true
		switch key {
		case "title":
			if !strings.HasPrefix(value, " ") || !validPlainTitle(strings.TrimPrefix(value, " ")) {
				return Note{}, errors.New("note title is not a safe plain scalar")
			}
			note.Title = strings.TrimPrefix(value, " ")
		case "created":
			parsed, err := parseDateValue(value)
			if err != nil {
				return Note{}, fmt.Errorf("invalid created date: %w", err)
			}
			note.Created = parsed
		case "updated":
			parsed, err := parseDateValue(value)
			if err != nil {
				return Note{}, fmt.Errorf("invalid updated date: %w", err)
			}
			note.Updated = parsed
		case "sources":
			if value != "" {
				return Note{}, errors.New("sources must be a block list")
			}
			inSources = true
		default:
			return Note{}, fmt.Errorf("unknown frontmatter field %q", key)
		}
	}
	if err := scanner.Err(); err != nil {
		return Note{}, err
	}
	if !closed || !seen["title"] || !seen["created"] || !seen["updated"] || !seen["sources"] || len(note.Sources) == 0 {
		return Note{}, errors.New("note frontmatter is incomplete")
	}
	if note.Updated.Before(note.Created) {
		return Note{}, errors.New("updated date precedes created date")
	}
	return note, nil
}

func parseDateValue(value string) (time.Time, error) {
	if len(value) != 11 || value[0] != ' ' {
		return time.Time{}, errors.New("date must use YYYY-MM-DD")
	}
	return time.Parse("2006-01-02", value[1:])
}

func validPlainTitle(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	if strings.Contains(value, ": ") || strings.HasSuffix(value, ":") || strings.Contains(value, " #") {
		return false
	}
	first, _ := utf8.DecodeRuneInString(value)
	if strings.ContainsRune("-?:,{}[]#&*!|>'\"%@`", first) {
		return false
	}
	lower := strings.ToLower(value)
	switch lower {
	case "~", "null", "true", "false", "yes", "no", "on", "off", ".inf", "-.inf", ".nan":
		return false
	}
	if numberPattern.MatchString(value) {
		return false
	}
	if _, err := time.Parse("2006-01-02", value); err == nil {
		return false
	}
	return true
}
