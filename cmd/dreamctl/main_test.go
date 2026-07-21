package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactNameKeepsTheProducerAttempt(t *testing.T) {
	if got := artifactName("control", "42", 3); got != "hourglass-control-42-3" {
		t.Fatalf("artifact name = %q", got)
	}
}

func TestWriteOutputsIsDeterministic(t *testing.T) {
	output := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", output)
	if err := writeOutputs(map[string]string{"z": "last", "a": "first"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "a=first\nz=last\n" {
		t.Fatalf("workflow outputs = %q", content)
	}
}

func TestWriteOutputsRejectsMultilineValues(t *testing.T) {
	output := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_OUTPUT", output)
	if err := writeOutputs(map[string]string{"unsafe": "one\ntwo"}); err == nil {
		t.Fatal("accepted a multiline workflow output")
	}
}
