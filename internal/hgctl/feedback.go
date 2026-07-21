package hgctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	localSurfaceReceiptSchemaVersion = 1
	maxLocalSurfaceReceipts          = 512
	maxLocalSurfaceReceiptBytes      = 128 * 1024
)

type localSurfaceReceipt struct {
	SchemaVersion int           `json:"schema_version"`
	Surface       RecallSurface `json:"surface"`
	Terminal      *Event        `json:"terminal,omitempty"`
}

func (a *App) runFeedback(ctx context.Context, args []string) error {
	client, rest, err := extractOption(args, "--client", "")
	if err != nil {
		return err
	}
	outcome, rest, err := extractOption(rest, "--outcome", "")
	if err != nil {
		return err
	}
	resultText, rest, err := extractOption(rest, "--result", "")
	if err != nil {
		return err
	}
	if !validEndpointClient(client) || outcome == "" || len(rest) != 1 {
		return errors.New("usage: hgctl feedback <surface-id> --client <claude|codex> --outcome <outcome> [--result <rank>]")
	}
	var result *int
	if resultText != "" {
		value, err := strconv.Atoi(resultText)
		if err != nil || value < 1 || value > MaxSurfaceResults {
			return errors.New("feedback --result must be a surfaced rank from 1 to 8")
		}
		result = &value
	}
	event, err := a.assessSurface(ctx, rest[0], client, outcome, result)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Out, event.ID)
	return err
}

