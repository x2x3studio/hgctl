// Package config owns where hgctl keeps things and what it persists there.
//
// It sits above the leaf packages (fsx for atomic writes and schema probing) and
// below every domain, which is why it holds no behaviour beyond load and save:
// a domain package should be able to depend on the SHAPE of persisted state
// without inheriting a dependency on the code that acts on it.
//
// Every persisted file carries a schema_version and is migrated on read. A file
// written by a NEWER hgctl is an error rather than a guess - a release candidate
// runs ProbeCompatibility read-only before it is promoted, so a downgrade is
// caught before it can rewrite state it does not understand.
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"
)

// Fixed identifiers for the endpoint. DefaultRepoURL is the data repository,
// ProjectName the Basic Memory project, LaunchLabel the one scheduler job.
const (
	DefaultRepoURL = "git@github.com:x2x3studio/hourglass.git"
	Protocol       = "hourglass.event/v1"
	ProjectName    = "hourglass"
	LaunchLabel    = "com.x2x3studio.hgctl.sync"

	IdentitySchemaVersion     = 1
	StateSchemaVersion        = 1
	IndexReceiptSchemaVersion = 1

	// ProbeMarker is printed by a candidate binary that read this machine's
	// state successfully; ProbeEnv is what puts it in that mode.
	ProbeMarker = "hgctl-state-compatible/v1"
	ProbeEnv    = "HGCTL_INTERNAL_STATE_PROBE"
)

type Paths struct {
	Home          string
	Data          string
	Control       string
	Queue         string
	Outbox        string
	Vault         string
	Shared        string
	Bin           string
	Versions      string
	Identity      string
	State         string
	IndexedSHA    string
	VaultMirror   string
	UpdateCheck   string
	LifecycleLock string
	SyncLock      string
	UpdateLock    string
}

// Default resolves the layout, honouring HGCTL_HOME, HGCTL_DATA_DIR and
// HOURGLASS_VAULT so a test or a second install can be pointed elsewhere.
func Default() (Paths, error) {
	home := os.Getenv("HGCTL_HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
	}
	data := os.Getenv("HGCTL_DATA_DIR")
	if data == "" {
		data = filepath.Join(home, ".local", "share", "hgctl")
	}
	vault := os.Getenv("HOURGLASS_VAULT")
	if vault == "" {
		vault = filepath.Join(home, "hourglass-vault")
	}
	return Paths{
		Home:          home,
		Data:          data,
		Control:       filepath.Join(data, "repo"),
		Queue:         filepath.Join(data, "queue"),
		Outbox:        filepath.Join(data, "outbox"),
		Vault:         vault,
		Shared:        filepath.Join(data, "shared"),
		Bin:           filepath.Join(home, ".local", "bin"),
		Versions:      filepath.Join(home, ".local", "lib", "hgctl", "versions"),
		Identity:      filepath.Join(data, "identity.json"),
		State:         filepath.Join(data, "state.json"),
		IndexedSHA:    filepath.Join(data, "indexed-shared"),
		VaultMirror:   filepath.Join(data, "vault-mirror.json"),
		UpdateCheck:   filepath.Join(data, "update-check.json"),
		LifecycleLock: filepath.Join(data, "lifecycle.lock"),
		SyncLock:      filepath.Join(data, "sync.lock"),
		UpdateLock:    filepath.Join(data, "update.lock"),
	}, nil
}

type Identity struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type BasicMemoryOwnership struct {
	ExternalID string `json:"external_id"`
	Path       string `json:"path"`
	Managed    bool   `json:"managed"`
	Pending    bool   `json:"pending,omitempty"`
}

type IndexReceipt struct {
	SchemaVersion     int    `json:"schema_version"`
	SharedSHA         string `json:"shared_sha"`
	ProjectExternalID string `json:"project_external_id"`
}

type State struct {
	SchemaVersion      int                   `json:"schema_version"`
	RepoURL            string                `json:"repo_url"`
	QueueBranch        string                `json:"queue_branch"`
	BasicMemoryProject *BasicMemoryOwnership `json:"basic_memory_project,omitempty"`
	BasicMemoryMCP     map[string]string     `json:"basic_memory_mcp,omitempty"`
}

