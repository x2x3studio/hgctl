package hgctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutboxSelectsSchemaHomogeneousV1BeforeFeedbackV2(t *testing.T) {
	app := testApp(t)
	identity, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Queue, 0o700); err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, identity, app.Now().UTC(), "memory/project/card.md")
	rank := 1
	feedback, err := newFeedbackEvent(identity, surface, "used", &rank, app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueueFeedback(feedback); err != nil {
		t.Fatal(err)
	}
	observation, err := newObservation(identity, "codex", "v1 durable evidence has priority", app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.enqueue(observation); err != nil {
		t.Fatal(err)
	}
	first, err := app.copyOutboxToQueue()
	if err != nil {
		t.Fatal(err)
	}
	if first.Schema != Protocol || len(first.EventPaths) != 1 || !strings.Contains(first.EventPaths[0], strings.TrimPrefix(observation.ID, "sha256:")) {
		t.Fatalf("first batch is not v1-only: %+v", first)
	}
	if strings.Contains(first.EventPaths[0], strings.TrimPrefix(feedback.ID, "sha256:")) {
		t.Fatal("feedback entered a v1 queue batch")
	}
	for _, path := range first.EventPaths {
		if err := os.Remove(filepath.Join(app.Paths.Queue, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range first.OutboxPaths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	second, err := app.copyOutboxToQueue()
	if err != nil {
		t.Fatal(err)
	}
	if second.Schema != FeedbackProtocol || len(second.EventPaths) != 1 || !strings.Contains(second.EventPaths[0], strings.TrimPrefix(feedback.ID, "sha256:")) {
		t.Fatalf("second batch is not feedback-only: %+v", second)
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
			surface := testSurfaceV2(t, fixture.id, issuedAt, "memory/project/card.md")
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
	surface := testSurfaceV2(t, fixture.id, issuedAt, "memory/project/card.md")
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

func TestInterruptedMixedStageRecoversV1AndReturnsV2ToOutbox(t *testing.T) {
	fixture := newGitFixture(t)
	observation, err := newObservation(fixture.id, "codex", "recover v1 first", fixture.app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueue(observation); err != nil {
		t.Fatal(err)
	}
	surface := testSurfaceV2(t, fixture.id, fixture.app.Now().UTC(), "memory/project/card.md")
	rank := 1
	feedback, err := newFeedbackEvent(fixture.id, surface, "used", &rank, fixture.app.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.enqueueFeedback(feedback); err != nil {
		t.Fatal(err)
	}
	v1Bytes, err := canonicalEventBytes(observation)
	if err != nil {
		t.Fatal(err)
	}
	v2Bytes, err := canonicalFeedbackEventBytes(feedback)
	if err != nil {
		t.Fatal(err)
	}
	v1Path := queueEventPath(observation.CapturedAt, observation.ID)
	v2Path := queueEventPath(feedback.CapturedAt, feedback.ID)
	for name, content := range map[string][]byte{v1Path: v1Bytes, v2Path: v2Bytes} {
		target := filepath.Join(fixture.app.Paths.Queue, filepath.FromSlash(name))
		if err := writeFileAtomic(target, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGitTest(t, fixture.app.Paths.Queue, "add", "--", v1Path, v2Path)
	recovered, err := fixture.app.recoverInterruptedQueueBatch(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Schema != Protocol || len(recovered.EventPaths) != 1 || recovered.EventPaths[0] != v1Path {
		t.Fatalf("mixed recovery did not isolate v1: %+v", recovered)
	}
	if _, err := os.Stat(filepath.Join(fixture.app.Paths.Queue, filepath.FromSlash(v2Path))); !os.IsNotExist(err) {
		t.Fatalf("v2 queue copy was not returned to outbox: %v", err)
	}
	v2Outbox := filepath.Join(fixture.app.Paths.Outbox, strings.TrimPrefix(feedback.ID, "sha256:")+".json")
	if _, err := os.Stat(v2Outbox); err != nil {
		t.Fatalf("v2 outbox source was lost: %v", err)
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
