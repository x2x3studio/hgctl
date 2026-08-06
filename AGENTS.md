# AGENTS.md - hgctl engineering contract

The Hourglass Project has exactly two repositories. This repository owns all Go
code and releases one static binary:

- `hgctl`: endpoint installation, per-session transcript ingest, queue
  transport, sync, update, Basic Memory project setup, MCP registration, and
  reindex.

`x2x3studio/hourglass` is the codeless control plane. It owns the reflect GitHub
Action (shell scripts + prompts + the official Claude Action), the onboarding
contract, and the knowledge branches (the `shared` product and each machine's
`queue/<machine-id>` raw intake). It contains no Go source.

## Language

Keep source, comments, tests, documentation, workflows, filenames, repository
metadata, and commit messages in English ASCII. Tests may use escaped
language-neutral code points when needed.

## What hgctl is (and is not)

`hgctl` is model-free git and transport automation. It NEVER calls a model: all
semantic distillation happens in the hourglass reflect Action (official Claude
using Basic Memory), never here. hgctl only ingests raw session transcripts,
moves them between the machine and Git, keeps Basic Memory indexed, and
self-updates.

## Package layout

One binary, packages layered so a dependency can only ever point downward:

    cmd/hgctl          main
    internal/hgctl     the CLI: App, command dispatch, sync, queue transport,
                       repository/worktrees, Basic Memory, scheduler, update
    internal/ingest    transcripts -> events (the single intake path)
    internal/event     the on-disk shape of one event, and the outbox
    internal/config    where things live and what is persisted there
    internal/gitx      git plumbing            \
    internal/proc      running external commands > leaves: no dependency on us
    internal/fsx       atomic writes, JSON, locks, schema probe

Everything used to live in one package, and the file every other file imported
was a grab-bag holding five unrelated things. The rule that replaced it: a
package is a boundary, so put a thing where its INVARIANT lives, not where it is
called from. proc owns "a subprocess is not trusted" (bounded output, redacted
errors); fsx owns "this process can die right now" (atomic writes, released
locks, refused future schemas); gitx owns "git answers through the exit status";
event owns "frontmatter stays closed"; ingest owns "intake is incremental per
transcript file". Each package's doc comment states its invariant, and its tests
pin that invariant rather than its implementation.

## Endpoint invariants

- Use Go and the standard library. Do not add Python, a project virtualenv, a
  server, or a resident application daemon.
- Support macOS with Homebrew and Ubuntu Linux.
- Persist one random app-scoped machine UUID. Hostname is mutable metadata.
- `~/.local/bin/hgctl` is the only scheduler command path.
- Exactly one logical scheduler `com.x2x3studio.hgctl.sync`, set up by `install`
  per OS: a macOS LaunchAgent, or on Linux a systemd USER timer + oneshot service
  (`~/.config/systemd/user/`, enabled with `systemctl --user enable --now`, plus
  `loginctl enable-linger` so it runs headless without an active login - if that
  needs privilege, install instructs to run `sudo loginctl enable-linger <uid>`
  once). Either runs `sync --update` about once a minute (systemd timer is
  `Persistent=true`, so a missed fire catches up); neither needs a resident daemon.
- Per-session transcript ingest is the single intake path (live + historical);
  there are no per-turn capture hooks. Intake is INCREMENTAL: a per-session ledger
  marker (emitted-turn cursor + transcript size + time) drives emission of only
  the NEW turns since the last ingest, throttled to at most once per
  `HG_INGEST_MIN_INTERVAL` (default 5 minutes). Turns are emitted COMPLETE (never
  truncated); a delta is split into chunk events each bounded at a turn boundary,
  so a session's first ingest streams the whole conversation as ordered chunks and
  later growth emits only its new turns. A session that stops growing produces no
  new event; a non-growing historical session ingests exactly once; empty sessions
  are never enqueued. Install, background repair, and uninstall prune any stale
  hgctl hook left in a client config; the `hook` subcommand is a clean no-op kept
  only so a stale registration disrupts nothing.
