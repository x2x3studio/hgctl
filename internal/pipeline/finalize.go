package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/x2x3studio/hgctl/internal/product"
)

const (
	ControlManifestName       = "control.json"
	PublicationManifestName   = "publication.json"
	PublicationFilesDirectory = "files"

	maxControlManifestBytes = 8 * 1024 * 1024
	maxModelArtifactBytes   = 256 * 1024 * 1024
	maxArtifactEntries      = 50_000
	maxChangedSemanticFiles = 32
	maxChangedSemanticBytes = 2 * 1024 * 1024
)

type artifactLimits struct {
	files     int
	entries   int
	fileBytes int64
	fileLimit func(string) int64
	total     int64
}

type baselineState struct {
	records         map[string]FileRecord
	semantic        map[string]FileRecord
	seen            map[string]string
	seenShards      map[string]map[string]string
	seenShardBytes  map[string]int64
	seenLedgerBytes int64
	rejections      map[string]rejectionEntry
	rejectionShards map[string]map[string]rejectionEntry
	rejectionBytes  map[string]int64
	rejectionTotal  int64
	semanticBytes   int64
}

// Finalize turns untrusted model output into a fresh, self-contained
// publication bundle. A terminal-only plan passes an empty modelRoot.
func Finalize(modelRoot, controlRoot, publicationRoot string) (PublicationManifest, error) {
	if err := requireDisjointArtifactRoots(modelRoot, controlRoot, publicationRoot); err != nil {
		return PublicationManifest{}, err
	}
	control, baseline, err := readControlArtifact(controlRoot)
	if err != nil {
		return PublicationManifest{}, err
	}
	if len(control.Events) > maxEventsPerDream {
		return PublicationManifest{}, errors.New("control artifact exceeds the Dream event limit")
	}

	publicationFiles := make(map[string][]byte)
	if modelRoot == "" {
		if len(control.Events) != 0 {
			return PublicationManifest{}, errors.New("a semantic batch requires a model artifact")
		}
	} else {
		modelLimit, err := modelArtifactLimit(control, baseline)
		if err != nil {
			return PublicationManifest{}, err
		}
		modelFiles, err := readArtifactTree(modelRoot, modelLimit)
		if err != nil {
			return PublicationManifest{}, fmt.Errorf("read model artifact: %w", err)
		}
		changed, err := validateModelOutput(control, baseline, modelFiles)
		if err != nil {
			return PublicationManifest{}, err
		}
		for name, content := range changed {
			publicationFiles[name] = content
		}
	}

	if err := addControlState(control, baseline, publicationFiles); err != nil {
		return PublicationManifest{}, err
	}
	if len(publicationFiles) == 0 {
		return PublicationManifest{}, errors.New("control manifest contains no publication work")
	}

	manifest := PublicationManifest{
		Schema: PublicationSchema, Repository: control.Repository, ControlSHA: control.ControlSHA,
		RunID: control.RunID, RunAttempt: control.RunAttempt, Shared: control.Shared,
		Files: recordsForFiles(publicationFiles),
	}
	manifestContent, err := EncodePublication(manifest)
	if err != nil {
		return PublicationManifest{}, fmt.Errorf("encode publication manifest: %w", err)
	}
	if err := writePublicationBundle(publicationRoot, publicationFiles, manifestContent); err != nil {
		return PublicationManifest{}, err
	}
	return manifest, nil
}

func requireDisjointArtifactRoots(modelRoot, controlRoot, publicationRoot string) error {
	type artifactRoot struct {
		name string
		path string
	}
	roots := []artifactRoot{
		{name: "control", path: controlRoot},
		{name: "publication", path: publicationRoot},
	}
	if modelRoot != "" {
		roots = append(roots, artifactRoot{name: "model", path: modelRoot})
	}
	for index := range roots {
		resolved, err := resolveArtifactRoot(roots[index].path)
		if err != nil {
			return fmt.Errorf("resolve %s artifact root: %w", roots[index].name, err)
		}
		roots[index].path = strings.ToLower(resolved)
	}
	for left := 0; left < len(roots); left++ {
		for right := left + 1; right < len(roots); right++ {
			if pathContains(roots[left].path, roots[right].path) || pathContains(roots[right].path, roots[left].path) {
				return fmt.Errorf("%s and %s artifact roots overlap", roots[left].name, roots[right].name)
			}
		}
	}
	return nil
}

