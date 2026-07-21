package hgctl

import (
	"encoding/json"
	"errors"
	"time"
)

type queuedEvent struct {
	Schema     string
	ID         string
	CapturedAt time.Time
	Canonical  []byte
}

func decodeCanonicalQueuedEvent(content []byte, filename, machineID string, now time.Time) (queuedEvent, error) {
	var probe struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(content, &probe); err != nil {
		return queuedEvent{}, err
	}
	switch probe.Schema {
	case Protocol:
		event, canonical, err := decodeCanonicalOutboxEvent(content, filename, machineID)
		if err != nil {
			return queuedEvent{}, err
		}
		return queuedEvent{Schema: event.Schema, ID: event.ID, CapturedAt: event.CapturedAt, Canonical: canonical}, nil
	case FeedbackProtocol:
		event, canonical, err := decodeCanonicalFeedbackEvent(content, filename, machineID, now)
		if err != nil {
			return queuedEvent{}, err
		}
		return queuedEvent{Schema: event.Schema, ID: event.ID, CapturedAt: event.CapturedAt, Canonical: canonical}, nil
	default:
		return queuedEvent{}, errors.New("unsupported local outbox event schema")
	}
}

func decodeCanonicalQueuedEventForRecovery(content []byte, filename, machineID string, now time.Time) (queuedEvent, bool, error) {
	event, err := decodeCanonicalQueuedEvent(content, filename, machineID, now)
	if !errors.Is(err, errFeedbackExpired) {
		return event, false, err
	}
	var envelope struct {
		CapturedAt time.Time `json:"captured_at"`
	}
	if decodeErr := json.Unmarshal(content, &envelope); decodeErr != nil || envelope.CapturedAt.IsZero() {
		return queuedEvent{}, false, err
	}
	event, decodeErr := decodeCanonicalQueuedEvent(content, filename, machineID, envelope.CapturedAt)
	if decodeErr != nil {
		return queuedEvent{}, false, decodeErr
	}
	return event, true, nil
}
