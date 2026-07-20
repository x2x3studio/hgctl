package hgctl

import (
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
	"strings"
	"time"
)

const (
	DefaultRepoURL = "git@github.com:x2x3studio/hourglass.git"
	Protocol       = "hourglass.event/v1"
	ProjectName    = "hourglass"
	LaunchLabel    = "com.x2x3studio.hgctl.sync"
)

var Version = "dev"

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
	ID        string    `json:"id"`
	Hostname  string    `json:"hostname"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type BasicMemoryOwnership struct {
	ExternalID string `json:"external_id"`
	Path       string `json:"path"`
	Managed    bool   `json:"managed"`
	Pending    bool   `json:"pending,omitempty"`
}

type BasicMemoryIndexReceipt struct {
	SharedSHA         string `json:"shared_sha"`
	ProjectExternalID string `json:"project_external_id"`
}

type State struct {
	RepoURL            string                `json:"repo_url"`
	QueueBranch        string                `json:"queue_branch"`
	BasicMemoryProject *BasicMemoryOwnership `json:"basic_memory_project,omitempty"`
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
	return &App{Paths: paths, In: in, Out: out, Err: errOut, Now: time.Now}, nil
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
		id = Identity{ID: machineID, Hostname: host, CreatedAt: now, UpdatedAt: now}
		if err := writeJSONAtomic(a.Paths.Identity, id, 0o600); err != nil {
			return Identity{}, err
		}
		return id, nil
	}
	if !validMachineID(id.ID) {
		return Identity{}, errors.New("identity.json has an invalid machine id")
	}
	if id.Hostname != host {
		id.Hostname = host
		id.UpdatedAt = a.Now().UTC()
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
	return state, nil
}

func (a *App) saveState(state State) error {
	return writeJSONAtomic(a.Paths.State, state, 0o600)
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
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func executableAssetName() string {
	return fmt.Sprintf("hgctl_%s_%s", runtime.GOOS, runtime.GOARCH)
}
