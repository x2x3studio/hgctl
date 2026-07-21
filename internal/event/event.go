// Package event independently validates Hourglass event/v1 queue objects.
// It intentionally does not share implementation code with any producer.
package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Schema = "hourglass.event/v1"

	MaxEventBytes   = 512 * 1024
	MaxTextBytes    = 120 * 1024
	MaxImportBytes  = 400 * 1024
	MaxImportItems  = 50
	MaxImportText   = 64 * 1024
	MaxImportSource = 256
	MaxHostname     = 255
	MaxClient       = 64
)

const (
	KindObservation Kind = "observation"
	KindTurn        Kind = "turn"
	KindImportBatch Kind = "import_batch"
)

var timestampPattern = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

type Kind string

type Binding struct {
	MachineID string
	Path      string
}

type Machine struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

type Source struct {
	Kind    string `json:"kind"`
	Locator string `json:"locator"`
}

type ObservationPayload struct {
	Text string `json:"text"`
}

type TurnPayload struct {
	Prompt   string `json:"prompt,omitempty"`
	Response string `json:"response,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	Model    string `json:"model,omitempty"`
}

type ImportItem struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

type ImportPayload struct {
	Source string       `json:"source"`
	Items  []ImportItem `json:"items"`
}

type Event struct {
	Schema     string
	ID         string
	Kind       Kind
	CapturedAt time.Time
	Machine    Machine
	Client     string
	SessionID  string
	TurnID     string
	Source     Source

	Observation *ObservationPayload
	Turn        *TurnPayload
	ImportBatch *ImportPayload
}

// InvalidEventError identifies terminally malformed v1 input or an invalid
// transport binding.
type InvalidEventError struct {
	Reason string
}

func (e *InvalidEventError) Error() string {
	return "invalid hourglass event/v1: " + e.Reason
}

type wireEvent struct {
	Schema     string          `json:"schema"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	CapturedAt string          `json:"captured_at"`
	Machine    Machine         `json:"machine"`
	Client     string          `json:"client"`
	SessionID  *string         `json:"session_id,omitempty"`
	TurnID     *string         `json:"turn_id,omitempty"`
	Source     Source          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
}

