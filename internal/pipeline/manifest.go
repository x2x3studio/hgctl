package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

const (
	ControlSchema          = "hourglass.control/v1"
	PublicationSchema      = "hourglass.publication/v1"
	maxControlEvents       = 8
	maxCursorOperations    = 32
	maxRejectionOperations = 32

	maxPublishedSemanticFiles   = maxChangedSemanticFiles
	maxPublishedSeenShards      = maxEventsPerDream
	maxPublishedCursorFiles     = maxCursorOperations
	maxPublishedRejectionShards = maxRejectionOperations
	maxPublicationFiles         = maxPublishedSemanticFiles + maxPublishedSeenShards +
		maxPublishedCursorFiles + maxPublishedRejectionShards
	maxPublicationBytes = maxSeenLedgerBytes + maxRejectionLedgerBytes + maxChangedSemanticBytes + 4*1024*1024
)

var (
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	machinePattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	runIDPattern      = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

type Revision struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

type FileRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type QueueTip struct {
	Machine string `json:"machine"`
	Commit  string `json:"commit"`
}

type SelectedEvent struct {
	Machine      string `json:"machine"`
	ID           string `json:"id"`
	QueueCommit  string `json:"queue_commit"`
	QueuePath    string `json:"queue_path"`
	Blob         string `json:"blob"`
	ArtifactPath string `json:"artifact_path"`
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
}

type CursorOperation struct {
	Machine string `json:"machine"`
	Commit  string `json:"commit"`
}

type RejectionOperation struct {
	Machine string `json:"machine"`
	Commit  string `json:"commit"`
	Reason  string `json:"reason"`
}

type ControlManifest struct {
	Schema     string               `json:"schema"`
	Repository string               `json:"repository"`
	ControlSHA string               `json:"control_sha"`
	RunID      string               `json:"run_id"`
	RunAttempt int                  `json:"run_attempt"`
	Shared     Revision             `json:"shared"`
	QueueTips  []QueueTip           `json:"queue_tips"`
	Events     []SelectedEvent      `json:"events"`
	Cursors    []CursorOperation    `json:"cursors"`
	Rejections []RejectionOperation `json:"rejections"`
	Baseline   []FileRecord         `json:"baseline"`
	Evidence   []FileRecord         `json:"evidence"`
	Prompt     FileRecord           `json:"prompt"`
}

type PublicationManifest struct {
	Schema     string       `json:"schema"`
	Repository string       `json:"repository"`
	ControlSHA string       `json:"control_sha"`
	RunID      string       `json:"run_id"`
	RunAttempt int          `json:"run_attempt"`
	Shared     Revision     `json:"shared"`
	Files      []FileRecord `json:"files"`
}

func EncodeControl(manifest ControlManifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return encodeJSON(manifest)
}

func DecodeControl(content []byte) (ControlManifest, error) {
	var manifest ControlManifest
	if err := decodeCanonicalJSON(content, &manifest); err != nil {
		return ControlManifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return ControlManifest{}, err
	}
	return manifest, nil
}

func EncodePublication(manifest PublicationManifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return encodeJSON(manifest)
}

func DecodePublication(content []byte) (PublicationManifest, error) {
	var manifest PublicationManifest
	if err := decodeCanonicalJSON(content, &manifest); err != nil {
		return PublicationManifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return PublicationManifest{}, err
	}
	return manifest, nil
}

func (manifest ControlManifest) Validate() error {
	if manifest.Schema != ControlSchema {
		return errors.New("unsupported control manifest schema")
	}
	if err := validateRunBinding(manifest.Repository, manifest.ControlSHA, manifest.RunID, manifest.RunAttempt, manifest.Shared); err != nil {
		return err
	}
	if len(manifest.Events) > maxControlEvents ||
		len(manifest.Cursors) > maxCursorOperations || len(manifest.Rejections) > maxRejectionOperations {
		return errors.New("control manifest exceeds operation limits")
	}
	if err := validateQueueTips(manifest.QueueTips); err != nil {
		return err
	}
	if err := validateSelectedEvents(manifest.Events); err != nil {
		return err
	}
	if err := validateCursorOperations(manifest.Cursors); err != nil {
		return err
	}
	if err := validateRejectionOperations(manifest.Rejections); err != nil {
		return err
	}
	if err := validateFileRecords(manifest.Baseline, "baseline", 0); err != nil {
		return err
	}
	if err := validateFileRecords(manifest.Evidence, "evidence", 768*1024); err != nil {
		return err
	}
	if err := validateFileRecord(manifest.Prompt); err != nil || manifest.Prompt.Path != "prompt.md" {
		return errors.New("control manifest has an invalid prompt record")
	}
	if len(manifest.Events) != len(manifest.Evidence) {
		return errors.New("control manifest event and evidence counts differ")
	}
	for index := range manifest.Events {
		if manifest.Events[index].ArtifactPath != manifest.Evidence[index].Path ||
			manifest.Events[index].SHA256 != manifest.Evidence[index].SHA256 ||
			manifest.Events[index].Bytes != manifest.Evidence[index].Bytes {
			return errors.New("control manifest evidence does not match selected events")
		}
	}
	return nil
}

func (manifest PublicationManifest) Validate() error {
	if manifest.Schema != PublicationSchema {
		return errors.New("unsupported publication manifest schema")
	}
	if err := validateRunBinding(manifest.Repository, manifest.ControlSHA, manifest.RunID, manifest.RunAttempt, manifest.Shared); err != nil {
		return err
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > maxPublicationFiles {
		return errors.New("publication manifest has an invalid file count")
	}
	return validateFileRecords(manifest.Files, "publication", maxPublicationBytes)
}

func validateRunBinding(repository, controlSHA, runID string, runAttempt int, shared Revision) error {
	if !repositoryPattern.MatchString(repository) || !commitPattern.MatchString(controlSHA) ||
		!runIDPattern.MatchString(runID) || runAttempt < 1 || runAttempt > 1_000_000 ||
		!commitPattern.MatchString(shared.Commit) || !commitPattern.MatchString(shared.Tree) {
		return errors.New("manifest has an invalid run binding")
	}
	return nil
}

func validateQueueTips(tips []QueueTip) error {
	previous := ""
	for _, tip := range tips {
		if !machinePattern.MatchString(tip.Machine) || !commitPattern.MatchString(tip.Commit) || tip.Machine <= previous {
			return errors.New("control manifest has invalid or unsorted queue tips")
		}
		previous = tip.Machine
	}
	return nil
}

func validateSelectedEvents(events []SelectedEvent) error {
	seen := make(map[string]struct{}, len(events))
	machine := ""
	for _, event := range events {
		if !machinePattern.MatchString(event.Machine) || !digestPattern.MatchString(event.ID) ||
			!commitPattern.MatchString(event.QueueCommit) || !commitPattern.MatchString(event.Blob) ||
			!digestPattern.MatchString(event.SHA256) || event.Bytes < 1 || event.Bytes > 512*1024 {
			return errors.New("control manifest has an invalid selected event")
		}
		if machine == "" {
			machine = event.Machine
		} else if event.Machine != machine {
			return errors.New("a semantic batch must contain events from one machine")
		}
		if event.ArtifactPath != ".hourglass-runtime/incoming/"+event.Machine+"/"+event.ID+".json" ||
			!validQueueEventPath(event.QueuePath, event.ID) {
			return errors.New("selected event paths are inconsistent")
		}
		if _, duplicate := seen[event.ID]; duplicate {
			return errors.New("control manifest contains a duplicate event")
		}
		seen[event.ID] = struct{}{}
	}
	return nil
}

func validateCursorOperations(operations []CursorOperation) error {
	previous := ""
	for _, operation := range operations {
		if !machinePattern.MatchString(operation.Machine) || !commitPattern.MatchString(operation.Commit) || operation.Machine <= previous {
			return errors.New("control manifest has invalid or unsorted cursor operations")
		}
		previous = operation.Machine
	}
	return nil
}

func validateRejectionOperations(operations []RejectionOperation) error {
	previous := ""
	for _, operation := range operations {
		key := operation.Machine + "/" + operation.Commit
		if !machinePattern.MatchString(operation.Machine) || !commitPattern.MatchString(operation.Commit) ||
			!validReason(operation.Reason) || key <= previous {
			return errors.New("control manifest has invalid or unsorted rejection operations")
		}
		previous = key
	}
	return nil
}

func validateFileRecords(records []FileRecord, name string, totalLimit int64) error {
	previous := ""
	var total int64
	for _, record := range records {
		if err := validateFileRecord(record); err != nil || record.Path <= previous {
			return fmt.Errorf("%s file records are invalid or unsorted", name)
		}
		previous = record.Path
		total += record.Bytes
		if totalLimit > 0 && total > totalLimit {
			return fmt.Errorf("%s files exceed the byte limit", name)
		}
	}
	return nil
}

func validateFileRecord(record FileRecord) error {
	if record.Path == "" || len(record.Path) > 4096 || strings.HasPrefix(record.Path, "/") ||
		path.Clean(record.Path) != record.Path || record.Path == "." || strings.ContainsAny(record.Path, "\\\x00\r\n\t") ||
		!digestPattern.MatchString(record.SHA256) || record.Bytes < 0 || record.Bytes > sharedPathByteLimit(record.Path) {
		return errors.New("invalid file record")
	}
	return nil
}

func validQueueEventPath(name, eventID string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "events" || len(parts[1]) != 4 || len(parts[2]) != 2 || parts[3] != eventID+".json" {
		return false
	}
	for _, value := range parts[1] + parts[2] {
		if value < '0' || value > '9' {
			return false
		}
	}
	return parts[2] >= "01" && parts[2] <= "12"
}

func validReason(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func encodeJSON(value any) ([]byte, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

func decodeCanonicalJSON(content []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	canonical, err := encodeJSON(destination)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, canonical) {
		return errors.New("manifest is not canonical JSON")
	}
	return nil
}
