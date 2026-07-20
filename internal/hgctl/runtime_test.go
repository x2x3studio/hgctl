package hgctl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "hgctl")
	paths := Paths{
		Home: home, Data: data,
		Control: filepath.Join(data, "repo"), Queue: filepath.Join(data, "queue"),
		Outbox: filepath.Join(data, "outbox"), Quarantine: filepath.Join(data, "quarantine"), Pending: filepath.Join(data, "pending"),
		Vault: filepath.Join(home, "hourglass-vault"), Bin: filepath.Join(home, ".local", "bin"),
		Versions: filepath.Join(home, ".local", "lib", "hgctl", "versions"),
		Identity: filepath.Join(data, "identity.json"), State: filepath.Join(data, "state.json"),
		Delivered:     filepath.Join(data, "delivered"),
		IndexedSHA:    filepath.Join(data, "indexed-shared"),
		UpdateCheck:   filepath.Join(data, "update-check.json"),
		LifecycleLock: filepath.Join(data, "lifecycle.lock"),
		SyncLock:      filepath.Join(data, "sync.lock"), UpdateLock: filepath.Join(data, "update.lock"),
		CodexLock: filepath.Join(data, "codex-trust.lock"), CodexCheck: filepath.Join(data, "codex-trust-check.json"),
	}
	return &App{
		Paths: paths, In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{},
		Now: func() time.Time { return time.Date(2026, 7, 21, 1, 2, 3, 0, time.UTC) },
	}
}

func TestIdentityIsStableAndHostnameIsMetadata(t *testing.T) {
	app := testApp(t)
	first, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.loadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("machine ID changed: %q != %q", first.ID, second.ID)
	}
	if len(first.ID) != 36 || first.ID[14] != '4' {
		t.Fatalf("not a UUID v4: %q", first.ID)
	}
	if first.Hostname == "" {
		t.Fatal("hostname metadata is empty")
	}
}

func TestLaunchAgentUsesOneStableLabelAndPath(t *testing.T) {
	body := launchAgent("/Users/test/.local/bin/hgctl", "/Users/test/.local/share/hgctl", "/Users/test", "/Users/test/hourglass-vault")
	if got := strings.Count(body, LaunchLabel); got != 1 {
		t.Fatalf("label occurs %d times", got)
	}
	if !strings.Contains(body, "/Users/test/.local/bin/hgctl") {
		t.Fatal("stable symlink is not the program path")
	}
	if strings.Contains(body, "/versions/") {
		t.Fatal("version-specific path leaked into LaunchAgent")
	}
	if !strings.Contains(body, "/opt/homebrew/bin") {
		t.Fatal("LaunchAgent cannot discover Homebrew gh")
	}
	for _, value := range []string{"HGCTL_HOME", "HGCTL_DATA_DIR", "HOURGLASS_VAULT"} {
		if !strings.Contains(body, value) {
			t.Fatalf("LaunchAgent is missing %s", value)
		}
	}
}

func TestWriteFileAtomicReplacesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state")
	if err := writeFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "two" {
		t.Fatalf("got %q", b)
	}
}

func TestChecksumFor(t *testing.T) {
	hash := strings.Repeat("a", 64)
	got, err := checksumFor(hash+"  hgctl_darwin_arm64\n", "hgctl_darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Fatalf("got %q", got)
	}
}

func TestVersionUpdateIsNewerOnly(t *testing.T) {
	tests := []struct {
		current, candidate string
		want               bool
	}{
		{"dev", "v0.1.0", true},
		{"v0.1.0", "v0.1.1", true},
		{"v0.2.0", "v0.1.9", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0-rc.1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0-rc.2", false},
	}
	for _, test := range tests {
		got, err := versionIsNewer(test.current, test.candidate)
		if err != nil || got != test.want {
			t.Fatalf("versionIsNewer(%q,%q)=(%v,%v), want %v", test.current, test.candidate, got, err, test.want)
		}
	}
}

func TestUpdateReceiptDoesNotRewriteInstallState(t *testing.T) {
	app := testApp(t)
	state := State{
		RepoURL: "git@github.com:x2x3studio/hourglass.git", QueueBranch: "queue/00000000-0000-4000-8000-000000000000",
		BasicMemoryProject: &BasicMemoryOwnership{ExternalID: "project-id", Path: app.Paths.Vault, Managed: true},
	}
	if err := app.saveState(state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(app.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(app.Paths.Home, "fake-bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '{\"tag_name\":\"\",\"assets\":[]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := app.update(testContext(t), true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(app.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("update rewrote install state:\nbefore=%s\nafter=%s", before, after)
	}
	var receipt updateCheck
	if err := readJSON(app.Paths.UpdateCheck, &receipt); err != nil || !receipt.CheckedAt.Equal(app.Now()) {
		t.Fatalf("independent update receipt=%+v err=%v", receipt, err)
	}
}

func TestReadLimitedRejectsOversizeResponse(t *testing.T) {
	if _, err := readLimited(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversize response was accepted")
	}
}

func TestOptionsMayFollowPositionalArguments(t *testing.T) {
	client, rest, err := extractOption([]string{"repo-name", "--client", "codex"}, "--client", "unknown")
	if err != nil {
		t.Fatal(err)
	}
	if client != "codex" || len(rest) != 1 || rest[0] != "repo-name" {
		t.Fatalf("client=%q rest=%v", client, rest)
	}
}
