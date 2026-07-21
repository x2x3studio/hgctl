package hgctl

import (
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
	if _, ok := a.deliveryReceiptPath(event.ID); event.Schema != Protocol || !ok {
		return errors.New("invalid event envelope")
	}
	b, err := canonicalEventBytes(event)
	if err != nil {
		return err
	}
	if len(b) > MaxEventBytes {
		return fmt.Errorf("event is %d bytes; limit is %d", len(b), MaxEventBytes)
	}
	if a.eventDelivered(event.ID) {
		return nil
	}
	name := strings.TrimPrefix(event.ID, "sha256:") + ".json"
	path := filepath.Join(a.Paths.Outbox, name)
	existing, err := os.ReadFile(path)
	if err == nil {
		if _, _, decodeErr := decodeCanonicalOutboxEvent(existing, name, event.Machine.ID); decodeErr != nil {
			return fmt.Errorf("outbox collision for %s: existing entry is invalid", name)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFileAtomic(path, b, 0o600)
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
