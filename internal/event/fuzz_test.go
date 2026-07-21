package event

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func FuzzDecodeCanonical(f *testing.F) {
	root := corpusRootForFuzz(f)
	manifest := loadCorpusManifestForFuzz(f, root)
	for _, test := range manifest.Cases {
		content, err := readFixtureForFuzz(root, test.File)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(content, test.Machine, test.Path)
	}
	f.Add([]byte(`{"schema":"hourglass.event/v1"}`+"\n"), fixtureMachine, "events/2026/07/invalid.json")
	f.Add([]byte{0xff, 0xfe, 0xfd}, fixtureMachine, "")

	f.Fuzz(func(t *testing.T, content []byte, machine, path string) {
		event, err := DecodeCanonical(content, Binding{MachineID: machine, Path: path})
		if err == nil {
			if event.Schema != Schema || event.ID == "" || event.Kind == "" {
				t.Fatalf("accepted incomplete event: %#v", event)
			}
			return
		}
		var invalidEvent *InvalidEventError
		if !errors.As(err, &invalidEvent) {
			t.Fatalf("decoder leaked an untyped error %T: %v", err, err)
		}
	})
}

func corpusRootForFuzz(f *testing.F) string {
	f.Helper()
	return "../hgctl/testdata/protocol/event"
}

func loadCorpusManifestForFuzz(f *testing.F, root string) corpusManifest {
	f.Helper()
	content, err := readFixtureForFuzz(root, "manifest.json")
	if err != nil {
		f.Fatal(err)
	}
	var manifest corpusManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		f.Fatal(err)
	}
	return manifest
}

func readFixtureForFuzz(root, relative string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
}
