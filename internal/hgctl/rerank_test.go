package hgctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFeedbackRerankMatchesFrozenVectors(t *testing.T) {
	type vectorItem struct {
		ID           string `json:"id"`
		Used         uint32 `json:"used"`
		Irrelevant   uint32 `json:"irrelevant"`
		Stale        uint32 `json:"stale"`
		Contradicted uint32 `json:"contradicted"`
		Class        int    `json:"class"`
	}
	var corpus struct {
		Schema  string `json:"schema"`
		Vectors []struct {
			Name     string       `json:"name"`
			Items    []vectorItem `json:"items"`
			Expected []string     `json:"expected"`
		} `json:"vectors"`
	}
	content, err := os.ReadFile(filepath.Join("testdata", "protocol", "event", "rerank.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &corpus); err != nil || corpus.Schema != "hourglass.feedback-rerank-v1" {
		t.Fatalf("invalid rerank corpus: %v", err)
	}
	for _, vector := range corpus.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			cards := make([]recallCard, len(vector.Items))
			aggregates := make(map[string]feedbackAggregate, len(vector.Items))
			idsByPath := make(map[string]string, len(vector.Items))
			for index, item := range vector.Items {
				name := "memory/test/card-" + formatIndex(index) + ".md"
				blob := repeatedObjectID(index + 1)
				cards[index].SurfaceResult = SurfaceResult{Path: name, Blob: blob}
				key := aggregateKey(name, blob)
				aggregate := feedbackAggregate{
					Key: key, Path: name, Blob: blob, Used: item.Used, Irrelevant: item.Irrelevant,
					Stale: item.Stale, Contradicted: item.Contradicted,
				}
				if got := feedbackClass(aggregate); got != item.Class {
					t.Fatalf("class(%s)=%d, want %d", item.ID, got, item.Class)
				}
				aggregates[key] = aggregate
				idsByPath[name] = item.ID
			}
			rerankRecallCards(cards, aggregates)
			got := make([]string, len(cards))
			for index, card := range cards {
				got[index] = idsByPath[card.Path]
				if card.swaps > 2 {
					t.Fatalf("%s moved through %d swaps", got[index], card.swaps)
				}
			}
			if !reflect.DeepEqual(got, vector.Expected) {
				t.Fatalf("rerank=%v, want %v", got, vector.Expected)
			}
		})
	}
}

func TestFeedbackShardDecoderIsClosedCanonicalAndVersionBound(t *testing.T) {
	name := "memory/test/card.md"
	blob := repeatedObjectID(1)
	key := aggregateKey(name, blob)
	valid, err := json.Marshal(feedbackShard{
		Schema: "hourglass.feedback-shard/v1", Shard: key[:2],
		Entries: []feedbackAggregate{{Key: key, Path: name, Blob: blob, Used: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	valid = append(valid, '\n')
	if entries, err := decodeFeedbackShard(valid, key[:2]); err != nil || len(entries) != 1 {
		t.Fatalf("valid shard rejected: entries=%v err=%v", entries, err)
	}
	for _, content := range [][]byte{
		append([]byte(" "), valid...),
		[]byte(`{"schema":"hourglass.feedback-shard/v1","shard":"` + key[:2] + `","entries":[{"key":"` + key + `","path":"` + name + `","blob":"` + blob + `","used":2,"irrelevant":0,"stale":0,"contradicted":0,"reason":"forbidden"}]}` + "\n"),
		[]byte(`{"schema":"hourglass.feedback-shard/v1","shard":"` + key[:2] + `","entries":[{"key":"` + key + `","path":"` + name + `","blob":"` + repeatedObjectID(2) + `","used":2,"irrelevant":0,"stale":0,"contradicted":0}]}` + "\n"),
	} {
		if _, err := decodeFeedbackShard(content, key[:2]); err == nil {
			t.Fatalf("invalid shard accepted: %s", content)
		}
	}
}

func repeatedObjectID(value int) string {
	characters := "0123456789abcdef"
	return repeatByte(characters[value%len(characters)], 40)
}

func repeatByte(value byte, count int) string {
	content := make([]byte, count)
	for index := range content {
		content[index] = value
	}
	return string(content)
}
