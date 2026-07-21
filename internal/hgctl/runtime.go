package hgctl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
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

const (
	defaultCommandOutputLimit     = 1 << 20
	gitCommandOutputLimit         = 8 << 20
	basicMemoryCommandOutputLimit = 4 << 20
)

var Version = "dev"

var errUnsupportedSchemaVersion = errors.New("unsupported persisted schema version")

var errCommandOutputLimit = errors.New("subprocess output limit exceeded")

type Paths struct {
	Home          string
	Data          string
	Control       string
	Queue         string
	Outbox        string
	Quarantine    string
	Pending       string
	Vault         string
	Bin           string
	Versions      string
	Identity      string
	State         string
	Delivered     string
	IndexedSHA    string
	UpdateCheck   string
	LifecycleLock string
	SyncLock      string
	UpdateLock    string
	CodexLock     string
	CodexCheck    string
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
		Quarantine:    filepath.Join(data, "quarantine"),
		Pending:       filepath.Join(data, "pending"),
		Vault:         vault,
		Bin:           filepath.Join(home, ".local", "bin"),
		Versions:      filepath.Join(home, ".local", "lib", "hgctl", "versions"),
		Identity:      filepath.Join(data, "identity.json"),
		State:         filepath.Join(data, "state.json"),
		Delivered:     filepath.Join(data, "delivered"),
		IndexedSHA:    filepath.Join(data, "indexed-shared"),
		UpdateCheck:   filepath.Join(data, "update-check.json"),
		LifecycleLock: filepath.Join(data, "lifecycle.lock"),
		SyncLock:      filepath.Join(data, "sync.lock"),
		UpdateLock:    filepath.Join(data, "update.lock"),
		CodexLock:     filepath.Join(data, "codex-trust.lock"),
		CodexCheck:    filepath.Join(data, "codex-trust-check.json"),
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
	if exists, err := probeJSONSchema(a.Paths.Identity, &identity, &identity.SchemaVersion, identitySchemaVersion); err != nil {
		return err
	} else if exists && !validMachineID(identity.ID) {
		return errors.New("identity.json has an invalid machine id")
	}

	var state State
	if _, err := probeJSONSchema(a.Paths.State, &state, &state.SchemaVersion, stateSchemaVersion); err != nil {
		return err
	}
	var receipt BasicMemoryIndexReceipt
	if _, err := probeJSONSchema(a.Paths.IndexedSHA, &receipt, &receipt.SchemaVersion, basicMemoryIndexReceiptSchemaVersion); err != nil {
		return err
	}
	var check updateCheck
	if _, err := probeJSONSchema(a.Paths.UpdateCheck, &check, &check.SchemaVersion, updateCheckSchemaVersion); err != nil {
		return err
	}
	return nil
}

func probeJSONSchema(path string, dst any, version *int, current int) (bool, error) {
	if err := readJSON(path, dst); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := migrateSchemaVersion(path, version, current); err != nil {
		return true, err
	}
	return true, nil
}

func (a *App) ensureDataDirs() error {
	for _, path := range []string{a.Paths.Data, a.Paths.Outbox, a.Paths.Quarantine, a.Paths.Pending} {
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
	if err := readJSON(a.Paths.Identity, &id); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Identity{}, err
		}
		now := a.Now().UTC()
		machineID, err := newUUID()
		if err != nil {
			return Identity{}, err
		}
		id = Identity{SchemaVersion: identitySchemaVersion, ID: machineID, Hostname: host, CreatedAt: now, UpdatedAt: now}
		if err := writeJSONAtomic(a.Paths.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
		return id, nil
	}
	migrated, err := migrateSchemaVersion(a.Paths.Identity, &id.SchemaVersion, identitySchemaVersion)
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
		if err := writeJSONAtomic(a.Paths.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
	}
	return id, nil
}

func (a *App) loadState() (State, error) {
	var state State
	if err := readJSON(a.Paths.State, &state); err != nil {
		return State{}, err
	}
	migrated, err := migrateSchemaVersion(a.Paths.State, &state.SchemaVersion, stateSchemaVersion)
	if err != nil {
		return State{}, err
	}
	if migrated {
		if err := writeJSONAtomic(a.Paths.State, state, 0o600); err != nil {
			return State{}, err
		}
	}
	return state, nil
}

func (a *App) saveState(state State) error {
	state.SchemaVersion = stateSchemaVersion
	return writeJSONAtomic(a.Paths.State, state, 0o600)
}

