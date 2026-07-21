package pipeline

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	rejectionShardSchema    = "hourglass.rejection-shard/v1"
	rejectionShardCount     = 256
	maxRejectionShardBytes  = 8 * 1024 * 1024
	maxRejectionLedgerBytes = 64 * 1024 * 1024
)

type rejectionEntry struct {
	Machine string `json:"machine"`
	Commit  string `json:"commit"`
	Reason  string `json:"reason"`
}

type rejectionShard struct {
	Schema  string           `json:"schema"`
	Shard   string           `json:"shard"`
	Entries []rejectionEntry `json:"entries"`
}

type rejectionLedger struct {
	entries map[string]rejectionEntry
	shards  map[string]map[string]rejectionEntry
}

func newRejectionLedger() rejectionLedger {
	return rejectionLedger{
		entries: make(map[string]rejectionEntry),
		shards:  make(map[string]map[string]rejectionEntry),
	}
}

func decodeRejectionLedger(contents map[string][]byte) (rejectionLedger, error) {
	ledger := newRejectionLedger()
	var total int64
	for name, content := range contents {
		if !strings.HasPrefix(name, ".hourglass/rejected/") {
			continue
		}
		total += int64(len(content))
		if total > maxRejectionLedgerBytes {
			return rejectionLedger{}, errors.New("shared rejection ledger exceeds its aggregate byte limit")
		}
		shard, entries, err := decodeRejectionShard(name, content)
		if err != nil {
			return rejectionLedger{}, err
		}
		if _, duplicate := ledger.shards[shard]; duplicate {
			return rejectionLedger{}, fmt.Errorf("shared rejection ledger contains duplicate shard %s", shard)
		}
		ledger.shards[shard] = entries
		for key, entry := range entries {
			if _, duplicate := ledger.entries[key]; duplicate {
				return rejectionLedger{}, fmt.Errorf("shared rejection ledger contains duplicate entry %s", key)
			}
			ledger.entries[key] = entry
		}
	}
	return ledger, nil
}

func decodeRejectionShard(name string, content []byte) (string, map[string]rejectionEntry, error) {
	shard, ok := rejectionShardName(name)
	if !ok {
		if legacyRejectionReceiptPath(name) {
			return "", nil, fmt.Errorf("legacy per-commit rejection receipt is not supported: %s", name)
		}
		return "", nil, fmt.Errorf("rejection ledger has an invalid shard path: %s", name)
	}
	if len(content) == 0 || len(content) > maxRejectionShardBytes {
		return "", nil, fmt.Errorf("rejection shard %s is empty or exceeds its byte limit", name)
	}
	var document rejectionShard
	if err := decodeCanonicalJSON(content, &document); err != nil {
		return "", nil, fmt.Errorf("rejection shard %s is not canonical: %w", name, err)
	}
	if document.Schema != rejectionShardSchema || document.Shard != shard || len(document.Entries) == 0 {
		return "", nil, fmt.Errorf("rejection shard %s has an invalid identity or empty entry set", name)
	}
	entries := make(map[string]rejectionEntry, len(document.Entries))
	previous := ""
	for _, entry := range document.Entries {
		key := rejectionKey(entry.Machine, entry.Commit)
		if !machinePattern.MatchString(entry.Machine) || !commitPattern.MatchString(entry.Commit) ||
			!strings.HasPrefix(entry.Commit, shard) || !validReason(entry.Reason) || key <= previous {
			return "", nil, fmt.Errorf("rejection shard %s has invalid, misplaced, or unsorted entries", name)
		}
		previous = key
		entries[key] = entry
	}
	return shard, entries, nil
}

func encodeRejectionShard(shard string, entries map[string]rejectionEntry) ([]byte, error) {
	if !validSeenShard(shard) || len(entries) == 0 {
		return nil, errors.New("cannot encode an invalid or empty rejection shard")
	}
	keys := make([]string, 0, len(entries))
	for key, entry := range entries {
		if key != rejectionKey(entry.Machine, entry.Commit) || !machinePattern.MatchString(entry.Machine) ||
			!commitPattern.MatchString(entry.Commit) || !strings.HasPrefix(entry.Commit, shard) || !validReason(entry.Reason) {
			return nil, errors.New("cannot encode an invalid rejection entry")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	document := rejectionShard{Schema: rejectionShardSchema, Shard: shard, Entries: make([]rejectionEntry, 0, len(keys))}
	for _, key := range keys {
		document.Entries = append(document.Entries, entries[key])
	}
	content, err := encodeJSON(document)
	if err != nil {
		return nil, err
	}
	if len(content) > maxRejectionShardBytes {
		return nil, fmt.Errorf("rejection shard %s exceeds its byte limit", shard)
	}
	return content, nil
}

func rejectionShardName(name string) (string, bool) {
	const prefix = ".hourglass/rejected/"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
		return "", false
	}
	shard := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
	return shard, validSeenShard(shard)
}

func legacyRejectionReceiptPath(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 4 && parts[0] == ".hourglass" && parts[1] == "rejected" &&
		machinePattern.MatchString(parts[2]) && strings.HasSuffix(parts[3], ".json") &&
		commitPattern.MatchString(strings.TrimSuffix(parts[3], ".json"))
}

func rejectionKey(machine, commit string) string {
	return machine + "/" + commit
}

func cloneRejectionEntries(entries map[string]rejectionEntry) map[string]rejectionEntry {
	clone := make(map[string]rejectionEntry, len(entries)+1)
	for key, entry := range entries {
		clone[key] = entry
	}
	return clone
}
