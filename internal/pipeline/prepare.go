package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/x2x3studio/hgctl/internal/event"
	"github.com/x2x3studio/hgctl/internal/product"
)

const (
	maxAncestryWindow       = 4096
	maxCommitsPerMachine    = 4
	maxEventsPerDream       = 2
	maxEvidenceBytes        = 768 * 1024
	maxTerminalCommits      = 32
	maxInspectedCommits     = 32
	maxQueueReferences      = 1024
	maxQueueReferenceOutput = 512 * 1024
	maxCommitDiffOutput     = 64 * 1024
	maxClassificationBlob   = 4 * 1024 * 1024
)

// PrepareOptions binds one deterministic preparation to its workflow run.
type PrepareOptions struct {
	SourceDirectory  string
	PromptPath       string
	ModelDirectory   string
	ControlDirectory string
	Repository       string
	ControlSHA       string
	RunID            string
	RunAttempt       int
	RunSlot          uint64
}

// QueueNotice reports a queue that was conservatively left pending.
type QueueNotice struct {
	Kind    string
	Machine string
	Commit  string
	Reason  string
}

// PrepareResult describes the artifacts and terminal work selected for a run.
type PrepareResult struct {
	Manifest            ControlManifest
	HasWork             bool
	HasSemanticEvidence bool
	Notices             []QueueNotice
}

type preparePaths struct {
	source  string
	prompt  string
	model   string
	control string
}

type queueReference struct {
	machine string
	ref     string
	tip     string
}

type preparedEvent struct {
	record  SelectedEvent
	content []byte
}

type commitInspection struct {
	events   []preparedEvent
	reject   string
	deferred string
}

type preparePlanner struct {
	repository       gitRepository
	sharedContents   map[string][]byte
	seen             map[string]string
	rejected         map[string]rejectionEntry
	mainCommits      map[string]struct{}
	semanticMachine  string
	runSeen          map[string]struct{}
	events           []preparedEvent
	evidenceBytes    int64
	cursors          map[string]string
	rejections       map[string]RejectionOperation
	notices          []QueueNotice
	terminalCommits  int
	inspectedCommits int
}

