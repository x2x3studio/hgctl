package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HGCTL_HOME", home)
	t.Setenv("HGCTL_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("HOURGLASS_VAULT", filepath.Join(home, "vault"))
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func clock() func() time.Time {
	return func() time.Time { return time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC) }
}

// The machine UUID names this machine's queue branch. Minting a second one
// orphans every event already published under the first, so identity must be
// stable across calls and across a hostname change.
func TestIdentityIsStableAndHostnameIsMetadata(t *testing.T) {
	p := testPaths(t)
	first, err := LoadIdentity(p, clock())
	if err != nil {
		t.Fatal(err)
	}
	if !ValidMachineID(first.ID) {
		t.Fatalf("minted an invalid machine id: %q", first.ID)
	}
	second, err := LoadIdentity(p, clock())
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("identity changed across calls: %q -> %q", first.ID, second.ID)
	}

	// Simulate a rename: the hostname is refreshed in place, the id is not.
	stored := first
	stored.Hostname = "some-other-name"
	if err := writeIdentity(p, stored); err != nil {
		t.Fatal(err)
	}
	third, err := LoadIdentity(p, clock())
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != first.ID {
		t.Fatal("a hostname change re-minted the machine id")
	}
	if third.Hostname == "some-other-name" {
		t.Fatal("hostname was not refreshed from the live machine")
	}
}

func writeIdentity(p Paths, id Identity) error {
	f, err := os.CreateTemp(filepath.Dir(p.Identity), "id-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	if err := os.WriteFile(name, []byte(`{"schema_version":1,"id":"`+id.ID+`","hostname":"`+id.Hostname+`"}`), 0o600); err != nil {
		return err
	}
	return os.Rename(name, p.Identity)
}

// A corrupted or hand-edited id must fail loudly: it would otherwise become a
// git ref this endpoint cannot own.
func TestInvalidMachineIDIsRefused(t *testing.T) {
	p := testPaths(t)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Identity, []byte(`{"schema_version":1,"id":"not-a-uuid","hostname":"h"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(p, clock()); err == nil {
		t.Fatal("an invalid machine id was accepted")
	}
}

func TestValidMachineID(t *testing.T) {
	good := "a943c6d2-e7a3-48a4-a562-849aa8fa0560"
	if !ValidMachineID(good) {
		t.Fatalf("%q rejected", good)
	}
	for _, bad := range []string{
		"", "not-a-uuid",
		"A943C6D2-E7A3-48A4-A562-849AA8FA0560", // uppercase would be a different ref
		"a943c6d2e7a348a4a562849aa8fa0560",     // no dashes
		"a943c6d2-e7a3-48a4-a562-849aa8fa056",  // one short
		"a943c6d2-e7a3-48a4-a562-849aa8fa056g", // not hex
		"a943c6d2-e7a3-48a4-a562-849aa8fa0560-",
	} {
		if ValidMachineID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStateAndReceiptRoundTrip(t *testing.T) {
	p := testPaths(t)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uninstalled machine err = %v, want os.ErrNotExist so callers can branch", err)
	}
	want := State{RepoURL: DefaultRepoURL, QueueBranch: "queue/x"}
	if err := SaveState(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoURL != want.RepoURL || got.QueueBranch != want.QueueBranch {
		t.Fatalf("state round trip = %+v", got)
	}
	if got.SchemaVersion != StateSchemaVersion {
		t.Fatalf("schema version not stamped: %d", got.SchemaVersion)
	}

	receipt := IndexReceipt{SharedSHA: "abc123", ProjectExternalID: "pid"}
	if err := SaveIndexReceipt(p, receipt); err != nil {
		t.Fatal(err)
	}
	back, err := LoadIndexReceipt(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.SharedSHA != "abc123" || back.SchemaVersion != IndexReceiptSchemaVersion {
		t.Fatalf("receipt round trip = %+v", back)
	}
}

// The layout is env-overridable so a test - or a second install - can be pointed
// somewhere other than the real machine's data.
func TestDefaultHonoursEnvironmentOverrides(t *testing.T) {
	p := testPaths(t)
	if !strings.HasSuffix(p.Data, "data") {
		t.Fatalf("HGCTL_DATA_DIR ignored: %q", p.Data)
	}
	if !strings.HasSuffix(p.Vault, "vault") {
		t.Fatalf("HOURGLASS_VAULT ignored: %q", p.Vault)
	}
	// Every derived path must live under the data dir, or an override leaks the
	// test's writes into the real machine's state.
	for name, path := range map[string]string{
		"Control": p.Control, "Queue": p.Queue, "Outbox": p.Outbox, "Shared": p.Shared,
		"Identity": p.Identity, "State": p.State, "IndexedSHA": p.IndexedSHA,
		"VaultMirror": p.VaultMirror, "UpdateCheck": p.UpdateCheck,
		"SyncLock": p.SyncLock, "LifecycleLock": p.LifecycleLock, "UpdateLock": p.UpdateLock,
	} {
		if !strings.HasPrefix(path, p.Data) {
			t.Errorf("%s = %q escapes the data dir %q", name, path, p.Data)
		}
	}
}

func TestAssetNameIsPlatformSpecific(t *testing.T) {
	if !strings.HasPrefix(AssetName(), "hgctl_") || strings.Count(AssetName(), "_") != 2 {
		t.Fatalf("asset name = %q, want hgctl_<os>_<arch>", AssetName())
	}
}
