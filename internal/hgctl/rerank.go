package hgctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const maxFeedbackShardBytes = 2 * 1024 * 1024

type feedbackAggregate struct {
	Key          string `json:"key"`
	Path         string `json:"path"`
	Blob         string `json:"blob"`
	Used         uint32 `json:"used"`
	Irrelevant   uint32 `json:"irrelevant"`
	Stale        uint32 `json:"stale"`
	Contradicted uint32 `json:"contradicted"`
}

type feedbackShard struct {
	Schema  string              `json:"schema"`
	Shard   string              `json:"shard"`
	Entries []feedbackAggregate `json:"entries"`
}

type recallCard struct {
	SurfaceResult
	Content   string
	Truncated bool
	class     int
	swaps     int
}

func loadFeedbackAggregates(ctx context.Context, repository, tree string, results []SurfaceResult) (map[string]feedbackAggregate, error) {
	prefixes := make(map[string]struct{})
	for _, result := range results {
		key := aggregateKey(result.Path, result.Blob)
		prefixes[key[:2]] = struct{}{}
	}
	ordered := make([]string, 0, len(prefixes))
	for prefix := range prefixes {
		ordered = append(ordered, prefix)
	}
	sort.Strings(ordered)
	loaded := make(map[string]feedbackAggregate)
	for _, prefix := range ordered {
		name := ".hourglass/feedback/" + prefix + ".json"
		blob, exists, err := exactFileBlob(ctx, repository, tree, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		content, truncated, err := readGitBlob(ctx, repository, blob, maxFeedbackShardBytes)
		if err != nil || truncated {
			return nil, fmt.Errorf("feedback shard %s exceeds its exact read bound", prefix)
		}
		entries, err := decodeFeedbackShard([]byte(content), prefix)
		if err != nil {
			return nil, fmt.Errorf("feedback shard %s: %w", prefix, err)
		}
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			paths = append(paths, entry.Path)
		}
		resolved, err := resolveTreeBlobs(ctx, repository, tree, paths)
		if err != nil {
			return nil, fmt.Errorf("feedback shard %s paths: %w", prefix, err)
		}
		for _, entry := range entries {
			if resolved[entry.Path] != entry.Blob {
				return nil, fmt.Errorf("feedback shard %s contains a retired or unbound card version", prefix)
			}
			loaded[entry.Key] = entry
		}
	}
	return loaded, nil
}

func decodeFeedbackShard(content []byte, prefix string) ([]feedbackAggregate, error) {
	if !validLowerHex(prefix, 2) || len(content) == 0 || len(content) > maxFeedbackShardBytes {
		return nil, errors.New("invalid shard identity or size")
	}
	var shard feedbackShard
	if err := decodeClosedJSON(content, &shard); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(shard)
	if err != nil {
		return nil, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(content, canonical) || shard.Schema != "hourglass.feedback-shard/v1" || shard.Shard != prefix || shard.Entries == nil {
		return nil, errors.New("shard is not canonical or has the wrong identity")
	}
	previous := ""
	for _, entry := range shard.Entries {
		if !validLowerHex(entry.Key, 64) || !strings.HasPrefix(entry.Key, prefix) || entry.Key <= previous ||
			!validMemoryPath(entry.Path) || !validObjectID(entry.Blob) || entry.Key != aggregateKey(entry.Path, entry.Blob) ||
			(entry.Used == 0 && entry.Irrelevant == 0 && entry.Stale == 0 && entry.Contradicted == 0) {
			return nil, errors.New("shard contains an invalid, misplaced, or unsorted aggregate")
		}
		previous = entry.Key
	}
	return shard.Entries, nil
}

func feedbackClass(entry feedbackAggregate) int {
	used := uint64(entry.Used)
	negative := uint64(entry.Irrelevant) + 2*uint64(entry.Stale) + 2*uint64(entry.Contradicted)
	switch {
	case used >= 2 && used >= 2*negative:
		return 1
	case negative >= 2 && negative >= 2*used:
		return -1
	default:
		return 0
	}
}

func rerankRecallCards(cards []recallCard, aggregates map[string]feedbackAggregate) {
	for index := range cards {
		cards[index].class = feedbackClass(aggregates[aggregateKey(cards[index].Path, cards[index].Blob)])
		cards[index].swaps = 0
	}
	for {
		changed := false
		for index := 0; index+1 < len(cards); index++ {
			left, right := &cards[index], &cards[index+1]
			if left.class >= right.class || left.swaps >= 2 || right.swaps >= 2 {
				continue
			}
			left.swaps++
			right.swaps++
			cards[index], cards[index+1] = cards[index+1], cards[index]
			changed = true
		}
		if !changed {
			return
		}
	}
}
