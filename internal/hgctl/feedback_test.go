package hgctl

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSurfaceAssessmentIsFirstWriterWinsAndRetryStable(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/card.md")
	if err := app.saveSurface(testContext(t), surface); err != nil {
		t.Fatal(err)
	}
	rank := 1
	first, err := app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(first.ID, "sha256:") + ".json"
	before, err := os.ReadFile(filepath.Join(app.Paths.Outbox, name))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("identical assessment did not retry exactly: event=%#v err=%v", retry, err)
	}
	after, err := os.ReadFile(filepath.Join(app.Paths.Outbox, name))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("identical retry changed canonical outbox bytes")
	}
	if _, err := app.assessSurface(testContext(t), surface.ID, "codex", "irrelevant", &rank); err == nil || !strings.Contains(err.Error(), "different terminal") {
		t.Fatalf("conflicting terminal assessment was accepted: %v", err)
	}
	if final, err := os.ReadFile(filepath.Join(app.Paths.Outbox, name)); err != nil || !bytes.Equal(before, final) {
		t.Fatal("conflicting assessment changed first-writer bytes")
	}
	decoded, _, err := decodeCanonicalFeedbackEvent(before, name, identity.ID, app.Now().UTC())
	if err != nil || decoded.Payload.Outcome != "used" || decoded.Payload.Result == nil || *decoded.Payload.Result != 1 {
		t.Fatalf("queued feedback changed: event=%#v err=%v", decoded, err)
	}
}

func TestFeedbackRequiresMatchingClientRankAndLiveReceipt(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := app.Now().UTC()
	surface := testSurfaceV2(t, identity, issuedAt, "memory/project/card.md")
	if err := app.saveSurface(testContext(t), surface); err != nil {
		t.Fatal(err)
	}
	rank := 2
	if _, err := app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank); err == nil {
		t.Fatal("feedback accepted a rank absent from its surface")
	}
	rank = 1
	if _, err := app.assessSurface(testContext(t), surface.ID, "claude", "used", &rank); err == nil {
		t.Fatal("feedback accepted the wrong client")
	}
	app.Now = func() time.Time { return issuedAt.Add(SurfaceLifetime + time.Second) }
	if _, err := app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank); !errors.Is(err, errFeedbackExpired) {
		t.Fatalf("expired local surface error=%v", err)
	}
}

