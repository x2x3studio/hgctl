package hgctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"

	"github.com/x2x3studio/hgctl/internal/config"
)

func testApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	data := filepath.Join(home, ".local", "share", "hgctl")
	paths := config.Paths{
		Home: home, Data: data,
		Control: filepath.Join(data, "repo"), Queue: filepath.Join(data, "queue"),
		Outbox: filepath.Join(data, "outbox"),
		Vault:  filepath.Join(home, "hourglass-vault"), Shared: filepath.Join(data, "shared"), Bin: filepath.Join(home, ".local", "bin"),
		Versions: filepath.Join(home, ".local", "lib", "hgctl", "versions"),
		Identity: filepath.Join(data, "identity.json"), State: filepath.Join(data, "state.json"),
		IndexedSHA:    filepath.Join(data, "indexed-shared"),
		VaultMirror:   filepath.Join(data, "vault-mirror.json"),
		UpdateCheck:   filepath.Join(data, "update-check.json"),
		LifecycleLock: filepath.Join(data, "lifecycle.lock"),
		SyncLock:      filepath.Join(data, "sync.lock"), UpdateLock: filepath.Join(data, "update.lock"),
	}
	return &App{
		Paths: paths, In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &bytes.Buffer{},
		Now: func() time.Time { return time.Date(2026, 7, 21, 1, 2, 3, 0, time.UTC) },
	}
}

