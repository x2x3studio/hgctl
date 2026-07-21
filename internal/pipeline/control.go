package pipeline

import (
	"errors"
	"fmt"
)

type sharedControlState struct {
	seen       seenLedger
	rejections rejectionLedger
}

func readControlArtifact(root string) (ControlManifest, baselineState, error) {
	if root == "" {
		return ControlManifest{}, baselineState{}, errors.New("control artifact path is required")
	}
	files, err := readArtifactTree(root, artifactLimits{
		files:     seenShardCount + rejectionShardCount + 1,
		entries:   seenShardCount + rejectionShardCount + 4,
		fileBytes: maxSeenShardBytes,
		fileLimit: controlArtifactFileLimit,
		total:     maxControlManifestBytes + maxSeenLedgerBytes + maxRejectionLedgerBytes,
	})
	if err != nil {
		return ControlManifest{}, baselineState{}, fmt.Errorf("read control artifact: %w", err)
	}
	content, exists := files[ControlManifestName]
	if !exists {
		return ControlManifest{}, baselineState{}, errors.New("control artifact has no control.json")
	}
	control, err := DecodeControl(content)
	if err != nil {
		return ControlManifest{}, baselineState{}, fmt.Errorf("decode control manifest: %w", err)
	}
	baseline, err := inspectBaseline(control.Baseline, files)
	if err != nil {
		return ControlManifest{}, baselineState{}, fmt.Errorf("validate control baseline: %w", err)
	}
	return control, baseline, nil
}

func controlArtifactFileLimit(name string) int64 {
	if name == ControlManifestName {
		return maxControlManifestBytes
	}
	if _, ok := seenShardName(name); ok {
		return maxSeenShardBytes
	}
	if _, ok := rejectionShardName(name); ok {
		return maxRejectionShardBytes
	}
	return maxControlManifestBytes
}