func TestExpiredFeedbackIsPrunedBeforeQueueSelection(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := app.Now().UTC().Add(-SurfaceLifetime)
	surface := testSurfaceV2(t, identity, issuedAt, "memory/project/card.md")
	rank := 1
	event, err := newFeedbackEvent(identity, surface, "used", &rank, issuedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueueFeedback(event); err != nil {
		t.Fatal(err)
	}
	app.Now = func() time.Time { return issuedAt.Add(SurfaceLifetime + time.Second) }
	if err := app.pruneExpiredFeedbackOutbox(app.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(app.Paths.Outbox)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired feedback remains in outbox: entries=%d err=%v", len(entries), err)
	}
}

func TestLocalSurfaceReceiptReadIsBoundedAndRegular(t *testing.T) {
	app := testApp(t)
	id := "sha256:" + strings.Repeat("a", 64)
	path, ok := app.surfaceReceiptPath(id)
	if !ok {
		t.Fatal("test surface id is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxLocalSurfaceReceiptBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.loadSurfaceReceipt(id); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversized local receipt was accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(app.Paths.Home, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.loadSurfaceReceipt(id); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink surface receipt was accepted: %v", err)
	}
}

func TestCorruptTransientSurfaceReceiptDoesNotBlockFutureRecall(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	corruptID := "sha256:" + strings.Repeat("b", 64)
	corruptPath, _ := app.surfaceReceiptPath(corruptID)
	if err := os.WriteFile(corruptPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/new-card.md")
	if err := app.saveSurface(testContext(t), surface); err != nil {
		t.Fatalf("corrupt transient receipt blocked a new surface: %v", err)
	}
	if _, err := os.Stat(corruptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt transient receipt was not pruned: %v", err)
	}
	if _, _, err := app.loadSurfaceReceipt(surface.ID); err != nil {
		t.Fatalf("new surface was not retained: %v", err)
	}
}

func TestTerminalReceiptReconstructsMissingOutboxAfterCrash(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/card.md")
	if err := app.saveSurface(testContext(t), surface); err != nil {
		t.Fatal(err)
	}
	rank := 1
	event, err := app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(event.ID, "sha256:") + ".json"
	path := filepath.Join(app.Paths.Outbox, name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := app.recoverTerminalFeedbackOutbox(testContext(t), app.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("terminal receipt did not restore exact feedback bytes: err=%v", err)
	}
}

func TestConcurrentConflictingAssessmentsHaveOneWinner(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/card.md")
	if err := app.saveSurface(testContext(t), surface); err != nil {
		t.Fatal(err)
	}
	type result struct {
		outcome string
		event   FeedbackEvent
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, outcome := range []string{"used", "irrelevant"} {
		workers.Add(1)
		go func(outcome string) {
			defer workers.Done()
			<-start
			rank := 1
			event, err := app.assessSurface(testContext(t), surface.ID, "codex", outcome, &rank)
			results <- result{outcome: outcome, event: event, err: err}
		}(outcome)
	}
	close(start)
	workers.Wait()
	close(results)
	winners := 0
	winningOutcome := ""
	for result := range results {
		if result.err == nil {
			winners++
			winningOutcome = result.outcome
		} else if !strings.Contains(result.err.Error(), "different terminal") {
			t.Fatalf("unexpected concurrent assessment error: %v", result.err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent assessments produced %d winners", winners)
	}
	receipt, _, err := app.loadSurfaceReceipt(surface.ID)
	if err != nil || receipt.Terminal == nil || receipt.Terminal.Payload.Outcome != winningOutcome {
		t.Fatalf("terminal receipt does not match winner: receipt=%#v err=%v", receipt, err)
	}
	name := strings.TrimPrefix(receipt.Terminal.ID, "sha256:") + ".json"
	content, err := os.ReadFile(filepath.Join(app.Paths.Outbox, name))
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := decodeCanonicalFeedbackEvent(content, name, identity.ID, app.Now().UTC())
	if err != nil || event.Payload.Outcome != winningOutcome {
		t.Fatalf("outbox does not match concurrent winner: event=%#v err=%v", event, err)
	}
}

func TestPreexistingConflictingV2OutboxIsNeverOverwritten(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/card.md")
	rank := 1
	first, err := newFeedbackEvent(identity, surface, "used", &rank, app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := newFeedbackEvent(identity, surface, "irrelevant", &rank, app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueueFeedback(first); err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(first.ID, "sha256:") + ".json"
	path := filepath.Join(app.Paths.Outbox, name)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueueFeedback(conflict); err == nil || !strings.Contains(err.Error(), "first terminal assessment differs") {
		t.Fatalf("conflicting same-id outbox was accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("conflicting same-id enqueue overwrote the first event")
	}
}

func TestAssessmentAdoptsPreexistingOutboxWinnerThroughSyncAndRetry(t *testing.T) {
	fixture := newGitFixture(t)
	surface := testSurfaceV2(t, fixture.id, fixture.app.Now().UTC(), "memory/project/card.md")
	if err := fixture.app.saveSurface(testContext(t), surface); err != nil {
		t.Fatal(err)
	}
	rank := 1
	winner, err := newFeedbackEvent(fixture.id, surface, "used", &rank, fixture.app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueueFeedback(winner); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.assessSurface(testContext(t), surface.ID, "codex", "irrelevant", &rank); err == nil || !strings.Contains(err.Error(), "different terminal") {
		t.Fatalf("assessment did not adopt the pre-existing outbox winner: %v", err)
	}
	receipt, _, err := fixture.app.loadSurfaceReceipt(surface.ID)
	if err != nil || receipt.Terminal == nil || receipt.Terminal.Payload.Outcome != "used" {
		t.Fatalf("receipt did not align to the pre-existing winner: receipt=%#v err=%v", receipt, err)
	}
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if retry, err := fixture.app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank); err != nil || retry.Payload.Outcome != "used" {
		t.Fatalf("winning assessment did not retry after delivery: event=%#v err=%v", retry, err)
	}
	if _, err := fixture.app.assessSurface(testContext(t), surface.ID, "codex", "irrelevant", &rank); err == nil {
		t.Fatal("losing assessment succeeded after the winner was delivered")
	}
	path := queueEventPath(winner.CapturedAt, winner.ID)
	remoteContent := runGitTest(t, "", "--git-dir", fixture.remote, "show", "refs/heads/"+fixture.state.QueueBranch+":"+path)
	remote, _, err := decodeCanonicalFeedbackEvent([]byte(remoteContent), filepath.Base(path), fixture.id.ID, fixture.app.Now().UTC())
	if err != nil || remote.Payload.Outcome != "used" {
		t.Fatalf("remote queue did not publish the adopted winner: event=%#v err=%v", remote, err)
	}
}

func TestDeliveredWithoutTerminalBytesFailsSafely(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/card.md")
	if err := app.saveSurface(testContext(t), surface); err != nil {
		t.Fatal(err)
	}
	rank := 1
	event, err := newFeedbackEvent(identity, surface, "used", &rank, app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueueFeedback(event); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(app.Paths.Outbox, strings.TrimPrefix(event.ID, "sha256:")+".json")
	if err := app.markDelivered([]string{path}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := app.assessSurface(testContext(t), surface.ID, "codex", "used", &rank); err == nil || !strings.Contains(err.Error(), "terminal bytes are unavailable") {
		t.Fatalf("delivered id without terminal bytes was accepted: %v", err)
	}
	receipt, _, err := app.loadSurfaceReceipt(surface.ID)
	if err != nil || receipt.Terminal != nil {
		t.Fatalf("failed assessment invented terminal bytes: receipt=%#v err=%v", receipt, err)
	}
}
