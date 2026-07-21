package pipeline

import (
	"context"
	"strings"
	"testing"
)

func TestSeenLedgerUsesCanonicalExactShards(t *testing.T) {
	secondID := "01" + strings.Repeat("f", 62)
	files := seenLedgerFiles(t, map[string]string{
		testDigest: testMachine,
		secondID:   "123e4567-e89b-42d3-b456-426614174001",
	})
	if len(files) != 1 {
		t.Fatalf("seen entries created %d shards", len(files))
	}
	ledger, err := decodeSeenLedger(files)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.entries[testDigest] != testMachine || ledger.entries[secondID] == "" {
		t.Fatalf("seen ledger lost exact attribution: %#v", ledger.entries)
	}
	if _, exists := ledger.entries[strings.Repeat("f", 64)]; exists {
		t.Fatal("seen ledger returned a false positive")
	}

	unsorted, err := encodeJSON(seenShard{
		Schema: seenShardSchema,
		Shard:  "01",
		Entries: []seenEntry{
			{ID: secondID, Machine: testMachine},
			{ID: testDigest, Machine: testMachine},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := decodeSeenShard(".hourglass/seen/01.json", unsorted); err == nil {
		t.Fatal("accepted an unsorted seen shard")
	}
	if _, _, err := decodeSeenShard(".hourglass/seen/01/"+testDigest, []byte(testMachine+"\n")); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy seen receipt was not rejected clearly: %v", err)
	}
}

func TestRejectionLedgerUsesCanonicalExactShards(t *testing.T) {
	secondCommit := "01" + strings.Repeat("f", 38)
	entries := map[string]rejectionEntry{
		rejectionKey(testMachine, testCommit): {
			Machine: testMachine, Commit: testCommit, Reason: "invalid-event",
		},
		rejectionKey(testMachine, secondCommit): {
			Machine: testMachine, Commit: secondCommit, Reason: "merge-commit",
		},
	}
	content, err := encodeRejectionShard("01", entries)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{".hourglass/rejected/01.json": content}
	ledger, err := decodeRejectionLedger(files)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger.entries[rejectionKey(testMachine, secondCommit)].Reason; got != "merge-commit" {
		t.Fatalf("rejection reason = %q", got)
	}
	if _, exists := ledger.entries[rejectionKey("123e4567-e89b-42d3-b456-426614174001", secondCommit)]; exists {
		t.Fatal("rejection ledger returned a false positive")
	}
	legacy := ".hourglass/rejected/" + testMachine + "/" + testCommit + ".json"
	if _, _, err := decodeRejectionShard(legacy, []byte("{}\n")); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("legacy rejection receipt was not rejected clearly: %v", err)
	}
}

func TestGitBatchBlobReaderEnforcesIndividualAndAggregateBounds(t *testing.T) {
	repository := newTestRepository(t)
	writeTestFile(t, repository, "a.txt", "alpha\n")
	writeTestFile(t, repository, "b.txt", "bravo\n")
	commitTestRepository(t, repository)
	first := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD:a.txt"))
	second := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD:b.txt"))
	requests := []blobRequest{{object: first, maximum: 6}, {object: second, maximum: 6}}
	git := gitRepository{directory: repository}
	contents, err := git.blobs(context.Background(), requests, 12)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents[first]) != "alpha\n" || string(contents[second]) != "bravo\n" {
		t.Fatalf("unexpected batch contents: %#v", contents)
	}
	if _, err := git.blobs(context.Background(), []blobRequest{{object: first, maximum: 5}}, 12); err == nil {
		t.Fatal("batch reader ignored an individual limit")
	}
	if _, err := git.blobs(context.Background(), requests, 11); err == nil {
		t.Fatal("batch reader ignored the aggregate limit")
	}
	if _, err := git.blobs(context.Background(), []blobRequest{{object: first, maximum: 6}, {object: first, maximum: 6}}, 12); err == nil {
		t.Fatal("batch reader accepted a duplicate object request")
	}
}
