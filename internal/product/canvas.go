package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
)

type canvasDocument struct {
	Nodes []map[string]json.RawMessage `json:"nodes"`
	Edges []map[string]json.RawMessage `json:"edges"`
}

func ValidateCanvas(content []byte) error {
	_, err := validateCanvas(content)
	return err
}

func ValidateCanvasReferences(content []byte, fileExists func(string) bool) error {
	document, err := validateCanvas(content)
	if err != nil {
		return err
	}
	if fileExists == nil {
		return errors.New("Canvas file lookup is required")
	}
	for index, node := range document.Nodes {
		kind, _ := requiredString(node, "type")
		if kind != "file" {
			continue
		}
		name, _ := requiredString(node, "file")
		if !fileExists(name) {
			return fmt.Errorf("node %d references missing Memory file %q", index, name)
		}
	}
	return nil
}

func validateCanvas(content []byte) (canvasDocument, error) {
	var raw map[string]json.RawMessage
	if err := decodeOne(content, &raw); err != nil {
		return canvasDocument{}, fmt.Errorf("parse Canvas: %w", err)
	}
	if len(raw) != 2 || raw["nodes"] == nil || raw["edges"] == nil {
		return canvasDocument{}, errors.New("Canvas root must contain only nodes and edges")
	}
	var document canvasDocument
	if err := decodeOne(content, &document); err != nil {
		return canvasDocument{}, fmt.Errorf("parse Canvas: %w", err)
	}
	if document.Nodes == nil || document.Edges == nil {
		return canvasDocument{}, errors.New("Canvas nodes and edges must be arrays")
	}

	ids := make(map[string]struct{}, len(document.Nodes))
	for index, node := range document.Nodes {
		id, err := requiredString(node, "id")
		if err != nil {
			return canvasDocument{}, fmt.Errorf("node %d: %w", index, err)
		}
		if _, duplicate := ids[id]; duplicate {
			return canvasDocument{}, fmt.Errorf("node %d has duplicate id %q", index, id)
		}
		ids[id] = struct{}{}
		kind, err := requiredString(node, "type")
		if err != nil {
			return canvasDocument{}, fmt.Errorf("node %d: %w", index, err)
		}
		for _, field := range []string{"x", "y"} {
			if _, err := requiredNumber(node, field, false); err != nil {
				return canvasDocument{}, fmt.Errorf("node %d: %w", index, err)
			}
		}
		for _, field := range []string{"width", "height"} {
			if _, err := requiredNumber(node, field, true); err != nil {
				return canvasDocument{}, fmt.Errorf("node %d: %w", index, err)
			}
		}
		switch kind {
		case "text":
			if _, err := requiredStringValue(node, "text", true); err != nil {
				return canvasDocument{}, fmt.Errorf("node %d: %w", index, err)
			}
		case "file":
			name, err := requiredString(node, "file")
			if err != nil || (name != "Home.md" && !IsMemoryPath(name)) {
				return canvasDocument{}, fmt.Errorf("node %d has an invalid Memory file", index)
			}
		case "link":
			value, err := requiredString(node, "url")
			if err != nil || !safeCanvasURL(value) {
				return canvasDocument{}, fmt.Errorf("node %d has an invalid URL", index)
			}
		case "group":
		default:
			return canvasDocument{}, fmt.Errorf("node %d has unsupported type %q", index, kind)
		}
	}

	edgeIDs := make(map[string]struct{}, len(document.Edges))
	for index, edge := range document.Edges {
		id, err := requiredString(edge, "id")
		if err != nil {
			return canvasDocument{}, fmt.Errorf("edge %d: %w", index, err)
		}
		if _, duplicate := edgeIDs[id]; duplicate {
			return canvasDocument{}, fmt.Errorf("edge %d has duplicate id %q", index, id)
		}
		edgeIDs[id] = struct{}{}
		for _, field := range []string{"fromNode", "toNode"} {
			nodeID, err := requiredString(edge, field)
			if err != nil {
				return canvasDocument{}, fmt.Errorf("edge %d: %w", index, err)
			}
			if _, exists := ids[nodeID]; !exists {
				return canvasDocument{}, fmt.Errorf("edge %d references unknown node %q", index, nodeID)
			}
		}
		for _, field := range []string{"fromSide", "toSide"} {
			if err := optionalEnum(edge, field, "top", "right", "bottom", "left"); err != nil {
				return canvasDocument{}, fmt.Errorf("edge %d: %w", index, err)
			}
		}
		for _, field := range []string{"fromEnd", "toEnd"} {
			if err := optionalEnum(edge, field, "none", "arrow"); err != nil {
				return canvasDocument{}, fmt.Errorf("edge %d: %w", index, err)
			}
		}
	}
	return document, nil
}

func decodeOne(content []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
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

func requiredString(object map[string]json.RawMessage, field string) (string, error) {
	return requiredStringValue(object, field, false)
}

func requiredStringValue(object map[string]json.RawMessage, field string, emptyAllowed bool) (string, error) {
	var value string
	if object[field] == nil || json.Unmarshal(object[field], &value) != nil || (!emptyAllowed && value == "") {
		return "", fmt.Errorf("%s must be a string", field)
	}
	return value, nil
}

func requiredNumber(object map[string]json.RawMessage, field string, positive bool) (float64, error) {
	var value float64
	if object[field] == nil || json.Unmarshal(object[field], &value) != nil || (positive && value <= 0) {
		return 0, fmt.Errorf("%s must be a valid number", field)
	}
	return value, nil
}

func optionalEnum(object map[string]json.RawMessage, field string, allowed ...string) error {
	raw := object[field]
	if raw == nil || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Errorf("%s must be a string", field)
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("%s has unsupported value %q", field, value)
}

func safeCanvasURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http")
}
