package hgctl

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SurfaceProtocol = "hourglass.surface/v1"

	MaxFeedbackEventBytes = 64 * 1024
	MaxSurfaceResults     = 8
	SurfaceLifetime       = 7 * 24 * time.Hour
	FeedbackFutureSkew    = 5 * time.Minute
)

var errFeedbackExpired = errors.New("feedback surface receipt expired")

type SharedRevision struct {
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
}

type SurfaceResult struct {
	Rank int    `json:"rank"`
	Path string `json:"path"`
	Blob string `json:"blob"`
}

type RecallSurface struct {
	Schema    string          `json:"schema"`
	ID        string          `json:"id"`
	Nonce     string          `json:"nonce"`
	IssuedAt  time.Time       `json:"issued_at"`
	MachineID string          `json:"machine_id"`
	Client    string          `json:"client"`
	Origin    string          `json:"origin"`
	Shared    SharedRevision  `json:"shared"`
	Results   []SurfaceResult `json:"results"`
}

type FeedbackPayload struct {
	Surface RecallSurface `json:"surface"`
	Outcome string        `json:"outcome"`
	Result  *int          `json:"result,omitempty"`
}

func newRecallSurface(identity Identity, client, origin string, revision SharedRevision, results []SurfaceResult, issuedAt time.Time) (RecallSurface, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return RecallSurface{}, fmt.Errorf("create surface nonce: %w", err)
	}
	surface := RecallSurface{
		Schema: SurfaceProtocol, Nonce: hex.EncodeToString(nonceBytes), IssuedAt: issuedAt.UTC(),
		MachineID: identity.ID, Client: client, Origin: origin, Shared: revision, Results: results,
	}
	id, err := recallSurfaceID(surface)
	if err != nil {
		return RecallSurface{}, err
	}
	surface.ID = id
	if err := validateRecallSurface(surface, identity.ID, client); err != nil {
		return RecallSurface{}, err
	}
	return surface, nil
}

func newFeedbackEvent(identity Identity, surface RecallSurface, outcome string, result *int, capturedAt time.Time) (Event, error) {
	payload := FeedbackPayload{Surface: surface, Outcome: outcome, Result: result}
	id, err := feedbackEventID(identity.ID, surface.Client, surface.ID)
	if err != nil {
		return Event{}, err
	}
	event := Event{
		Schema: Protocol, ID: id, Kind: "feedback", CapturedAt: capturedAt.UTC(),
		Machine: Machine{ID: identity.ID, Hostname: identity.Hostname}, Client: surface.Client,
		Source: Source{Kind: "recall", Locator: surface.ID}, Payload: payload,
	}
	name := strings.TrimPrefix(id, "sha256:") + ".json"
	if err := validateProducerEvent(event, name, identity.ID); err != nil {
		return Event{}, err
	}
	return event, nil
}

func recallSurfaceID(surface RecallSurface) (string, error) {
	return semanticID(struct {
		Kind      string          `json:"kind"`
		MachineID string          `json:"machine_id"`
		Client    string          `json:"client"`
		Origin    string          `json:"origin"`
		Nonce     string          `json:"nonce"`
		IssuedAt  time.Time       `json:"issued_at"`
		Shared    SharedRevision  `json:"shared"`
		Results   []SurfaceResult `json:"results"`
	}{"recall_surface", surface.MachineID, surface.Client, surface.Origin, surface.Nonce, surface.IssuedAt, surface.Shared, surface.Results})
}

func feedbackEventID(machine, client, surfaceID string) (string, error) {
	return semanticID(struct {
		Kind      string `json:"kind"`
		Machine   string `json:"machine"`
		Client    string `json:"client"`
		SurfaceID string `json:"surface_id"`
	}{"feedback", machine, client, surfaceID})
}

