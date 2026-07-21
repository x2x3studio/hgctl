package event

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const fixtureMachine = "00000000-0000-4000-8000-000000000000"

type corpusManifest struct {
	Schema       string       `json:"schema"`
	Protocol     string       `json:"protocol"`
	SourceFile   string       `json:"source_file"`
	SourceSHA256 string       `json:"source_sha256"`
	Cases        []corpusCase `json:"cases"`
}

type corpusCase struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Machine string `json:"machine"`
	Path    string `json:"path"`
	Expect  string `json:"expect"`
	Kind    string `json:"kind,omitempty"`
	EventID string `json:"event_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

type corpusSource struct {
	Schema    string `json:"schema"`
	Protocol  string `json:"protocol"`
	Authority string `json:"authority"`
	Encoding  string `json:"encoding"`
	Purpose   string `json:"purpose"`
}

func TestConformanceCorpus(t *testing.T) {
	root := corpusRoot(t)
	manifest := loadCorpusManifest(t, root)
	if manifest.Schema != "hourglass.conformance-manifest/v1" || manifest.Protocol != Schema {
		t.Fatalf("unexpected manifest identity: %#v", manifest)
	}
	if len(manifest.Cases) < 15 {
		t.Fatalf("conformance corpus has only %d cases", len(manifest.Cases))
	}

	sourceContent := readFixture(t, root, manifest.SourceFile)
	if digest(sourceContent) != manifest.SourceSHA256 {
		t.Fatal("source metadata hash does not match manifest")
	}
	var source corpusSource
	decodeClosedFixtureJSON(t, sourceContent, &source)
	if source.Schema != "hourglass.conformance-source/v1" || source.Protocol != Schema ||
		source.Authority != "protocol/event.md" || source.Encoding == "" || source.Purpose == "" {
		t.Fatalf("invalid corpus source metadata: %#v", source)
	}
	listed := make(map[string]struct{}, len(manifest.Cases))
	for _, test := range manifest.Cases {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			if test.Name == "" || test.File == "" || test.Reason == "" && test.Expect != "valid" {
				t.Fatal("corpus case metadata is incomplete")
			}
			if _, duplicate := listed[test.File]; duplicate {
				// Identical raw bytes may appear under separate paths, but each path
				// must still have one unambiguous expectation.
				t.Fatalf("duplicate corpus path %s", test.File)
			}
			listed[test.File] = struct{}{}
			content := readFixture(t, root, test.File)
			if got := digest(content); got != test.SHA256 {
				t.Fatalf("fixture hash=%s, want %s", got, test.SHA256)
			}
			event, err := DecodeCanonical(content, Binding{MachineID: test.Machine, Path: test.Path})
			switch test.Expect {
			case "valid":
				if !semanticKind(test.Kind) {
					var invalidEvent *InvalidEventError
					if !errors.As(err, &invalidEvent) {
						t.Fatalf("out-of-scope kind returned %T: %v", err, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("valid fixture rejected: %v", err)
				}
				if event.ID != test.EventID || string(event.Kind) != test.Kind {
					t.Fatalf("decoded event=%s/%s, want %s/%s", event.ID, event.Kind, test.EventID, test.Kind)
				}
			case "invalid":
				var invalidEvent *InvalidEventError
				if !errors.As(err, &invalidEvent) {
					t.Fatalf("invalid fixture returned %T: %v", err, err)
				}
			default:
				t.Fatalf("unknown corpus expectation %q", test.Expect)
			}
		})
	}

	var rawFiles []string
	for _, directory := range []string{"positive", "negative"} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
				t.Fatalf("unexpected corpus entry %s/%s", directory, entry.Name())
			}
			rawFiles = append(rawFiles, filepath.ToSlash(filepath.Join(directory, entry.Name())))
		}
	}
	sort.Strings(rawFiles)
	if len(rawFiles) != len(listed) {
		t.Fatalf("manifest lists %d raw fixtures, disk has %d", len(listed), len(rawFiles))
	}
	for _, path := range rawFiles {
		if _, ok := listed[path]; !ok {
			t.Fatalf("raw fixture is missing from manifest: %s", path)
		}
	}
}

func TestSemanticIDWireContract(t *testing.T) {
	observation, err := observationID(fixtureMachine, "codex", ObservationPayload{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:216ed82b4fb25119a592db77f2f14b9392a679e398a1db29a77c4ec42b26dd7b"; observation != want {
		t.Fatalf("observation id=%s, want %s", observation, want)
	}

	turn, err := turnID(fixtureMachine, "claude", "s", "", TurnPayload{Prompt: "p", Response: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:79426bfe2f11ff2813a8da2a4710ba14d76096f5c83916b6f43759e4fa2a019c"; turn != want {
		t.Fatalf("turn id=%s, want %s", turn, want)
	}

	item, err := importItemID("memory/a.md", "2be23c585f15e5fd3279d0663036dd9f6e634f4225ef326fc83fb874dbb81a0f")
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:99992a5adeec4b58021c7679bc21b9141949dfa98e417c298cd286684e015c01"; item != want {
		t.Fatalf("import item id=%s, want %s", item, want)
	}

	batch, err := importBatchID([]ImportItem{{
		ID: item, Path: "memory/a.md",
		SHA256:  "2be23c585f15e5fd3279d0663036dd9f6e634f4225ef326fc83fb874dbb81a0f",
		Content: "why",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "sha256:5f4c3064d1cc7776a11025b32aa014ec2d380ec6b5b62016038493eca5cb945f"; batch != want {
		t.Fatalf("import batch id=%s, want %s", batch, want)
	}
}

func TestTextAndEnvelopeLimits(t *testing.T) {
	valid := canonicalObservation(t, strings.Repeat("x", MaxTextBytes), "2026-07-21T00:00:00Z")
	if _, err := DecodeCanonical(valid.content, valid.binding); err != nil {
		t.Fatalf("maximum observation text was rejected: %v", err)
	}

	tooLong := canonicalObservation(t, strings.Repeat("x", MaxTextBytes+1), "2026-07-21T00:00:00Z")
	assertInvalid(t, tooLong.content, tooLong.binding)

	oversized := make([]byte, MaxEventBytes+1)
	copy(oversized, []byte(`{"schema":"hourglass.event/v1"}`))
	assertInvalid(t, oversized, valid.binding)

	badUTF8 := append([]byte(nil), valid.content...)
	index := strings.Index(string(badUTF8), strings.Repeat("x", 8))
	if index < 0 {
		t.Fatal("fixture text not found")
	}
	badUTF8[index] = 0xff
	if utf8.Valid(badUTF8) {
		t.Fatal("test did not create invalid UTF-8")
	}
	assertInvalid(t, badUTF8, valid.binding)
}

func TestGoJSONEscapingIsCanonical(t *testing.T) {
	fixture := canonicalObservation(t, "<&>\u2028\u2029", "2026-07-21T00:00:00Z")
	for _, escaped := range [][]byte{[]byte(`\u003c`), []byte(`\u0026`), []byte(`\u003e`), []byte(`\u2028`), []byte(`\u2029`)} {
		if !bytes.Contains(fixture.content, escaped) {
			t.Fatalf("canonical event does not contain %s", escaped)
		}
	}
	if _, err := DecodeCanonical(fixture.content, fixture.binding); err != nil {
		t.Fatalf("canonical Go escaping was rejected: %v", err)
	}
	noncanonical := bytes.Replace(fixture.content, []byte(`\u003c`), []byte("<"), 1)
	assertInvalid(t, noncanonical, fixture.binding)
}

func TestImportAggregateLimit(t *testing.T) {
	items := make([]ImportItem, 0, 7)
	for index := 0; index < 7; index++ {
		content := strings.Repeat(string(rune('a'+index)), MaxImportText)
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		path := fmt.Sprintf("memory/%d.md", index)
		id, err := importItemID(path, hash)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, ImportItem{ID: id, Path: path, SHA256: hash, Content: content})
	}
	payload := ImportPayload{Source: "fixture", Items: items}
	id, err := importBatchID(items)
	if err != nil {
		t.Fatal(err)
	}
	content := canonicalWire(t, wireEvent{
		Schema: Schema, ID: id, Kind: string(KindImportBatch), CapturedAt: "2026-07-21T00:00:00Z",
		Machine: Machine{ID: fixtureMachine, Hostname: "fixture-host"}, Client: "import",
		Source: Source{Kind: "bootstrap", Locator: "fixture"}, Payload: mustJSON(t, payload),
	})
	binding := Binding{MachineID: fixtureMachine, Path: "events/2026/07/" + strings.TrimPrefix(id, "sha256:") + ".json"}
	assertInvalid(t, content, binding)
}

func TestUnsupportedSchemasAreTerminal(t *testing.T) {
	binding := Binding{MachineID: fixtureMachine, Path: "events/2026/07/" + strings.Repeat("0", 64) + ".json"}
	for _, schema := range []string{"hourglass.event/unsupported", "hourglass.event/vx", "other.event/v1"} {
		content := []byte(fmt.Sprintf("{\"schema\":%q}\n", schema))
		assertInvalid(t, content, binding)
	}

	duplicate := []byte("{\"schema\":\"hourglass.event/v1\",\"schema\":\"hourglass.event/unsupported\"}\n")
	assertInvalid(t, duplicate, binding)
}

func TestPresentOptionalEnvelopeIDsCannotBeEmpty(t *testing.T) {
	root := corpusRoot(t)
	manifest := loadCorpusManifest(t, root)
	var turnCase corpusCase
	for _, test := range manifest.Cases {
		if test.File == "positive/turn.json" {
			turnCase = test
			break
		}
	}
	if turnCase.File == "" {
		t.Fatal("turn fixture is missing")
	}
	binding := Binding{MachineID: turnCase.Machine, Path: turnCase.Path}
	content := string(readFixture(t, root, turnCase.File))
	emptySession := strings.Replace(content, `"session_id":"s"`, `"session_id":""`, 1)
	assertInvalid(t, []byte(emptySession), binding)
	emptyTurn := strings.Replace(content, `"session_id":"s"`, `"session_id":"s","turn_id":""`, 1)
	assertInvalid(t, []byte(emptyTurn), binding)
}

func TestDecodedPayloadIsTyped(t *testing.T) {
	manifest := loadCorpusManifest(t, corpusRoot(t))
	for _, test := range manifest.Cases {
		if test.Expect != "valid" || !semanticKind(test.Kind) {
			continue
		}
		event, err := DecodeCanonical(readFixture(t, corpusRoot(t), test.File), Binding{MachineID: test.Machine, Path: test.Path})
		if err != nil {
			t.Fatal(err)
		}
		set := 0
		for _, present := range []bool{event.Observation != nil, event.Turn != nil, event.ImportBatch != nil} {
			if present {
				set++
			}
		}
		if set != 1 {
			t.Fatalf("event %s has %d typed payloads", event.ID, set)
		}
	}
}

func semanticKind(kind string) bool {
	return kind == string(KindObservation) || kind == string(KindTurn) || kind == string(KindImportBatch)
}

type observationFixture struct {
	content []byte
	binding Binding
}

func canonicalObservation(t *testing.T, text, capturedAt string) observationFixture {
	t.Helper()
	payload := ObservationPayload{Text: text}
	id, err := observationID(fixtureMachine, "codex", payload)
	if err != nil {
		t.Fatal(err)
	}
	content := canonicalWire(t, wireEvent{
		Schema: Schema, ID: id, Kind: string(KindObservation), CapturedAt: capturedAt,
		Machine: Machine{ID: fixtureMachine, Hostname: "fixture-host"}, Client: "codex",
		Source: Source{Kind: "explicit", Locator: "stdin"}, Payload: mustJSON(t, payload),
	})
	parsed, err := time.Parse(time.RFC3339Nano, capturedAt)
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{
		MachineID: fixtureMachine,
		Path:      fmt.Sprintf("events/%04d/%02d/%s.json", parsed.UTC().Year(), int(parsed.UTC().Month()), strings.TrimPrefix(id, "sha256:")),
	}
	return observationFixture{content: content, binding: binding}
}

func canonicalWire(t *testing.T, wire wireEvent) []byte {
	t.Helper()
	content, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func assertInvalid(t *testing.T, content []byte, binding Binding) {
	t.Helper()
	_, err := DecodeCanonical(content, binding)
	var invalidEvent *InvalidEventError
	if !errors.As(err, &invalidEvent) {
		t.Fatalf("expected InvalidEventError, got %T: %v", err, err)
	}
}

func corpusRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "hgctl", "testdata", "protocol", "event")
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); err != nil {
		t.Fatalf("locate conformance corpus: %v", err)
	}
	return root
}

func loadCorpusManifest(t *testing.T, root string) corpusManifest {
	t.Helper()
	var manifest corpusManifest
	if err := json.Unmarshal(readFixture(t, root, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func decodeClosedFixtureJSON(t *testing.T, content []byte, dst any) {
	t.Helper()
	if err := decodeClosedJSON(content, dst); err != nil {
		t.Fatalf("decode fixture metadata: %v", err)
	}
}

func readFixture(t *testing.T, root, relative string) []byte {
	t.Helper()
	clean := filepath.ToSlash(filepath.Clean(relative))
	if clean != relative || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		t.Fatalf("unsafe fixture path %q", relative)
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
