package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/x2x3studio/hgctl/internal/product"
)

const maxPublicationManifestBytes = 1024 * 1024

type RunBinding struct {
	Repository string
	ControlSHA string
	RunID      string
	RunAttempt int
}

type ApplyOptions struct {
	Publication string
	Control     string
	Repository  string
	Binding     RunBinding
}

type ApplyResult struct {
	Paths       []string
	FilePattern string
	HasChanges  bool
}

func Apply(ctx context.Context, options ApplyOptions) (ApplyResult, error) {
	control, baseline, err := readControlArtifact(options.Control)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := matchControlRunBinding(control, options.Binding); err != nil {
		return ApplyResult{}, err
	}
	manifestContent, err := readApplyFile(filepath.Join(options.Publication, "publication.json"), maxPublicationManifestBytes)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("read publication manifest: %w", err)
	}
	manifest, err := DecodePublication(manifestContent)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("decode publication manifest: %w", err)
	}
	if err := matchRunBinding(manifest, options.Binding); err != nil {
		return ApplyResult{}, err
	}
	if err := matchControlPublication(control, manifest); err != nil {
		return ApplyResult{}, err
	}

	files, err := loadPublicationFiles(options.Publication, manifest.Files)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := verifyPublicationDirectory(options.Publication, manifest.Files); err != nil {
		return ApplyResult{}, err
	}
	if err := authenticatePublication(control, baseline, manifest.Files, files); err != nil {
		return ApplyResult{}, err
	}

	repository := gitRepository{directory: options.Repository}
	revision, err := repository.revision(ctx, "HEAD")
	if err != nil {
		return ApplyResult{}, err
	}
	status, err := repository.run(ctx, 1024*1024, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return ApplyResult{}, err
	}
	if len(status) != 0 {
		return ApplyResult{}, errors.New("publisher checkout is not clean")
	}

	_, currentContents, _, err := readSharedTree(ctx, repository, revision)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("validate publication baseline: %w", err)
	}
	if revision == manifest.Shared {
		if err := verifyBaselineRecords(control.Baseline, currentContents); err != nil {
			return ApplyResult{}, err
		}
	} else {
		if publicationAlreadyApplied(manifest.Files, files, currentContents) {
			return ApplyResult{HasChanges: false}, nil
		}
		return ApplyResult{}, errors.New("shared changed after preparation")
	}

	for _, record := range manifest.Files {
		if err := writeApplyFile(options.Repository, record.Path, files[record.Path]); err != nil {
			return ApplyResult{}, fmt.Errorf("apply %s: %w", record.Path, err)
		}
	}
	changed, err := changedRepositoryPaths(ctx, repository)
	if err != nil {
		return ApplyResult{}, err
	}
	want := make([]string, 0, len(manifest.Files))
	for _, record := range manifest.Files {
		want = append(want, record.Path)
	}
	if !equalStrings(changed, want) {
		return ApplyResult{}, fmt.Errorf("publisher diff does not match publication manifest: got %v, want %v", changed, want)
	}
	return ApplyResult{Paths: want, FilePattern: publicationFilePattern(want), HasChanges: true}, nil
}

func publicationAlreadyApplied(records []FileRecord, files, current map[string][]byte) bool {
	for _, record := range records {
		content, exists := current[record.Path]
		if !exists || !bytes.Equal(content, files[record.Path]) {
			return false
		}
	}
	return true
}

func matchControlRunBinding(control ControlManifest, binding RunBinding) error {
	if control.Repository != binding.Repository || control.ControlSHA != binding.ControlSHA ||
		control.RunID != binding.RunID || control.RunAttempt != binding.RunAttempt {
		return errors.New("control plan does not belong to this workflow attempt")
	}
	return nil
}

func matchControlPublication(control ControlManifest, publication PublicationManifest) error {
	if control.Repository != publication.Repository || control.ControlSHA != publication.ControlSHA ||
		control.RunID != publication.RunID || control.RunAttempt != publication.RunAttempt ||
		control.Shared != publication.Shared {
		return errors.New("publication does not match the trusted control plan")
	}
	return nil
}

