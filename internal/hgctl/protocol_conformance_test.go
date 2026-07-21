package hgctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const conformanceManifestSHA256 = "ee80adeac83438302f4d75b0f2160dfb1d9733adaa9d9651edbdf3f81a9e5f5b"

type producerCorpusManifest struct {
	Schema       string               `json:"schema"`
	Protocol     string               `json:"protocol"`
	SourceFile   string               `json:"source_file"`
	SourceSHA256 string               `json:"source_sha256"`
	Cases        []producerCorpusCase `json:"cases"`
}

type producerCorpusCase struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Machine string `json:"machine"`
	Path    string `json:"path"`
	Expect  string `json:"expect"`
	Kind    string `json:"kind,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	EventID string `json:"event_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type producerCorpusSource struct {
	Schema    string `json:"schema"`
	Protocol  string `json:"protocol"`
	Authority string `json:"authority"`
	Encoding  string `json:"encoding"`
	Purpose   string `json:"purpose"`
}

func TestProducerConformsToPinnedHourglassEventCorpus(t *testing.T) {
	root := filepath.Join("testdata", "protocol", "event")
	manifestContent := readProducerFixture(t, root, "manifest.json")
	if got := producerFixtureDigest(manifestContent); got != conformanceManifestSHA256 {
		t.Fatalf("conformance manifest hash=%s, want pinned %s", got, conformanceManifestSHA256)
	}
	var manifest producerCorpusManifest
	decodeProducerFixture(t, manifestContent, &manifest)
	if manifest.Schema != "hourglass.conformance-manifest/v1" || manifest.Protocol != Protocol {
		t.Fatalf("unexpected conformance manifest identity: %#v", manifest)
	}

	sourceContent := readProducerFixture(t, root, manifest.SourceFile)
	if producerFixtureDigest(sourceContent) != manifest.SourceSHA256 {
		t.Fatal("conformance source metadata does not match its pinned digest")
	}
	var source producerCorpusSource
	decodeProducerFixture(t, sourceContent, &source)
	if source.Schema != "hourglass.conformance-source/v1" || source.Protocol != Protocol ||
		source.Authority != "protocol/event.md" || source.Encoding == "" || source.Purpose == "" {
		t.Fatalf("invalid conformance source metadata: %#v", source)
	}
	listed := make(map[string]struct{}, len(manifest.Cases))
	for _, test := range manifest.Cases {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			if test.Name == "" || test.File == "" || test.Machine == "" || test.Path == "" ||
				(test.Expect != "valid" && test.Reason == "") {
				t.Fatal("conformance case metadata is incomplete")
			}
			if _, duplicate := listed[test.File]; duplicate {
				t.Fatalf("duplicate conformance fixture %s", test.File)
			}
			listed[test.File] = struct{}{}
			content := readProducerFixture(t, root, test.File)
			if got := producerFixtureDigest(content); got != test.SHA256 {
				t.Fatalf("fixture hash=%s, want %s", got, test.SHA256)
			}

			filename := path.Base(test.Path)
			event, canonical, err := decodeCanonicalEvent(content, filename, test.Machine)
			accepted := err == nil && bytes.Equal(canonical, content) && producerQueuePath(event) == test.Path
			switch test.Expect {
			case "valid":
				if !accepted {
					t.Fatalf("valid producer fixture is not endpoint-compatible: %v", err)
				}
				if event.ID != test.EventID || event.Kind != test.Kind {
					t.Fatalf("decoded event=%s/%s, want %s/%s", event.ID, event.Kind, test.EventID, test.Kind)
				}
			case "invalid":
				if accepted {
					t.Fatalf("producer accepted %s fixture %s", test.Expect, test.File)
				}
			default:
				t.Fatalf("unknown conformance expectation %q", test.Expect)
			}
		})
	}
	verifyProducerCorpusInventory(t, root, listed)
}