// Prepare screens bounded queue history and creates the isolated model and
// control artifacts. Queue-local corruption is isolated or terminally rejected
// without preventing healthy queues from making progress.
func Prepare(ctx context.Context, options PrepareOptions) (result PrepareResult, returnErr error) {
	paths, err := validatePrepareOptions(options)
	if err != nil {
		return PrepareResult{}, err
	}
	repository := gitRepository{directory: paths.source}
	if status, err := repository.run(ctx, 1024*1024, "status", "--porcelain=v1", "-z", "--untracked-files=all"); err != nil {
		return PrepareResult{}, err
	} else if len(status) != 0 {
		return PrepareResult{}, errors.New("shared source checkout is dirty")
	}

	shared, err := repository.revision(ctx, "HEAD")
	if err != nil {
		return PrepareResult{}, fmt.Errorf("resolve shared HEAD: %w", err)
	}
	remoteShared, err := repository.revision(ctx, "origin/shared")
	if err != nil {
		return PrepareResult{}, fmt.Errorf("resolve origin/shared: %w", err)
	}
	if shared != remoteShared {
		return PrepareResult{}, errors.New("shared checkout does not exactly match origin/shared")
	}
	mainRevision, err := repository.revision(ctx, "origin/main")
	if err != nil {
		return PrepareResult{}, fmt.Errorf("resolve origin/main: %w", err)
	}
	if mainRevision.Commit != options.ControlSHA {
		return PrepareResult{}, errors.New("origin/main does not match the trusted control SHA")
	}

	entries, sharedContents, sharedControl, err := readSharedTree(ctx, repository, shared)
	if err != nil {
		return PrepareResult{}, err
	}
	baseline := make([]FileRecord, 0, len(entries))
	for _, entry := range entries {
		baseline = append(baseline, fileRecord(entry.Path, sharedContents[entry.Path]))
	}
	promptContent, err := readRegularFile(paths.prompt, maxSharedFileBytes)
	if err != nil {
		return PrepareResult{}, fmt.Errorf("read Dream prompt: %w", err)
	}
	if len(promptContent) == 0 {
		return PrepareResult{}, errors.New("Dream prompt is empty")
	}
	promptRecord := fileRecord("prompt.md", promptContent)

	mainChain, err := boundedFirstParentChain(ctx, repository, mainRevision.Commit)
	if err != nil {
		return PrepareResult{}, fmt.Errorf("read bounded main ancestry: %w", err)
	}
	mainCommits := make(map[string]struct{}, len(mainChain))
	for _, commit := range mainChain {
		mainCommits[commit] = struct{}{}
	}
	queues, queueTips, notices, err := discoverQueues(ctx, repository)
	if err != nil {
		return PrepareResult{}, err
	}
	planner := preparePlanner{
		repository: repository, sharedContents: sharedContents, seen: sharedControl.seen.entries,
		rejected: sharedControl.rejections.entries, mainCommits: mainCommits,
		runSeen: make(map[string]struct{}), cursors: make(map[string]string),
		rejections: make(map[string]RejectionOperation), notices: notices,
	}
	if len(queues) != 0 {
		start := int(options.RunSlot % uint64(len(queues)))
		for offset := 0; offset < len(queues) && planner.inspectedCommits < maxInspectedCommits; offset++ {
			queue := queues[(start+offset)%len(queues)]
			planner.planQueue(ctx, queue)
		}
	}

	sort.Slice(planner.events, func(left, right int) bool {
		return planner.events[left].record.ArtifactPath < planner.events[right].record.ArtifactPath
	})
	events := make([]SelectedEvent, 0, len(planner.events))
	evidence := make([]FileRecord, 0, len(planner.events))
	for _, event := range planner.events {
		events = append(events, event.record)
		evidence = append(evidence, FileRecord{
			Path: event.record.ArtifactPath, SHA256: event.record.SHA256, Bytes: event.record.Bytes,
		})
	}
	cursors := make([]CursorOperation, 0, len(planner.cursors))
	for machine, commit := range planner.cursors {
		cursors = append(cursors, CursorOperation{Machine: machine, Commit: commit})
	}
	sort.Slice(cursors, func(left, right int) bool { return cursors[left].Machine < cursors[right].Machine })
	rejections := make([]RejectionOperation, 0, len(planner.rejections))
	for _, operation := range planner.rejections {
		rejections = append(rejections, operation)
	}
	sort.Slice(rejections, func(left, right int) bool {
		leftKey := rejections[left].Machine + "/" + rejections[left].Commit
		rightKey := rejections[right].Machine + "/" + rejections[right].Commit
		return leftKey < rightKey
	})
	sort.Slice(planner.notices, func(left, right int) bool {
		leftKey := planner.notices[left].Kind + "/" + planner.notices[left].Machine + "/" + planner.notices[left].Commit
		rightKey := planner.notices[right].Kind + "/" + planner.notices[right].Machine + "/" + planner.notices[right].Commit
		return leftKey < rightKey
	})

	manifest := ControlManifest{
		Schema: ControlSchema, Repository: options.Repository, ControlSHA: options.ControlSHA,
		RunID: options.RunID, RunAttempt: options.RunAttempt, Shared: shared,
		QueueTips: queueTips, Events: events, Cursors: cursors, Rejections: rejections,
		Baseline: baseline, Evidence: evidence, Prompt: promptRecord,
	}
	if err := manifest.Validate(); err != nil {
		return PrepareResult{}, fmt.Errorf("build control manifest: %w", err)
	}
	result = PrepareResult{
		Manifest: manifest, HasSemanticEvidence: len(events) != 0,
		HasWork: len(events) != 0 || len(cursors) != 0 || len(rejections) != 0,
		Notices: planner.notices,
	}
	if !result.HasWork {
		return result, nil
	}
	if err := writePrepareArtifacts(paths, sharedContents, planner.events, promptContent, manifest); err != nil {
		return PrepareResult{}, err
	}
	return result, nil
}

