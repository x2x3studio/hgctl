package fsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The point of an atomic write is that a reader never sees a half-written file.
// The endpoint is a scheduled job on a laptop that sleeps, so being killed
// mid-write is ordinary, not exotic.
func TestWriteAtomicLeavesNoPartialFileOrTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "state.json")
	if err := WriteAtomic(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want the second write", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("left a stray file behind: %s", e.Name())
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.json")
	type doc struct {
		SchemaVersion int    `json:"schema_version"`
		Name          string `json:"name"`
	}
	if err := WriteJSON(path, doc{1, "x"}, 0o600); err != nil {
		t.Fatal(err)
	}
	var got doc
	if err := ReadJSON(path, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "x" || got.SchemaVersion != 1 {
		t.Fatalf("round trip = %+v", got)
	}
	if err := ReadJSON(filepath.Join(t.TempDir(), "missing.json"), &got); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error = %v, want os.ErrNotExist so callers can branch on it", err)
	}
}

// Reading a file written by a NEWER hgctl must be an error, not a guess.
// Guessing is how persisted state gets corrupted by a downgrade.
func TestSchemaMigrationRefusesWhatItCannotUnderstand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")

	legacy := 0
	moved, err := MigrateSchema(path, &legacy, 1)
	if err != nil || !moved || legacy != 1 {
		t.Fatalf("legacy schema: moved=%v version=%d err=%v; want a silent upgrade to 1", moved, legacy, err)
	}
	current := 1
	if moved, err := MigrateSchema(path, &current, 1); err != nil || moved {
		t.Fatalf("current schema should be a no-op, got moved=%v err=%v", moved, err)
	}
	future := 99
	if _, err := MigrateSchema(path, &future, 1); !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("future schema err = %v, want ErrUnsupportedSchema", err)
	}
}

// ProbeSchema is what a candidate binary runs before promotion, so it must be
// read-only AND must treat a machine with no state yet as compatible.
func TestProbeSchemaIsReadOnlyAndToleratesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.json")
	var v struct {
		SchemaVersion int `json:"schema_version"`
	}
	exists, err := ProbeSchema(missing, &v, &v.SchemaVersion, 1)
	if err != nil || exists {
		t.Fatalf("missing file: exists=%v err=%v; a fresh machine is compatible", exists, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe created the file it was only supposed to read")
	}

	present := filepath.Join(dir, "v.json")
	if err := WriteJSON(present, map[string]int{"schema_version": 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(present)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeSchema(present, &v, &v.SchemaVersion, 1); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(present)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("probe rewrote the file; an older binary could then downgrade newer state")
	}
}

// The scheduler fires once a minute and a sync can outlast that. A second tick
// must SKIP silently, not queue up behind the first.
func TestWithLockSkipsWhenHeldAndWaitBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = WithLock(path, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ran := false
	if err := WithLock(path, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("contended WithLock returned an error instead of skipping: %v", err)
	}
	if ran {
		t.Fatal("a second holder ran concurrently; the lock does not exclude")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := WithLockWait(ctx, path, func() error { return nil }); err == nil {
		t.Fatal("WithLockWait returned success while the lock was held")
	}

	close(release)
	wg.Wait()
	if err := WithLock(path, func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("lock was not released after the holder returned")
	}
}

func TestBoundNeverSplitsARune(t *testing.T) {
	// Three bytes each, so a naive cut at 4 lands mid-rune.
	value := "中文字"
	got := Bound(value, 4)
	if len(got) > 4 {
		t.Fatalf("Bound returned %d bytes, over the limit", len(got))
	}
	for _, r := range got {
		if r == '�' {
			t.Fatalf("Bound produced a replacement char: %q", got)
		}
	}
	if Bound("short", 100) != "short" {
		t.Fatal("Bound altered a value under the limit")
	}
	if Bound("ok\xff", 100) == "ok\xff" {
		t.Fatal("Bound left invalid UTF-8 in place")
	}
}

func TestCanonicalResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Ownership checks compare paths; two spellings of one directory must not
	// look like two directories.
	if Canonical(link) != Canonical(real) {
		t.Fatalf("Canonical(link)=%q != Canonical(real)=%q", Canonical(link), Canonical(real))
	}
}