// DecodeCanonical validates one complete queue file and binds it to its queue
// branch and Git path.
func DecodeCanonical(content []byte, binding Binding) (Event, error) {
	if len(content) == 0 {
		return Event{}, invalid("file is empty")
	}
	if !utf8.Valid(content) {
		return Event{}, invalid("file is not valid UTF-8")
	}
	if len(content) > MaxEventBytes {
		return Event{}, invalid("file exceeds %d bytes", MaxEventBytes)
	}

	var wire wireEvent
	if err := decodeClosedJSON(content, &wire); err != nil {
		return Event{}, invalid("closed envelope: %v", err)
	}
	if wire.Schema != Schema {
		return Event{}, invalid("unsupported schema %q", wire.Schema)
	}
	if !validDigest(wire.ID) {
		return Event{}, invalid("id must be a lowercase sha256 digest")
	}
	if !validMachineID(wire.Machine.ID) {
		return Event{}, invalid("machine id is not a lowercase UUIDv4")
	}
	if !validRequiredString(wire.Machine.Hostname, MaxHostname) {
		return Event{}, invalid("machine hostname is empty, invalid UTF-8, or too long")
	}
	if !validRequiredString(wire.Client, MaxClient) {
		return Event{}, invalid("client is empty, invalid UTF-8, or too long")
	}
	if !validRequiredString(wire.Source.Kind, 64) || !validRequiredString(wire.Source.Locator, 4096) {
		return Event{}, invalid("source is incomplete, invalid UTF-8, or too long")
	}
	if wire.SessionID != nil && !validRequiredString(*wire.SessionID, 512) {
		return Event{}, invalid("a present session id must be nonempty valid UTF-8 within its limit")
	}
	if wire.TurnID != nil && !validRequiredString(*wire.TurnID, 512) {
		return Event{}, invalid("a present turn id must be nonempty valid UTF-8 within its limit")
	}

	capturedAt, err := parseCanonicalTimestamp(wire.CapturedAt)
	if err != nil {
		return Event{}, invalid("captured_at: %v", err)
	}

	event := Event{
		Schema: Schema, ID: wire.ID, Kind: Kind(wire.Kind), CapturedAt: capturedAt,
		Machine: wire.Machine, Client: wire.Client, Source: wire.Source,
	}
	if wire.SessionID != nil {
		event.SessionID = *wire.SessionID
	}
	if wire.TurnID != nil {
		event.TurnID = *wire.TurnID
	}

	switch event.Kind {
	case KindObservation:
		if wire.SessionID != nil || wire.TurnID != nil {
			return Event{}, invalid("observation cannot carry session or turn ids")
		}
		var payload ObservationPayload
		if err := decodeCanonicalPayload(wire.Payload, &payload); err != nil {
			return Event{}, invalid("observation payload: %v", err)
		}
		if !validRequiredString(payload.Text, MaxTextBytes) {
			return Event{}, invalid("observation text is empty, invalid UTF-8, or too long")
		}
		expected, err := observationID(event.Machine.ID, event.Client, payload)
		if err != nil || event.ID != expected {
			return Event{}, invalid("observation semantic id mismatch")
		}
		event.Observation = &payload

	case KindTurn:
		var payload TurnPayload
		if err := decodeCanonicalPayload(wire.Payload, &payload); err != nil {
			return Event{}, invalid("turn payload: %v", err)
		}
		if payload.Prompt == "" && payload.Response == "" {
			return Event{}, invalid("turn must contain a prompt or response")
		}
		if !validOptionalString(payload.Prompt, MaxTextBytes) ||
			!validOptionalString(payload.Response, MaxTextBytes) ||
			!validOptionalString(payload.CWD, 4096) ||
			!validOptionalString(payload.Model, 256) {
			return Event{}, invalid("turn payload contains invalid UTF-8 or an oversized field")
		}
		expected, err := turnID(event.Machine.ID, event.Client, event.SessionID, event.TurnID, payload)
		if err != nil || event.ID != expected {
			return Event{}, invalid("turn semantic id mismatch")
		}
		event.Turn = &payload

	case KindImportBatch:
		if wire.SessionID != nil || wire.TurnID != nil {
			return Event{}, invalid("import batch cannot carry session or turn ids")
		}
		var payload ImportPayload
		if err := decodeCanonicalPayload(wire.Payload, &payload); err != nil {
			return Event{}, invalid("import payload: %v", err)
		}
		if err := validateImportPayload(payload); err != nil {
			return Event{}, err
		}
		expected, err := importBatchID(payload.Items)
		if err != nil || event.ID != expected {
			return Event{}, invalid("import batch semantic id mismatch")
		}
		event.ImportBatch = &payload

	default:
		return Event{}, invalid("unsupported event kind %q", wire.Kind)
	}

	canonical, err := json.Marshal(wire)
	if err != nil {
		return Event{}, invalid("canonical envelope encoding: %v", err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(content, canonical) {
		return Event{}, invalid("file is not exact canonical Go JSON followed by LF")
	}
	if err := validateBinding(event, binding); err != nil {
		return Event{}, err
	}
	return event, nil
}

func validateImportPayload(payload ImportPayload) error {
	if !validRequiredString(payload.Source, MaxImportSource) {
		return invalid("import source is empty, invalid UTF-8, or too long")
	}
	if len(payload.Items) == 0 || len(payload.Items) > MaxImportItems {
		return invalid("import item count is outside the v1 limit")
	}
	total := 0
	for _, item := range payload.Items {
		if !validDigest(item.ID) || !validRequiredString(item.Path, 4096) ||
			!validLowerHex(item.SHA256, sha256.Size*2) ||
			!validOptionalString(item.Content, MaxImportText) {
			return invalid("import item is malformed")
		}
		sum := sha256.Sum256([]byte(item.Content))
		if item.SHA256 != hex.EncodeToString(sum[:]) {
			return invalid("import item checksum mismatch")
		}
		expected, err := importItemID(item.Path, item.SHA256)
		if err != nil || item.ID != expected {
			return invalid("import item semantic id mismatch")
		}
		total += len(item.Content)
	}
	if total > MaxImportBytes {
		return invalid("import content exceeds %d bytes", MaxImportBytes)
	}
	return nil
}

func validateBinding(event Event, binding Binding) error {
	if !validMachineID(binding.MachineID) {
		return invalid("queue binding has an invalid machine id")
	}
	if event.Machine.ID != binding.MachineID {
		return invalid("event machine does not match its queue branch")
	}
	digest := strings.TrimPrefix(event.ID, "sha256:")
	want := fmt.Sprintf("events/%04d/%02d/%s.json",
		event.CapturedAt.UTC().Year(), int(event.CapturedAt.UTC().Month()), digest)
	if binding.Path != want {
		return invalid("event path does not match its UTC capture month and id")
	}
	return nil
}

func decodeClosedJSON(content []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func decodeCanonicalPayload(content json.RawMessage, dst any) error {
	if len(content) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return errors.New("payload is required")
	}
	if err := decodeClosedJSON(content, dst); err != nil {
		return err
	}
	canonical, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(content, canonical) {
		return errors.New("payload is not canonical for its kind")
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func parseCanonicalTimestamp(value string) (time.Time, error) {
	if !timestampPattern.MatchString(value) {
		return time.Time{}, errors.New("timestamp does not match the event grammar")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp is not a real calendar time: %w", err)
	}
	encoded, err := parsed.MarshalJSON()
	if err != nil {
		return time.Time{}, err
	}
	want, err := json.Marshal(value)
	if err != nil {
		return time.Time{}, err
	}
	if !bytes.Equal(encoded, want) {
		return time.Time{}, errors.New("timestamp is not canonical Go time JSON")
	}
	return parsed, nil
}

func observationID(machine, client string, payload ObservationPayload) (string, error) {
	return semanticID(struct {
		Kind    string             `json:"kind"`
		Machine string             `json:"machine"`
		Client  string             `json:"client"`
		Payload ObservationPayload `json:"payload"`
	}{string(KindObservation), machine, client, payload})
}

func turnID(machine, client, sessionID, turnIDValue string, payload TurnPayload) (string, error) {
	return semanticID(struct {
		Kind      string      `json:"kind"`
		Machine   string      `json:"machine"`
		Client    string      `json:"client"`
		SessionID string      `json:"session_id"`
		TurnID    string      `json:"turn_id"`
		Payload   TurnPayload `json:"payload"`
	}{string(KindTurn), machine, client, sessionID, turnIDValue, payload})
}

func importBatchID(items []ImportItem) (string, error) {
	return semanticID(struct {
		Kind  string       `json:"kind"`
		Items []ImportItem `json:"items"`
	}{string(KindImportBatch), items})
}

func importItemID(path, hash string) (string, error) {
	return semanticID(struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
	}{path, hash})
}

func semanticID(value any) (string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validRequiredString(value string, limit int) bool {
	return len(value) > 0 && validOptionalString(value, limit)
}

func validOptionalString(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit
}

func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validLowerHex(strings.TrimPrefix(value, "sha256:"), sha256.Size*2)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validMachineID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	if value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b' {
		return false
	}
	return validLowerHex(value[:8]+value[9:13]+value[14:18]+value[19:23]+value[24:], 32)
}

func invalid(format string, args ...any) error {
	return &InvalidEventError{Reason: fmt.Sprintf(format, args...)}
}
