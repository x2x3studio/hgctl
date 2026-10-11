# hgctl

`hgctl` is the static endpoint runtime for Hourglass and the only binary this
repository builds. The private `x2x3studio/hourglass` repository runs the unified
Memory and Knowledge Project workflow from shell scripts plus the official Claude
Action; projection does not run inside this client.

## Minimal loop

```text
Claude Code / Codex / Copilot   (sessions persist as transcripts on disk)
  -> hgctl two-ledger ingest     (Memory + Knowledge, live + historical)
  -> queue/<machine-id>          (memory/events/ + knowledge/events/)
  -> one Project workflow        (independent Memory + Knowledge projections)
  -> one atomic shared commit    (memory/** + knowledge/**)
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
hgctl ingest [--client all|claude|codex|copilot] [--target memory|knowledge|all] [--limit N]
hgctl update
hgctl mcp
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
flag or `HOURGLASS_REPO`). Install is idempotent. On first connect, any empty
Memory or Knowledge ledger is backfilled from the machine's complete local
session history and published before the scheduler takes over; a repair install
skips ledgers that are already populated.

Install creates:

```text
~/.local/bin/hgctl -> ~/.local/lib/hgctl/versions/<version>/hgctl

~/.local/share/hgctl/
  identity.json      stable random machine UUID
  state.json         repo URL + queue branch
  repo/              control clone
  queue/             this machine's queue worktree
  shared/            product worktree
  outbox/ knowledge-outbox/
  ingested-sessions.json  knowledge-ingested-sessions.json
  indexed-shared  vault-mirror.json  update-check.json

~/hourglass-vault/   Basic Memory project (recall mirror)
```

It also installs one sync scheduler, the Basic Memory project `hourglass`, and
one shared Basic Memory HTTP service per user on this machine. All installed
Claude Code and Codex clients register `hourglass-memory` at
`http://127.0.0.1:18765/mcp`; opening another session no longer starts another
Python server. Copilot is an ingest source but does not receive this MCP.
Existing capture hooks from older hgctl versions are pruned.

The service label is `com.x2x3studio.hgctl.memory`: a keep-alive LaunchAgent on
macOS, or a restarting systemd user service on Ubuntu. It binds only to loopback,
never the LAN. Host/Origin protection rejects untrusted browser origins and
rebinding hosts; abandoned MCP sessions expire after an hour. This requires a
Basic Memory build with FastMCP HTTP host/origin protection (verified with Basic
Memory 0.23.2); migration probes the protection rather than trusting a version
string. Other machines run their own service over their own Git-backed
mirror; this is not a central network server or a new cloud backend. Basic Memory
keeps its existing semantic search, full-text search, and graph functionality.

`hgctl mcp` migrates existing managed stdio registrations and repairs the service
without ingesting, publishing, or reinstalling the sync scheduler. Scheduled
sync also migrates legacy registrations after an hgctl upgrade. Existing client
sessions must reconnect or restart to release their old stdio processes; hgctl
never kills running client sessions. An occupied port or modified unowned entry
is an error, not permission to replace another service. `hgctl doctor` verifies
the service definition, supervisor, MCP handshake and recall tool listing, client
URLs, project identity, and indexed `shared` revision. Uninstall stops and removes
only the owned service and registrations, preserving the vault and identity.

The separate sync label remains `com.x2x3studio.hgctl.sync` (a LaunchAgent on
macOS, a user systemd timer on Ubuntu). It runs `hgctl sync --update` about once a
minute; hgctl itself is not a resident daemon. Basic Memory remains the only
Python runtime dependency.

## SQLite vector cleanup compatibility

Basic Memory 0.23.2 ships a correlated sqlite-vec orphan-cleanup DELETE that can
exceed the 30-minute reindex timeout on a large vault even after all embeddings
are complete. hgctl backports the exact upstream fix from
`basicmachines-co/basic-memory@1ad049c704a51ca5477b2cdb814cdc0121578f82` before
reindex and shared-service setup. It changes only the matching SQL block in a
standard uv/virtualenv installation, preserves the original source beside it as
`.hgctl-before-vector-cleanup`, and leaves already-fixed or unknown source alone.
No vectors, notes, model settings, or index receipts are bypassed or discarded.
A future Basic Memory release carrying the fix needs no compatibility rewrite.

## Ingest and sync

Per-session transcript ingest is the single parser for live and historical
sessions - there are no per-turn capture hooks. Memory and Knowledge each own an
independent turn ledger and outbox, so either product can backfill and advance
without making the other skip a session. `hgctl ingest` and every scheduled
`hgctl sync` read local Claude Code, Codex, and GitHub Copilot App/CLI transcripts
(`~/.claude/projects/**`, `~/.codex/sessions/**`,
`~/.copilot/session-state/*/events.jsonl`) and enqueue only the NEW turns for each
product. Copilot intake keeps the root user/assistant conversation and drops tool,
reasoning, hook, system, and sub-agent events. An event is Markdown with closed
origin frontmatter and a free-form body; intake has no semantic kinds.

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
`claude`, `codex`, or `copilot`); `--target` selects `memory`, `knowledge`, or
`all`; and `--limit` caps each selected target. Each scheduled `hgctl sync` folds
in a bounded re-ingest before it drains both outboxes.

`hgctl sync` drains both outboxes, appends bounded queue commits, pushes only the
machine branch, pulls `shared`, and reindexes Basic Memory when the shared
revision changes. A rewritten remote `shared` root is adopted with a guarded hard
reset because the local mirror is disposable. `shared/knowledge` remains the
canonical OKF bundle: its `/concepts/**` links are preserved there. While copying
that bundle into the repository-rooted `~/hourglass-vault`, hgctl renders those
links as `/knowledge/concepts/**` so Obsidian resolves the same concepts without
creating duplicate IDs or writing compatibility changes back to `shared`. A mirror
rendering-schema change invalidates the local index receipt and forces one rebuild.
A new machine's queue branch is
self-seeded as an orphan root, never inheriting `main` or `shared`; all later
endpoint
commits are append-only events under `memory/events/` for Memory and
`knowledge/events/` for Knowledge. During migration, interrupted legacy
`events/` stages remain recoverable, but new writes are namespaced. After one
atomic product publish, Project archives each lane's consumed input into separate
archive trees, fast-forward-only; `hgctl
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

This repository is public, so self-update fetches the latest release and assets
over unauthenticated HTTPS with the Go standard library - no `gh` needed. The
endpoint does not call the private repository's Actions API: it appends queue
events and fast-forwards `shared`; Project drains by its gated schedule and
self-chain. Git transport uses SSH.

## Development

```sh
gofmt -w cmd internal
go test ./...
go test -race ./internal/...
go vet ./...
```

All CI and release jobs use `[self-hosted, Linux, X64, x2x3studio-paas]`.
