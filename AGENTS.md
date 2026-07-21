# AGENTS.md - hgctl engineering contract

The Hourglass Project has exactly two repositories. This repository owns all
Go code and releases two static binaries:

- `hgctl`: endpoint installation, hooks, capture, queue transport, sync,
  update, Basic Memory project setup, MCP registration, and reindex;
- `dreamctl`: typed prepare, finalize, apply, and bootstrap operations used
  only by the companion Hourglass GitHub Actions.

`x2x3studio/hourglass` owns the agent contract, protocol corpus, Dream prompt,
workflows, and knowledge branches. It contains no Go source.

## Language

Keep source, comments, tests, documentation, workflows, filenames, repository
metadata, and commit messages in English ASCII. Tests may use escaped
language-neutral code points when needed.

## Endpoint invariants

- Use Go and the standard library. Do not add Python, a project virtual
  environment, a server, or a resident application daemon.
- Support macOS with Homebrew and Ubuntu Linux.
- Persist one random app-scoped machine UUID. Hostname is mutable metadata.
- Use `~/.local/bin/hgctl` as the only hook and scheduler command path.
- Use exactly one macOS LaunchAgent label:
  `com.x2x3studio.hgctl.sync`.
- Hooks are fail-open, bounded, atomic, and concurrency safe.
- Only `sync` writes Git. It appends to `queue/<machine-id>` and
  fast-forwards the local `shared` worktree. It never commits to `main` or
  `shared`.
- Every new machine queue starts from the exact machine-neutral orphan
  `queue-template` branch, never from `main` or `shared`.
- Use an OS-released advisory lock for sync. Process death must not leave a
  logical lock or require manual cleanup.
- Never scan source Git history or bulk-copy transcript stores and tool output.
- Auto-update is checksum verified and atomically retargets the stable symlink.

## Recall boundary

Basic Memory MCP exclusively owns search, recall, and read. `hgctl` must not
implement a second recall command, local search API, surface receipt, feedback
event, or reranker.

Install creates or adopts the Basic Memory project `hourglass`, reindexes its
disposable local cache only after `shared` advances, and configures the
`hourglass-memory` MCP server for every installed Claude Code and Codex
client. Configuration must be exact, idempotent, and ownership safe.
Uninstall removes only MCP entries it created.

Agents use Basic Memory read/search tools and never its write/edit/delete
tools. Durable writes enter only through hooks, `observe`, or `import`, then
pass through Dream.

## Protocol

The single closed `hourglass.event/v1` protocol has exactly three kinds:
`observation`, `turn`, and `import_batch`. The companion authority is
`hourglass/protocol/event.md`; this repository pins a byte-identical
conformance corpus. Do not add parallel protocol versions or future-stage
compatibility branches during the MVP.

## dreamctl boundary

`dreamctl` is model free. It validates Git, queue events, control manifests,
model output, publication paths, baselines, and exact publisher changes. It
does not call Claude. The Hourglass workflow downloads the released Linux
binary and the official Claude Action performs the semantic Dream stage.

Dream semantic output is limited to `memory/**/*.md`, `Home.md`, and
`Hourglass.canvas`. Durable control state is limited to seen, rejected, and
cursor shards.

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

Cross-build `hgctl` for Darwin/Linux on amd64/arm64 and `dreamctl` for
Linux amd64. CI and releases use only
`[self-hosted, Linux, X64, x2x3studio-paas]`.
