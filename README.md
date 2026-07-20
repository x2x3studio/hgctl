# hgctl

`hgctl` is the lightweight endpoint for Hourglass. It is one static Go binary
that wires Claude Code and Codex to a Git-backed shared memory without running a
local server or semantic daemon.

Hourglass itself lives in a separate repository. `hgctl` owns transport and
endpoint integration; GitHub Actions owns Dream; Basic Memory owns local recall.

## MVP responsibilities

- install one versioned binary behind the stable `~/.local/bin/hgctl` symlink;
- create a stable random machine ID and retain hostname as mutable metadata;
- maintain hidden control and queue worktrees plus `~/hourglass-vault` for
  `shared`;
- install fail-open Claude Code and Codex lifecycle hooks;
- register `~/hourglass-vault` as the Basic Memory project `hourglass` and add
  its MCP server to both clients;
- capture bounded turns and explicit observations in an atomic local outbox;
- commit and push only `queue/<machine-id>` and fast-forward only `shared`;
- initialize existing durable memory through deterministic import batches;
- periodically sync and check GitHub Releases for a newer binary;
- uninstall integration without deleting knowledge or machine identity.

It does not classify knowledge, maintain a local search database, parse entire
historical transcript stores, or write canonical memory.

## Filesystem layout

```text
~/.local/bin/hgctl -> ~/.local/lib/hgctl/versions/<version>/hgctl

~/.local/share/hgctl/
  identity.json
  state.json
  outbox/
  repo/                 # hidden main/control checkout
  queue/                # hidden queue/<machine-id> worktree

~/hourglass-vault/      # shared worktree; Basic Memory and Obsidian read here
```

On macOS, the single scheduler label is exactly:

```text
com.x2x3studio.hgctl.sync
```

The LaunchAgent always executes the stable symlink, never a version-specific
path. Updating the binary therefore does not create another Background Activity
entry and does not require administrator privileges. Ubuntu uses one user-level
systemd timer with the same logical name.

## Command surface

```text
hgctl install [--repo <git-url>] [--import <path>]
hgctl init [--repo <git-url>] [--import <path>]
hgctl hook --client <claude|codex> --event <session-start|user-prompt|stop|compact>
hgctl observe --client <claude|codex>
hgctl import <path> [--source <name>]
hgctl sync [--update]
hgctl context <repo-path> --client <claude|codex>
hgctl recall <query> --client <claude|codex>
hgctl update
hgctl doctor
hgctl uninstall
hgctl version
```

Hooks always return success to the calling agent. A local disk, Git, network,
Basic Memory, or update failure is reported to diagnostics and retried by the
periodic sync path; it never blocks Claude Code or Codex.

## First run

`init` has two phases:

1. **Read initialization:** clone the control branch, create the shared and
   queue worktrees, fast-forward `shared`, configure Basic Memory, and install
   client adapters.
2. **Backfill:** discover existing durable agent memory and optionally import
   an existing vault's current checkout. Imported Markdown enters the same
   machine queue as every later event; it never writes `shared` directly.

Imports are deterministic, bounded, resumable, and do not inspect source Git
history. A second machine initializes its own sources but does not replay the
global vault bootstrap.

Existing raw Claude Code and Codex session stores can be hundreds of megabytes
or more and have unstable schemas. They are deliberately not bulk-imported.
Prospective hooks capture a bounded user/assistant turn without tool output.

## Client integration

Both clients receive `SessionStart`, `UserPromptSubmit`, and `Stop` hooks:

- `SessionStart` performs a bounded pull and tells the agent that Hourglass is
  available through the Basic Memory project `hourglass`.
- `UserPromptSubmit` atomically stages the bounded prompt for the current turn.
- `Stop` pairs the prompt with the bounded assistant response and emits one
  queue event.

Claude Code and current Codex releases share the same hook event format. Codex
requires one trust review for newly installed user hooks; this is the only
manual first-install step. Thereafter hook execution and updates are automatic.

## Recall

`recall` delegates to `basic-memory tool search-notes` against the local
`hourglass` project. The Basic Memory MCP server gives both agents richer recall
tools. Its SQLite and embedding data remain disposable local implementation
details and never enter Git.

## Updates

Release artifacts are built by GitHub Actions for macOS and Linux on amd64 and
arm64. `update` verifies the release checksum, installs into a new version
directory, and atomically retargets the stable symlink. Private-repository
downloads use `GH_TOKEN`/`GITHUB_TOKEN` when present and otherwise the existing
authenticated `gh` CLI.

The scheduler checks frequently during the MVP. A missing release or temporary
GitHub failure is a NOOP and does not affect sync.

## Supported systems

- macOS with Homebrew available;
- Ubuntu Linux;
- Claude Code and Codex are the MVP adapters.

Hermes can implement the same event protocol later without changing branch or
Dream semantics.

## Development

The binary uses the Go standard library and targets static `darwin`/`linux`
builds. Run:

```sh
go test ./...
go vet ./...
```

The companion repository is `x2x3studio/hourglass`.