func TestEnqueueRejectsInvalidProducerEventsBeforeOutboxWrite(t *testing.T) {
	tests := map[string]func(*testing.T, Identity) Event{
		"semantic mismatch": func(t *testing.T, identity Identity) Event {
			event, err := newObservation(identity, "codex", "original", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			event.Payload = ObservationPayload{Text: "changed without a new id"}
			return event
		},
		"observation session id": func(t *testing.T, identity Identity) Event {
			event, err := newObservation(identity, "codex", "observation", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			event.SessionID = "forbidden"
			return event
		},
		"source locator limit": func(t *testing.T, identity Identity) Event {
			event, err := newObservation(identity, "codex", "observation", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			event.Source.Locator = strings.Repeat("s", MaxSourceLocator+1)
			return event
		},
		"invalid UTF-8 source": func(t *testing.T, identity Identity) Event {
			event, err := newObservation(identity, "codex", "observation", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			event.Source.Locator = "source-\xff"
			return event
		},
		"import turn id": func(t *testing.T, identity Identity) Event {
			return producerImportEvent(t, identity, "memory.md", "content", "forbidden")
		},
		"invalid UTF-8 import path": func(t *testing.T, identity Identity) Event {
			return producerImportEvent(t, identity, "memory-\xff.md", "content", "")
		},
		"import path limit": func(t *testing.T, identity Identity) Event {
			return producerImportEvent(t, identity, strings.Repeat("p", MaxImportPath+1), "content", "")
		},
	}

	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			app := testApp(t)
			identity, err := app.loadIdentity()
			if err != nil {
				t.Fatal(err)
			}
			event := build(t, identity)
			if err := app.enqueue(event); err == nil {
				t.Fatal("invalid producer event reached enqueue")
			}
			entries, err := os.ReadDir(app.Paths.Outbox)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("invalid producer event created %d outbox files", len(entries))
			}
		})
	}
}

func producerImportEvent(t *testing.T, identity Identity, itemPath, content, turnID string) Event {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	itemID, err := importItemID(itemPath, hash)
	if err != nil {
		t.Fatal(err)
	}
	items := []ImportItem{{ID: itemID, Path: itemPath, SHA256: hash, Content: content}}
	eventID, err := importEventID(items)
	if err != nil {
		t.Fatal(err)
	}
	return Event{
		Schema: Protocol, ID: eventID, Kind: "import_batch", CapturedAt: time.Now().UTC(),
		Machine: Machine{ID: identity.ID, Hostname: identity.Hostname}, Client: "import", TurnID: turnID,
		Source:  Source{Kind: "bootstrap", Locator: "fixture"},
		Payload: ImportPayload{Source: "fixture", Items: items},
	}
}

func producerQueuePath(event Event) string {
	return path.Join("events", event.CapturedAt.UTC().Format("2006"), event.CapturedAt.UTC().Format("01"), strings.TrimPrefix(event.ID, "sha256:")+".json")
}

func verifyProducerCorpusInventory(t *testing.T, root string, listed map[string]struct{}) {
	t.Helper()
	expected := make(map[string]struct{}, len(listed)+3)
	for name := range listed {
		expected[name] = struct{}{}
	}
	expected["manifest.json"] = struct{}{}
	expected["source.json"] = struct{}{}
	seen := make(map[string]struct{}, len(expected))
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil || relative == "." {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			switch relative {
			case "positive", "negative":
				return nil
			default:
				t.Fatalf("unexpected corpus directory %s", relative)
			}
		}
		if !entry.Type().IsRegular() {
			t.Fatalf("unexpected non-regular corpus entry %s", relative)
		}
		if _, exists := expected[relative]; !exists {
			t.Fatalf("unexpected corpus file %s", relative)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(expected) {
		for name := range expected {
			if _, exists := seen[name]; !exists {
				t.Fatalf("corpus file is missing: %s", name)
			}
		}
	}
}

func decodeProducerFixture(t *testing.T, content []byte, destination any) {
	t.Helper()
	if err := decodeClosedJSON(content, destination); err != nil {
		t.Fatalf("decode corpus metadata: %v", err)
	}
}

func readProducerFixture(t *testing.T, root, relative string) []byte {
	t.Helper()
	clean := path.Clean(relative)
	if clean != relative || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		t.Fatalf("unsafe corpus path %q", relative)
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func producerFixtureDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
