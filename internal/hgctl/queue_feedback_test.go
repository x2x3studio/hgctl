package hgctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutboxBatchesEveryEventKindTogether(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Queue, 0o700); err != nil {
		t.Fatal(err)
	}
	surface := testSurface(t, identity, app.Now().UTC(), "memory/project/card.md")
	rank := 1
	feedback, err := newFeedbackEvent(identity, surface, "used", &rank, app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueueFeedback(feedback); err != nil {
		t.Fatal(err)
	}
	observation, err := newObservation(identity, "codex", "durable evidence shares one protocol", app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueue(observation); err != nil {
		t.Fatal(err)
	}
	turn, err := newTurnEvent(identity, pendingTurn{
		Client: "codex", SessionID: "session", TurnID: "turn", Prompt: "why",
	}, "because", app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueue(turn); err != nil {
		t.Fatal(err)
	}
	importBatch := producerImportEvent(t, identity, "memory.md", "durable import", "")
	if err := app.enqueue(importBatch); err != nil {
		t.Fatal(err)
	}
	batch, err := app.copyOutboxToQueue()
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.EventPaths) != MaxSyncEvents {
		t.Fatalf("all-kind one-protocol batch: %+v", batch)
	}
	joined := strings.Join(batch.EventPaths, "\n")
	for _, id := range []string{observation.ID, turn.ID, importBatch.ID, feedback.ID} {
		if !strings.Contains(joined, strings.TrimPrefix(id, "sha256:")) {
			t.Fatalf("all-kind batch omitted %s: %+v", id, batch)
		}
	}
}

func TestExpiredInterruptedFeedbackStageIsDiscardedBeforeSync(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "untracked"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newGitFixture(t)
			issuedAt := fixture.app.Now().UTC()
			surface := testSurface(t, fixture.id, issuedAt, "memory/project/card.md")
			rank := 1
			feedback, err := newFeedbackEvent(fixture.id, surface, "used", &rank, issuedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.app.enqueueFeedback(feedback); err != nil {
				t.Fatal(err)
			}
			batch, err := fixture.app.copyOutboxToQueue()
			if err != nil || len(batch.EventPaths) != 1 {
				t.Fatalf("feedback batch=%+v err=%v", batch, err)
			}
			if staged {
				runGitTest(t, fixture.app.Paths.Queue, "add", "--", batch.EventPaths[0])
			}
			fixture.app.Now = func() time.Time { return issuedAt.Add(SurfaceLifetime + time.Second) }
			if err := fixture.app.sync(testContext(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(batch.OutboxPaths[0]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expired outbox twin remains: %v", err)
			}
			queueRef := "refs/heads/" + fixture.state.QueueBranch
			tree := runGitTest(t, "", "--git-dir", fixture.remote, "ls-tree", "-r", "--name-only", queueRef)
			if strings.Contains(tree, strings.TrimPrefix(feedback.ID, "sha256:")) {
				t.Fatal("expired interrupted feedback was pushed")
			}
		})
	}
}

func TestExpiredCleanUnpushedFeedbackCommitIsRebuiltNotPushed(t *testing.T) {
	fixture := newGitFixture(t)
	issuedAt := fixture.app.Now().UTC()
	surface := testSurface(t, fixture.id, issuedAt, "memory/project/card.md")
	rank := 1
	feedback, err := newFeedbackEvent(fixture.id, surface, "used", &rank, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueueFeedback(feedback); err != nil {
		t.Fatal(err)
	}
	batch, err := fixture.app.copyOutboxToQueue()
	if err != nil || len(batch.EventPaths) != 1 {
		t.Fatalf("feedback batch=%+v err=%v", batch, err)
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "--", batch.EventPaths[0])
	runGitTest(t, fixture.app.Paths.Queue, "commit", "-m", "local feedback before offline expiry")
	localCommit := strings.TrimSpace(runGitTest(t, fixture.app.Paths.Queue, "rev-parse", "HEAD"))
	fixture.app.Now = func() time.Time { return issuedAt.Add(SurfaceLifetime + time.Second) }
	if err := fixture.app.sync(testContext(t)); err != nil {
		t.Fatal(err)
	}
	after := strings.TrimSpace(runGitTest(t, fixture.app.Paths.Queue, "rev-parse", "HEAD"))
	if after == localCommit {
		t.Fatal("expired unpublished feedback commit was retained")
	}
	queueRef := "refs/heads/" + fixture.state.QueueBranch
	tree := runGitTest(t, "", "--git-dir", fixture.remote, "ls-tree", "-r", "--name-only", queueRef)
	if strings.Contains(tree, strings.TrimPrefix(feedback.ID, "sha256:")) {
		t.Fatal("expired clean unpublished feedback was pushed")
	}
	if _, err := os.Stat(batch.OutboxPaths[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired unpublished outbox twin remains: %v", err)
	}
}

func TestInterruptedMixedKindStageRecoversEveryEvent(t *testing.T) {
	fixture := newGitFixture(t)
	observation, err := newObservation(fixture.id, "codex", "recover every event kind", fixture.app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueue(observation); err != nil {
		t.Fatal(err)
	}
	surface := testSurface(t, fixture.id, fixture.app.Now().UTC(), "memory/project/card.md")
	rank := 1
	feedback, err := newFeedbackEvent(fixture.id, surface, "used", &rank, fixture.app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueueFeedback(feedback); err != nil {
		t.Fatal(err)
	}
	observationBytes, err := canonicalEventBytes(observation)
	if err != nil {
		t.Fatal(err)
	}
	feedbackBytes, err := canonicalEventBytes(feedback)
	if err != nil {
		t.Fatal(err)
	}
	observationPath := queueEventPath(observation.CapturedAt, observation.ID)
	feedbackPath := queueEventPath(feedback.CapturedAt, feedback.ID)
	for name, content := range map[string][]byte{observationPath: observationBytes, feedbackPath: feedbackBytes} {
		target := filepath.Join(fixture.app.Paths.Queue, filepath.FromSlash(name))
		if err := writeFileAtomic(target, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "--", observationPath, feedbackPath)
	recovered, err := fixture.app.recoverInterruptedQueueBatch(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.EventPaths) != 2 {
		t.Fatalf("mixed-kind recovery lost an event: %+v", recovered)
	}
	joined := strings.Join(recovered.EventPaths, "\n")
	for _, path := range []string{observationPath, feedbackPath} {
		if !strings.Contains(joined, path) {
			t.Fatalf("mixed-kind recovery omitted %s: %+v", path, recovered)
		}
	}
	if staged := strings.TrimSpace(runGitTest(t, fixture.app.Paths.Queue, "diff", "--cached", "--name-only")); staged != "" {
		t.Fatalf("recovery left staged files: %q", staged)
	}
}

func queueEventPath(capturedAt time.Time, id string) string {
	return filepath.ToSlash(filepath.Join(
		"events", capturedAt.UTC().Format("2006"), capturedAt.UTC().Format("01"),
		strings.TrimPrefix(id, "sha256:")+".json",
	))
}
