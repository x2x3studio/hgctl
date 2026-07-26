package hgctl

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/x2x3studio/hgctl/internal/fsx"
)

const (
	DefaultRepoURL = "git@github.com:x2x3studio/hourglass.git"
	Protocol       = "hourglass.event/v1"
	ProjectName    = "hourglass"
	LaunchLabel    = "com.x2x3studio.hgctl.sync"

	identitySchemaVersion                = 1
	stateSchemaVersion                   = 1
	basicMemoryIndexReceiptSchemaVersion = 1
	stateProbeMarker                     = "hgctl-state-compatible/v1"
	stateProbeEnvironment                = "HGCTL_INTERNAL_STATE_PROBE"
)

var Version = "v0.2.0"

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

func DefaultPaths() (Paths, error) {
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

type BasicMemoryIndexReceipt struct {
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

type App struct {
	Paths Paths
	In    io.Reader
	Out   io.Writer
	Err   io.Writer
	Now   func() time.Time
}

func New(in io.Reader, out, errOut io.Writer) (*App, error) {
	paths, err := DefaultPaths()
	if err != nil {
		return nil, err
	}
	app := &App{Paths: paths, In: in, Out: out, Err: errOut, Now: time.Now}
	if os.Getenv(stateProbeEnvironment) == "1" {
		if err := app.probePersistedStateCompatibility(); err != nil {
			return nil, fmt.Errorf("persisted state compatibility: %w", err)
		}
		if _, err := fmt.Fprintln(out, stateProbeMarker); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// probePersistedStateCompatibility is deliberately read-only. A candidate
// binary runs it before promotion so an older release cannot rewrite a newer
// persisted schema while merely checking compatibility.
func (a *App) probePersistedStateCompatibility() error {
	var identity Identity
	if exists, err := fsx.ProbeSchema(a.Paths.Identity, &identity, &identity.SchemaVersion, identitySchemaVersion); err != nil {
		return err
	} else if exists && !validMachineID(identity.ID) {
		return errors.New("identity.json has an invalid machine id")
	}

	var state State
	if _, err := fsx.ProbeSchema(a.Paths.State, &state, &state.SchemaVersion, stateSchemaVersion); err != nil {
		return err
	}
	var receipt BasicMemoryIndexReceipt
	if _, err := fsx.ProbeSchema(a.Paths.IndexedSHA, &receipt, &receipt.SchemaVersion, basicMemoryIndexReceiptSchemaVersion); err != nil {
		return err
	}
	var check updateCheck
	if _, err := fsx.ProbeSchema(a.Paths.UpdateCheck, &check, &check.SchemaVersion, updateCheckSchemaVersion); err != nil {
		return err
	}
	return nil
}

func (a *App) ensureDataDirs() error {
	for _, path := range []string{a.Paths.Data, a.Paths.Outbox} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) loadIdentity() (Identity, error) {
	if err := a.ensureDataDirs(); err != nil {
		return Identity{}, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var id Identity
	if err := fsx.ReadJSON(a.Paths.Identity, &id); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Identity{}, err
		}
		now := a.Now().UTC()
		machineID, err := newUUID()
		if err != nil {
			return Identity{}, err
		}
		id = Identity{SchemaVersion: identitySchemaVersion, ID: machineID, Hostname: host, CreatedAt: now, UpdatedAt: now}
		if err := fsx.WriteJSON(a.Paths.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
		return id, nil
	}
	migrated, err := fsx.MigrateSchema(a.Paths.Identity, &id.SchemaVersion, identitySchemaVersion)
	if err != nil {
		return Identity{}, err
	}
	if !validMachineID(id.ID) {
		return Identity{}, errors.New("identity.json has an invalid machine id")
	}
	changed := migrated
	if id.Hostname != host {
		id.Hostname = host
		id.UpdatedAt = a.Now().UTC()
		changed = true
	}
	if changed {
		if err := fsx.WriteJSON(a.Paths.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func (a *App) loadState() (State, error) {
	var state State
	if err := fsx.ReadJSON(a.Paths.State, &state); err != nil {
		return State{}, err
	}
	migrated, err := fsx.MigrateSchema(a.Paths.State, &state.SchemaVersion, stateSchemaVersion)
	if err != nil {
		return State{}, err
	}
	if migrated {
		if err := fsx.WriteJSON(a.Paths.State, state, 0o600); err != nil {
			return State{}, err
		}
	}
	return state, nil
}

func (a *App) saveState(state State) error {
	state.SchemaVersion = stateSchemaVersion
	return fsx.WriteJSON(a.Paths.State, state, 0o600)
}

func (a *App) loadBasicMemoryIndexReceipt() (BasicMemoryIndexReceipt, error) {
	var receipt BasicMemoryIndexReceipt
	if err := fsx.ReadJSON(a.Paths.IndexedSHA, &receipt); err != nil {
		return BasicMemoryIndexReceipt{}, err
	}
	migrated, err := fsx.MigrateSchema(a.Paths.IndexedSHA, &receipt.SchemaVersion, basicMemoryIndexReceiptSchemaVersion)
	if err != nil {
		return BasicMemoryIndexReceipt{}, err
	}
	if migrated {
		if err := fsx.WriteJSON(a.Paths.IndexedSHA, receipt, 0o600); err != nil {
			return BasicMemoryIndexReceipt{}, err
		}
	}
	return receipt, nil
}

func (a *App) saveBasicMemoryIndexReceipt(receipt BasicMemoryIndexReceipt) error {
	receipt.SchemaVersion = basicMemoryIndexReceiptSchemaVersion
	return fsx.WriteJSON(a.Paths.IndexedSHA, receipt, 0o600)
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

func executableAssetName() string {
	return fmt.Sprintf("hgctl_%s_%s", runtime.GOOS, runtime.GOARCH)
}
