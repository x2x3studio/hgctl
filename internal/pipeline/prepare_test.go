package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const secondTestMachine = "223e4567-e89b-42d3-b456-426614174000"

type prepareFixture struct {
	repository string
	main       string
	prompt     string
}

type fixtureEvent struct {
	path    string
	content []byte
	id      string
}

func TestPrepareCreatesClosedArtifactsAndExactManifest(t *testing.T) {
	fixture := newPrepareFixture(t)
	first := makeObservationEvent(t, testMachine, "first")
	second := makeObservationEvent(t, testMachine, "second")
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{
		{first.path: first.content},
		{second.path: second.content},
	})
	options := fixture.options(t, 0)

	result, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.HasWork || !result.HasSemanticEvidence || len(result.Manifest.Events) != 2 {
		t.Fatalf("unexpected prepare result: %#v", result)
	}
	if len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Commit != commits[1] {
		t.Fatalf("cursor did not advance through the selected commits: %#v", result.Manifest.Cursors)
	}
	if len(result.Manifest.Baseline) != 3 {
		t.Fatalf("baseline omitted tracked shared files: %#v", result.Manifest.Baseline)
	}
	baselinePaths := []string{
		result.Manifest.Baseline[0].Path,
		result.Manifest.Baseline[1].Path,
		result.Manifest.Baseline[2].Path,
	}
	if want := []string{".gitignore", "Home.md", "Hourglass.canvas"}; !reflect.DeepEqual(baselinePaths, want) {
		t.Fatalf("baseline paths = %v, want %v", baselinePaths, want)
	}

	controlContent, err := os.ReadFile(filepath.Join(options.ControlDirectory, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeControl(controlContent)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, result.Manifest) {
		t.Fatalf("control artifact differs from result\nartifact: %#v\nresult: %#v", decoded, result.Manifest)
	}
	files := artifactFiles(t, options.ModelDirectory)
	wantFiles := []string{
		"prompt.md",
		"workspace/.hourglass-runtime/incoming/" + testMachine + "/" + first.id + ".json",
		"workspace/.hourglass-runtime/incoming/" + testMachine + "/" + second.id + ".json",
		"workspace/Home.md",
		"workspace/Hourglass.canvas",
	}
	sort.Strings(wantFiles)
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("model artifact files = %v, want %v", files, wantFiles)
	}
	for _, event := range []fixtureEvent{first, second} {
		content, err := os.ReadFile(filepath.Join(options.ModelDirectory, "workspace", filepath.FromSlash(".hourglass-runtime/incoming/"+testMachine+"/"+event.id+".json")))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(content, event.content) {
			t.Fatalf("evidence %s changed in the artifact", event.id)
		}
	}
	if _, err := Prepare(context.Background(), options); err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("prepare reused non-fresh destinations: %v", err)
	}
}

func TestPrepareRotatesQueuesAndSelectsOneSemanticMachine(t *testing.T) {
	fixture := newPrepareFixture(t)
	first := makeObservationEvent(t, testMachine, "machine-a")
	second := makeObservationEvent(t, secondTestMachine, "machine-b")
	fixture.createQueue(t, testMachine, []map[string][]byte{{first.path: first.content}})
	fixture.createQueue(t, secondTestMachine, []map[string][]byte{{second.path: second.content}})

	machines := []string{testMachine, secondTestMachine}
	sort.Strings(machines)
	for slot, wantMachine := range machines {
		options := fixture.options(t, uint64(slot))
		result, err := Prepare(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Manifest.Events) != 1 || result.Manifest.Events[0].Machine != wantMachine {
			t.Fatalf("slot %d selected %#v, want machine %s", slot, result.Manifest.Events, wantMachine)
		}
		if got := result.Manifest.QueueTips; len(got) != 2 || got[0].Machine != machines[0] || got[1].Machine != machines[1] {
			t.Fatalf("queue tips are not complete and sorted: %#v", got)
		}
	}
}

