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
carrying the full session-so-far, and enqueue it stamped with the session's
latest-activity time. An event is just a Markdown file with closed frontmatter
(`captured_at`, `client`, `machine`) and a free-form body - the intake protocol
has no kinds or validation.

Intake is hot and cumulative: knowledge flows in while a session is live. A
per-session ledger marker records the transcript byte size and time of the last
snapshot; a session is re-snapshotted whenever its transcript has grown past that
marker, throttled to at most once per `HG_INGEST_MIN_INTERVAL` (a Go duration,
default 5 minutes) so a rapidly-growing live session does not churn. A session
that stops growing simply produces no new snapshot - its last snapshot is the
final complete one, so there is no idle or session-end handling. A historical
session that is not growing ingests exactly once. Sessions that render to an
empty body are never enqueued. The reflect step is idempotent, so re-processing a
grown session hot-updates its note and the summary accumulates.

`hgctl ingest` is the operator/bulk entry point: it snapshots every new-or-grown
session, drains the whole backlog in one batch, and pushes once so it lands on
origin before the command returns. `--client` selects the source (`all`,
`claude`, or `codex`); `--limit` caps a run. Each scheduled `hgctl sync` folds in
a small, bounded re-ingest before it drains the outbox - filtering cheaply by the
ledger marker and file size first, and capping how many it parses per run - so
live sessions land per-session with no per-turn hooks.

`hgctl sync` atomically drains the outbox, appends bounded queue commits, pushes
only the machine branch, requests reconciliation, pulls `shared`, and reindexes
Basic Memory only after a fast-forward. A new machine's queue branch is
self-seeded as an orphan root (or adopted from an existing `queue-template` when
the remote carries one), never inheriting `main` or `shared`; all later endpoint
commits are append-only events. The reflect Action separately archives a queue
branch's consumed events into `archive/<YYYY-MM>/` - or `archive/poison/<YYYY-MM>/`
for a skipped, never-distilled slice - fast-forward-only; `hgctl
sync` fast-forwards through those archive commits, and self-heals a divergence (a
local unpushed append that raced an archive) by resetting onto the remote and
replaying the event from the retained outbox.

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
