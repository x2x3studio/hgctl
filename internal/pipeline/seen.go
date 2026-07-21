package pipeline

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	seenShardSchema    = "hourglass.seen-shard/v1"
	seenShardCount     = 256
	maxSeenShardBytes  = 64 * 1024 * 1024
	maxSeenLedgerBytes = 256 * 1024 * 1024
)

type seenEntry struct {
	ID      string `json:"id"`
	Machine string `json:"machine"`
}

type seenShard struct {
	Schema  string      `json:"schema"`
	Shard   string      `json:"shard"`
	Entries []seenEntry `json:"entries"`
}

type seenLedger struct {
	entries map[string]string
	shards  map[string]map[string]string
}

func newSeenLedger() seenLedger {
	return seenLedger{
		entries: make(map[string]string),
		shards:  make(map[string]map[string]string),
	}
}

func decodeSeenLedger(contents map[string][]byte) (seenLedger, error) {
	ledger := newSeenLedger()
	var total int64
	for name, content := range contents {
		if !strings.HasPrefix(name, ".hourglass/seen/") {
			continue
		}
		total += int64(len(content))
		if total > maxSeenLedgerBytes {
			return seenLedger{}, errors.New("shared seen ledger exceeds its aggregate byte limit")
		}
		shard, entries, err := decodeSeenShard(name, content)
		if err != nil {
			return seenLedger{}, err
		}
		if _, duplicate := ledger.shards[shard]; duplicate {
			return seenLedger{}, fmt.Errorf("shared seen ledger contains duplicate shard %s", shard)
		}
		ledger.shards[shard] = entries
		for id, machine := range entries {
			if _, duplicate := ledger.entries[id]; duplicate {
				return seenLedger{}, fmt.Errorf("shared seen ledger contains duplicate event %s", id)
			}
			ledger.entries[id] = machine
		}
	}
	return ledger, nil
}

func decodeSeenShard(name string, content []byte) (string, map[string]string, error) {
	shard, ok := seenShardName(name)
	if !ok {
		if legacySeenReceiptPath(name) {
			return "", nil, fmt.Errorf("legacy per-event seen receipt is not supported: %s", name)
		}
		return "", nil, fmt.Errorf("seen ledger has an invalid shard path: %s", name)
	}
	if len(content) == 0 || len(content) > maxSeenShardBytes {
		return "", nil, fmt.Errorf("seen shard %s is empty or exceeds its byte limit", name)
	}
	var document seenShard
	if err := decodeCanonicalJSON(content, &document); err != nil {
		return "", nil, fmt.Errorf("seen shard %s is not canonical: %w", name, err)
	}
	if document.Schema != seenShardSchema || document.Shard != shard || len(document.Entries) == 0 {
		return "", nil, fmt.Errorf("seen shard %s has an invalid identity or empty entry set", name)
	}
	entries := make(map[string]string, len(document.Entries))
	previous := ""
	for _, entry := range document.Entries {
		if !digestPattern.MatchString(entry.ID) || !strings.HasPrefix(entry.ID, shard) ||
			!machinePattern.MatchString(entry.Machine) || entry.ID <= previous {
			return "", nil, fmt.Errorf("seen shard %s has invalid, misplaced, or unsorted entries", name)
		}
		previous = entry.ID
		entries[entry.ID] = entry.Machine
	}
	return shard, entries, nil
}

func encodeSeenShard(shard string, entries map[string]string) ([]byte, error) {
	if !validSeenShard(shard) || len(entries) == 0 {
		return nil, errors.New("cannot encode an invalid or empty seen shard")
	}
	ids := make([]string, 0, len(entries))
	for id, machine := range entries {
		if !digestPattern.MatchString(id) || !strings.HasPrefix(id, shard) || !machinePattern.MatchString(machine) {
			return nil, errors.New("cannot encode an invalid seen entry")
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	document := seenShard{Schema: seenShardSchema, Shard: shard, Entries: make([]seenEntry, 0, len(ids))}
	for _, id := range ids {
		document.Entries = append(document.Entries, seenEntry{ID: id, Machine: entries[id]})
	}
	content, err := encodeJSON(document)
	if err != nil {
		return nil, err
	}
	if len(content) > maxSeenShardBytes {
		return nil, fmt.Errorf("seen shard %s exceeds its byte limit", shard)
	}
	return content, nil
}

func seenShardName(name string) (string, bool) {
	const prefix = ".hourglass/seen/"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
		return "", false
	}
	shard := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
	return shard, validSeenShard(shard)
}

func validSeenShard(value string) bool {
	if len(value) != 2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func legacySeenReceiptPath(name string) bool {
	parts := strings.Split(name, "/")
	return len(parts) == 4 && parts[0] == ".hourglass" && parts[1] == "seen" &&
		validSeenShard(parts[2]) && digestPattern.MatchString(parts[3])
}

func cloneSeenEntries(entries map[string]string) map[string]string {
	clone := make(map[string]string, len(entries)+1)
	for id, machine := range entries {
		clone[id] = machine
	}
	return clone
}