func TestPrepareBoundsInspectedCommitsAcrossResourceDeferredQueues(t *testing.T) {
	fixture := newPrepareFixture(t)
	identifier := strings.Repeat("a", 64)
	oversized := bytes.Repeat([]byte("x"), maxClassificationBlob+1)
	machines := make([]string, 40)
	for index := range machines {
		machines[index] = indexedTestMachine(index)
	}
	commits := fixture.createQueue(t, machines[0], []map[string][]byte{{
		"events/2026/07/" + identifier + ".json": oversized,
	}})
	for _, machine := range machines[1:] {
		runTestGit(t, fixture.repository, "update-ref", "refs/remotes/origin/queue/"+machine, commits[0])
	}

	result, err := Prepare(context.Background(), fixture.options(t, 35))
	if err != nil {
		t.Fatal(err)
	}
	if result.HasWork || result.HasSemanticEvidence || len(result.Manifest.QueueTips) != len(machines) {
		t.Fatalf("bounded inspection changed work or queue discovery: %#v", result)
	}
	if len(result.Notices) != maxInspectedCommits {
		t.Fatalf("inspected %d deferred commits, want %d", len(result.Notices), maxInspectedCommits)
	}
	inspected := make(map[string]struct{}, len(result.Notices))
	for _, notice := range result.Notices {
		if notice.Kind != "deferred" || notice.Reason != "event-too-large-to-classify" {
			t.Fatalf("unexpected inspection notice: %#v", notice)
		}
		inspected[notice.Machine] = struct{}{}
	}
	if _, exists := inspected[machines[35]]; !exists {
		t.Fatal("rotated starting queue was not inspected")
	}
	if _, exists := inspected[machines[34]]; exists {
		t.Fatal("inspection continued beyond the global budget")
	}
}

func TestPrepareResourceDeferralDoesNotBlockAnotherQueue(t *testing.T) {
	fixture := newPrepareFixture(t)
	identifier := strings.Repeat("a", 64)
	oversized := bytes.Repeat([]byte("x"), maxClassificationBlob+1)
	fixture.createQueue(t, testMachine, []map[string][]byte{{
		"events/2026/07/" + identifier + ".json": oversized,
	}})
	badCommits := fixture.createQueue(t, secondTestMachine, []map[string][]byte{{"README.md": []byte("not an event\n")}})

	options := fixture.options(t, 0)
	result, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasSemanticEvidence || !result.HasWork {
		t.Fatalf("unexpected work classification: %#v", result)
	}
	if len(result.Manifest.Rejections) != 1 || result.Manifest.Rejections[0].Machine != secondTestMachine ||
		result.Manifest.Rejections[0].Commit != badCommits[0] {
		t.Fatalf("malformed queue did not receive a terminal rejection: %#v", result.Manifest.Rejections)
	}
	if len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Machine != secondTestMachine {
		t.Fatalf("only the malformed queue should advance: %#v", result.Manifest.Cursors)
	}
	for _, operation := range result.Manifest.Rejections {
		if operation.Machine == testMachine {
			t.Fatal("resource-deferred queue received a rejection")
		}
	}
	deferred := false
	for _, notice := range result.Notices {
		if notice.Kind == "deferred" && notice.Machine == testMachine && notice.Reason == "event-too-large-to-classify" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatalf("resource-limited queue was not explicitly deferred: %#v", result.Notices)
	}
}

func TestInvalidKnownEventDoesNotDiscardValidPeer(t *testing.T) {
	fixture := newPrepareFixture(t)
	durable := makeObservationEvent(t, testMachine, "preserve me")
	invalidID := strings.Repeat("a", 64)
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{{
		durable.path:                            durable.content,
		"events/2026/07/" + invalidID + ".json": []byte(`{"schema":"hourglass.event/v1"}` + "\n"),
	}})
	result, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Events) != 1 || result.Manifest.Events[0].ID != durable.id {
		t.Fatalf("valid event was discarded with its invalid peer: %#v", result.Manifest.Events)
	}
	if len(result.Manifest.Rejections) != 1 || result.Manifest.Rejections[0].Reason != "invalid-event" ||
		len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Commit != commits[0] {
		t.Fatalf("partially invalid commit was not terminal: %#v", result.Manifest)
	}
}

