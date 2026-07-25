package hgctl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// machineMetaFile names the one tracked path a queue branch carries outside
// events/ and archive/.
//
// WHY IT EXISTS. A queue branch is identified only by the machine UUID in its
// name, which says nothing about which machine that is. Reading the product,
// triaging a stalled drain, or deciding whether a queue is worth keeping all
// begin with "whose is this?", and the answer lived nowhere - it had to be
// recovered from an event's frontmatter, which is gone once the events are
// archived.
//
// WHY IT DOES NOT BREAK THE APPEND-ONLY INVARIANT. Selection reads only
// events/, and archiving moves only events/, so a file at the branch root is
// invisible to both. It is the single exception to "a queue carries raw
// evidence and nothing else", and it earns that by being the thing you need
// before you can interpret the evidence.
const machineMetaFile = "machine.json"

// machineMeta is deliberately small and slow-moving. Anything that changes on
// every sync - a heartbeat, a last-seen stamp, an event count - would commit
// once a minute forever, and git already records liveness in the commit dates
// of the events themselves.
type machineMeta struct {
	SchemaVersion int    `json:"schema_version"`
	MachineID     string `json:"machine_id"`
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	HgctlVersion  string `json:"hgctl_version"`
}

func renderMachineMeta(id Identity) ([]byte, error) {
	body, err := json.MarshalIndent(machineMeta{
		SchemaVersion: 1,
		MachineID:     id.ID,
		Hostname:      id.Hostname,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		HgctlVersion:  Version,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// revertQueueMachineMeta puts the metadata file back to whatever HEAD says,
// discarding a stage left behind by an interrupted sync. Reverting is safe
// precisely because the file is derived: upsertMachineMeta re-applies it later
// in the same sync, from the live identity, so nothing is lost by throwing a
// half-finished write away.
func (a *App) revertQueueMachineMeta(ctx context.Context) error {
	if _, err := runCommand(ctx, a.Paths.Queue, "git", "cat-file", "-e", "HEAD:"+machineMetaFile); err == nil {
		_, err := runCommand(ctx, a.Paths.Queue, "git", "restore", "--staged", "--worktree", "--", machineMetaFile)
		return err
	}
	// Not in HEAD, so the leftover is a first-ever creation: drop it entirely
	// rather than restoring a version that does not exist.
	if _, err := runCommand(ctx, a.Paths.Queue, "git", "rm", "--cached", "--force", "--quiet", "--ignore-unmatch", "--", machineMetaFile); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(a.Paths.Queue, machineMetaFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// anything actually changed. Byte-comparing first is what makes this an upsert
// rather than a write: the scheduler calls it about once a minute, and a file
// rewritten every time would bury the queue's real history - the event
// captures - under commits that say nothing.
func (a *App) upsertMachineMeta(ctx context.Context, id Identity) (bool, error) {
	want, err := renderMachineMeta(id)
	if err != nil {
		return false, err
	}
	path := filepath.Join(a.Paths.Queue, machineMetaFile)
	if current, err := os.ReadFile(path); err == nil && string(current) == string(want) {
		return false, nil
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.WriteFile(path, want, 0o600); err != nil {
		return false, err
	}
	if _, err := runCommand(ctx, a.Paths.Queue, "git", "add", "--", machineMetaFile); err != nil {
		return false, err
	}
	return gitHasStagedChanges(ctx, a.Paths.Queue)
}
