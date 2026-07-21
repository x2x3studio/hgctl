package hgctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxEventBytes      = 512 * 1024
	MaxTextBytes       = 120 * 1024
	MaxImportBytes     = 400 * 1024
	MaxImportFiles     = 50
	MaxImportText      = 64 * 1024
	MaxImportSource    = 256
	MaxImportPath      = 4096
	MaxSourceKind      = 64
	MaxSourceLocator   = 4096
	MaxMachineHostname = 255
	MaxClient          = 64
	MaxSessionID       = 512
	MaxTurnID          = 512
	MaxCWD             = 4096
	MaxModel           = 256
	MaxSyncEvents      = 4
	MaxSyncBytes       = 512 * 1024
)

type Machine struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

type Source struct {
	Kind    string `json:"kind"`
	Locator string `json:"locator"`
}

type Event struct {
	Schema     string    `json:"schema"`
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	CapturedAt time.Time `json:"captured_at"`
	Machine    Machine   `json:"machine"`
	Client     string    `json:"client"`
	SessionID  string    `json:"session_id,omitempty"`
	TurnID     string    `json:"turn_id,omitempty"`
	Source     Source    `json:"source"`
	Payload    any       `json:"payload"`
}

type wireEvent struct {
	Schema     string          `json:"schema"`
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	CapturedAt time.Time       `json:"captured_at"`
	Machine    Machine         `json:"machine"`
	Client     string          `json:"client"`
	SessionID  string          `json:"session_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	Source     Source          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
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

func (event *Event) UnmarshalJSON(content []byte) error {
	var wire wireEvent
	if err := decodeClosedJSON(content, &wire); err != nil {
		return err
	}
	payload, err := decodeEventPayload(wire.Kind, wire.Payload)
	if err != nil {
		return err
	}
	*event = Event{
		Schema: wire.Schema, ID: wire.ID, Kind: wire.Kind, CapturedAt: wire.CapturedAt,
		Machine: wire.Machine, Client: wire.Client, SessionID: wire.SessionID,
		TurnID: wire.TurnID, Source: wire.Source, Payload: payload,
	}
	return nil
}

func decodeEventPayload(kind string, content json.RawMessage) (any, error) {
	if len(content) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return nil, errors.New("event payload is required")
	}
	var value any
	switch kind {
	case "observation":
		value = &ObservationPayload{}
	case "turn":
		value = &TurnPayload{}
	case "import_batch":
		value = &ImportPayload{}
	case "feedback":
		value = &FeedbackPayload{}
	default:
		return nil, fmt.Errorf("unsupported event kind %q", kind)
	}
	if err := decodeClosedJSON(content, value); err != nil {
		return nil, fmt.Errorf("invalid %s payload", kind)
	}
	switch typed := value.(type) {
	case *ObservationPayload:
		return *typed, nil
	case *TurnPayload:
		return *typed, nil
	case *ImportPayload:
		return *typed, nil
	case *FeedbackPayload:
		return *typed, nil
	default:
		return nil, errors.New("unsupported event payload type")
	}
}

func newObservation(id Identity, client, text string, now time.Time) (Event, error) {
	client = boundString(client, 64)
	payload := ObservationPayload{Text: boundText(text)}
	digest, err := observationEventID(id.ID, client, payload)
	if err != nil {
		return Event{}, err
	}
	return Event{
		Schema: Protocol, ID: digest, Kind: "observation", CapturedAt: now,
		Machine: Machine{ID: id.ID, Hostname: id.Hostname}, Client: client,
		Source: Source{Kind: "explicit", Locator: "stdin"}, Payload: payload,
	}, nil
}

func newTurnEvent(id Identity, pending pendingTurn, response string, now time.Time) (Event, error) {
	payload := TurnPayload{
		Prompt: boundText(pending.Prompt), Response: boundText(response),
		CWD: pending.CWD, Model: pending.Model,
	}
	digest, err := turnEventID(id.ID, pending.Client, pending.SessionID, pending.TurnID, payload)
	if err != nil {
		return Event{}, err
	}
	locator := pending.Client + ":" + pending.SessionID
	if pending.TurnID != "" {
		locator += ":" + pending.TurnID
	}
	return Event{
		Schema: Protocol, ID: digest, Kind: "turn", CapturedAt: now,
		Machine: Machine{ID: id.ID, Hostname: id.Hostname}, Client: pending.Client,
		SessionID: pending.SessionID, TurnID: pending.TurnID,
		Source: Source{Kind: "hook", Locator: locator}, Payload: payload,
	}, nil
}

func observationEventID(machine, client string, payload ObservationPayload) (string, error) {
	return semanticID(struct {
		Kind    string             `json:"kind"`
		Machine string             `json:"machine"`
		Client  string             `json:"client"`
		Payload ObservationPayload `json:"payload"`
	}{"observation", machine, client, payload})
}

func turnEventID(machine, client, sessionID, turnID string, payload TurnPayload) (string, error) {
	return semanticID(struct {
		Kind      string      `json:"kind"`
		Machine   string      `json:"machine"`
		Client    string      `json:"client"`
		SessionID string      `json:"session_id"`
		TurnID    string      `json:"turn_id"`
		Payload   TurnPayload `json:"payload"`
	}{"turn", machine, client, sessionID, turnID, payload})
}

func importEventID(items []ImportItem) (string, error) {
	return semanticID(struct {
		Kind  string       `json:"kind"`
		Items []ImportItem `json:"items"`
	}{"import_batch", items})
}

func importItemID(path, hash string) (string, error) {
	return semanticID(struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
	}{path, hash})
}

func semanticID(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func boundText(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return '\uFFFD'
		}
		return r
	}, value)
	if len(value) <= MaxTextBytes {
		return value
	}
	b := []byte(value)[:MaxTextBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func canonicalEventBytes(event Event) ([]byte, error) {
	b, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func decodeCanonicalEvent(content []byte, filename, machineID string, now time.Time) (Event, []byte, error) {
	if len(content) == 0 || len(content) > MaxEventBytes || !utf8.Valid(content) {
		return Event{}, nil, errors.New("event is empty, oversized, or not UTF-8")
	}
	var event Event
	if err := decodeClosedJSON(content, &event); err != nil {
		return Event{}, nil, fmt.Errorf("parse event: %w", err)
	}
	if event.Kind == "feedback" && len(content) > MaxFeedbackEventBytes {
		return Event{}, nil, fmt.Errorf("feedback event exceeds %d bytes", MaxFeedbackEventBytes)
	}
	if err := validateEvent(event, filename, machineID, now); err != nil {
		return Event{}, nil, err
	}
	canonical, err := canonicalEventBytes(event)
	if err != nil {
		return Event{}, nil, err
	}
	if !bytes.Equal(content, canonical) {
		return Event{}, nil, errors.New("event is not canonical JSON")
	}
	return event, canonical, nil
}

func decodeCanonicalEventForRecovery(content []byte, filename, machineID string, now time.Time) (Event, []byte, bool, error) {
	event, canonical, err := decodeCanonicalEvent(content, filename, machineID, now)
	if !errors.Is(err, errFeedbackExpired) {
		return event, canonical, false, err
	}
	var envelope struct {
		CapturedAt time.Time `json:"captured_at"`
	}
	if decodeErr := json.Unmarshal(content, &envelope); decodeErr != nil || envelope.CapturedAt.IsZero() {
		return Event{}, nil, false, err
	}
	event, canonical, decodeErr := decodeCanonicalEvent(content, filename, machineID, envelope.CapturedAt)
	if decodeErr != nil {
		return Event{}, nil, false, decodeErr
	}
	return event, canonical, true, nil
}

func validateEvent(event Event, filename, machineID string, now time.Time) error {
	if err := validateEventEnvelope(event, filename, machineID); err != nil {
		return err
	}
	switch value := event.Payload.(type) {
	case ObservationPayload:
		if event.Kind != "observation" {
			return errors.New("event payload type does not match its kind")
		}
		return validateObservation(event, value)
	case TurnPayload:
		if event.Kind != "turn" {
			return errors.New("event payload type does not match its kind")
		}
		return validateTurn(event, value)
	case ImportPayload:
		if event.Kind != "import_batch" {
			return errors.New("event payload type does not match its kind")
		}
		return validateImportEvent(event, value)
	case FeedbackPayload:
		if event.Kind != "feedback" {
			return errors.New("event payload type does not match its kind")
		}
		return validateFeedbackEvent(event, value, now)
	default:
		return fmt.Errorf("event kind %q has an unsupported payload type", event.Kind)
	}
}

func validateProducerEvent(event Event, filename, machineID string) error {
	return validateEvent(event, filename, machineID, event.CapturedAt.UTC())
}

func validateEventEnvelope(event Event, filename, machineID string) error {
	digest, ok := eventDigest(event.ID)
	if event.Schema != Protocol || !ok || filename != digest+".json" {
		return errors.New("invalid event schema, id, or filename")
	}
	if event.Kind == "" || len(event.Kind) > 64 || !utf8.ValidString(event.Kind) || !validEventTime(event.CapturedAt) {
		return errors.New("event kind and captured_at are required")
	}
	if !validMachineID(event.Machine.ID) || event.Machine.ID != machineID ||
		!validRequiredString(event.Machine.Hostname, MaxMachineHostname) {
		return errors.New("event machine does not match the local identity")
	}
	if !validRequiredString(event.Client, MaxClient) ||
		!validOptionalString(event.SessionID, MaxSessionID) || !validOptionalString(event.TurnID, MaxTurnID) {
		return errors.New("invalid event client or session identifiers")
	}
	if !validRequiredString(event.Source.Kind, MaxSourceKind) ||
		!validRequiredString(event.Source.Locator, MaxSourceLocator) {
		return errors.New("event source is incomplete")
	}
	return nil
}

func validateObservation(event Event, value ObservationPayload) error {
	if event.SessionID != "" || event.TurnID != "" {
		return errors.New("observation cannot carry session or turn identifiers")
	}
	if !validRequiredString(value.Text, MaxTextBytes) {
		return errors.New("invalid observation payload")
	}
	expected, err := observationEventID(event.Machine.ID, event.Client, value)
	if err != nil || event.ID != expected {
		return errors.New("observation semantic id mismatch")
	}
	return nil
}

func validateTurn(event Event, value TurnPayload) error {
	if (value.Prompt == "" && value.Response == "") ||
		!validOptionalString(value.Prompt, MaxTextBytes) || !validOptionalString(value.Response, MaxTextBytes) ||
		!validOptionalString(value.CWD, MaxCWD) || !validOptionalString(value.Model, MaxModel) {
		return errors.New("invalid turn payload")
	}
	expected, err := turnEventID(event.Machine.ID, event.Client, event.SessionID, event.TurnID, value)
	if err != nil || event.ID != expected {
		return errors.New("turn semantic id mismatch")
	}
	return nil
}

func validateImportEvent(event Event, value ImportPayload) error {
	if event.SessionID != "" || event.TurnID != "" {
		return errors.New("import batch cannot carry session or turn identifiers")
	}
	if err := validateImportPayload(value); err != nil {
		return err
	}
	expected, err := importEventID(value.Items)
	if err != nil || event.ID != expected {
		return errors.New("import batch semantic id mismatch")
	}
	return nil
}

func decodeClosedJSON(content []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validEventTime(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	year := value.UTC().Year()
	return year >= 0 && year <= 9999 && len(value.UTC().Format("2006")) == 4
}

func validateImportPayload(payload ImportPayload) error {
	if !validRequiredString(payload.Source, MaxImportSource) || len(payload.Items) == 0 || len(payload.Items) > MaxImportFiles {
		return errors.New("invalid import source or item count")
	}
	total := 0
	for _, item := range payload.Items {
		if _, ok := eventDigest(item.ID); !ok || !validRequiredString(item.Path, MaxImportPath) ||
			!validOptionalString(item.Content, MaxImportText) || !validLowerHex(item.SHA256, sha256.Size*2) {
			return errors.New("invalid import item")
		}
		sum := sha256.Sum256([]byte(item.Content))
		if item.SHA256 != hex.EncodeToString(sum[:]) {
			return errors.New("import item checksum mismatch")
		}
		expected, err := importItemID(item.Path, item.SHA256)
		if err != nil || item.ID != expected {
			return errors.New("import item semantic id mismatch")
		}
		total += len(item.Content)
	}
	if total > MaxImportBytes {
		return errors.New("import payload exceeds source limit")
	}
	return nil
}

func eventDigest(id string) (string, bool) {
	if !strings.HasPrefix(id, "sha256:") {
		return "", false
	}
	digest := strings.TrimPrefix(id, "sha256:")
	return digest, validLowerHex(digest, sha256.Size*2)
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

func validRequiredString(value string, limit int) bool {
	return value != "" && validOptionalString(value, limit)
}

func validOptionalString(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit
}

func boundString(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	b := []byte(value)[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
