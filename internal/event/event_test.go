package event

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample() Raw {
	return Raw{
		CapturedAt: time.Date(2026, 7, 21, 1, 2, 3, 456_000_000, time.UTC),
		Client:     "claude",
		Machine:    "a943c6d2-e7a3-48a4-a562-849aa8fa0560",
		Body:       "USER: hello",
	}
}

// Filenames are the ONLY reliable clock in the queue: commit dates are import
// time, so selection keys off the leading timestamp. Two events captured in the
// same millisecond are ordinary during a backfill, so the name must still be
// unique.
func TestFilenameSortsChronologicallyAndIsUnique(t *testing.T) {
	early := sample()
	late := sample()
	late.CapturedAt = late.CapturedAt.Add(time.Second)
	if !(early.Filename() < late.Filename()) {
		t.Fatalf("filenames do not sort by capture time: %q !< %q", early.Filename(), late.Filename())
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		name := sample().Filename()
		if seen[name] {
			t.Fatalf("duplicate filename for same-millisecond events: %q", name)
		}
		seen[name] = true
	}
	if !strings.HasSuffix(early.Filename(), ".md") {
		t.Fatalf("filename %q is not Markdown", early.Filename())
	}
}

// Frontmatter is closed. A field carrying a newline or a colon would let captured
// text terminate the block and forge the fields below it, so values are bounded
// and sanitised rather than trusted.
func TestMarshalKeepsFrontmatterClosed(t *testing.T) {
	e := sample()
	e.Title = "line one\nclient: forged\nmachine: forged"
	e.Project = strings.Repeat("x", 10_000)
	out := string(e.Marshal())

	parts := strings.SplitN(out, "---\n", 3)
	if len(parts) != 3 {
		t.Fatalf("marshal did not produce one frontmatter block:\n%s", out)
	}
	front := parts[1]
	// A newline in a value is what could close the block early or forge a key,
	// so count only LINE-INITIAL keys - the same words appearing inside a
	// flattened value are harmless text.
	keys := map[string]int{}
	for _, line := range strings.Split(strings.TrimSuffix(front, "\n"), "\n") {
		if k, _, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, " ") {
			keys[k]++
		}
	}
	for k, n := range keys {
		if n != 1 {
			t.Fatalf("key %q appears %d times; a value forged a field:\n%s", k, n, front)
		}
	}
	// The value is flattened onto its own line, so "forged" survives as TEXT -
	// what must not happen is a new top-level key appearing.
	for _, line := range strings.Split(front, "\n") {
		if strings.HasPrefix(line, "machine:") && !strings.Contains(line, "a943c6d2") {
			t.Fatalf("a newline in a value forged a field: %q", line)
		}
	}
	if strings.Count(front, "\nmachine:") > 1 {
		t.Fatalf("duplicate machine field:\n%s", front)
	}
	for _, want := range []string{"captured_at:", "client:", "machine:"} {
		if !strings.Contains(front, want) {
			t.Errorf("frontmatter missing %s", want)
		}
	}
	if !strings.Contains(out, "USER: hello") {
		t.Error("body was lost")
	}
}

// The outbox is the whole delivery guarantee: ingest writes here, sync moves a
// batch into git, and nothing is deleted until the push succeeds. So the write
// itself must be atomic - a half-written event would be published as evidence.
func TestEnqueueWritesAnAtomicReadableEvent(t *testing.T) {
	outbox := t.TempDir()
	e := sample()
	if err := Enqueue(outbox, e); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("outbox holds %d files, want exactly the event", len(entries))
	}
	// Filename() carries random bytes, so compare the parts that are derived
	// from the event rather than calling it twice.
	name := entries[0].Name()
	if !strings.HasPrefix(name, "20260721T010203") || !strings.HasSuffix(name, ".md") {
		t.Fatalf("filename %q does not encode the capture time", name)
	}
	content, err := os.ReadFile(filepath.Join(outbox, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(e.Marshal()) {
		t.Fatal("what landed in the outbox is not what Marshal produced")
	}
	_ = e
	info, err := os.Stat(filepath.Join(outbox, name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

// Intake is loose by design - no kinds, no schema, no validation - so an event
// with only the required fields must still marshal.
func TestMinimalEventIsValid(t *testing.T) {
	e := Raw{CapturedAt: time.Now().UTC(), Client: "codex", Machine: "m", Body: "x"}
	if out := string(e.Marshal()); !strings.HasPrefix(out, "---\n") {
		t.Fatalf("minimal event did not marshal:\n%s", out)
	}
}
