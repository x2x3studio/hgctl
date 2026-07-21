package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/x2x3studio/hgctl/internal/product"
)

const (
	maxSharedTreeListing = 8 * 1024 * 1024
	maxSharedFileBytes   = 512 * 1024
	maxSharedFiles       = 20_000
	maxSharedTreeBytes   = maxModelArtifactBytes + maxSeenLedgerBytes + maxRejectionLedgerBytes
)

type treeEntry struct {
	Mode   string
	Type   string
	Object string
	Path   string
}

func readSharedTree(ctx context.Context, repository gitRepository, revision Revision) ([]treeEntry, map[string][]byte, sharedControlState, error) {
	output, err := repository.run(ctx, maxSharedTreeListing, "ls-tree", "-r", "-z", "--full-tree", revision.Commit)
	if err != nil {
		return nil, nil, sharedControlState{}, err
	}
	entries, err := parseTree(output)
	if err != nil {
		return nil, nil, sharedControlState{}, err
	}
	if len(entries) > maxSharedFiles {
		return nil, nil, sharedControlState{}, errors.New("shared contains too many files")
	}

	seenFolded := make(map[string]string, len(entries))
	objectLimits := make(map[string]int64, len(entries))
	for _, entry := range entries {
		if entry.Mode != "100644" || entry.Type != "blob" {
			return nil, nil, sharedControlState{}, fmt.Errorf("shared contains a non-regular file: %s", entry.Path)
		}
		if legacySeenReceiptPath(entry.Path) {
			return nil, nil, sharedControlState{}, fmt.Errorf("shared contains a legacy per-event seen receipt: %s", entry.Path)
		}
		if legacyRejectionReceiptPath(entry.Path) {
			return nil, nil, sharedControlState{}, fmt.Errorf("shared contains a legacy per-commit rejection receipt: %s", entry.Path)
		}
		if !allowedSharedPath(entry.Path) {
			return nil, nil, sharedControlState{}, fmt.Errorf("shared contains a forbidden path: %s", entry.Path)
		}
		folded := strings.ToLower(entry.Path)
		if previous, duplicate := seenFolded[folded]; duplicate {
			return nil, nil, sharedControlState{}, fmt.Errorf("shared contains case-colliding paths: %s and %s", previous, entry.Path)
		}
		seenFolded[folded] = entry.Path
		limit := sharedPathByteLimit(entry.Path)
		if previous, exists := objectLimits[entry.Object]; !exists || limit < previous {
			objectLimits[entry.Object] = limit
		}
	}
	objects := make([]string, 0, len(objectLimits))
	for object := range objectLimits {
		objects = append(objects, object)
	}
	sort.Strings(objects)
	requests := make([]blobRequest, 0, len(objects))
	for _, object := range objects {
		requests = append(requests, blobRequest{object: object, maximum: objectLimits[object]})
	}
	blobs, err := repository.blobs(ctx, requests, maxSharedTreeBytes)
	if err != nil {
		return nil, nil, sharedControlState{}, fmt.Errorf("read shared blobs: %w", err)
	}
	contents, err := materializeTree(entries, blobs, maxSharedTreeBytes)
	if err != nil {
		return nil, nil, sharedControlState{}, err
	}
	control, err := validateSharedContents(contents)
	if err != nil {
		return nil, nil, sharedControlState{}, err
	}
	return entries, contents, control, nil
}

func materializeTree(entries []treeEntry, blobs map[string][]byte, maximum int64) (map[string][]byte, error) {
	if maximum < 0 {
		return nil, errors.New("invalid shared logical byte limit")
	}
	contents := make(map[string][]byte, len(entries))
	var total int64
	for _, entry := range entries {
		content, exists := blobs[entry.Object]
		if !exists {
			return nil, fmt.Errorf("shared blob is missing for %s", entry.Path)
		}
		size := int64(len(content))
		if size > maximum-total {
			return nil, errors.New("shared exceeds its aggregate logical byte limit")
		}
		total += size
		contents[entry.Path] = content
	}
	return contents, nil
}