func TestDuplicateEventIDPreservesFirstDeterministicEvent(t *testing.T) {
	fixture := newPrepareFixture(t)
	july := makeObservationEvent(t, testMachine, "same semantic event")
	june := fixtureEvent{
		path:    strings.Replace(july.path, "/07/", "/06/", 1),
		content: bytes.Replace(july.content, []byte("2026-07-21T00:00:00Z"), []byte("2026-06-21T00:00:00Z"), 1),
		id:      july.id,
	}
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{{
		june.path: june.content,
		july.path: july.content,
	}})
	result, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Events) != 1 || result.Manifest.Events[0].QueuePath != june.path {
		t.Fatalf("duplicate identity did not preserve the first sorted event: %#v", result.Manifest.Events)
	}
	if len(result.Manifest.Rejections) != 1 || result.Manifest.Rejections[0].Reason != "duplicate-event-id" ||
		len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Commit != commits[0] {
		t.Fatalf("duplicate identity commit was not terminal: %#v", result.Manifest)
	}
}

func TestInvalidDuplicatePathCannotShadowLaterValidEvent(t *testing.T) {
	fixture := newPrepareFixture(t)
	valid := makeObservationEvent(t, testMachine, "valid duplicate candidate")
	invalidPath := strings.Replace(valid.path, "/07/", "/06/", 1)
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{{
		invalidPath: []byte(`{"schema":"hourglass.event/v1"}` + "\n"),
		valid.path:  valid.content,
	}})
	result, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Events) != 1 || result.Manifest.Events[0].QueuePath != valid.path {
		t.Fatalf("invalid first duplicate shadowed the valid event: %#v", result.Manifest.Events)
	}
	if len(result.Manifest.Rejections) != 1 || result.Manifest.Rejections[0].Reason != "invalid-event" ||
		len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Commit != commits[0] {
		t.Fatalf("invalid duplicate commit was not terminal: %#v", result.Manifest)
	}
}

func TestPrepareHonorsSeenReceiptsAndFourCommitWindow(t *testing.T) {
	fixture := newPrepareFixture(t)
	commitsInput := make([]map[string][]byte, 0, 5)
	events := make([]fixtureEvent, 0, 5)
	for index := 0; index < 5; index++ {
		event := makeObservationEvent(t, testMachine, "seen-"+strconv.Itoa(index))
		events = append(events, event)
		commitsInput = append(commitsInput, map[string][]byte{event.path: event.content})
	}
	commits := fixture.createQueue(t, testMachine, commitsInput)
	fixture.updateShared(t, func() {
		seen := make(map[string]string, len(events))
		for _, event := range events {
			seen[event.id] = testMachine
		}
		writeSeenLedger(t, fixture.repository, seen)
	})

	options := fixture.options(t, 0)
	result, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasSemanticEvidence || !result.HasWork || len(result.Manifest.Cursors) != 1 {
		t.Fatalf("unexpected seen-only result: %#v", result)
	}
	if result.Manifest.Cursors[0].Commit != commits[3] {
		t.Fatalf("cursor = %s, want fourth commit %s", result.Manifest.Cursors[0].Commit, commits[3])
	}
	publicationRoot := filepath.Join(t.TempDir(), "publication")
	if _, err := os.Stat(options.ModelDirectory); !os.IsNotExist(err) {
		t.Fatalf("terminal-only prepare materialized a model artifact: %v", err)
	}
	publication, err := Finalize("", options.ControlDirectory, publicationRoot)
	if err != nil {
		t.Fatalf("finalize terminal-only prepare artifacts: %v", err)
	}
	if paths := publicationPaths(publication); len(paths) != 1 || paths[0] != ".hourglass/cursors/"+testMachine {
		t.Fatalf("terminal-only publication paths = %v", paths)
	}
}

func TestPrepareDoesNotAdvancePartiallySelectedCommit(t *testing.T) {
	fixture := newPrepareFixture(t)
	changes := make(map[string][]byte)
	for index := 0; index < 4; index++ {
		event := makeObservationEvent(t, testMachine, "partial-"+strconv.Itoa(index))
		changes[event.path] = event.content
	}
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{changes})

	result, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Events) != maxEventsPerDream || len(result.Manifest.Cursors) != 0 {
		t.Fatalf("partial commit was not left pending: events=%d cursors=%#v", len(result.Manifest.Events), result.Manifest.Cursors)
	}
	fixture.updateShared(t, func() {
		seen := make(map[string]string, len(result.Manifest.Events))
		for _, event := range result.Manifest.Events {
			seen[event.ID] = testMachine
		}
		writeSeenLedger(t, fixture.repository, seen)
	})

	second, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Manifest.Events) != maxEventsPerDream || len(second.Manifest.Cursors) != 1 ||
		second.Manifest.Cursors[0].Commit != commits[0] {
		t.Fatalf("second pass did not finish the partial commit: events=%d cursors=%#v", len(second.Manifest.Events), second.Manifest.Cursors)
	}
}

