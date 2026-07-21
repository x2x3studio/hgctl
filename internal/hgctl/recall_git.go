package hgctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxRenderedCardBytes = 64 * 1024

func (a *App) currentSharedRevision(ctx context.Context) (SharedRevision, error) {
	status, err := runCommand(ctx, a.Paths.Vault, "git", "status", "--porcelain")
	if err != nil {
		return SharedRevision{}, err
	}
	if strings.TrimSpace(status) != "" {
		return SharedRevision{}, errors.New("shared worktree is dirty")
	}
	commit, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return SharedRevision{}, err
	}
	tree, err := runCommand(ctx, a.Paths.Vault, "git", "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return SharedRevision{}, err
	}
	revision := SharedRevision{Commit: strings.TrimSpace(commit), Tree: strings.TrimSpace(tree)}
	if !validObjectID(revision.Commit) || !validObjectID(revision.Tree) {
		return SharedRevision{}, errors.New("shared revision is not a full SHA-1 commit and tree")
	}
	return revision, nil
}

func resolveTreeBlobs(ctx context.Context, repository, tree string, names []string) (map[string]string, error) {
	if !validObjectID(tree) {
		return nil, errors.New("invalid tree object id")
	}
	unique := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" || strings.ContainsRune(name, 0) || strings.ContainsRune(name, '\n') {
			return nil, fmt.Errorf("invalid tree path %q", name)
		}
		unique[name] = struct{}{}
	}
	sorted := make([]string, 0, len(unique))
	for name := range unique {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	resolved := make(map[string]string, len(sorted))
	for start := 0; start < len(sorted); {
		end := start
		argumentBytes := 0
		for end < len(sorted) && end-start < 128 {
			if end > start && argumentBytes+len(sorted[end])+1 > 64*1024 {
				break
			}
			argumentBytes += len(sorted[end]) + 1
			end++
		}
		args := []string{"ls-tree", "-z", tree, "--"}
		args = append(args, sorted[start:end]...)
		output, err := runCommand(ctx, repository, "git", args...)
		if err != nil {
			return nil, err
		}
		for _, record := range strings.Split(output, "\x00") {
			if record == "" {
				continue
			}
			metadata, name, ok := strings.Cut(record, "\t")
			fields := strings.Fields(metadata)
			if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !validObjectID(fields[2]) {
				return nil, errors.New("shared tree contains a non-regular or malformed entry")
			}
			if _, requested := unique[name]; !requested {
				return nil, fmt.Errorf("shared tree returned an unexpected path %q", name)
			}
			if _, duplicate := resolved[name]; duplicate {
				return nil, fmt.Errorf("shared tree returned duplicate path %q", name)
			}
			resolved[name] = fields[2]
		}
		start = end
	}
	return resolved, nil
}

func readGitBlob(ctx context.Context, repository, blob string, maximum int) (string, bool, error) {
	if !validObjectID(blob) || maximum <= 0 {
		return "", false, errors.New("invalid blob read request")
	}
	sizeText, err := runCommand(ctx, repository, "git", "cat-file", "-s", blob)
	if err != nil {
		return "", false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
	if err != nil || size < 0 || size > gitCommandOutputLimit {
		return "", false, errors.New("shared card blob is invalid or exceeds the read bound")
	}
	content, err := runCommand(ctx, repository, "git", "cat-file", "blob", blob)
	if err != nil {
		return "", false, err
	}
	if int64(len(content)) != size || !utf8.ValidString(content) {
		return "", false, errors.New("shared card blob is not exact UTF-8 content")
	}
	if len(content) <= maximum {
		return content, false, nil
	}
	bounded := []byte(content[:maximum])
	for len(bounded) > 0 && !utf8.Valid(bounded) {
		bounded = bounded[:len(bounded)-1]
	}
	return string(bounded), true, nil
}

func exactFileBlob(ctx context.Context, repository, tree, name string) (string, bool, error) {
	entries, err := resolveTreeBlobs(ctx, repository, tree, []string{name})
	if err != nil {
		return "", false, err
	}
	blob, exists := entries[name]
	return blob, exists, nil
}

func ensureManagedVault(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed vault is not a directory: %s", filepath.Clean(path))
	}
	return nil
}
