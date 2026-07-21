package hgctl

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func (a *App) importDurableAgentMemory() error {
	claudeRoot := filepath.Join(a.Paths.Home, ".claude", "projects")
	if _, err := os.Stat(claudeRoot); err == nil {
		files, err := collectMarkdown(claudeRoot, func(path string) bool {
			return filepath.Base(filepath.Dir(path)) == "memory"
		})
		if err != nil {
			return err
		}
		if _, err := a.importFiles(claudeRoot, "claude-memory", files); err != nil {
			return err
		}
	}
	codexRoot := filepath.Join(a.Paths.Home, ".codex", "memories")
	if _, err := os.Stat(codexRoot); err == nil {
		files, err := collectMarkdown(codexRoot, nil)
		if err != nil {
			return err
		}
		if _, err := a.importFiles(codexRoot, "codex-memory", files); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) importMarkdownTree(root, source string) (int, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("import path is not a directory: %s", root)
	}
	files, err := collectMarkdown(root, nil)
	if err != nil {
		return 0, err
	}
	return a.importFiles(root, source, files)
}

func collectMarkdown(root string, predicate func(string) bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".obsidian", ".github", "99-Meta", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".md") && (predicate == nil || predicate(path)) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (a *App) importFiles(root, source string, files []string) (int, error) {
	source = boundString(source, MaxImportSource)
	if strings.TrimSpace(source) == "" {
		return 0, errors.New("import source is empty")
	}
	id, err := a.loadIdentity()
	if err != nil {
		return 0, err
	}
	var batch []ImportItem
	batchBytes := 0
	count := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		payload := ImportPayload{Source: source, Items: batch}
		digest, err := importEventID(batch)
		if err != nil {
			return err
		}
		event := Event{
			Schema: Protocol, ID: digest, Kind: "import_batch", CapturedAt: a.Now().UTC(),
			Machine: Machine{ID: id.ID, Hostname: id.Hostname}, Client: "import",
			Source: Source{Kind: "bootstrap", Locator: source}, Payload: payload,
		}
		if err := a.enqueue(event); err != nil {
			return err
		}
		count++
		batch = nil
		batchBytes = 0
		return nil
	}
	addItem := func(path, content string) error {
		if !validRequiredString(path, MaxImportPath) {
			return fmt.Errorf("import item path is invalid or exceeds %d bytes", MaxImportPath)
		}
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		itemID, err := importItemID(path, hash)
		if err != nil {
			return err
		}
		item := ImportItem{ID: itemID, Path: path, SHA256: hash, Content: content}
		candidate := append(append([]ImportItem(nil), batch...), item)
		encodedSize, err := maxImportEventSize(candidate)
		if err != nil {
			return err
		}
		if len(batch) > 0 && (len(batch) == MaxImportFiles || batchBytes+len(content) > MaxImportBytes || encodedSize > MaxEventBytes) {
			if err := flush(); err != nil {
				return err
			}
			candidate = []ImportItem{item}
			encodedSize, err = maxImportEventSize(candidate)
			if err != nil {
				return err
			}
		}
		if len(content) > MaxImportText || encodedSize > MaxEventBytes {
			return fmt.Errorf("import item %s cannot fit in an event", path)
		}
		batch = append(batch, item)
		batchBytes += len(content)
		return nil
	}

	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return count, err
		}
		rel = filepath.ToSlash(rel)
		first := ""
		chunks := 0
		err = streamTextChunks(path, MaxImportText, func(chunk string) error {
			chunks++
			if chunks == 1 {
				first = chunk
				return nil
			}
			if chunks == 2 {
				if err := addItem(fmt.Sprintf("%s#chunk-%04d", rel, 1), first); err != nil {
					return err
				}
				first = ""
			}
			return addItem(fmt.Sprintf("%s#chunk-%04d", rel, chunks), chunk)
		})
		if err != nil {
			return count, err
		}
		if chunks == 1 {
			if err := addItem(rel, first); err != nil {
				return count, err
			}
		}
	}
	if err := flush(); err != nil {
		return count, err
	}
	return count, nil
}

func streamTextChunks(path string, limit int, emit func(string) error) error {
	if limit < utf8.UTFMax {
		return errors.New("text chunk limit is too small")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("import source is not a regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64*1024)
	var chunk strings.Builder
	emitted := false
	flush := func() error {
		if err := emit(chunk.String()); err != nil {
			return err
		}
		chunk.Reset()
		emitted = true
		return nil
	}
	for {
		r, size, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if r == utf8.RuneError && size == 1 {
			r = '\uFFFD'
		}
		width := utf8.RuneLen(r)
		if chunk.Len() > 0 && chunk.Len()+width > limit {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk.WriteRune(r)
	}
	if chunk.Len() > 0 || !emitted {
		return flush()
	}
	return nil
}

func maxImportEventSize(items []ImportItem) (int, error) {
	event := Event{
		Schema:     Protocol,
		ID:         "sha256:" + strings.Repeat("0", sha256.Size*2),
		Kind:       "import_batch",
		CapturedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		Machine:    Machine{ID: "00000000-0000-4000-8000-000000000000", Hostname: strings.Repeat("h", 255)},
		Client:     "import",
		Source:     Source{Kind: "bootstrap", Locator: strings.Repeat("s", MaxImportSource)},
		Payload:    ImportPayload{Source: strings.Repeat("s", MaxImportSource), Items: items},
	}
	b, err := canonicalEventBytes(event)
	return len(b), err
}
