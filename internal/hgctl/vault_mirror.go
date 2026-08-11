package hgctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/x2x3studio/hgctl/internal/fsx"
	"github.com/x2x3studio/hgctl/internal/proc"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const vaultMirrorSchemaVersion = 3

// productSubset names the paths mirrored out of the shared worktree. Everything
// else in the vault - Basic Memory's own scratch, an .obsidian/ directory - is
// left alone, because the vault is Basic Memory's working copy and only this
// subset is ours to own.
var productSubset = []string{"memory", "knowledge", "Home.md", "Hourglass.canvas"}

// vaultMirror records the sha256 of each SOURCE file as of the last mirror.
//
// WHY THE SOURCE AND NOT THE DESTINATION. The obvious incremental mirror
// compares the shared file against the vault file and skips when they match.
// That never skips anything here: Basic Memory rewrites the files it indexes -
// it re-serialises the YAML frontmatter and stamps in a permalink - so 294 of
// 298 notes differ from their source the moment they are indexed. Comparing
// destinations would rewrite the whole product every sync, which is the bug
// this manifest exists to close. Hashing the source answers the only question
// that matters: did the PRODUCT change?
type vaultMirror struct {
	SchemaVersion int               `json:"schema_version"`
	Sources       map[string]string `json:"sources"`
}