func (a *App) loadBasicMemoryIndexReceipt() (BasicMemoryIndexReceipt, error) {
	var receipt BasicMemoryIndexReceipt
	if err := readJSON(a.Paths.IndexedSHA, &receipt); err != nil {
		return BasicMemoryIndexReceipt{}, err
	}
	migrated, err := migrateSchemaVersion(a.Paths.IndexedSHA, &receipt.SchemaVersion, basicMemoryIndexReceiptSchemaVersion)
	if err != nil {
		return BasicMemoryIndexReceipt{}, err
	}
	if migrated {
		if err := writeJSONAtomic(a.Paths.IndexedSHA, receipt, 0o600); err != nil {
			return BasicMemoryIndexReceipt{}, err
		}
	}
	return receipt, nil
}

func (a *App) saveBasicMemoryIndexReceipt(receipt BasicMemoryIndexReceipt) error {
	receipt.SchemaVersion = basicMemoryIndexReceiptSchemaVersion
	return writeJSONAtomic(a.Paths.IndexedSHA, receipt, 0o600)
}

func migrateSchemaVersion(path string, version *int, current int) (bool, error) {
	switch *version {
	case 0:
		if current != 1 {
			return false, fmt.Errorf("%w: %s requires an explicit migration from the legacy schema to %d", errUnsupportedSchemaVersion, path, current)
		}
		*version = 1
		return true, nil
	case current:
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s has unsupported schema_version %d; current is %d", errUnsupportedSchemaVersion, path, *version, current)
	}
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(path, b, mode)
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".hgctl-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
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

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runCommandEnv(ctx, dir, nil, name, args...)
}

func runCommandEnv(ctx context.Context, dir string, environment []string, name string, args ...string) (string, error) {
	if name == "git" {
		args = append([]string{
			"-c", "core.hooksPath=/dev/null",
			"-c", "commit.gpgSign=false",
			"-c", "tag.gpgSign=false",
		}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), environment...)
	if name == "git" {
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
		if os.Getenv("GIT_SSH_COMMAND") == "" {
			cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -oBatchMode=yes -oConnectTimeout=10")
		}
	}
	policy := commandPolicyFor(name)
	output := boundedCommandOutput{limit: policy.outputLimit}
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if output.truncated {
		return output.String(), &commandRunError{class: policy.class, cause: errors.Join(err, errCommandOutputLimit), output: output.String(), outputLimit: policy.outputLimit}
	}
	if err != nil {
		return output.String(), &commandRunError{class: policy.class, cause: err, output: output.String()}
	}
	return output.String(), nil
}

type commandPolicy struct {
	class       string
	outputLimit int
}

func commandPolicyFor(name string) commandPolicy {
	switch name {
	case "git":
		return commandPolicy{class: "git", outputLimit: gitCommandOutputLimit}
	case "basic-memory":
		return commandPolicy{class: "Basic Memory", outputLimit: basicMemoryCommandOutputLimit}
	case "gh":
		return commandPolicy{class: "GitHub CLI", outputLimit: 2 << 20}
	case "launchctl", "systemctl", "loginctl":
		return commandPolicy{class: "scheduler", outputLimit: 256 << 10}
	default:
		return commandPolicy{class: "external", outputLimit: defaultCommandOutputLimit}
	}
}

type boundedCommandOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedCommandOutput) Write(content []byte) (int, error) {
	written := len(content)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(content) > remaining {
			_, _ = b.buffer.Write(content[:remaining])
		} else {
			_, _ = b.buffer.Write(content)
		}
	}
	if len(content) > remaining {
		b.truncated = true
	}
	return written, nil
}

func (b *boundedCommandOutput) String() string {
	return b.buffer.String()
}

type commandRunError struct {
	class       string
	cause       error
	output      string
	outputLimit int
}

func (e *commandRunError) Error() string {
	if e.outputLimit > 0 {
		return fmt.Sprintf("%s command exceeded its %d-byte output limit", e.class, e.outputLimit)
	}
	return fmt.Sprintf("%s command failed: %v (output suppressed)", e.class, e.cause)
}

func (e *commandRunError) Unwrap() error {
	return e.cause
}

func commandFailureOutput(err error) string {
	var commandErr *commandRunError
	if errors.As(err, &commandErr) {
		return commandErr.output
	}
	return ""
}

func executableAssetName() string {
	return fmt.Sprintf("hgctl_%s_%s", runtime.GOOS, runtime.GOARCH)
}
