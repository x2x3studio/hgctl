package hgctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const vaultMirrorSchemaVersion = 1

// productSubset names the paths mirrored out of the shared worktree. Everything
// else in the vault - Basic Memory's own scratch, an .obsidian/ directory - is
// left alone, because the vault is Basic Memory's working copy and only this
// subset is ours to own.
var productSubset = []string{"memory", "Home.md", "Hourglass.canvas"}

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
	previous := a.loadVaultMirror()
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
	return writeJSONAtomic(a.Paths.VaultMirror, vaultMirror{
		SchemaVersion: vaultMirrorSchemaVersion,
		Sources:       sources,
	}, 0o600)
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
func (a *App) loadVaultMirror() map[string]string {
	var manifest vaultMirror
	if err := readJSON(a.Paths.VaultMirror, &manifest); err != nil {
		return nil
	}
	if manifest.SchemaVersion != vaultMirrorSchemaVersion {
		return nil
	}
	return manifest.Sources
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
		if _, err := runCommand(ctx, shared, "git", "cat-file", "-e", sha+"^{commit}"); err != nil {
			return false
		}
	}
	args := append([]string{"diff", "--quiet", from, to, "--"}, productSubset...)
	// `git diff --quiet` exits 1 when there ARE differences, so only a clean exit
	// means unchanged; a real git failure also exits non-zero and lands on false.
	_, err := runCommand(ctx, shared, "git", args...)
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