func authenticatePublication(control ControlManifest, baseline baselineState, records []FileRecord, files map[string][]byte) error {
	semantic := make(map[string][]byte)
	controlState := make(map[string][]byte)
	var semanticBytes int64
	for _, record := range records {
		content := files[record.Path]
		if err := validatePublicationContent(record.Path, content); err != nil {
			return err
		}
		if original, exists := baseline.records[record.Path]; exists && matchesRecord(original, content) {
			return fmt.Errorf("publication contains an unchanged baseline file: %s", record.Path)
		}
		if product.IsSemanticPath(record.Path) {
			semantic[record.Path] = content
			semanticBytes += int64(len(content))
			continue
		}
		controlState[record.Path] = content
	}
	if len(semantic) > maxChangedSemanticFiles || semanticBytes > maxChangedSemanticBytes {
		return errors.New("publication semantic output exceeds the change limit")
	}
	if err := validateSemanticChanges(control, baseline, semantic); err != nil {
		return fmt.Errorf("authenticate semantic publication: %w", err)
	}
	expectedControlState := make(map[string][]byte, len(semantic))
	for name, content := range semantic {
		expectedControlState[name] = content
	}
	if err := addControlState(control, baseline, expectedControlState); err != nil {
		return fmt.Errorf("derive control operations: %w", err)
	}
	for name := range semantic {
		delete(expectedControlState, name)
	}
	for name, content := range controlState {
		expected, exists := expectedControlState[name]
		if !exists || !bytes.Equal(expected, content) {
			return fmt.Errorf("publication contains an operation not authorized by the control plan: %s", name)
		}
		delete(expectedControlState, name)
	}
	if len(expectedControlState) != 0 {
		missing := make([]string, 0, len(expectedControlState))
		for name := range expectedControlState {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("publication omits control operations: %v", missing)
	}
	return nil
}

func verifyBaselineRecords(records []FileRecord, current map[string][]byte) error {
	actual := recordsForFiles(current)
	if len(records) != len(actual) {
		return errors.New("control baseline does not match the shared tree")
	}
	for index := range records {
		if records[index] != actual[index] {
			return fmt.Errorf("control baseline differs at %s", actual[index].Path)
		}
	}
	return nil
}

func matchRunBinding(manifest PublicationManifest, binding RunBinding) error {
	if manifest.Repository != binding.Repository || manifest.ControlSHA != binding.ControlSHA ||
		manifest.RunID != binding.RunID || manifest.RunAttempt != binding.RunAttempt {
		return errors.New("publication does not belong to this workflow attempt")
	}
	return nil
}

func loadPublicationFiles(root string, records []FileRecord) (map[string][]byte, error) {
	files := make(map[string][]byte, len(records))
	for _, record := range records {
		if !allowedPublicationPath(record.Path) {
			return nil, fmt.Errorf("publication contains a forbidden path: %s", record.Path)
		}
		content, err := readApplyFile(filepath.Join(root, "files", filepath.FromSlash(record.Path)), sharedPathByteLimit(record.Path))
		if err != nil {
			return nil, fmt.Errorf("read publication file %s: %w", record.Path, err)
		}
		sum := sha256.Sum256(content)
		if int64(len(content)) != record.Bytes || hex.EncodeToString(sum[:]) != record.SHA256 {
			return nil, fmt.Errorf("publication file %s does not match its digest", record.Path)
		}
		files[record.Path] = content
	}
	return files, nil
}

func verifyPublicationDirectory(root string, records []FileRecord) error {
	want := make(map[string]struct{}, len(records)+1)
	want["publication.json"] = struct{}{}
	for _, record := range records {
		want["files/"+record.Path] = struct{}{}
	}
	seen := make(map[string]struct{}, len(want))
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == root {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("publication contains a non-regular path: %s", relative)
		}
		if info.IsDir() {
			return nil
		}
		if _, expected := want[relative]; !expected {
			return fmt.Errorf("publication contains an unexpected file: %s", relative)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(want) {
		return errors.New("publication is missing a manifest file")
	}
	return nil
}

func allowedPublicationPath(name string) bool {
	return product.IsSemanticPath(name) || (allowedSharedPath(name) && strings.HasPrefix(name, ".hourglass/"))
}

func validatePublicationContent(name string, content []byte) error {
	switch {
	case name == "Hourglass.canvas":
		if err := product.ValidateCanvas(content); err != nil {
			return fmt.Errorf("publication Canvas: %w", err)
		}
	case product.IsMemoryPath(name):
		if _, err := product.ParseNote(content); err != nil {
			return fmt.Errorf("publication note %s: %w", name, err)
		}
	case strings.HasPrefix(name, ".hourglass/cursors/"):
		value := strings.TrimSuffix(string(content), "\n")
		if string(content) != value+"\n" || !commitPattern.MatchString(value) {
			return fmt.Errorf("publication cursor %s is invalid", name)
		}
	case strings.HasPrefix(name, ".hourglass/seen/"):
		if _, _, err := decodeSeenShard(name, content); err != nil {
			return fmt.Errorf("publication seen shard: %w", err)
		}
	case strings.HasPrefix(name, ".hourglass/rejected/"):
		if _, _, err := decodeRejectionShard(name, content); err != nil {
			return fmt.Errorf("publication rejection shard: %w", err)
		}
	}
	return nil
}

func readApplyFile(name string, limit int64) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit {
		return nil, errors.New("path is not a bounded regular file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != info.Size() || int64(len(content)) > limit {
		return nil, errors.New("file changed while reading")
	}
	return content, nil
}

func writeApplyFile(root, relative string, content []byte) error {
	destination := filepath.Join(root, filepath.FromSlash(relative))
	if err := ensureApplyDirectory(root, filepath.Dir(destination)); err != nil {
		return err
	}
	if info, err := os.Lstat(destination); err == nil && !info.Mode().IsRegular() {
		return errors.New("destination is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".hourglass-apply-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, destination); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensureApplyDirectory(root, directory string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("destination escapes the publisher checkout")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("publisher parent is not a real directory: %s", current)
		}
	}
	return nil
}

func changedRepositoryPaths(ctx context.Context, repository gitRepository) ([]string, error) {
	tracked, err := repository.run(ctx, 1024*1024, "diff", "--name-only", "-z", "--no-ext-diff")
	if err != nil {
		return nil, err
	}
	untracked, err := repository.run(ctx, 1024*1024, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	paths := splitNULPaths(append(tracked, untracked...))
	sort.Strings(paths)
	return paths, nil
}

func splitNULPaths(content []byte) []string {
	var paths []string
	for _, item := range bytes.Split(content, []byte{0}) {
		if len(item) != 0 {
			paths = append(paths, string(item))
		}
	}
	return paths
}

func publicationFilePattern(paths []string) string {
	groups := make(map[string]struct{})
	for _, name := range paths {
		switch {
		case name == "Home.md", name == "Hourglass.canvas":
			groups[name] = struct{}{}
		case strings.HasPrefix(name, "memory/"):
			groups["memory"] = struct{}{}
		case strings.HasPrefix(name, ".hourglass/cursors/"):
			groups[".hourglass/cursors"] = struct{}{}
		case strings.HasPrefix(name, ".hourglass/seen/"):
			groups[".hourglass/seen"] = struct{}{}
		case strings.HasPrefix(name, ".hourglass/rejected/"):
			groups[".hourglass/rejected"] = struct{}{}
		}
	}
	values := make([]string, 0, len(groups))
	for group := range groups {
		values = append(values, group)
	}
	sort.Strings(values)
	return strings.Join(values, " ")
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