func validatePrepareOptions(options PrepareOptions) (preparePaths, error) {
	if !repositoryPattern.MatchString(options.Repository) || !commitPattern.MatchString(options.ControlSHA) ||
		!runIDPattern.MatchString(options.RunID) || options.RunAttempt < 1 || options.RunAttempt > 1_000_000 {
		return preparePaths{}, errors.New("prepare options have an invalid run binding")
	}
	values := []string{options.SourceDirectory, options.PromptPath, options.ModelDirectory, options.ControlDirectory}
	for _, value := range values {
		if value == "" {
			return preparePaths{}, errors.New("prepare paths must be nonempty")
		}
	}
	abs := make([]string, len(values))
	for index, value := range values {
		resolved, err := filepath.Abs(value)
		if err != nil {
			return preparePaths{}, err
		}
		abs[index] = filepath.Clean(resolved)
	}
	paths := preparePaths{source: abs[0], prompt: abs[1], model: abs[2], control: abs[3]}
	for _, item := range []struct {
		name   string
		target *string
	}{{"source", &paths.source}, {"model", &paths.model}, {"control", &paths.control}} {
		resolved, err := resolveArtifactRoot(*item.target)
		if err != nil {
			return preparePaths{}, fmt.Errorf("resolve %s path: %w", item.name, err)
		}
		*item.target = resolved
	}
	if pathContains(paths.model, paths.control) || pathContains(paths.control, paths.model) {
		return preparePaths{}, errors.New("model and control destinations must be separate")
	}
	if pathContains(paths.source, paths.model) || pathContains(paths.source, paths.control) {
		return preparePaths{}, errors.New("prepare destinations must be outside the source checkout")
	}
	for _, destination := range []string{paths.model, paths.control} {
		if _, err := os.Lstat(destination); err == nil {
			return preparePaths{}, fmt.Errorf("prepare destination already exists: %s", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return preparePaths{}, err
		}
	}
	return paths, nil
}

func discoverQueues(ctx context.Context, repository gitRepository) ([]queueReference, []QueueTip, []QueueNotice, error) {
	output, err := repository.run(ctx, maxQueueReferenceOutput, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/queue/")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list queue references: %w", err)
	}
	lines := strings.Fields(string(output))
	if len(lines) > maxQueueReferences {
		return nil, nil, nil, errors.New("repository has too many queue references")
	}
	sort.Strings(lines)
	queues := make([]queueReference, 0, len(lines))
	notices := make([]QueueNotice, 0)
	const prefix = "refs/remotes/origin/queue/"
	for _, ref := range lines {
		machine := strings.TrimPrefix(ref, prefix)
		if machine == ref || !machinePattern.MatchString(machine) {
			notices = append(notices, QueueNotice{Kind: "isolated", Machine: machine, Reason: "invalid-queue-identity"})
			continue
		}
		revision, err := repository.revision(ctx, ref)
		if err != nil {
			notices = append(notices, QueueNotice{Kind: "isolated", Machine: machine, Reason: "unreadable-queue-tip"})
			continue
		}
		queues = append(queues, queueReference{machine: machine, ref: ref, tip: revision.Commit})
	}
	sort.Slice(queues, func(left, right int) bool { return queues[left].machine < queues[right].machine })
	tips := make([]QueueTip, 0, len(queues))
	for _, queue := range queues {
		tips = append(tips, QueueTip{Machine: queue.machine, Commit: queue.tip})
	}
	return queues, tips, notices, nil
}

func boundedFirstParentChain(ctx context.Context, repository gitRepository, tip string) ([]string, error) {
	output, err := repository.run(ctx, (maxAncestryWindow+2)*41,
		"rev-list", "--first-parent", "--max-count="+strconv.Itoa(maxAncestryWindow+1), tip)
	if err != nil {
		return nil, err
	}
	chain := strings.Fields(string(output))
	if len(chain) == 0 || chain[0] != tip {
		return nil, errors.New("first-parent ancestry did not begin at the queue tip")
	}
	seen := make(map[string]struct{}, len(chain))
	for _, commit := range chain {
		if !commitPattern.MatchString(commit) {
			return nil, errors.New("first-parent ancestry contains an invalid object id")
		}
		if _, duplicate := seen[commit]; duplicate {
			return nil, errors.New("first-parent ancestry contains a duplicate commit")
		}
		seen[commit] = struct{}{}
	}
	return chain, nil
}