func TestPrepareLeavesCommitPendingWhenEvidenceBudgetIsExhausted(t *testing.T) {
	fixture := newPrepareFixture(t)
	first := makeImportEvent(t, testMachine, "first", 'a')
	second := makeImportEvent(t, testMachine, "second", 'b')
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{
		{first.path: first.content},
		{second.path: second.content},
	})

	result, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Manifest.Events) != 1 || result.Manifest.Events[0].ID != first.id {
		t.Fatalf("evidence budget selected unexpected events: %#v", result.Manifest.Events)
	}
	if len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Commit != commits[0] {
		t.Fatalf("cursor advanced past pending evidence: %#v", result.Manifest.Cursors)
	}
	if result.Manifest.Evidence[0].Bytes > maxEvidenceBytes {
		t.Fatalf("selected evidence exceeds budget: %#v", result.Manifest.Evidence)
	}
}

func TestPrepareRejectsCommitAboveAggregateEventByteLimit(t *testing.T) {
	fixture := newPrepareFixture(t)
	first := makeImportEvent(t, testMachine, "oversized-first", 'a')
	second := makeImportEvent(t, testMachine, "oversized-second", 'b')
	commits := fixture.createQueue(t, testMachine, []map[string][]byte{{
		first.path:  first.content,
		second.path: second.content,
	}})
	result, err := Prepare(context.Background(), fixture.options(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	if result.HasSemanticEvidence || len(result.Manifest.Events) != 0 {
		t.Fatalf("oversized queue commit evidence was selected: %#v", result.Manifest)
	}
	if len(result.Manifest.Rejections) != 1 || result.Manifest.Rejections[0].Reason != "commit-bytes" ||
		len(result.Manifest.Cursors) != 1 || result.Manifest.Cursors[0].Commit != commits[0] {
		t.Fatalf("oversized queue commit was not terminally rejected: %#v", result.Manifest)
	}
}

func TestPrepareRejectsSourceAsDestination(t *testing.T) {
	fixture := newPrepareFixture(t)
	for _, destination := range []string{"model", "control"} {
		t.Run(destination, func(t *testing.T) {
			options := fixture.options(t, 0)
			if destination == "model" {
				options.ModelDirectory = fixture.repository
			} else {
				options.ControlDirectory = fixture.repository
			}
			if _, err := Prepare(context.Background(), options); err == nil || !strings.Contains(err.Error(), "outside the source checkout") {
				t.Fatalf("source checkout accepted as %s destination: %v", destination, err)
			}
		})
	}
}

func TestPrepareRejectsDestinationThroughSymlinkedParent(t *testing.T) {
	fixture := newPrepareFixture(t)
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(fixture.repository, alias); err != nil {
		t.Fatal(err)
	}
	options := fixture.options(t, 0)
	options.ModelDirectory = filepath.Join(alias, "model")

	if _, err := Prepare(context.Background(), options); err == nil || !strings.Contains(err.Error(), "outside the source checkout") {
		t.Fatalf("destination through a source symlink was accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.repository, "model")); !os.IsNotExist(err) {
		t.Fatalf("rejected destination changed the source checkout: %v", err)
	}
}

func TestWritePrepareArtifactsPreservesDestinationItDidNotCreate(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model")
	control := filepath.Join(root, "control")
	if err := os.Mkdir(control, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(control, "sentinel")
	if err := os.WriteFile(sentinel, []byte("owned elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := writePrepareArtifacts(preparePaths{model: model, control: control}, nil, nil, nil, ControlManifest{})
	if err == nil {
		t.Fatal("artifact writer reused an existing destination")
	}
	if _, err := os.Stat(model); !os.IsNotExist(err) {
		t.Fatalf("partially created model destination was not cleaned up: %v", err)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "owned elsewhere\n" {
		t.Fatalf("pre-existing control destination changed: content=%q err=%v", content, err)
	}
}

func TestPrepareIsolatesCursorOutsideBoundedAncestry(t *testing.T) {
	fixture := newPrepareFixture(t)
	fixture.updateShared(t, func() {
		writeTestFile(t, fixture.repository, ".hourglass/cursors/"+testMachine, fixture.main+"\n")
	})
	parent := createLinearQueueHistory(t, fixture.repository, fixture.main, maxAncestryWindow+1)
	runTestGit(t, fixture.repository, "update-ref", "refs/remotes/origin/queue/"+testMachine, parent)

	options := fixture.options(t, 0)
	result, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.HasWork || result.HasSemanticEvidence || len(result.Manifest.Cursors) != 0 || len(result.Manifest.Rejections) != 0 {
		t.Fatalf("isolated queue produced terminal work: %#v", result)
	}
	if _, err := os.Stat(options.ModelDirectory); !os.IsNotExist(err) {
		t.Fatalf("no-work prepare created a model artifact: %v", err)
	}
	found := false
	for _, notice := range result.Notices {
		if notice.Machine == testMachine && notice.Reason == "cursor-outside-ancestry-window" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bounded ancestry isolation was not reported: %#v", result.Notices)
	}
}

func newPrepareFixture(t *testing.T) prepareFixture {
	t.Helper()
	repository := newTestRepository(t)
	runTestGit(t, repository, "checkout", "-q", "-B", "main")
	writeTestFile(t, repository, "control.txt", "trusted control\n")
	commitTestRepository(t, repository)
	main := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
	runTestGit(t, repository, "update-ref", "refs/remotes/origin/main", main)

	runTestGit(t, repository, "checkout", "-q", "-B", "shared")
	runTestGit(t, repository, "rm", "-q", "control.txt")
	writeTestFile(t, repository, ".gitignore", ".hourglass-runtime/\n")
	writeTestFile(t, repository, "Home.md", "# Hourglass\n")
	writeTestFile(t, repository, "Hourglass.canvas", `{"nodes":[],"edges":[]}`)
	commitTestRepository(t, repository)
	shared := strings.TrimSpace(runTestGit(t, repository, "rev-parse", "HEAD"))
	runTestGit(t, repository, "update-ref", "refs/remotes/origin/shared", shared)

	promptDirectory := t.TempDir()
	prompt := filepath.Join(promptDirectory, "dream.md")
	if err := os.WriteFile(prompt, []byte("Reconcile durable evidence.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return prepareFixture{repository: repository, main: main, prompt: prompt}
}

func (fixture prepareFixture) options(t *testing.T, slot uint64) PrepareOptions {
	t.Helper()
	root := t.TempDir()
	return PrepareOptions{
		SourceDirectory: fixture.repository, PromptPath: fixture.prompt,
		ModelDirectory: filepath.Join(root, "model"), ControlDirectory: filepath.Join(root, "control"),
		Repository: "x2x3studio/hourglass", ControlSHA: fixture.main,
		RunID: "29795588883", RunAttempt: 1, RunSlot: slot,
	}
}

func (fixture prepareFixture) createQueue(t *testing.T, machine string, commits []map[string][]byte) []string {
	t.Helper()
	branch := "fixture-queue-" + machine
	runTestGit(t, fixture.repository, "checkout", "-q", "-B", branch, fixture.main)
	result := make([]string, 0, len(commits))
	for _, changes := range commits {
		for name, content := range changes {
			writeTestFile(t, fixture.repository, name, string(content))
		}
		commitTestRepository(t, fixture.repository)
		result = append(result, strings.TrimSpace(runTestGit(t, fixture.repository, "rev-parse", "HEAD")))
	}
	tip := fixture.main
	if len(result) != 0 {
		tip = result[len(result)-1]
	}
	runTestGit(t, fixture.repository, "update-ref", "refs/remotes/origin/queue/"+machine, tip)
	runTestGit(t, fixture.repository, "checkout", "-q", "shared")
	return result
}

func (fixture prepareFixture) updateShared(t *testing.T, update func()) {
	t.Helper()
	runTestGit(t, fixture.repository, "checkout", "-q", "shared")
	update()
	commitTestRepository(t, fixture.repository)
	shared := strings.TrimSpace(runTestGit(t, fixture.repository, "rev-parse", "HEAD"))
	runTestGit(t, fixture.repository, "update-ref", "refs/remotes/origin/shared", shared)
}

func makeObservationEvent(t *testing.T, machine, text string) fixtureEvent {
	t.Helper()
	type payload struct {
		Text string `json:"text"`
	}
	value := payload{Text: text}
	semantic := struct {
		Kind    string  `json:"kind"`
		Machine string  `json:"machine"`
		Client  string  `json:"client"`
		Payload payload `json:"payload"`
	}{"observation", machine, "codex", value}
	encoded, err := json.Marshal(semantic)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	id := hex.EncodeToString(digest[:])
	wire := struct {
		Schema     string `json:"schema"`
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		CapturedAt string `json:"captured_at"`
		Machine    struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
		} `json:"machine"`
		Client string `json:"client"`
		Source struct {
			Kind    string `json:"kind"`
			Locator string `json:"locator"`
		} `json:"source"`
		Payload payload `json:"payload"`
	}{Schema: "hourglass.event/v1", ID: "sha256:" + id, Kind: "observation", CapturedAt: "2026-07-21T00:00:00Z", Client: "codex", Payload: value}
	wire.Machine.ID = machine
	wire.Machine.Hostname = "fixture-host"
	wire.Source.Kind = "explicit"
	wire.Source.Locator = "stdin"
	content, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	return fixtureEvent{path: "events/2026/07/" + id + ".json", content: content, id: id}
}

func makeImportEvent(t *testing.T, machine, prefix string, fill byte) fixtureEvent {
	t.Helper()
	type importItem struct {
		ID      string `json:"id"`
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
		Content string `json:"content"`
	}
	items := make([]importItem, 0, 7)
	for index := 0; index < 7; index++ {
		size := 64 * 1024
		if index == 6 {
			size = 15 * 1024
		}
		content := strings.Repeat(string(fill), size)
		contentDigest := sha256.Sum256([]byte(content))
		contentHash := hex.EncodeToString(contentDigest[:])
		path := prefix + "/" + strconv.Itoa(index) + ".md"
		semantic := struct {
			Path string `json:"path"`
			Hash string `json:"hash"`
		}{path, contentHash}
		encoded, err := json.Marshal(semantic)
		if err != nil {
			t.Fatal(err)
		}
		itemDigest := sha256.Sum256(encoded)
		items = append(items, importItem{
			ID:      "sha256:" + hex.EncodeToString(itemDigest[:]),
			Path:    path,
			SHA256:  contentHash,
			Content: content,
		})
	}
	batchSemantic := struct {
		Kind  string       `json:"kind"`
		Items []importItem `json:"items"`
	}{"import_batch", items}
	encoded, err := json.Marshal(batchSemantic)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	id := hex.EncodeToString(digest[:])
	wire := struct {
		Schema     string `json:"schema"`
		ID         string `json:"id"`
		Kind       string `json:"kind"`
		CapturedAt string `json:"captured_at"`
		Machine    struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
		} `json:"machine"`
		Client string `json:"client"`
		Source struct {
			Kind    string `json:"kind"`
			Locator string `json:"locator"`
		} `json:"source"`
		Payload struct {
			Source string       `json:"source"`
			Items  []importItem `json:"items"`
		} `json:"payload"`
	}{Schema: "hourglass.event/v1", ID: "sha256:" + id, Kind: "import_batch", CapturedAt: "2026-07-21T00:00:00Z", Client: "codex"}
	wire.Machine.ID = machine
	wire.Machine.Hostname = "fixture-host"
	wire.Source.Kind = "explicit"
	wire.Source.Locator = "stdin"
	wire.Payload.Source = prefix
	wire.Payload.Items = items
	content, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	return fixtureEvent{path: "events/2026/07/" + id + ".json", content: content, id: id}
}

func artifactFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func createLinearQueueHistory(t *testing.T, repository, parent string, count int) string {
	t.Helper()
	const branch = "refs/heads/fixture-bounded-history"
	var stream strings.Builder
	stream.WriteString("feature done\n")
	for index := 0; index < count; index++ {
		stream.WriteString("commit " + branch + "\n")
		stream.WriteString("committer Fixture <fixture@example.invalid> 0 +0000\n")
		stream.WriteString("data 7\nbounded\n")
		if index == 0 {
			stream.WriteString("from " + parent + "\n")
		}
		stream.WriteByte('\n')
	}
	stream.WriteString("done\n")
	command := exec.Command("git", "-C", repository, "fast-import", "--quiet")
	command.Stdin = strings.NewReader(stream.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create bounded history: %v: %s", err, output)
	}
	return strings.TrimSpace(runTestGit(t, repository, "rev-parse", branch))
}

func indexedTestMachine(index int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", index+1)
}