func resolveArtifactRoot(name string) (string, error) {
	current, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func inspectBaseline(records []FileRecord, controlFiles map[string][]byte) (baselineState, error) {
	if len(records) == 0 || len(records) > maxSharedFiles {
		return baselineState{}, errors.New("control manifest has an invalid baseline file count")
	}
	state := baselineState{
		records:         make(map[string]FileRecord, len(records)),
		semantic:        make(map[string]FileRecord),
		seen:            make(map[string]string),
		seenShards:      make(map[string]map[string]string),
		seenShardBytes:  make(map[string]int64),
		rejections:      make(map[string]rejectionEntry),
		rejectionShards: make(map[string]map[string]rejectionEntry),
		rejectionBytes:  make(map[string]int64),
	}
	folded := make(map[string]string, len(records))
	seenContents := make(map[string][]byte)
	rejectionContents := make(map[string][]byte)
	for _, record := range records {
		if legacySeenReceiptPath(record.Path) {
			return baselineState{}, fmt.Errorf("control baseline contains a legacy per-event seen receipt: %s", record.Path)
		}
		if legacyRejectionReceiptPath(record.Path) {
			return baselineState{}, fmt.Errorf("control baseline contains a legacy per-commit rejection receipt: %s", record.Path)
		}
		if !allowedSharedPath(record.Path) {
			return baselineState{}, fmt.Errorf("control baseline contains a forbidden path: %s", record.Path)
		}
		lower := strings.ToLower(record.Path)
		if previous, duplicate := folded[lower]; duplicate {
			return baselineState{}, fmt.Errorf("control baseline contains case-colliding paths: %s and %s", previous, record.Path)
		}
		folded[lower] = record.Path
		state.records[record.Path] = record
		if product.IsSemanticPath(record.Path) {
			state.semantic[record.Path] = record
			state.semanticBytes += record.Bytes
		}
		if shard, ok := seenShardName(record.Path); ok {
			content, exists := controlFiles[record.Path]
			if !exists || !matchesRecord(record, content) {
				return baselineState{}, fmt.Errorf("control artifact is missing authenticated seen shard %s", record.Path)
			}
			seenContents[record.Path] = content
			state.seenShardBytes[shard] = int64(len(content))
			state.seenLedgerBytes += int64(len(content))
		} else if shard, ok := rejectionShardName(record.Path); ok {
			content, exists := controlFiles[record.Path]
			if !exists || !matchesRecord(record, content) {
				return baselineState{}, fmt.Errorf("control artifact is missing authenticated rejection shard %s", record.Path)
			}
			rejectionContents[record.Path] = content
			state.rejectionBytes[shard] = int64(len(content))
			state.rejectionTotal += int64(len(content))
		}
	}
	for name := range controlFiles {
		if name == ControlManifestName {
			continue
		}
		_, seen := seenShardName(name)
		_, rejected := rejectionShardName(name)
		if !seen && !rejected {
			return baselineState{}, fmt.Errorf("control artifact contains an unexpected file: %s", name)
		}
		if _, exists := state.records[name]; !exists {
			return baselineState{}, fmt.Errorf("control artifact ledger shard is absent from baseline: %s", name)
		}
	}
	ledger, err := decodeSeenLedger(seenContents)
	if err != nil {
		return baselineState{}, err
	}
	state.seen = ledger.entries
	state.seenShards = ledger.shards
	rejections, err := decodeRejectionLedger(rejectionContents)
	if err != nil {
		return baselineState{}, err
	}
	state.rejections = rejections.entries
	state.rejectionShards = rejections.shards
	for _, required := range []string{".gitignore", "Home.md", "Hourglass.canvas"} {
		if _, exists := state.records[required]; !exists {
			return baselineState{}, fmt.Errorf("control baseline is missing %s", required)
		}
	}
	return state, nil
}

func modelArtifactLimit(control ControlManifest, baseline baselineState) (artifactLimits, error) {
	total := baseline.semanticBytes + control.Prompt.Bytes + maxChangedSemanticBytes
	for _, record := range control.Evidence {
		total += record.Bytes
	}
	if total < 0 || total > maxModelArtifactBytes {
		return artifactLimits{}, errors.New("model artifact exceeds the total byte limit")
	}
	files := len(baseline.semantic) + len(control.Evidence) + 1 + maxChangedSemanticFiles
	if files > maxSharedFiles+64 {
		return artifactLimits{}, errors.New("model artifact exceeds the file limit")
	}
	return artifactLimits{
		files: files, entries: maxArtifactEntries, fileBytes: maxSharedFileBytes, total: total,
	}, nil
}

func validateModelOutput(control ControlManifest, baseline baselineState, files map[string][]byte) (map[string][]byte, error) {
	prompt, exists := files[control.Prompt.Path]
	if !exists || !matchesRecord(control.Prompt, prompt) {
		return nil, errors.New("model artifact prompt does not match the control manifest")
	}
	consumed := map[string]struct{}{control.Prompt.Path: {}}
	for _, record := range control.Evidence {
		name := "workspace/" + record.Path
		content, exists := files[name]
		if !exists || !matchesRecord(record, content) {
			return nil, fmt.Errorf("model artifact evidence does not match the control manifest: %s", record.Path)
		}
		consumed[name] = struct{}{}
	}

	semantic := make(map[string][]byte)
	for name, content := range files {
		if _, fixed := consumed[name]; fixed {
			continue
		}
		if !strings.HasPrefix(name, "workspace/") {
			return nil, fmt.Errorf("model artifact contains an extra path: %s", name)
		}
		productPath := strings.TrimPrefix(name, "workspace/")
		if !product.IsSemanticPath(productPath) {
			return nil, fmt.Errorf("model artifact contains a forbidden workspace path: %s", productPath)
		}
		semantic[productPath] = content
	}
	for name := range baseline.semantic {
		if _, exists := semantic[name]; !exists {
			return nil, fmt.Errorf("model artifact deleted durable semantic content: %s", name)
		}
	}

	changed := make(map[string][]byte)
	var changedBytes int64
	for name, content := range semantic {
		record := fileRecord(name, content)
		original, existed := baseline.semantic[name]
		if existed && record.SHA256 == original.SHA256 && record.Bytes == original.Bytes {
			continue
		}
		changed[name] = content
		changedBytes += int64(len(content))
	}
	if len(changed) > maxChangedSemanticFiles || changedBytes > maxChangedSemanticBytes {
		return nil, errors.New("model semantic output exceeds the change limit")
	}
	if len(control.Events) == 0 && len(changed) != 0 {
		return nil, errors.New("semantic changes require selected events")
	}
	if err := validateSemanticChanges(control, baseline, changed); err != nil {
		return nil, err
	}
	return changed, nil
}

func validateSemanticChanges(control ControlManifest, baseline baselineState, changed map[string][]byte) error {
	current := make(map[string]struct{}, len(control.Events))
	for _, event := range control.Events {
		current[event.ID] = struct{}{}
	}
	resultingSemanticPaths := make(map[string]struct{}, len(baseline.semantic)+len(changed))
	for name := range baseline.semantic {
		resultingSemanticPaths[name] = struct{}{}
	}
	for name := range changed {
		resultingSemanticPaths[name] = struct{}{}
	}
	memoryChanged := false
	homeChanged := false
	canvasChanged := false
	for name, content := range changed {
		switch {
		case product.IsMemoryPath(name):
			note, err := product.ParseNote(content)
			if err != nil {
				return fmt.Errorf("changed note %s: %w", name, err)
			}
			citesCurrent := false
			for _, source := range note.Sources {
				if _, exists := current[source]; exists {
					citesCurrent = true
					continue
				}
				if _, exists := baseline.seen[source]; !exists {
					return fmt.Errorf("changed note %s cites an unknown event", name)
				}
			}
			if !citesCurrent {
				return fmt.Errorf("changed note %s does not cite this Dream batch", name)
			}
			memoryChanged = true
		case name == "Hourglass.canvas":
			canvasChanged = true
			if err := product.ValidateCanvasReferences(content, func(name string) bool {
				_, exists := resultingSemanticPaths[name]
				return exists
			}); err != nil {
				return fmt.Errorf("changed Hourglass.canvas: %w", err)
			}
		case name == "Home.md":
			homeChanged = true
		default:
			return fmt.Errorf("changed semantic path is forbidden: %s", name)
		}
	}
	if homeChanged && !memoryChanged && !canvasChanged {
		return errors.New("Home.md changed without a sourced memory or topology change")
	}
	return nil
}

func addControlState(control ControlManifest, baseline baselineState, files map[string][]byte) error {
	generated := make(map[string][]byte)
	for _, event := range control.Events {
		if _, exists := baseline.seen[event.ID]; exists {
			return fmt.Errorf("selected event already has a seen receipt: %s", event.ID)
		}
	}
	shards := make(map[string]map[string]string)
	selected := make([]struct {
		id      string
		machine string
	}, 0, len(control.Events))
	for _, event := range control.Events {
		selected = append(selected, struct {
			id      string
			machine string
		}{event.ID, event.Machine})
	}
	for _, event := range selected {
		shard := event.id[:2]
		entries, exists := shards[shard]
		if !exists {
			entries = cloneSeenEntries(baseline.seenShards[shard])
			shards[shard] = entries
		}
		if machine, duplicate := entries[event.id]; duplicate {
			return fmt.Errorf("selected event %s already belongs to machine %s", event.id, machine)
		}
		entries[event.id] = event.machine
	}
	seenTotal := baseline.seenLedgerBytes
	for shard, entries := range shards {
		name := ".hourglass/seen/" + shard + ".json"
		content, err := encodeSeenShard(shard, entries)
		if err != nil {
			return err
		}
		seenTotal = seenTotal - baseline.seenShardBytes[shard] + int64(len(content))
		if seenTotal > maxSeenLedgerBytes {
			return errors.New("resulting seen ledger exceeds its aggregate byte limit")
		}
		if err := addPublicationFile(generated, name, content); err != nil {
			return err
		}
	}
	for _, operation := range control.Cursors {
		name := ".hourglass/cursors/" + operation.Machine
		if err := addPublicationFile(generated, name, []byte(operation.Commit+"\n")); err != nil {
			return err
		}
	}
	rejectionShards := make(map[string]map[string]rejectionEntry)
	for _, operation := range control.Rejections {
		key := rejectionKey(operation.Machine, operation.Commit)
		if _, exists := baseline.rejections[key]; exists {
			return fmt.Errorf("rejection already exists: %s", key)
		}
		shard := operation.Commit[:2]
		entries, exists := rejectionShards[shard]
		if !exists {
			entries = cloneRejectionEntries(baseline.rejectionShards[shard])
			rejectionShards[shard] = entries
		}
		entries[key] = rejectionEntry{Machine: operation.Machine, Commit: operation.Commit, Reason: operation.Reason}
	}
	rejectionTotal := baseline.rejectionTotal
	for shard, entries := range rejectionShards {
		name := ".hourglass/rejected/" + shard + ".json"
		content, err := encodeRejectionShard(shard, entries)
		if err != nil {
			return err
		}
		rejectionTotal = rejectionTotal - baseline.rejectionBytes[shard] + int64(len(content))
		if rejectionTotal > maxRejectionLedgerBytes {
			return errors.New("resulting rejection ledger exceeds its aggregate byte limit")
		}
		if err := addPublicationFile(generated, name, content); err != nil {
			return err
		}
	}
	for name, content := range generated {
		if err := addPublicationFile(files, name, content); err != nil {
			return err
		}
	}
	return nil
}

func addPublicationFile(files map[string][]byte, name string, content []byte) error {
	if _, duplicate := files[name]; duplicate {
		return fmt.Errorf("publication contains duplicate output path: %s", name)
	}
	if !allowedSharedPath(name) {
		return fmt.Errorf("publication contains a forbidden output path: %s", name)
	}
	files[name] = content
	return nil
}

func recordsForFiles(files map[string][]byte) []FileRecord {
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	records := make([]FileRecord, 0, len(paths))
	for _, name := range paths {
		records = append(records, fileRecord(name, files[name]))
	}
	return records
}

func fileRecord(name string, content []byte) FileRecord {
	digest := sha256.Sum256(content)
	return FileRecord{Path: name, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
}

func matchesRecord(record FileRecord, content []byte) bool {
	actual := fileRecord(record.Path, content)
	return actual.SHA256 == record.SHA256 && actual.Bytes == record.Bytes
}

func readArtifactTree(root string, limits artifactLimits) (map[string][]byte, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("artifact root is not a regular directory")
	}
	files := make(map[string][]byte)
	directories := make(map[string]struct{})
	folded := make(map[string]string)
	entries := 0
	var total int64
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil || relative == "." {
			return err
		}
		entries++
		if entries > limits.entries {
			return errors.New("artifact contains too many filesystem entries")
		}
		relative = filepath.ToSlash(relative)
		if !validArtifactPath(relative) {
			return fmt.Errorf("artifact contains an invalid path: %q", relative)
		}
		lower := strings.ToLower(relative)
		if previous, collision := folded[lower]; collision {
			return fmt.Errorf("artifact contains case-colliding paths: %s and %s", previous, relative)
		}
		folded[lower] = relative
		if entry.IsDir() {
			directories[relative] = struct{}{}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact contains a symlink or special file: %s", relative)
		}
		if len(files) >= limits.files {
			return errors.New("artifact contains too many files")
		}
		maximum := limits.fileBytes
		if limits.fileLimit != nil {
			maximum = limits.fileLimit(relative)
		}
		if maximum < 0 {
			return fmt.Errorf("artifact file %s has no configured byte limit", relative)
		}
		content, err := readRegularFile(name, maximum)
		if err != nil {
			return fmt.Errorf("read artifact file %s: %w", relative, err)
		}
		total += int64(len(content))
		if total > limits.total {
			return errors.New("artifact exceeds the total byte limit")
		}
		files[relative] = content
		return nil
	})
	if err != nil {
		return nil, err
	}
	requiredDirectories := make(map[string]struct{})
	for name := range files {
		for directory := path.Dir(name); directory != "."; directory = path.Dir(directory) {
			requiredDirectories[directory] = struct{}{}
		}
	}
	for directory := range directories {
		if _, required := requiredDirectories[directory]; !required {
			return nil, fmt.Errorf("artifact contains an empty extra directory: %s", directory)
		}
	}
	return files, nil
}

func validArtifactPath(name string) bool {
	if name == "" || len(name) > product.MaxPathBytes || !utf8.ValidString(name) || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, character := range name {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func writePublicationBundle(root string, files map[string][]byte, manifest []byte) error {
	if _, err := os.Lstat(root); err == nil {
		return errors.New("publication destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(root)
		}
	}()
	for _, record := range recordsForFiles(files) {
		name := filepath.Join(root, PublicationFilesDirectory, filepath.FromSlash(record.Path))
		if err := writeSanitizedFile(name, files[record.Path]); err != nil {
			return err
		}
	}
	if err := writeSanitizedFile(filepath.Join(root, PublicationManifestName), manifest); err != nil {
		return err
	}
	complete = true
	return nil
}

func writeSanitizedFile(name string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
