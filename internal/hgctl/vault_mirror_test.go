package hgctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x2x3studio/hgctl/internal/proc"
)

// seedProduct lays out a small product in the shared worktree.
func seedProduct(t *testing.T, app *App, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(app.Paths.Shared, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func vaultMTimes(t *testing.T, app *App) map[string]time.Time {
	t.Helper()
	stamps := make(map[string]time.Time)
	err := filepath.Walk(app.Paths.Vault, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(app.Paths.Vault, path)
		if err != nil {
			return err
		}
		stamps[filepath.ToSlash(rel)] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stamps
}

// The mirror used to RemoveAll and re-copy the product on every scheduled sync,
// which refreshed every mtime once a minute. Basic Memory's incremental scan
// keys off mtime, so it concluded the whole corpus had changed and re-embedded
// all of it - a local model over 22k vector chunks, 466% CPU for minutes at a
// time, continuously, for a product that had changed by one note. An unchanged
// product must therefore leave the vault byte- and mtime-identical.
func TestMirrorLeavesUnchangedProductUntouched(t *testing.T) {
	app := testApp(t)
	seedProduct(t, app, map[string]string{
		"Home.md":                 "# Home\n",
		"memory/world/a.md":       "note a\n",
		"memory/experiences/b.md": "note b\n",
		"memory/models/deep/c.md": "note c\n",
	})
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	before := vaultMTimes(t, app)
	if len(before) != 4 {
		t.Fatalf("expected 4 mirrored files, got %d", len(before))
	}

	// A scheduled sync where shared did not move.
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	for rel, stamp := range vaultMTimes(t, app) {
		if !stamp.Equal(before[rel]) {
			t.Fatalf("%s was rewritten by a no-op mirror (mtime %s -> %s); "+
				"Basic Memory will re-embed it", rel, before[rel], stamp)
		}
	}
}

// The trap that makes the obvious fix wrong: Basic Memory rewrites the files it
// indexes (it re-serialises the YAML frontmatter and stamps in a permalink), so
// comparing the vault file against its source finds a difference for nearly
// every note and rewrites the whole product anyway. The manifest hashes the
// SOURCE, so Basic Memory's own edits are invisible to the mirror.
func TestMirrorIgnoresBasicMemorysOwnRewrites(t *testing.T) {
	app := testApp(t)
	seedProduct(t, app, map[string]string{"memory/world/a.md": "---\ntitle: A\n---\nbody\n"})
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	indexed := filepath.Join(app.Paths.Vault, "memory", "world", "a.md")
	stamped := "---\ntitle: A\npermalink: memory/world/a\n---\nbody\n"
	if err := os.WriteFile(indexed, []byte(stamped), 0o600); err != nil {
		t.Fatal(err)
	}
	before := vaultMTimes(t, app)

	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != stamped {
		t.Fatalf("mirror clobbered Basic Memory's permalink stamp, forcing it to re-stamp and re-embed:\n%s", got)
	}
	if after := vaultMTimes(t, app)["memory/world/a.md"]; !after.Equal(before["memory/world/a.md"]) {
		t.Fatal("mirror touched a file whose source had not changed")
	}
}

func TestMirrorPropagatesRealChanges(t *testing.T) {
	app := testApp(t)
	seedProduct(t, app, map[string]string{
		"memory/world/a.md":       "note a\n",
		"memory/models/deep/c.md": "note c\n",
	})
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	before := vaultMTimes(t, app)

	// Edit one note, delete another (with its now-empty directory), add a third.
	seedProduct(t, app, map[string]string{
		"memory/world/a.md":         "note a, revised\n",
		"memory/experiences/new.md": "note new\n",
	})
	if err := os.Remove(filepath.Join(app.Paths.Shared, "memory", "models", "deep", "c.md")); err != nil {
		t.Fatal(err)
	}
	// Sleep past the filesystem's mtime resolution so a rewrite is detectable.
	time.Sleep(15 * time.Millisecond)
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(app.Paths.Vault, "memory", "world", "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "note a, revised\n" {
		t.Fatalf("edited note not propagated: %q", body)
	}
	if after := vaultMTimes(t, app)["memory/world/a.md"]; after.Equal(before["memory/world/a.md"]) {
		t.Fatal("edited note kept its old mtime, so Basic Memory will never re-index it")
	}
	if _, err := os.Stat(filepath.Join(app.Paths.Vault, "memory", "experiences", "new.md")); err != nil {
		t.Fatalf("added note not mirrored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.Paths.Vault, "memory", "models", "deep", "c.md")); !os.IsNotExist(err) {
		t.Fatal("deleted note survived in the vault and stays recallable")
	}
	if _, err := os.Stat(filepath.Join(app.Paths.Vault, "memory", "models", "deep")); !os.IsNotExist(err) {
		t.Fatal("emptied directory was left behind for Basic Memory to scan")
	}
}

// The manifest is a cache, never a source of truth. Losing it, or finding a
// vault file gone, must cost one re-copy - not a permanently stale mirror.
func TestMirrorSelfHeals(t *testing.T) {
	app := testApp(t)
	seedProduct(t, app, map[string]string{"memory/world/a.md": "note a\n"})
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	indexed := filepath.Join(app.Paths.Vault, "memory", "world", "a.md")

	// A vault file vanishes while the manifest still calls it current.
	if err := os.Remove(indexed); err != nil {
		t.Fatal(err)
	}
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexed); err != nil {
		t.Fatalf("missing vault file not restored: %v", err)
	}

	// A corrupt manifest degrades to a full re-copy rather than failing the sync.
	if err := os.WriteFile(app.Paths.VaultMirror, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatalf("corrupt manifest failed the sync instead of rebuilding: %v", err)
	}
	if got := app.loadVaultMirror()["memory/world/a.md"]; got == "" {
		t.Fatal("manifest was not rebuilt")
	}
}

// Basic Memory keeps its own files in the vault (.obsidian/, scratch). The
// mirror owns only the product roots and must not delete the rest.
func TestMirrorLeavesNonProductFilesAlone(t *testing.T) {
	app := testApp(t)
	seedProduct(t, app, map[string]string{"memory/world/a.md": "note a\n"})
	if err := os.MkdirAll(filepath.Join(app.Paths.Vault, ".obsidian"), 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(app.Paths.Vault, ".obsidian", "workspace.json")
	if err := os.WriteFile(foreign, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.mirrorProductToVault(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("mirror deleted Basic Memory's own file: %v", err)
	}
}

// gitProduct makes a shared-like repo and returns a commit function.
func gitProduct(t *testing.T, dir string) func(files map[string]string, message string) string {
	t.Helper()
	ctx := testContext(t)
	run := func(args ...string) string {
		t.Helper()
		out, err := proc.Run(ctx, dir, "git", args...)
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return out
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	run("init", "--quiet")
	run("config", "user.name", "hgctl-test")
	run("config", "user.email", "hgctl-test@example.invalid")
	return func(files map[string]string, message string) string {
		t.Helper()
		for rel, body := range files {
			path := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		run("add", "--all")
		run("commit", "--quiet", "--allow-empty", "-m", message)
		return strings.TrimSpace(run("rev-parse", "HEAD"))
	}
}

// reflect advances its cursor past a noop slice with an EMPTY commit, which moves
// the SHA the index receipt keys on while leaving every indexed file identical.
// Reindexing there costs a full local-embedding-model load for nothing.
func TestProductUnchangedAcrossACursorOnlyCommit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	commit := gitProduct(t, dir)
	first := commit(map[string]string{"memory/world/a.md": "note a\n"}, "distill")
	cursorOnly := commit(nil, "reflect: advance cursor over noop slice")

	if !productUnchangedBetween(testContext(t), dir, first, cursorOnly) {
		t.Fatal("a cursor-only commit was treated as a product change; every noop slice reindexes the corpus")
	}
	edited := commit(map[string]string{"memory/world/a.md": "note a, revised\n"}, "distill")
	if productUnchangedBetween(testContext(t), dir, cursorOnly, edited) {
		t.Fatal("a real product edit was skipped; recall would go permanently stale")
	}
	added := commit(map[string]string{"Home.md": "# Home\n"}, "home")
	if productUnchangedBetween(testContext(t), dir, edited, added) {
		t.Fatal("a new Home.md was skipped")
	}
}

// The two mistakes are not symmetric. A wrong "changed" costs one reindex; a
// wrong "unchanged" writes a receipt claiming the mirror is indexed when it is
// not, and recall stays stale with nothing reporting it. So every ambiguous
// input must answer false.
func TestProductUnchangedIsConservative(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	commit := gitProduct(t, dir)
	head := commit(map[string]string{"memory/world/a.md": "note a\n"}, "distill")
	ctx := testContext(t)

	for _, tc := range []struct{ name, from, to string }{
		{"no receipt yet", "", head},
		{"empty head", head, ""},
		{"same commit is decided by the caller", head, head},
		{"unknown from", "0000000000000000000000000000000000000000", head},
		{"unknown to", head, "0000000000000000000000000000000000000000"},
		{"not a repository at all", "", ""},
	} {
		if productUnchangedBetween(ctx, dir, tc.from, tc.to) {
			t.Errorf("%s: answered unchanged, which would skip a needed reindex", tc.name)
		}
	}
}

// Only the product roots decide. A change confined to something the vault never
// mirrors must not force a reindex.
func TestProductUnchangedIgnoresNonProductPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	commit := gitProduct(t, dir)
	first := commit(map[string]string{"memory/world/a.md": "note a\n"}, "distill")
	other := commit(map[string]string{"README.md": "not part of the product\n"}, "docs")

	if !productUnchangedBetween(testContext(t), dir, first, other) {
		t.Fatal("a change outside memory/ and Home.md forced a reindex")
	}
}

// The denominator doctor compares the index against. Counting the wrong thing
// makes the check either useless (0 notes always passes) or a liar.
func TestProductNoteCountSeesOnlyProductMarkdown(t *testing.T) {
	vault := t.TempDir()
	for rel, body := range map[string]string{
		"Home.md":                      "# Home\n",
		"memory/world/a.md":            "a\n",
		"memory/experiences/deep/b.md": "b\n",
		"memory/models/c.md":           "c\n",
		"memory/world/notes.txt":       "not markdown\n",
		".obsidian/workspace.json":     "{}\n",
		"scratch/d.md":                 "outside the product roots\n",
	} {
		path := filepath.Join(vault, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := productNoteCount(vault); got != 4 {
		t.Fatalf("counted %d, want 4 (Home.md plus three notes; .txt, .obsidian and scratch/ are not the product)", got)
	}
	if got := productNoteCount(filepath.Join(vault, "does-not-exist")); got != 0 {
		t.Fatalf("a missing vault counted %d, want 0", got)
	}
}