- On the endpoint, only `sync` writes Git: it APPENDS raw events to
  `queue/<machine-id>`'s `events/` and fast-forwards its local `shared` and
  `queue` worktrees; it never commits to `main` or `shared`, and never archives.
  The reflect Action owns archiving a queue branch's CONSUMED events (moving
  `events/` into `archive/<YYYY-MM>/` - or `archive/poison/<YYYY-MM>/` for a
  skipped, never-distilled slice - fast-forward-only); the endpoint only
  fast-forwards through those archive commits. If a local committed-but-unpushed
  append races an archive and the branches diverge, `sync` self-heals by
  resetting onto the remote and replaying the event from the outbox (retained
  until a successful push), so no event is lost.
- A new machine's `queue/<machine-id>` is an ORPHAN root with no `main` or
  `shared` history: adopted when the remote already carries this machine's
  branch, otherwise self-seeded locally. All later commits are append-only
  event commits.
- Events under `events/` are CREATE-ONLY; a modified event means captured
  evidence was altered and the guards must keep refusing it. `machine.json` at
  the branch root is the one tracked file rewritten in place (a hostname edit,
  an OS upgrade, an hgctl release), so it is the one path where a modification
  is legal. It is derived state, never evidence: a stage left behind by an
  interrupted sync is REVERTED and re-derived, because recovery runs first and a
  refusal there wedges every later sync permanently.
- Use an OS-released advisory lock for sync; process death must not leave a
  logical lock or require manual cleanup.
- Auto-update is checksum-verified and atomically retargets the stable symlink.
  The repo is public, so the updater fetches releases over unauthenticated HTTPS
  with the Go standard library (no `gh` needed); the check is throttled to once
  an hour and `hgctl update` forces it now.

- A FIRST install backfills the machine's whole session history in one unbounded
  pass, because the scheduled path cannot: one sync parses at most
  syncIngestLimit transcripts, so a machine with thousands of sessions would need
  hours of ticks to finish it, and nobody should have to remember to run `hgctl
  ingest` on a machine that was just connected. A re-run of install is repair,
  not onboarding - a populated ledger takes the ordinary bounded sync. The
  backfill is never fatal: a machine whose backfill fails is still installed and
  still scheduled, it just catches up over ticks.

## Intake protocol (loose)

An event is just a Markdown file with closed frontmatter (`captured_at`,
`client`, `machine`) and a free-form body. There are no kinds, schema, or
validation: intake is deliberately dumb and liberal (loose in, strict out) -
all strictness lives in the one central reflect step, not at intake.
`hgctl ingest [--client all|claude|codex|copilot]` is the operator/bulk entry point;
every scheduled `hgctl sync` folds in a small, bounded re-ingest of new-or-grown
sessions before draining the outbox. Each event carries a chunk of a session's
NEW turns (complete, never truncated), stamped with those turns' latest-activity
time and tagged with the session origin; the reflect step refines the session's
note from each delta, so its distillation accumulates as the session grows rather
than duplicating.

## Adding a new client (data source)

Supported clients today are Claude Code, Codex, and GitHub Copilot App/CLI.
Everything downstream of ingest is client-agnostic: the event `client` field is
a free-form string, and the queue, the reflect step, `sources`, and recall never
enumerate clients. So a new agent (for example a Hermes agent) is added ENTIRELY
at the ingest boundary in `internal/ingest/ingest.go`, in three small steps:

1. `<client>SessionFiles()` - return that client's transcript files on disk via
   its own glob/walk, mirroring `claudeSessionFiles` / `codexSessionFiles` /
   `copilotSessionFiles`.
2. `extract<Client>Session(path) (ingestSession, bool)` - parse that client's
   transcript format into `ingestSession` turns (role + text + timestamp),
   dropping tool noise; return false when nothing qualifies.
3. Register the client string in `Clients` and wire its files + extractor into
   the `gatherSessions` switch.