func parseTree(output []byte) ([]treeEntry, error) {
	records := bytes.Split(output, []byte{0})
	entries := make([]treeEntry, 0, len(records)-1)
	for _, record := range records {
		if len(record) == 0 {
			continue
		}
		metadata, name, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, errors.New("Git tree record has no path separator")
		}
		fields := strings.Fields(string(metadata))
		if len(fields) != 3 {
			return nil, errors.New("Git tree record has invalid metadata")
		}
		entry := treeEntry{Mode: fields[0], Type: fields[1], Object: fields[2], Path: string(name)}
		if !commitPattern.MatchString(entry.Object) {
			return nil, errors.New("Git tree contains an unsupported object id")
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
	return entries, nil
}

func allowedSharedPath(name string) bool {
	switch name {
	case ".gitignore", "Home.md", "Hourglass.canvas":
		return true
	}
	if product.IsMemoryPath(name) {
		return true
	}
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 3 && parts[0] == ".hourglass" && parts[1] == "cursors":
		return machinePattern.MatchString(parts[2])
	case strings.HasPrefix(name, ".hourglass/seen/"):
		_, valid := seenShardName(name)
		return valid
	case strings.HasPrefix(name, ".hourglass/rejected/"):
		_, valid := rejectionShardName(name)
		return valid
	default:
		return false
	}
}

func validateSharedContents(contents map[string][]byte) (sharedControlState, error) {
	gitignore, hasGitignore := contents[".gitignore"]
	_, hasHome := contents["Home.md"]
	canvas, hasCanvas := contents["Hourglass.canvas"]
	if !hasGitignore || !hasHome || !hasCanvas {
		return sharedControlState{}, errors.New("shared must contain .gitignore, Home.md, and Hourglass.canvas")
	}
	ignored := false
	for _, line := range strings.Split(string(gitignore), "\n") {
		if line == ".hourglass-runtime/" {
			ignored = true
		}
	}
	if !ignored {
		return sharedControlState{}, errors.New("shared .gitignore must exclude .hourglass-runtime/")
	}
	if err := product.ValidateCanvasReferences(canvas, func(name string) bool {
		_, exists := contents[name]
		return exists
	}); err != nil {
		return sharedControlState{}, fmt.Errorf("shared Hourglass.canvas: %w", err)
	}
	ledger, err := decodeSeenLedger(contents)
	if err != nil {
		return sharedControlState{}, err
	}
	rejections, err := decodeRejectionLedger(contents)
	if err != nil {
		return sharedControlState{}, err
	}
	for name, content := range contents {
		switch {
		case product.IsMemoryPath(name):
			note, err := product.ParseNote(content)
			if err != nil {
				return sharedControlState{}, fmt.Errorf("shared note %s: %w", name, err)
			}
			for _, source := range note.Sources {
				if _, exists := ledger.entries[source]; !exists {
					return sharedControlState{}, fmt.Errorf("shared note %s cites an event absent from the seen ledger", name)
				}
			}
		case strings.HasPrefix(name, ".hourglass/cursors/"):
			value := strings.TrimSuffix(string(content), "\n")
			if string(content) != value+"\n" || !commitPattern.MatchString(value) {
				return sharedControlState{}, fmt.Errorf("shared cursor %s is invalid", name)
			}
		case strings.HasPrefix(name, ".hourglass/seen/"):
			// The complete ledger was decoded above so cross-shard invariants are checked once.
		case strings.HasPrefix(name, ".hourglass/rejected/"):
			// Rejection shards were decoded above so ordering and identity are checked once.
		}
	}
	return sharedControlState{seen: ledger, rejections: rejections}, nil
}

func sharedPathByteLimit(name string) int64 {
	if _, ok := seenShardName(name); ok {
		return maxSeenShardBytes
	}
	if _, ok := rejectionShardName(name); ok {
		return maxRejectionShardBytes
	}
	return maxSharedFileBytes
}
