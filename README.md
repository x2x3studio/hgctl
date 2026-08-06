# hgctl

`hgctl` is the static endpoint runtime for Hourglass and the only binary this
repository builds. The companion `x2x3studio/hourglass` runs its reflect step
from shell scripts plus the official Claude Action, not from any tool here.

## Minimal loop

```text
Claude Code / Codex / Copilot   (sessions persist as transcripts on disk)
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
hgctl ingest [--client all|claude|codex|copilot] [--limit N]
hgctl update
hgctl doctor
hgctl uninstall
hgctl version
```

Intake, sync, and update failures are non-fatal: disk, Git, network, Basic
Memory, and update errors are retried by the next scheduled sync.

Supported session clients are Claude Code, Codex, and GitHub Copilot App/CLI.
Onboarding a machine that already runs one of them needs no code - just `hgctl
install`. Teaching hgctl a brand-new transcript format (another agent as a data
source) is a small ingest-side change - see "Adding a new client" in
[AGENTS.md](AGENTS.md).

## Install

`hgctl` ships as a prebuilt, date-versioned release binary
(`v0.YYYYMMDD.<secs>`), so onboarding needs no Go or build toolchain. The
authoritative autonomous onboarding contract is
`x2x3studio/hourglass/ONBOARDING.md`. In short, the Agent:

1. ensures `git` (with SSH access to the data repo) and `uv` are present - `uv`
   via `brew install uv` when Homebrew exists, else the official
   `astral.sh/uv/install.sh` (works on macOS and Linux); `gh` is NOT required;
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
`hourglass-memory` MCP server in every installed Claude Code or Codex client, and
prunes any stale hgctl capture hook left in their config by an older version. It
verifies the exact MCP command, arguments, environment, project identity, and
indexed `shared` revision. Run `hgctl doctor` until all managed checks pass.

On macOS the one scheduler label is `com.x2x3studio.hgctl.sync` (a LaunchAgent);
Ubuntu uses a user systemd timer with the same logical name. Neither needs an
application daemon or a Go/Python runtime. The scheduler runs `hgctl sync
--update` about once a minute.

## Ingest and sync

Per-session transcript ingest is the single intake path, for both live and
historical sessions - there are no per-turn capture hooks. `hgctl ingest` and
every scheduled `hgctl sync` read the local Claude Code, Codex, and GitHub
Copilot App/CLI transcripts on disk (`~/.claude/projects/**`,
`~/.codex/sessions/**`, `~/.copilot/session-state/*/events.jsonl`) and enqueue
only the NEW turns of each session since it was last ingested, stamped with
those turns' latest-activity time. Copilot intake keeps only the root
`user.message` and `assistant.message` conversation, grouping multi-part
assistant responses and dropping tool, reasoning, hook, system, and sub-agent
events. An event is just a Markdown file with closed frontmatter (`captured_at`,
`client`, `machine`, and the origin `session`/`project`/`title` with a `turns`
range) and a free-form body - the intake protocol has no kinds or validation.

Intake is incremental and complete: knowledge flows in while a session is live. A
per-session ledger marker records the emitted-turn cursor (with transcript size
and time); each ingest emits only the turns after the cursor, throttled to at
most once per `HG_INGEST_MIN_INTERVAL` (a Go duration, default 5 minutes) so a
rapidly-growing live session does not churn. Turns are emitted in full (never
truncated); a delta is split into chunk events each bounded at a turn boundary, so
a session's first ingest streams the whole conversation as ordered chunks and
later growth adds only its new turns. A session that stops growing produces no new
event; a non-growing historical session ingests exactly once. Empty sessions are
never enqueued. The reflect step refines a session's note from each delta, so its
distillation accumulates as the session grows.

`hgctl ingest` is the operator/bulk entry point: it emits every new-or-grown
session's new turns, drains the whole backlog in one batch, and pushes once so it
lands on
origin before the command returns. `--client` selects the source (`all`,
`claude`, `codex`, or `copilot`); `--limit` caps a run. Each scheduled `hgctl
sync` folds in a small, bounded re-ingest before it drains the outbox - filtering
cheaply by the ledger marker and file size first, and capping how many it parses
per run - so live sessions land per-session with no per-turn hooks.

`hgctl sync` atomically drains the outbox, appends bounded queue commits, pushes
only the machine branch, requests reconciliation, pulls `shared`, and reindexes
Basic Memory only after a fast-forward. A new machine's queue branch is
self-seeded as an orphan root, never inheriting `main` or `shared`; all later
endpoint
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
needed. The endpoint never triggers reflect: it only appends events to the
queue and fast-forwards `shared`; the reflect Action drains on its own
schedule. Git transport uses SSH.

## Development

```sh
gofmt -w cmd internal
go test ./...
go test -race ./internal/...
go vet ./...
```

All CI and release jobs use `[self-hosted, Linux, X64, x2x3studio-paas]`.