func (planner *preparePlanner) planQueue(ctx context.Context, queue queueReference) {
	chain, err := boundedFirstParentChain(ctx, planner.repository, queue.tip)
	if err != nil {
		planner.isolate(queue.machine, queue.tip, "unreadable-bounded-ancestry")
		return
	}
	cursor := ""
	cursorPath := ".hourglass/cursors/" + queue.machine
	if content, exists := planner.sharedContents[cursorPath]; exists {
		cursor = strings.TrimSuffix(string(content), "\n")
	} else {
		for _, commit := range chain {
			if _, onMain := planner.mainCommits[commit]; onMain {
				cursor = commit
				break
			}
		}
		if cursor == "" {
			planner.isolate(queue.machine, queue.tip, "base-outside-ancestry-window")
			return
		}
	}
	cursorIndex := -1
	for index, commit := range chain {
		if commit == cursor {
			cursorIndex = index
			break
		}
	}
	if cursorIndex < 0 {
		reason := "cursor-not-on-first-parent"
		if len(chain) == maxAncestryWindow+1 {
			reason = "cursor-outside-ancestry-window"
		}
		planner.isolate(queue.machine, queue.tip, reason)
		return
	}

	pending := make([]string, 0, cursorIndex)
	for index := cursorIndex - 1; index >= 0; index-- {
		pending = append(pending, chain[index])
	}
	if len(pending) > maxCommitsPerMachine {
		pending = pending[:maxCommitsPerMachine]
	}
	through := cursor
	for _, commit := range pending {
		if planner.terminalCommits >= maxTerminalCommits || planner.inspectedCommits >= maxInspectedCommits {
			break
		}
		planner.inspectedCommits++
		inspection, err := inspectQueueCommit(ctx, planner.repository, queue.machine, commit)
		if err != nil {
			planner.isolate(queue.machine, commit, "unreadable-queue-commit")
			break
		}
		if inspection.deferred != "" {
			planner.notices = append(planner.notices, QueueNotice{
				Kind: "deferred", Machine: queue.machine, Commit: commit, Reason: inspection.deferred,
			})
			break
		}
		unseen := make([]preparedEvent, 0, len(inspection.events))
		for _, event := range inspection.events {
			if _, seen := planner.seen[event.record.ID]; seen {
				continue
			}
			if _, seen := planner.runSeen[event.record.ID]; seen {
				continue
			}
			unseen = append(unseen, event)
		}
		if len(unseen) == 0 {
			planner.recordRejection(queue.machine, commit, inspection.reject)
			through = commit
			planner.terminalCommits++
			continue
		}
		selected := 0
		for selected < len(unseen) {
			event := unseen[selected]
			if planner.semanticMachine != "" && planner.semanticMachine != queue.machine {
				break
			}
			if len(planner.events) >= maxEventsPerDream {
				break
			}
			if planner.evidenceBytes+event.record.Bytes > maxEvidenceBytes {
				break
			}
			planner.semanticMachine = queue.machine
			planner.events = append(planner.events, event)
			planner.runSeen[event.record.ID] = struct{}{}
			planner.evidenceBytes += event.record.Bytes
			selected++
		}
		if selected != len(unseen) {
			break
		}
		planner.recordRejection(queue.machine, commit, inspection.reject)
		through = commit
		planner.terminalCommits++
	}
	if through != cursor {
		planner.cursors[queue.machine] = through
	}
}

func (planner *preparePlanner) recordRejection(machine, commit, reason string) {
	if reason == "" {
		return
	}
	if _, exists := planner.rejected[rejectionKey(machine, commit)]; exists {
		return
	}
	key := machine + "/" + commit
	planner.rejections[key] = RejectionOperation{Machine: machine, Commit: commit, Reason: reason}
}

