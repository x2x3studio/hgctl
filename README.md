# hgctl

`hgctl` is the static endpoint runtime for Hourglass and the only binary this
repository builds. The companion `x2x3studio/hourglass` runs its reflect step
from shell scripts plus the official Claude Action, not from any tool here.

## Minimal loop

```text
Claude Code / Codex
  -> hgctl hooks and capture
  -> queue/<machine-id>
  -> GitHub Action (reflect: official Claude + Basic Memory)
  -> shared
  -> hgctl pull and Basic Memory reindex
  -> Basic Memory MCP recall
```

Basic Memory exclusively owns recall/search/read. `hgctl` has no recall or
feedback command. Obsidian may open `~/hourglass-vault` as a human view but
must not run Git automation.

## Commands

```text
hgctl install [--repo <git-url>]
hgctl hook --client <claude|codex> --event <user-prompt|stop>
hgctl sync [--update]
hgctl ingest [--client all|claude|codex] [--limit N]
hgctl update
hgctl doctor
hgctl uninstall
hgctl version
```

Hooks always return success to the client. Disk, Git, network, Basic Memory,
and update failures are diagnostic and retried by the next sync.

## Install

`hgctl` ships as a prebuilt, date-versioned release binary
(`v0.YYYYMMDD.<secs>`), so onboarding needs no Go or build toolchain. The
authoritative autonomous onboarding contract is
`x2x3studio/hourglass/AGENTS.md`. In short, the Agent:

1. installs missing `git`, `gh` (authenticated to github.com), and `uv`
   (Homebrew on macOS or the supported Ubuntu paths);
2. runs `uv tool install --upgrade basic-memory`;
3. downloads the current platform release asset and `checksums.txt`, verifies
   the exact checksum, then runs:

```sh
./hgctl_<os>_<arch> install --repo git@github.com:x2x3studio/hourglass.git
```

`--repo` defaults to `git@github.com:x2x3studio/hourglass.git` (override with the
flag or `HOURGLASS_REPO`). Install is idempotent.

Install creates:

```text
~/.local/bin/hgctl -> ~/.local/lib/hgctl/versions/<version>/hgctl

~/.local/share/hgctl/
  identity.json      stable random machine UUID
  state.json         repo URL + queue branch
  repo/              control clone (main, shared, queue/<machine-id>)
  outbox/ queue/ pending/ shared/

~/hourglass-vault/   Basic Memory project (recall mirror)
```

It also installs one scheduler, managed Claude Code and Codex hooks, the Basic
Memory project `hourglass`, and the `hourglass-memory` MCP server in every
installed client. It verifies the exact MCP command, arguments, environment,
project identity, and indexed `shared` revision. Run `hgctl doctor` until all
managed checks pass.

On macOS the one scheduler label is `com.x2x3studio.hgctl.sync` (a LaunchAgent);
Ubuntu uses a user systemd timer with the same logical name. Neither needs an
application daemon or a Go/Python runtime. The scheduler runs `hgctl sync
--update` about once a minute.

## Capture, ingest, and sync

Capture is automatic and hook-driven. `UserPromptSubmit` stages the bounded
prompt; `Stop` pairs it with the bounded response and enqueues one turn event.
An event is just a Markdown file with closed frontmatter (`captured_at`,
`client`, `machine`) and a free-form body - the intake protocol has no kinds or
validation.

`hgctl ingest` is the one-time historical backlog counterpart to the Stop hook.
It reads local Claude Code and Codex transcripts, stamps each session with its
real historical time, and enqueues every new one oldest-first in one batch, then
pushes once so the backlog lands on origin before the command returns. A dedup
ledger keeps re-runs idempotent. `--client` selects the source (`all`, `claude`,
or `codex`); `--limit` caps a run.

`hgctl sync` atomically drains the outbox, appends bounded queue commits, pushes
only the machine branch, requests reconciliation, pulls `shared`, and reindexes
Basic Memory only after a fast-forward. A new machine's queue branch is
self-seeded as an orphan root (or adopted from an existing `queue-template` when
the remote carries one), never inheriting `main` or `shared`; all later commits
are append-only events.

## Releases and updates

The Release workflow runs on every push to `main`: it cross-builds the four
platform binaries, writes `checksums.txt`, and publishes a date-versioned
(`v0.YYYYMMDD.<secs>`) GitHub release.

```text
hgctl_darwin_amd64
hgctl_darwin_arm64
hgctl_linux_amd64
hgctl_linux_arm64
checksums.txt
```

Auto-update runs inside the scheduled `sync --update` (throttled to at most one
check every five minutes): it fetches the latest release, verifies the checksum,
and atomically retargets the stable `~/.local/bin/hgctl` symlink. `hgctl update`
forces a check now.

This repository is currently private, so the updater uses authenticated `gh` to
fetch releases. Going public and dropping the `gh` dependency is a planned
formal-release step.

## Development

```sh
gofmt -w cmd internal
go test ./...
go test -race ./internal/...
go vet ./...
```

All CI and release jobs use `[self-hosted, Linux, X64, x2x3studio-paas]`.
