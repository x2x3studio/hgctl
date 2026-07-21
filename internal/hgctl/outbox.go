package hgctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type deliveryReceipt struct {
	ID          string    `json:"id"`
	DeliveredAt time.Time `json:"delivered_at"`
}

func (a *App) enqueue(event Event) error {
	if _, ok := a.deliveryReceiptPath(event.ID); event.Schema != Protocol || event.Kind == "feedback" || !ok {
		return errors.New("invalid event envelope")
	}
	identity, err := a.loadIdentity()
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(event.ID, "sha256:") + ".json"
	if err := validateProducerEvent(event, name, identity.ID); err != nil {
		return fmt.Errorf("validate event before enqueue: %w", err)
	}
	b, err := canonicalEventBytes(event)
	if err != nil {
		return err
	}
	if len(b) > MaxEventBytes {
		return fmt.Errorf("event is %d bytes; limit is %d", len(b), MaxEventBytes)
	}
	_, canonical, err := decodeCanonicalEvent(b, name, identity.ID, event.CapturedAt.UTC())
	if err != nil {
		return fmt.Errorf("validate event before enqueue: %w", err)
	}
	if a.eventDelivered(event.ID) {
		return nil
	}
	path := filepath.Join(a.Paths.Outbox, name)
	existing, err := os.ReadFile(path)
	if err == nil {
		if existingEvent, _, decodeErr := decodeCanonicalEvent(existing, name, identity.ID, event.CapturedAt.UTC()); decodeErr != nil || existingEvent.Kind == "feedback" {
			return fmt.Errorf("outbox collision for %s: existing entry is invalid", name)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFileAtomic(path, canonical, 0o600)
}

func (a *App) enqueueFeedback(event Event) error {
	if event.Schema != Protocol || event.Kind != "feedback" {
		return errors.New("invalid feedback event envelope")
	}
	identity, err := a.loadIdentity()
	if err != nil {
		return err
	}
	name := strings.TrimPrefix(event.ID, "sha256:") + ".json"
	if err := validateProducerEvent(event, name, identity.ID); err != nil {
		return fmt.Errorf("validate feedback before enqueue: %w", err)
	}
	canonical, err := canonicalEventBytes(event)
	if err != nil {
		return err
	}
	if len(canonical) > MaxFeedbackEventBytes {
		return fmt.Errorf("feedback event is %d bytes; limit is %d", len(canonical), MaxFeedbackEventBytes)
	}
	if a.eventDelivered(event.ID) {
		return nil
	}
	path := filepath.Join(a.Paths.Outbox, name)
	existing, err := os.ReadFile(path)
	if err == nil {
		existingEvent, existingCanonical, decodeErr := decodeCanonicalEvent(existing, name, identity.ID, event.CapturedAt.UTC())
		if decodeErr != nil || existingEvent.Kind != "feedback" {
			return fmt.Errorf("outbox collision for %s: existing entry is not feedback", name)
		}
		if !bytes.Equal(existingCanonical, canonical) {
			return fmt.Errorf("outbox collision for %s: first terminal assessment differs", name)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFileAtomic(path, canonical, 0o600)
}

func (a *App) eventDelivered(id string) bool {
	if receiptPath, ok := a.deliveryReceiptPath(id); ok {
		var receipt deliveryReceipt
		if readJSON(receiptPath, &receipt) == nil && receipt.ID == id {
			return true
		}
	}
	return false
}

func (a *App) markDelivered(paths []string) error {
	for _, path := range paths {
		var event Event
		if err := readJSON(path, &event); err != nil {
			return err
		}
		receiptPath, ok := a.deliveryReceiptPath(event.ID)
		if !ok {
			return fmt.Errorf("invalid delivered event id %q", event.ID)
		}
		receipt := deliveryReceipt{ID: event.ID, DeliveredAt: a.Now().UTC()}
		if err := writeJSONAtomic(receiptPath, receipt, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) deliveryReceiptPath(id string) (string, bool) {
	digest := strings.TrimPrefix(id, "sha256:")
	if len(digest) != sha256.Size*2 {
		return "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return filepath.Join(a.Paths.Delivered, digest[:2], digest), true
}

func (a *App) pruneExpiredFeedbackOutbox(now time.Time) error {
	entries, err := os.ReadDir(a.Paths.Outbox)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, err := a.loadIdentity()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(a.Paths.Outbox, entry.Name())
		content, err := readOutboxFile(path)
		if err != nil {
			continue
		}
		_, _, decodeErr := decodeCanonicalEvent(content, entry.Name(), identity.ID, now)
		if !errors.Is(decodeErr, errFeedbackExpired) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