func validateFeedbackEvent(event Event, payload FeedbackPayload, now time.Time) error {
	if event.Kind != "feedback" || event.SessionID != "" || event.TurnID != "" || !validEndpointClient(event.Client) {
		return errors.New("feedback machine or client is invalid")
	}
	if event.Source.Kind != "recall" || event.Source.Locator != payload.Surface.ID {
		return errors.New("feedback source does not bind its surface")
	}
	if !validEventTime(event.CapturedAt) || now.IsZero() {
		return errors.New("feedback captured_at is invalid")
	}
	if err := validateRecallSurface(payload.Surface, event.Machine.ID, event.Client); err != nil {
		return err
	}
	if err := validateFeedbackOutcome(payload); err != nil {
		return err
	}
	issuedAt := payload.Surface.IssuedAt
	if issuedAt.After(now.Add(FeedbackFutureSkew)) || event.CapturedAt.After(now.Add(FeedbackFutureSkew)) ||
		event.CapturedAt.Before(issuedAt) || event.CapturedAt.Sub(issuedAt) > SurfaceLifetime {
		return errors.New("feedback timestamps are outside the allowed clock skew")
	}
	if now.Sub(issuedAt) > SurfaceLifetime {
		return errFeedbackExpired
	}
	wantID, err := feedbackEventID(event.Machine.ID, event.Client, payload.Surface.ID)
	if err != nil || event.ID != wantID {
		return errors.New("feedback semantic id mismatch")
	}
	return nil
}

func validateRecallSurface(surface RecallSurface, machineID, client string) error {
	if surface.Schema != SurfaceProtocol || surface.MachineID != machineID || surface.Client != client ||
		!validEndpointClient(client) || (surface.Origin != "explicit" && surface.Origin != "session_start") ||
		!validLowerHex(surface.Nonce, 32) || !validObjectID(surface.Shared.Commit) || !validObjectID(surface.Shared.Tree) ||
		surface.Results == nil || len(surface.Results) > MaxSurfaceResults || !validEventTime(surface.IssuedAt) {
		return errors.New("surface identity or binding is invalid")
	}
	seen := make(map[string]struct{}, len(surface.Results))
	for index, result := range surface.Results {
		if result.Rank != index+1 || !validMemoryPath(result.Path) || !validObjectID(result.Blob) {
			return errors.New("surface result is invalid or out of order")
		}
		if _, duplicate := seen[result.Path]; duplicate {
			return errors.New("surface contains a duplicate result path")
		}
		seen[result.Path] = struct{}{}
	}
	wantID, err := recallSurfaceID(surface)
	if err != nil || surface.ID != wantID {
		return errors.New("surface semantic id mismatch")
	}
	return nil
}

func validateFeedbackOutcome(payload FeedbackPayload) error {
	switch payload.Outcome {
	case "zero_hit":
		if payload.Surface.Origin != "explicit" || len(payload.Surface.Results) != 0 || payload.Result != nil {
			return errors.New("zero_hit requires an explicit empty surface")
		}
	case "used", "irrelevant", "stale", "contradicted":
		if len(payload.Surface.Results) == 0 || payload.Result == nil || *payload.Result < 1 || *payload.Result > len(payload.Surface.Results) {
			return errors.New("card feedback must select a surfaced result")
		}
	default:
		return fmt.Errorf("unsupported feedback outcome %q", payload.Outcome)
	}
	return nil
}

func validMemoryPath(name string) bool {
	if name == "" || len(name) > MaxImportPath || !utf8.ValidString(name) || strings.Contains(name, "\\") ||
		strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, character := range name {
		if character == 0 || character < 0x20 || character == 0x7f {
			return false
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) < 2 || parts[0] != "memory" || !strings.HasSuffix(parts[len(parts)-1], ".md") {
		return false
	}
	blockedFiles := map[string]struct{}{
		".mcp.json": {}, "agents.md": {}, "agents.override.md": {}, "claude.local.md": {},
		"claude.md": {}, "gemini.md": {}, "skill.md": {},
	}
	blockedDirectories := map[string]struct{}{
		".agents": {}, ".claude": {}, ".codex": {}, ".cursor": {}, ".gemini": {},
	}
	for index, part := range parts {
		folded := strings.ToLower(part)
		if _, blocked := blockedDirectories[folded]; blocked {
			return false
		}
		if index == len(parts)-1 {
			if _, blocked := blockedFiles[folded]; blocked {
				return false
			}
		}
	}
	for _, part := range parts[1:] {
		if len(part) == 0 || len(part) > 128 || !lowerLetterOrDigit(part[0]) {
			return false
		}
		for index := 1; index < len(part); index++ {
			value := part[index]
			if !lowerLetterOrDigit(value) && value != '.' && value != '_' && value != '-' {
				return false
			}
		}
	}
	return true
}

func lowerLetterOrDigit(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func validObjectID(value string) bool {
	return validLowerHex(value, 40)
}

func aggregateKey(name, blob string) string {
	digest := sha256.Sum256([]byte(name + "\x00" + blob))
	return hex.EncodeToString(digest[:])
}