func (a *App) saveSurface(ctx context.Context, surface RecallSurface) error {
	return withFileLockWait(ctx, a.Paths.SurfaceLock, func() error {
		if err := a.pruneSurfaceReceiptsLocked(a.Now().UTC(), true); err != nil {
			return err
		}
		path, ok := a.surfaceReceiptPath(surface.ID)
		if !ok {
			return errors.New("surface has an invalid id")
		}
		if _, err := os.Lstat(path); err == nil {
			return errors.New("surface id collision")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return writeJSONAtomic(path, localSurfaceReceipt{
			SchemaVersion: localSurfaceReceiptSchemaVersion,
			Surface:       surface,
		}, 0o600)
	})
}

func (a *App) assessSurface(ctx context.Context, surfaceID, client, outcome string, result *int) (Event, error) {
	var accepted Event
	err := withFileLockWait(ctx, a.Paths.SurfaceLock, func() error {
		receipt, path, err := a.loadSurfaceReceipt(surfaceID)
		if err != nil {
			return err
		}
		identity, err := a.loadIdentity()
		if err != nil {
			return err
		}
		if receipt.Surface.MachineID != identity.ID || receipt.Surface.Client != client {
			return errors.New("surface does not belong to this machine and client")
		}
		now := a.Now().UTC()
		if now.Sub(receipt.Surface.IssuedAt) > SurfaceLifetime || receipt.Surface.IssuedAt.After(now.Add(FeedbackFutureSkew)) {
			return errFeedbackExpired
		}
		if receipt.Terminal != nil {
			payload, err := feedbackPayload(*receipt.Terminal)
			if err != nil {
				return err
			}
			if payload.Outcome != outcome || !sameOptionalRank(payload.Result, result) {
				return errors.New("surface already has a different terminal assessment")
			}
			accepted = *receipt.Terminal
			return a.enqueueFeedback(accepted)
		}
		event, err := newFeedbackEvent(identity, receipt.Surface, outcome, result, now)
		if err != nil {
			return err
		}
		existing, exists, err := a.loadFeedbackOutbox(event.ID, identity.ID, now)
		if err != nil {
			return err
		}
		if exists {
			payload, err := feedbackPayload(existing)
			if err != nil {
				return err
			}
			receipt.Terminal = &existing
			if err := writeJSONAtomic(path, receipt, 0o600); err != nil {
				return err
			}
			if payload.Outcome != outcome || !sameOptionalRank(payload.Result, result) {
				return errors.New("surface already has a different terminal assessment")
			}
			accepted = existing
			return nil
		}
		if a.eventDelivered(event.ID) {
			return errors.New("surface assessment was delivered but its local terminal bytes are unavailable")
		}
		receipt.Terminal = &event
		if err := writeJSONAtomic(path, receipt, 0o600); err != nil {
			return err
		}
		accepted = event
		return a.enqueueFeedback(event)
	})
	return accepted, err
}

func (a *App) loadFeedbackOutbox(id, machineID string, now time.Time) (Event, bool, error) {
	digest, ok := eventDigest(id)
	if !ok {
		return Event{}, false, errors.New("invalid feedback event id")
	}
	name := digest + ".json"
	content, err := readOutboxFile(filepath.Join(a.Paths.Outbox, name))
	if errors.Is(err, os.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	event, _, err := decodeCanonicalEvent(content, name, machineID, now)
	if err != nil {
		return Event{}, false, fmt.Errorf("existing same-id feedback outbox is invalid: %w", err)
	}
	if event.Kind != "feedback" {
		return Event{}, false, errors.New("existing same-id feedback outbox is not feedback")
	}
	return event, true, nil
}

func feedbackPayload(event Event) (FeedbackPayload, error) {
	payload, ok := event.Payload.(FeedbackPayload)
	if !ok || event.Kind != "feedback" {
		return FeedbackPayload{}, errors.New("event is not feedback")
	}
	return payload, nil
}

func sameOptionalRank(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (a *App) surfaceReceiptPath(id string) (string, bool) {
	digest, ok := eventDigest(id)
	if !ok {
		return "", false
	}
	return filepath.Join(a.Paths.Surfaces, digest+".json"), true
}

func (a *App) loadSurfaceReceipt(id string) (localSurfaceReceipt, string, error) {
	path, ok := a.surfaceReceiptPath(id)
	if !ok {
		return localSurfaceReceipt{}, "", errors.New("invalid surface id")
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return localSurfaceReceipt{}, "", errors.New("surface receipt is missing or expired")
		}
		return localSurfaceReceipt{}, "", err
	}
	if !info.Mode().IsRegular() {
		return localSurfaceReceipt{}, "", errors.New("local surface receipt is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return localSurfaceReceipt{}, "", err
	}
	content, readErr := io.ReadAll(io.LimitReader(file, maxLocalSurfaceReceiptBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return localSurfaceReceipt{}, "", readErr
	}
	if closeErr != nil {
		return localSurfaceReceipt{}, "", closeErr
	}
	if len(content) > maxLocalSurfaceReceiptBytes {
		return localSurfaceReceipt{}, "", errors.New("local surface receipt exceeds its byte limit")
	}
	var receipt localSurfaceReceipt
	if err := decodeClosedJSON(content, &receipt); err != nil {
		return localSurfaceReceipt{}, "", fmt.Errorf("parse local surface receipt: %w", err)
	}
	if receipt.SchemaVersion != localSurfaceReceiptSchemaVersion || receipt.Surface.ID != id ||
		validateRecallSurface(receipt.Surface, receipt.Surface.MachineID, receipt.Surface.Client) != nil {
		return localSurfaceReceipt{}, "", errors.New("local surface receipt is invalid")
	}
	if receipt.Terminal != nil {
		payload, payloadErr := feedbackPayload(*receipt.Terminal)
		name := strings.TrimPrefix(receipt.Terminal.ID, "sha256:") + ".json"
		if payloadErr != nil || payload.Surface.ID != receipt.Surface.ID ||
			validateEvent(*receipt.Terminal, name, receipt.Surface.MachineID, receipt.Terminal.CapturedAt) != nil {
			return localSurfaceReceipt{}, "", errors.New("local terminal assessment is invalid")
		}
	}
	return receipt, path, nil
}

func (a *App) recoverTerminalFeedbackOutbox(ctx context.Context, now time.Time) error {
	return withFileLockWait(ctx, a.Paths.SurfaceLock, func() error {
		if err := a.pruneSurfaceReceiptsLocked(now, false); err != nil {
			return err
		}
		entries, err := os.ReadDir(a.Paths.Surfaces)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
				!validLowerHex(strings.TrimSuffix(entry.Name(), ".json"), 64) {
				continue
			}
			id := "sha256:" + strings.TrimSuffix(entry.Name(), ".json")
			receipt, _, err := a.loadSurfaceReceipt(id)
			if err != nil {
				return err
			}
			if receipt.Terminal == nil || a.eventDelivered(receipt.Terminal.ID) {
				continue
			}
			if now.Sub(receipt.Surface.IssuedAt) > SurfaceLifetime || receipt.Surface.IssuedAt.After(now.Add(FeedbackFutureSkew)) {
				continue
			}
			if err := a.enqueueFeedback(*receipt.Terminal); err != nil {
				return fmt.Errorf("restore terminal assessment %s: %w", receipt.Terminal.ID, err)
			}
		}
		return nil
	})
}

func (a *App) pruneSurfaceReceiptsLocked(now time.Time, makeRoom bool) error {
	if err := os.MkdirAll(a.Paths.Surfaces, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(a.Paths.Surfaces)
	if err != nil {
		return err
	}
	type retainedReceipt struct {
		path     string
		issuedAt time.Time
	}
	retained := make([]retainedReceipt, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || !validLowerHex(strings.TrimSuffix(entry.Name(), ".json"), 64) {
			continue
		}
		id := "sha256:" + strings.TrimSuffix(entry.Name(), ".json")
		receipt, path, err := a.loadSurfaceReceipt(id)
		if err != nil {
			path = filepath.Join(a.Paths.Surfaces, entry.Name())
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			continue
		}
		if now.Sub(receipt.Surface.IssuedAt) > SurfaceLifetime {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		retained = append(retained, retainedReceipt{path: path, issuedAt: receipt.Surface.IssuedAt})
	}
	limit := maxLocalSurfaceReceipts
	if makeRoom {
		limit--
	}
	if len(retained) <= limit {
		return nil
	}
	sort.Slice(retained, func(i, j int) bool {
		if retained[i].issuedAt.Equal(retained[j].issuedAt) {
			return retained[i].path < retained[j].path
		}
		return retained[i].issuedAt.Before(retained[j].issuedAt)
	})
	for _, receipt := range retained[:len(retained)-limit] {
		if err := os.Remove(receipt.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
