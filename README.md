# hgctl

`hgctl` is the static endpoint runtime for Hourglass. This repository also
builds `dreamctl`, the model-free control tool used by the companion
`x2x3studio/hourglass` GitHub Actions.

## Minimal loop

```text
Claude Code / Codex
  -> hgctl hooks and capture
  -> queue/<machine-id>
  -> GitHub Action + dreamctl + official Claude
  -> shared
  -> hgctl pull and Basic Memory reindex
  -> Basic Memory MCP recall
```

Basic Memory exclusively owns recall/search/read. `hgctl` has no recall or
feedback command. Obsidian may open `~/hourglass-vault` as a human view but
must not run Git automation.

## Commands

```text
hgctl install [--repo <git-url>] [--import <path>]
hgctl hook --client <claude|codex> --event <session-start|user-prompt|stop>
hgctl observe --client <claude|codex>
hgctl import <path> [--source <name>]
hgctl sync [--update]
hgctl context <repo-path> --client <claude|codex>
hgctl update
hgctl doctor
hgctl uninstall
hgctl version
```

Hooks always return success to the client. Disk, Git, network, Basic Memory,
and update failures are diagnostic and retried by sync.

## Install

The authoritative autonomous onboarding instructions are
`x2x3studio/hourglass/AGENTS.md`. In short, the Agent:

1. installs missing `git`, `gh`, and `uv` with Homebrew on macOS or the
   supported Ubuntu paths;
2. runs `uv tool install --upgrade basic-memory`;
3. downloads the current platform release asset and `checksums.txt`;
4. verifies the exact checksum and runs:

```sh
./hgctl_<os>_<arch> install --repo git@github.com:x2x3studio/hourglass.git
```

For a deliberate one-time migration, append `--import
/path/to/current-markdown-tree`. Import reads the current tree only and does
not inspect Git history.

Install creates:

```text
~/.local/bin/hgctl -> ~/.local/lib/hgctl/versions/<version>/hgctl

~/.local/share/hgctl/
  identity.json
  state.json
  outbox/
  delivered/
  repo/
  queue/

~/hourglass-vault/
```

It also installs one scheduler, managed Claude Code and Codex hooks, the Basic
Memory project `hourglass`, and the `hourglass-memory` MCP server in every
installed MVP client. It verifies exact MCP command, arguments, environment,
project identity, and indexed `shared` revision. Run `hgctl doctor` until
all managed checks pass.

On macOS the one scheduler label is
`com.x2x3studio.hgctl.sync`. Ubuntu uses a user systemd timer with the same
logical name. Neither needs an application daemon or a Go/Python runtime.

## Capture and sync

`SessionStart` performs a bounded sync and tells the Agent to use Basic
Memory MCP. `UserPromptSubmit` stages the bounded prompt. `Stop` pairs it
with the bounded response and emits one turn.

Explicit durable evidence enters through:

```sh
printf '%s\n' '<private durable observation>' |
  hgctl observe --client <claude|codex>
```

The event protocol contains only `turn`, `observation`, and
`import_batch`. Sync atomically drains the outbox, appends bounded queue
commits, pushes only the machine branch, requests reconciliation, pulls
`shared`, and reindexes Basic Memory only after a fast-forward.

Repository bootstrap also creates a machine-neutral orphan `queue-template`
whose tree contains only `.hourglass-queue`. A new machine queue starts from
that exact commit rather than inheriting `main` or `shared`; all later commits
are append-only event commits.

## Release artifacts

GoReleaser publishes:

```text
hgctl_darwin_amd64
hgctl_darwin_arm64
hgctl_linux_amd64
hgctl_linux_arm64
dreamctl_linux_amd64
checksums.txt
```

The endpoint updater installs only `hgctl`. Hourglass Actions download the
exact released `dreamctl_linux_amd64` and verify its pinned checksum.

## Development

```sh
gofmt -w cmd internal
go test ./...
go test -race ./internal/...
go vet ./...
```

All CI and release jobs use
`[self-hosted, Linux, X64, x2x3studio-paas]`.