func (planner *preparePlanner) isolate(machine, commit, reason string) {
	planner.notices = append(planner.notices, QueueNotice{
		Kind: "isolated", Machine: machine, Commit: commit, Reason: reason,
	})
}

func inspectQueueCommit(ctx context.Context, repository gitRepository, machine, commit string) (commitInspection, error) {
	lineage, err := repository.run(ctx, 256, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return commitInspection{}, err
	}
	parents := strings.Fields(string(lineage))
	if len(parents) != 2 || parents[0] != commit {
		return commitInspection{reject: "merge-commit"}, nil
	}
	diff, err := repository.run(ctx, maxCommitDiffOutput,
		"diff-tree", "--no-commit-id", "--name-status", "-r", "-z", "--no-renames", commit)
	if errors.Is(err, errGitOutputLimit) {
		return commitInspection{reject: "change-list-limit"}, nil
	}
	if err != nil {
		return commitInspection{}, err
	}
	paths, reason := parseAppendOnlyEventChanges(diff)
	if reason != "" {
		return commitInspection{reject: reason}, nil
	}

	inspection := commitInspection{events: make([]preparedEvent, 0, len(paths))}
	var totalBytes int64
	for _, eventPath := range paths {
		entriesOutput, err := repository.run(ctx, 8192, "ls-tree", "-z", commit, "--", eventPath)
		if err != nil {
			return commitInspection{}, err
		}
		entries, err := parseTree(entriesOutput)
		if err != nil || len(entries) != 1 || entries[0].Path != eventPath || entries[0].Mode != "100644" || entries[0].Type != "blob" {
			return commitInspection{reject: "invalid-event-object"}, nil
		}
		entry := entries[0]
		size, err := gitBlobSize(ctx, repository, entry.Object)
		if err != nil {
			return commitInspection{}, err
		}
		if size > maxClassificationBlob {
			return commitInspection{deferred: "event-too-large-to-classify"}, nil
		}
		totalBytes += size
		content, err := repository.run(ctx, int(size)+1, "cat-file", "blob", entry.Object)
		if err != nil || int64(len(content)) != size {
			if err == nil {
				err = errors.New("Git blob size changed while reading")
			}
			return commitInspection{}, err
		}
		identifier := strings.TrimSuffix(filepath.Base(eventPath), ".json")
		digest := sha256.Sum256(content)
		artifactPath := ".hourglass-runtime/incoming/" + machine + "/" + identifier + ".json"
		record := SelectedEvent{
			Machine: machine, ID: identifier, QueueCommit: commit, QueuePath: eventPath,
			Blob: entry.Object, ArtifactPath: artifactPath,
			SHA256: hex.EncodeToString(digest[:]), Bytes: size,
		}
		_, decodeErr := event.DecodeCanonical(content, event.Binding{MachineID: machine, Path: eventPath})
		if decodeErr != nil {
			inspection.reject = "invalid-event"
			continue
		}
		inspection.events = append(inspection.events, preparedEvent{record: record, content: content})
	}

	if totalBytes > event.MaxEventBytes {
		return commitInspection{reject: "commit-bytes"}, nil
	}
	inspection.events = retainFirstEventIDs(inspection.events, &inspection.reject)
	return inspection, nil
}

func retainFirstEventIDs(events []preparedEvent, rejection *string) []preparedEvent {
	seen := make(map[string]struct{}, len(events))
	kept := events[:0]
	for _, event := range events {
		if _, duplicate := seen[event.record.ID]; duplicate {
			if *rejection == "" {
				*rejection = "duplicate-event-id"
			}
			continue
		}
		seen[event.record.ID] = struct{}{}
		kept = append(kept, event)
	}
	return kept
}

