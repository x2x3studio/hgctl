# hgctl

`hgctl` is the static endpoint runtime for Hourglass and the only binary this
repository builds. The companion `x2x3studio/hourglass` runs its reflect step
from shell scripts plus the official Claude Action, not from any tool here.

## Minimal loop

```text
Claude Code / Codex             (sessions persist as transcripts on disk)
  -> hgctl per-session ingest    (scheduler-driven; live + historical)
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
hgctl sync [--update]
hgctl ingest [--client all|claude|codex] [--limit N]
hgctl update
hgctl doctor
hgctl uninstall
hgctl version
```

Intake, sync, and update failures are non-fatal: disk, Git, network, Basic
Memory, and update errors are retried by the next scheduled sync.

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
  outbox/ queue/ shared/

~/hourglass-vault/   Basic Memory project (recall mirror)
```

It also installs one scheduler, the Basic Memory project `hourglass`, and the
`hourglass-memory` MCP server in every installed client, and prunes any stale
hgctl capture hook left in a Claude Code or Codex config by an older version. It
verifies the exact MCP command, arguments, environment, project identity, and
indexed `shared` revision. Run `hgctl doctor` until all managed checks pass.

On macOS the one scheduler label is `com.x2x3studio.hgctl.sync` (a LaunchAgent);
Ubuntu uses a user systemd timer with the same logical name. Neither needs an
application daemon or a Go/Python runtime. The scheduler runs `hgctl sync
--update` about once a minute.

## Ingest and sync

Per-session transcript ingest is the single intake path, for both live and
historical sessions - there are no per-turn capture hooks. `hgctl ingest` and
every scheduled `hgctl sync` read the local Claude Code and Codex transcripts on
disk (`~/.claude/projects/**`, `~/.codex/sessions/**`), render one bounded event
per session, and enqueue new sessions oldest-first stamped with their real
historical time. An event is just a Markdown file with closed frontmatter
(`captured_at`, `client`, `machine`) and a free-form body - the intake protocol
has no kinds or validation.

A session is ingested only once its transcript file has been idle (unmodified)
for `HG_INGEST_IDLE` (a Go duration, default 15 minutes), so an in-progress
conversation is never ingested mid-flight while a completed or historical
session is eligible by definition. A client-namespaced dedup ledger keeps
re-runs idempotent, and a session that renders to an empty body is never
enqueued.

`hgctl ingest` is the operator/bulk entry point: it drains the whole backlog in
one batch and pushes once so it lands on origin before the command returns.
`--client` selects the source (`all`, `claude`, or `codex`); `--limit` caps a
run. Each scheduled `hgctl sync` folds in a small, bounded ingest of newly-idle
sessions before it drains the outbox - filtering by the dedup ledger and file
mtime first and capping how many it parses per run - so live sessions land
per-session with no per-turn hooks.

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
check per hour): it fetches the latest release, verifies the checksum, and
atomically retargets the stable `~/.local/bin/hgctl` symlink. `hgctl update`
forces a check now.

This repository is public, so the self-update fetches the latest release and
its assets over unauthenticated HTTPS with the Go standard library - no `gh`
needed. `gh` is still used for the optional `repository_dispatch` that notifies
the private hourglass data repo, and git transport uses SSH; only the `hgctl`
self-update is gh-free.

## Development

```sh
gofmt -w cmd internal
go test ./...
go test -race ./internal/...
go vet ./...
```

All CI and release jobs use `[self-hosted, Linux, X64, x2x3studio-paas]`.