Everything else is reused unchanged: the per-file ledger keying, the delta
cursor, chunking, the `session`/`project`/`title`/`turns` frontmatter, the queue,
the reflect refine, and recall. Emit the client's own name as the event `client`;
reflect carries it into `sources` verbatim (no closed enum anywhere). Onboarding
a MACHINE that already runs a supported client needs no code - that is just
`hgctl install` (see the onboarding contract); this section is only for teaching
hgctl a brand-new transcript format.

## Recall boundary

Basic Memory MCP exclusively owns search, recall, and read on the endpoint.
`hgctl` must not implement a second recall command, local search API, or
reranker. Install creates or adopts the Basic Memory project `hourglass`,
reindexes its disposable local mirror after `shared` advances, and configures
the `hourglass-memory` MCP server (read and search tools only) for every
installed Claude Code and Codex client. Configuration must be exact, idempotent,
and ownership safe; uninstall removes only MCP entries it created. Durable
writes enter only through per-session ingest, reach the queue, and are distilled
by the reflect Action - the endpoint never authors product.

The mirror into that vault must stay INCREMENTAL, and this is a correctness
constraint rather than an optimisation. Basic Memory's scan keys off mtime, and
its reindex embeds notes with a local model, so refreshing every file's mtime
makes it re-embed the whole corpus - measured at 466% CPU for 6+ minutes against
298 notes, once a minute, for a product that had changed by one note. Do not
compare the vault file against its source to decide: Basic Memory rewrites the
files it indexes (re-serialised YAML frontmatter, an added permalink), so 294 of
298 differ from their source and the comparison never skips anything. Hash the
SOURCE - see `vault_mirror.go`.

Reindex is also gated on the PRODUCT changing, not on shared's sha moving:
reflect advances its cursor past a noop slice with an empty commit, and
consolidate carries watermark trailers forward the same way. That gate is
conservative by construction - an unknown commit or any git failure reindexes -
because the two mistakes are not symmetric: a wrong "changed" costs one reindex,
a wrong "unchanged" writes a receipt claiming the mirror is indexed when it is
not, and recall goes stale with nothing reporting it.

## Cost of the once-a-minute path

`sync` runs every 60 seconds forever, so anything on it is paid ~1440 times a
day and must earn that. Two costs that did not: a full vault re-copy (above),
and a `git ls-remote` probe for `queue-template` - one SSH round trip, measured
at 3.7s, for a branch the control plane never grew and no caller read. Before
adding work here, ask what a day of it costs and whether the steady state reads
the answer; prefer gating on a local receipt or marker over asking the network
again. When something turns out to be dead, DELETE it rather than gating it -
a flag around dead code leaves the reader believing the path is live.

Onboarding depends on nothing being present server-side: a machine with no queue
branch on the remote self-seeds an orphan one. There is no template branch, and
reintroducing one would put a network probe back on this path.

## What doctor may claim

Every line must be something doctor actually verified. A line that lies is worse
than one that is missing, in BOTH directions - reporting a healthy endpoint as
broken sends the reader chasing nothing, and reporting a broken one as healthy
is how the endpoint went 46 hours emitting nothing while exiting 0. So a check
either proves its claim end to end or narrows the claim until it can. The index
line proves the receipt is current AND that the index holds entries; it does not
assert a ratio between index size and note count, because Basic Memory mints
extra entities for forward-referenced wikilinks and no honest threshold exists.
It prints both numbers so a reader can see what doctor cannot assert.

## Configuration safety

Preserve unrelated Claude Code, Codex, Basic Memory, LaunchAgent, systemd, and
MCP configuration. Repeated setup is idempotent. Uninstall removes managed
integration but preserves the shared vault, outbox, and machine identity.

## Verification

Every behavior change needs focused tests. Before committing, run:

```sh
gofmt -w cmd internal
go test ./...
go test -race ./internal/...
go vet ./...
```

CI cross-builds `hgctl` for Darwin and Linux on amd64 and arm64. Because this
repo is public, CI and the date-versioned Release (`v0.YYYYMMDD.<secs>`,
published on every push to `main`) run on GitHub-hosted `ubuntu-latest`; the
self-hosted `x2x3studio-paas` runner is reserved for the private hourglass
reflect Action.