func parseAppendOnlyEventChanges(output []byte) ([]string, string) {
	records := strings.Split(string(output), "\x00")
	if len(records) > 0 && records[len(records)-1] == "" {
		records = records[:len(records)-1]
	}
	if len(records)%2 != 0 {
		return nil, "invalid-change-list"
	}
	paths := make([]string, 0, len(records)/2)
	seen := make(map[string]struct{}, len(records)/2)
	for index := 0; index < len(records); index += 2 {
		status, name := records[index], records[index+1]
		if status != "A" {
			return nil, "not-append-only"
		}
		parts := strings.Split(name, "/")
		if len(parts) != 4 || parts[0] != "events" || !strings.HasSuffix(parts[3], ".json") {
			return nil, "invalid-event-path"
		}
		identifier := strings.TrimSuffix(parts[3], ".json")
		if !digestPattern.MatchString(identifier) || !validQueueEventPath(name, identifier) {
			return nil, "invalid-event-path"
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, "invalid-change-list"
		}
		seen[name] = struct{}{}
		paths = append(paths, name)
	}
	if len(paths) == 0 || len(paths) > 4 {
		return nil, "event-count"
	}
	sort.Strings(paths)
	return paths, ""
}

func gitBlobSize(ctx context.Context, repository gitRepository, object string) (int64, error) {
	output, err := repository.run(ctx, 64, "cat-file", "-s", object)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || size < 0 {
		return 0, errors.New("Git returned an invalid blob size")
	}
	return size, nil
}

func writePrepareArtifacts(paths preparePaths, shared map[string][]byte, events []preparedEvent, prompt []byte, manifest ControlManifest) (returnErr error) {
	created := make([]string, 0, 2)
	cleanup := func() {
		for _, destination := range created {
			_ = os.RemoveAll(destination)
		}
	}
	destinations := []string{paths.control}
	if len(events) != 0 {
		destinations = []string{paths.model, paths.control}
	}
	for _, destination := range destinations {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			cleanup()
			return err
		}
		if err := os.Mkdir(destination, 0o700); err != nil {
			cleanup()
			return err
		}
		created = append(created, destination)
	}
	defer func() {
		if returnErr != nil {
			cleanup()
		}
	}()
	if len(events) != 0 {
		workspace := filepath.Join(paths.model, "workspace")
		semanticPaths := make([]string, 0)
		for name := range shared {
			if product.IsSemanticPath(name) {
				semanticPaths = append(semanticPaths, name)
			}
		}
		sort.Strings(semanticPaths)
		for _, name := range semanticPaths {
			if err := writeArtifactFile(workspace, name, shared[name]); err != nil {
				return err
			}
		}
		for _, event := range events {
			if err := writeArtifactFile(workspace, event.record.ArtifactPath, event.content); err != nil {
				return err
			}
		}
		if err := writeArtifactFile(paths.model, "prompt.md", prompt); err != nil {
			return err
		}
	}
	encoded, err := EncodeControl(manifest)
	if err != nil {
		return err
	}
	if err := writeArtifactFile(paths.control, "control.json", encoded); err != nil {
		return err
	}
	controlPaths := make([]string, 0)
	for name := range shared {
		if _, ok := seenShardName(name); ok {
			controlPaths = append(controlPaths, name)
		} else if _, ok := rejectionShardName(name); ok {
			controlPaths = append(controlPaths, name)
		}
	}
	sort.Strings(controlPaths)
	for _, name := range controlPaths {
		if err := writeArtifactFile(paths.control, name, shared[name]); err != nil {
			return err
		}
	}
	return nil
}

func writeArtifactFile(root, name string, content []byte) error {
	full := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, full)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid artifact path %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readRegularFile(name string, maximum int64) ([]byte, error) {
	walked, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !walked.Mode().IsRegular() || walked.Size() < 0 || walked.Size() > maximum {
		return nil, fmt.Errorf("file is not regular or exceeds %d bytes", maximum)
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(walked, opened) || opened.Size() != walked.Size() {
		return nil, errors.New("file changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, after) || after.Size() != opened.Size() ||
		after.ModTime() != opened.ModTime() || int64(len(content)) != opened.Size() {
		return nil, errors.New("file changed while reading")
	}
	return content, nil
}