func TestIdentityIsStableAndHostnameIsMetadata(t *testing.T) {
	app := testApp(t)
	first, err := config.LoadIdentity(app.Paths, app.Now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.LoadIdentity(app.Paths, app.Now)
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
	if first.SchemaVersion != config.IdentitySchemaVersion {
		t.Fatalf("identity schema_version=%d", first.SchemaVersion)
	}
}

func TestLegacyPersistedFilesMigrateToCurrentSchemas(t *testing.T) {
	app := testApp(t)
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	legacyIdentity := map[string]any{
		"id": "00000000-0000-4000-8000-000000000000", "hostname": hostname,
		"created_at": created, "updated_at": updated,
	}
	if err := fsx.WriteJSON(app.Paths.Identity, legacyIdentity, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := config.LoadIdentity(app.Paths, app.Now)
	if err != nil {
		t.Fatal(err)
	}
	if identity.SchemaVersion != config.IdentitySchemaVersion || !identity.CreatedAt.Equal(created) || !identity.UpdatedAt.Equal(updated) {
		t.Fatalf("migrated identity=%+v", identity)
	}

	legacyState := map[string]any{
		"repo_url":     "git@github.com:x2x3studio/hourglass.git",
		"queue_branch": "queue/00000000-0000-4000-8000-000000000000",
		"basic_memory_project": map[string]any{
			"external_id": "project-id", "path": app.Paths.Vault, "managed": true,
		},
	}
	if err := fsx.WriteJSON(app.Paths.State, legacyState, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := config.LoadState(app.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != config.StateSchemaVersion || state.BasicMemoryProject == nil || state.BasicMemoryProject.ExternalID != "project-id" {
		t.Fatalf("migrated state=%+v", state)
	}

	checkedAt := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	if err := fsx.WriteJSON(app.Paths.UpdateCheck, map[string]any{"checked_at": checkedAt}, 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := app.loadUpdateCheck()
	if err != nil {
		t.Fatal(err)
	}
	if check.SchemaVersion != updateCheckSchemaVersion || !check.CheckedAt.Equal(checkedAt) {
		t.Fatalf("migrated update check=%+v", check)
	}

	legacyIndex := map[string]any{"shared_sha": strings.Repeat("a", 40), "project_external_id": "project-id"}
	if err := fsx.WriteJSON(app.Paths.IndexedSHA, legacyIndex, 0o600); err != nil {
		t.Fatal(err)
	}
	index, err := config.LoadIndexReceipt(app.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if index.SchemaVersion != config.IndexReceiptSchemaVersion || index.ProjectExternalID != "project-id" {
		t.Fatalf("migrated Basic Memory receipt=%+v", index)
	}

	for _, path := range []string{app.Paths.Identity, app.Paths.State, app.Paths.UpdateCheck, app.Paths.IndexedSHA} {
		var content map[string]any
		if err := fsx.ReadJSON(path, &content); err != nil {
			t.Fatal(err)
		}
		if content["schema_version"] != float64(1) {
			t.Fatalf("%s schema_version=%v", path, content["schema_version"])
		}
	}
}

func TestPersistedFilesRejectFutureSchemas(t *testing.T) {
	tests := []struct {
		name string
		path func(*App) string
		load func(*App) error
	}{
		{"identity", func(app *App) string { return app.Paths.Identity }, func(app *App) error { _, err := config.LoadIdentity(app.Paths, app.Now); return err }},
		{"state", func(app *App) string { return app.Paths.State }, func(app *App) error { _, err := config.LoadState(app.Paths); return err }},
		{"update", func(app *App) string { return app.Paths.UpdateCheck }, func(app *App) error { _, err := app.loadUpdateCheck(); return err }},
		{"basic-memory", func(app *App) string { return app.Paths.IndexedSHA }, func(app *App) error { _, err := config.LoadIndexReceipt(app.Paths); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := testApp(t)
			if err := fsx.WriteJSON(test.path(app), map[string]any{"schema_version": 2}, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.load(app); err == nil || !strings.Contains(err.Error(), "unsupported schema_version 2") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestLaunchAgentUsesOneStableLabelAndPath(t *testing.T) {
	body := launchAgent("/Users/test/.local/bin/hgctl", "/Users/test/.local/share/hgctl", "/Users/test", "/Users/test/hourglass-vault")
	if got := strings.Count(body, config.LaunchLabel); got != 1 {
		t.Fatalf("label occurs %d times", got)
	}
	if !strings.Contains(body, "/Users/test/.local/bin/hgctl") {
		t.Fatal("stable symlink is not the program path")
	}
	if strings.Contains(body, "/versions/") {
		t.Fatal("version-specific path leaked into LaunchAgent")
	}
	if !strings.Contains(body, schedulerOwnership) {
		t.Fatal("versioned scheduler ownership is missing")
	}
	if strings.Contains(body, "<key>HGCTLOwnership</key>") || !strings.Contains(body, "<key>HGCTL_SCHEDULER_OWNERSHIP</key>") {
		t.Fatal("LaunchAgent ownership is not stored in the supported environment dictionary")
	}
	if strings.Contains(body, "sync.log") || strings.Contains(body, "sync.err.log") || strings.Count(body, "<string>/dev/null</string>") != 2 {
		t.Fatal("LaunchAgent output is not bounded by /dev/null")
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
	if err := fsx.WriteAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteAtomic(path, []byte("two"), 0o600); err != nil {
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
		{"v1.0.0-alpha", "v1.0.0-alpha.1", true},
		{"v1.0.0-alpha.1", "v1.0.0-alpha.beta", true},
		{"v1.0.0-beta.2", "v1.0.0-beta.11", true},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", true},
		{"v1.0.0+build.1", "v1.0.0+build.2", false},
	}
	for _, test := range tests {
		got, err := versionIsNewer(test.current, test.candidate)
		if err != nil || got != test.want {
			t.Fatalf("versionIsNewer(%q,%q)=(%v,%v), want %v", test.current, test.candidate, got, err, test.want)
		}
	}
}

func TestSemanticVersionRejectsMalformedIdentifiers(t *testing.T) {
	for _, value := range []string{"1.0", "1.0.0-", "1.0.0-alpha..1", "1.0.0-01", "1.0.0+", "1.0.0+build..1", "1.0.0-alpha_1"} {
		if _, err := parseSemanticVersion(value); err == nil {
			t.Fatalf("accepted malformed version %q", value)
		}
	}
}

func TestReleaseInstallRequiresCandidateVersionToMatchTag(t *testing.T) {
	app := testApp(t)
	oldTarget := filepath.Join(app.Paths.Versions, "0.1.0", "hgctl")
	if err := os.MkdirAll(filepath.Dir(oldTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldTarget, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(app.Paths.Bin, "hgctl")
	if err := replaceStableSymlink(stable, oldTarget); err != nil {
		t.Fatal(err)
	}

	wrong := []byte("#!/bin/sh\nprintf '" + config.ProbeMarker + "\\nv9.9.9\\n'\n")
	if err := app.installReleaseBinary(testContext(t), "v0.2.0", wrong); err == nil || !strings.Contains(err.Error(), "expected exact tag") {
		t.Fatalf("got %v", err)
	}
	if target, err := os.Readlink(stable); err != nil || target != oldTarget {
		t.Fatalf("stable link changed after rejected candidate: target=%q err=%v", target, err)
	}
	if _, err := os.Stat(filepath.Join(app.Paths.Versions, "0.2.0", "hgctl")); !os.IsNotExist(err) {
		t.Fatalf("rejected candidate was promoted: %v", err)
	}

	matching := []byte("#!/bin/sh\nprintf '" + config.ProbeMarker + "\\nv0.2.0\\n'\n")
	if err := app.installReleaseBinary(testContext(t), "v0.2.0", matching); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(app.Paths.Versions, "0.2.0", "hgctl")
	if target, err := os.Readlink(stable); err != nil || target != want {
		t.Fatalf("stable link target=%q err=%v, want %q", target, err, want)
	}
}

func TestPersistedStateCompatibilityProbeIsReadOnly(t *testing.T) {
	app := testApp(t)
	content := []byte("{\n  \"schema_version\": 2,\n  \"repo_url\": \"private\"\n}\n")
	if err := fsx.WriteAtomic(app.Paths.State, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.probePersistedStateCompatibility(); err == nil || !errors.Is(err, fsx.ErrUnsupportedSchema) {
		t.Fatalf("future schema probe error=%v", err)
	}
	after, err := os.ReadFile(app.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, content) {
		t.Fatalf("compatibility probe rewrote state:\n%s", after)
	}
}

func TestCandidateMustImplementOfflineStateProbe(t *testing.T) {
	app := testApp(t)
	legacyCandidate := []byte("#!/bin/sh\nprintf 'v0.2.0\\n'\n")
	if err := app.installReleaseBinary(testContext(t), "v0.2.0", legacyCandidate); err == nil || !strings.Contains(err.Error(), "compatibility marker") {
		t.Fatalf("candidate without state probe was accepted: %v", err)
	}
}

func TestRepeatedReleaseInstallDoesNotOverwriteTheActiveTarget(t *testing.T) {
	app := testApp(t)
	target := filepath.Join(app.Paths.Versions, "0.2.0", "hgctl")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("#!/bin/sh\nprintf '" + config.ProbeMarker + "\\nv0.2.0\\n'\n")
	if err := os.WriteFile(target, content, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.Paths.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceStableSymlink(filepath.Join(app.Paths.Bin, "hgctl"), target); err != nil {
		t.Fatal(err)
	}
	if err := app.installReleaseBinary(testContext(t), "v0.2.0", content); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o701 {
		t.Fatalf("same-target install replaced the active inode: mode=%o", info.Mode().Perm())
	}

	different := append(append([]byte(nil), content...), []byte("# different build\n")...)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.installReleaseBinary(testContext(t), "v0.2.0", different); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("same-target content collision was accepted: %v", err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("same-target collision overwrote the active binary")
	}
}

func TestManagedVersionPruningRetainsCurrentAndOneRollback(t *testing.T) {
	root := t.TempDir()
	for _, version := range []string{"0.1.0", "0.2.0", "0.3.0"} {
		path := filepath.Join(root, version, "hgctl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(version), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unmanaged := filepath.Join(root, "notes", "hgctl")
	if err := os.MkdirAll(filepath.Dir(unmanaged), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unmanaged, []byte("user"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pruneManagedVersions(root, filepath.Join(root, "0.3.0", "hgctl"), filepath.Join(root, "0.2.0", "hgctl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "0.1.0")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old managed version remains: %v", err)
	}
	for _, path := range []string{filepath.Join(root, "0.2.0", "hgctl"), filepath.Join(root, "0.3.0", "hgctl"), unmanaged} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained path %s: %v", path, err)
		}
	}
}

func TestUpdateReceiptDoesNotRewriteInstallState(t *testing.T) {
	app := testApp(t)
	state := config.State{
		RepoURL: "git@github.com:x2x3studio/hourglass.git", QueueBranch: "queue/00000000-0000-4000-8000-000000000000",
		BasicMemoryProject: &config.BasicMemoryOwnership{ExternalID: "project-id", Path: app.Paths.Vault, Managed: true},
	}
	if err := config.SaveState(app.Paths, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(app.Paths.State)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"","assets":[]}`))
	}))
	defer server.Close()
	withReleaseLatestURL(t, server.URL)
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
	if err := fsx.ReadJSON(app.Paths.UpdateCheck, &receipt); err != nil || !receipt.CheckedAt.Equal(app.Now()) {
		t.Fatalf("independent update receipt=%+v err=%v", receipt, err)
	}
}

func TestReadLimitedRejectsOversizeResponse(t *testing.T) {
	if _, err := readLimited(strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversize response was accepted")
	}
}

func withReleaseLatestURL(t *testing.T, url string) {
	t.Helper()
	previous := releaseLatestURL
	releaseLatestURL = url
	t.Cleanup(func() { releaseLatestURL = previous })
}

func TestLatestReleaseParsesAssetsAndSendsHeaders(t *testing.T) {
	var accept, agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		agent = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.0","assets":[{"name":"hgctl_linux_amd64","browser_download_url":"https://cdn.example/hgctl_linux_amd64","url":"https://api.example/ignored"}]}`))
	}))
	defer server.Close()
	withReleaseLatestURL(t, server.URL)

	rel, err := latestRelease(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if rel.TagName != "v0.3.0" || len(rel.Assets) != 1 {
		t.Fatalf("parsed release = %+v", rel)
	}
	if rel.Assets[0].Name != "hgctl_linux_amd64" || rel.Assets[0].URL != "https://cdn.example/hgctl_linux_amd64" {
		t.Fatalf("asset = %+v, want browser_download_url", rel.Assets[0])
	}
	if accept != "application/vnd.github+json" {
		t.Fatalf("Accept = %q", accept)
	}
	if agent == "" {
		t.Fatal("GitHub API requires a non-empty User-Agent")
	}
}

func TestUpdateInstallsReleaseOverHTTP(t *testing.T) {
	app := testApp(t)
	tag := "v0.3.0"
	assetName := config.AssetName()
	binary := []byte("#!/bin/sh\nprintf '" + config.ProbeMarker + "\\n" + tag + "\\n'\n")
	sum := sha256.Sum256(binary)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"

	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("release request missing User-Agent")
		}
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
			tag, assetName, base+"/download/"+assetName, base+"/download/checksums.txt")
	})
	// Redirect the binary to a distinct path to prove net/http follows the CDN hop.
	mux.HandleFunc("/download/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, base+"/cdn/"+assetName, http.StatusFound)
	})
	mux.HandleFunc("/cdn/"+assetName, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(checksums)) })
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	withReleaseLatestURL(t, server.URL+"/releases/latest")

	if err := app.update(testContext(t), true); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(app.Paths.Bin, "hgctl"))
	if err != nil {
		t.Fatalf("stable symlink not created: %v", err)
	}
	if !strings.Contains(target, filepath.Join("versions", "0.3.0", "hgctl")) {
		t.Fatalf("symlink target = %q, want the 0.3.0 version dir", target)
	}
}

func TestUpdateRejectsChecksumMismatch(t *testing.T) {
	app := testApp(t)
	tag := "v0.3.0"
	assetName := config.AssetName()
	binary := []byte("#!/bin/sh\nprintf '" + config.ProbeMarker + "\\n" + tag + "\\n'\n")
	wrongChecksums := strings.Repeat("0", 64) + "  " + assetName + "\n"

	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
			tag, assetName, base+"/download/"+assetName, base+"/download/checksums.txt")
	})
	mux.HandleFunc("/download/"+assetName, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/download/checksums.txt", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(wrongChecksums)) })
	server := httptest.NewServer(mux)
	defer server.Close()
	base = server.URL
	withReleaseLatestURL(t, server.URL+"/releases/latest")

	if err := app.update(testContext(t), true); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("update err = %v, want checksum mismatch", err)
	}
	if _, err := os.Lstat(filepath.Join(app.Paths.Bin, "hgctl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a mismatched download must not install a binary: %v", err)
	}
}