// mirrorProductToVault copies the distilled product subset from the shared git
// worktree into the Basic Memory vault, a disposable non-git directory. This
// decouples Basic Memory (which rewrites indexed files) from tracked history:
// only reflect writes shared, and the vault is a throwaway copy. Extraneous
// product files are removed so supersessions and deletions propagate.
//
// It is incremental, and that is load-bearing rather than an optimisation. This
// used to RemoveAll the three product roots and copy them back wholesale on
// every scheduled sync. Every file therefore got a fresh mtime once a minute,
// Basic Memory's incremental scan concluded the entire product had changed, and
// `basic-memory reindex` re-embedded all of it - a local fastembed model over
// 22k vector chunks, measured at 466% CPU for 6+ minutes per pass. Because
// reflect self-chains while draining a backlog, shared moved again before each
// pass finished, so the machine re-embedded a corpus that had changed by one
// note, continuously. Touching only genuinely-changed files turns that back into
// what it should have been: a couple of notes re-embedded in seconds.
func (a *App) mirrorProductToVault() error {
	if err := os.MkdirAll(a.Paths.Vault, 0o700); err != nil {
		return err
	}
	sources, err := scanProductSources(a.Paths.Shared)
	if err != nil {
		return err
	}
	previous, currentSchema := a.loadVaultMirror()
	if !currentSchema {
		// A schema change alters the rendered bytes without moving shared HEAD. Make
		// the old index receipt unusable before touching any vault file, so an I/O
		// failure can leave a recoverable partial mirror but never a partial mirror
		// that claims to be fully indexed. The next sync sees the stale manifest and
		// retries every source file.
		if err := os.Remove(a.Paths.IndexedSHA); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for rel, sum := range sources {
		dst := filepath.Join(a.Paths.Vault, filepath.FromSlash(rel))
		if previous[rel] == sum {
			// Unchanged at the source. Leave the file untouched - including its
			// mtime, which is the whole point, and including whatever Basic
			// Memory has since written into it.
			if _, err := os.Stat(dst); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		data, err := os.ReadFile(filepath.Join(a.Paths.Shared, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		data = renderVaultFile(rel, data)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	if err := pruneVault(a.Paths.Vault, sources); err != nil {
		return err
	}
	return fsx.WriteJSON(a.Paths.VaultMirror, vaultMirror{
		SchemaVersion: vaultMirrorSchemaVersion,
		Sources:       sources,
	}, 0o600)
}

type vaultLinkEdit struct {
	start int
	stop  int
}

// renderVaultFile uses a CommonMark parser to decide which nodes are ordinary
// links, then edits only their destination bytes. Shared stays byte-identical and
// every unsupported or ambiguous Markdown shape stays untouched in the vault.
func renderVaultFile(rel string, data []byte) []byte {
	if !strings.HasPrefix(rel, "knowledge/") || !strings.HasSuffix(rel, ".md") {
		return data
	}
	doc := goldmark.DefaultParser().Parse(text.NewReader(data))
	bodyStart := markdownBodyStart(data)
	var edits []vaultLinkEdit
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := node.(*ast.Link)
		if !ok || link.Reference != nil || link.Pos() < bodyStart || hasImageAncestor(link) ||
			!bytes.HasPrefix(link.Destination, []byte("/concepts/")) {
			return ast.WalkContinue, nil
		}
		if edit, ok := locateInlineLinkDestination(data, link); ok {
			edits = append(edits, edit)
		}
		return ast.WalkContinue, nil
	})
	if len(edits) == 0 {
		return data
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out bytes.Buffer
	cursor := 0
	for _, edit := range edits {
		if edit.start < cursor || edit.stop > len(data) {
			continue
		}
		out.Write(data[cursor:edit.start])
		out.WriteString("/knowledge/concepts/")
		cursor = edit.stop
	}
	out.Write(data[cursor:])
	return out.Bytes()
}

func markdownBodyStart(source []byte) int {
	if !bytes.HasPrefix(source, []byte("---\n")) && !bytes.HasPrefix(source, []byte("---\r\n")) {
		return 0
	}
	for offset := bytes.IndexByte(source, '\n') + 1; offset < len(source); {
		next := bytes.IndexByte(source[offset:], '\n')
		stop := len(source)
		if next >= 0 {
			stop = offset + next + 1
		}
		line := bytes.TrimSuffix(source[offset:stop], []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.Equal(line, []byte("---")) {
			return stop
		}
		offset = stop
	}
	return 0
}

func hasImageAncestor(node ast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, ok := parent.(*ast.Image); ok {
			return true
		}
	}
	return false
}

func locateInlineLinkDestination(source []byte, link *ast.Link) (vaultLinkEdit, bool) {
	start := link.Pos()
	if start < 0 || start >= len(source) || source[start] != '[' {
		return vaultLinkEdit{}, false
	}
	close := findMarkdownLabelClose(source, start)
	if close < 0 {
		return vaultLinkEdit{}, false
	}
	at := close + 1
	for at < len(source) && (source[at] == ' ' || source[at] == '\t' || source[at] == '\n' || source[at] == '\r') {
		at++
	}
	if at >= len(source) || source[at] != '(' {
		return vaultLinkEdit{}, false
	}
	at++
	for at < len(source) && (source[at] == ' ' || source[at] == '\t' || source[at] == '\n' || source[at] == '\r') {
		at++
	}
	if at < len(source) && source[at] == '<' {
		at++
	}
	if at+len("/concepts/") > len(source) || !bytes.Equal(source[at:at+len("/concepts/")], []byte("/concepts/")) {
		return vaultLinkEdit{}, false
	}
	return vaultLinkEdit{start: at, stop: at + len("/concepts/")}, true
}

func findMarkdownLabelClose(source []byte, start int) int {
	depth := 0
	for i := start; i < len(source); i++ {
		if source[i] == '\\' {
			i++
			continue
		}
		if source[i] == '`' {
			run := 1
			for i+run < len(source) && source[i+run] == '`' {
				run++
			}
			if close := findClosingBacktickRun(source, i+run, run); close >= 0 {
				i = close - 1
				continue
			}
		}
		switch source[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func findClosingBacktickRun(source []byte, start, want int) int {
	for i := start; i < len(source); i++ {
		if source[i] != '`' {
			continue
		}
		run := 1
		for i+run < len(source) && source[i+run] == '`' {
			run++
		}
		if run == want {
			return i + run
		}
		i += run - 1
	}
	return -1
}

// scanProductSources maps each product file's slash-separated path, relative to
// the shared worktree, to the sha256 of its contents.

func scanProductSources(shared string) (map[string]string, error) {
	sources := make(map[string]string)
	for _, name := range productSubset {
		root := filepath.Join(shared, name)
		if _, err := os.Lstat(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !entry.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(shared, path)
			if err != nil {
				return err
			}
			sum, err := fileSHA256(path)
			if err != nil {
				return err
			}
			sources[filepath.ToSlash(rel)] = sum
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return sources, nil
}

// pruneVault removes product files the source no longer has, so a superseded or
// deleted note stops being recalled. It walks only the product roots, so Basic
// Memory's own files elsewhere in the vault survive.
func pruneVault(vault string, sources map[string]string) error {
	for _, name := range productSubset {
		root := filepath.Join(vault, name)
		info, err := os.Lstat(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.IsDir() {
			if _, ok := sources[name]; !ok {
				if err := os.Remove(root); err != nil {
					return err
				}
			}
			continue
		}
		var dirs []string
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				dirs = append(dirs, path)
				return nil
			}
			rel, err := filepath.Rel(vault, path)
			if err != nil {
				return err
			}
			if _, ok := sources[filepath.ToSlash(rel)]; ok {
				return nil
			}
			return os.Remove(path)
		})
		if err != nil {
			return err
		}
		// Deepest first, so a directory emptied by the walk above is itself
		// removed rather than left as an empty shell for Basic Memory to scan.
		for i := len(dirs) - 1; i >= 0; i-- {
			entries, err := os.ReadDir(dirs[i])
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			if len(entries) != 0 {
				continue
			}
			if err := os.Remove(dirs[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// loadVaultMirror never fails a sync. A missing, unreadable, or
// wrong-schema manifest degrades to "mirror everything once and rebuild it",
// which costs one full reindex and then self-heals - strictly better than
// wedging the sync that keeps recall current.
func (a *App) loadVaultMirror() (map[string]string, bool) {
	var manifest vaultMirror
	if err := fsx.ReadJSON(a.Paths.VaultMirror, &manifest); err != nil {
		return nil, false
	}
	if manifest.SchemaVersion != vaultMirrorSchemaVersion {
		return nil, false
	}
	return manifest.Sources, true
}

// productUnchangedBetween reports whether the product subset is identical at two
// shared commits. It is deliberately CONSERVATIVE: an empty or unknown commit, a
// git error, anything ambiguous answers false, so the worst case is running the
// reindex that would have run anyway. A wrong "true" is the expensive mistake -
// it would leave recall permanently stale with the receipt claiming otherwise.
func productUnchangedBetween(ctx context.Context, shared, from, to string) bool {
	if from == "" || to == "" || from == to {
		return false
	}
	for _, sha := range []string{from, to} {
		if _, err := proc.Run(ctx, shared, "git", "cat-file", "-e", sha+"^{commit}"); err != nil {
			return false
		}
	}
	args := append([]string{"diff", "--quiet", from, to, "--"}, productSubset...)
	// `git diff --quiet` exits 1 when there ARE differences, so only a clean exit
	// means unchanged; a real git failure also exits non-zero and lands on false.
	_, err := proc.Run(ctx, shared, "git", args...)
	return err == nil
}

// productNoteCount counts the Markdown notes actually sitting in the vault. It
// is the denominator doctor compares the index against: the receipt alone only
// proves a reindex once ran to completion, not that its result survived.
func productNoteCount(vault string) int {
	count := 0
	for _, name := range productSubset {
		_ = filepath.WalkDir(filepath.Join(vault, name), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".md") {
				count++
			}
			return nil
		})
	}
	return count
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
