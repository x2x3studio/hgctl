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

## Endpoint invariants

- Use Go and the standard library. Do not add Python, a project virtualenv, a
  server, or a resident application daemon.
- Support macOS with Homebrew and Ubuntu Linux.
- Persist one random app-scoped machine UUID. Hostname is mutable metadata.
- `~/.local/bin/hgctl` is the only scheduler command path.
- Exactly one macOS LaunchAgent label `com.x2x3studio.hgctl.sync` (Ubuntu uses a
  user systemd timer with the same logical name). The scheduler runs
  `sync --update` about once a minute; neither needs a daemon.
- Per-session transcript ingest is the single intake path (live + historical);
  there are no per-turn capture hooks. Ingest is idle-gated (a transcript must be
  unmodified for `HG_INGEST_IDLE`, default 15 minutes, before it is eligible),
  bounded, oldest-first, and idempotent via a client-namespaced dedup ledger; a
  session that renders empty is never enqueued. Install, background repair, and
  uninstall prune any stale hgctl hook left in a client config; the `hook`
  subcommand is a clean no-op kept only so a stale registration disrupts nothing.
- Only `sync` writes Git. It appends to `queue/<machine-id>` and fast-forwards
  the local `shared` worktree; it never commits to `main` or `shared`.
- A new machine's `queue/<machine-id>` is an ORPHAN root with no `main` or
  `shared` history: self-seeded locally when the remote carries no
  `queue-template`, or adopted from `queue-template` when one exists. All later
  commits are append-only event commits.
- Use an OS-released advisory lock for sync; process death must not leave a
  logical lock or require manual cleanup.
- Auto-update is checksum-verified and atomically retargets the stable symlink.
  The repo is public, so the updater fetches releases over unauthenticated HTTPS
  with the Go standard library (no `gh` needed); the check is throttled to once
  an hour and `hgctl update` forces it now.

## Intake protocol (loose)

An event is just a Markdown file with closed frontmatter (`captured_at`,
`client`, `machine`) and a free-form body. There are no kinds, schema, or
validation: intake is deliberately dumb and liberal (loose in, strict out) -
all strictness lives in the one central reflect step, not at intake.
`hgctl ingest [--client all|claude|codex]` is the operator/bulk entry point for
per-session ingest; every scheduled `hgctl sync` folds in a small, bounded
ingest of newly-idle sessions before draining the outbox. Both read the local
Claude Code and Codex transcripts, stamp each session with its real historical
time, and enqueue new ones oldest-first.

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