// EnsureDirs creates the directories the endpoint writes into.
func (p Paths) EnsureDirs() error {
	for _, dir := range []string{p.Data, p.Outbox} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// LoadIdentity returns this machine's identity, minting it on first call.
//
// The UUID is app-scoped and permanent: it names this machine's queue branch, so
// re-minting one would orphan every event already published under the old name.
// The hostname is mutable metadata and is refreshed in place when it changes.
func LoadIdentity(p Paths, now func() time.Time) (Identity, error) {
	if err := p.EnsureDirs(); err != nil {
		return Identity{}, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var id Identity
	if err := fsx.ReadJSON(p.Identity, &id); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Identity{}, err
		}
		stamp := now().UTC()
		machineID, err := newUUID()
		if err != nil {
			return Identity{}, err
		}
		id = Identity{SchemaVersion: IdentitySchemaVersion, ID: machineID, Hostname: host, CreatedAt: stamp, UpdatedAt: stamp}
		if err := fsx.WriteJSON(p.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
		return id, nil
	}
	migrated, err := fsx.MigrateSchema(p.Identity, &id.SchemaVersion, IdentitySchemaVersion)
	if err != nil {
		return Identity{}, err
	}
	if !ValidMachineID(id.ID) {
		return Identity{}, errors.New("identity.json has an invalid machine id")
	}
	changed := migrated
	if id.Hostname != host {
		id.Hostname = host
		id.UpdatedAt = now().UTC()
		changed = true
	}
	if changed {
		if err := fsx.WriteJSON(p.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

// LoadState reads install state. A missing file is os.ErrNotExist, which callers
// branch on to mean "not installed".
func LoadState(p Paths) (State, error) {
	var state State
	if err := fsx.ReadJSON(p.State, &state); err != nil {
		return State{}, err
	}
	migrated, err := fsx.MigrateSchema(p.State, &state.SchemaVersion, StateSchemaVersion)
	if err != nil {
		return State{}, err
	}
	if migrated {
		if err := fsx.WriteJSON(p.State, state, 0o600); err != nil {
			return State{}, err
		}
	}
	return state, nil
}

// SaveState writes install state, stamping the current schema version.
func SaveState(p Paths, state State) error {
	state.SchemaVersion = StateSchemaVersion
	return fsx.WriteJSON(p.State, state, 0o600)
}

// LoadIndexReceipt reads which shared revision the local recall mirror holds.
func LoadIndexReceipt(p Paths) (IndexReceipt, error) {
	var receipt IndexReceipt
	if err := fsx.ReadJSON(p.IndexedSHA, &receipt); err != nil {
		return IndexReceipt{}, err
	}
	migrated, err := fsx.MigrateSchema(p.IndexedSHA, &receipt.SchemaVersion, IndexReceiptSchemaVersion)
	if err != nil {
		return IndexReceipt{}, err
	}
	if migrated {
		if err := fsx.WriteJSON(p.IndexedSHA, receipt, 0o600); err != nil {
			return IndexReceipt{}, err
		}
	}
	return receipt, nil
}

// SaveIndexReceipt records which shared revision is indexed.
func SaveIndexReceipt(p Paths, receipt IndexReceipt) error {
	receipt.SchemaVersion = IndexReceiptSchemaVersion
	return fsx.WriteJSON(p.IndexedSHA, receipt, 0o600)
}

// ValidMachineID accepts only the canonical lowercase UUID form. The id names a
// git branch, so anything else would produce a ref this endpoint cannot own.
func ValidMachineID(value string) bool {
	if len(value) != 36 {
		return false
	}
	groups := strings.Split(value, "-")
	want := []int{8, 4, 4, 4, 12}
	if len(groups) != len(want) {
		return false
	}
	for i, g := range groups {
		if len(g) != want[i] {
			return false
		}
		for _, r := range g {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
	}
	return true
}

// AssetName is this machine's release asset, used by the updater.
func AssetName() string {
	return fmt.Sprintf("hgctl_%s_%s", runtime.GOOS, runtime.GOARCH)
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
