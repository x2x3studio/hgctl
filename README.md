# hgctl

`hgctl` is the lightweight endpoint for Hourglass. It is one static Go binary
that wires Claude Code and Codex to a Git-backed shared memory without running a
local server or semantic daemon.

Hourglass itself lives in a separate repository. `hgctl` owns transport and
endpoint integration; GitHub Actions owns Dream; Basic Memory owns local recall.
Together, `x2x3studio/hgctl` and `x2x3studio/hourglass` are the two repositories
of the Hourglass Project.

## MVP responsibilities

- install one versioned binary behind the stable `~/.local/bin/hgctl` symlink;
- create a stable random machine ID and retain hostname as mutable metadata;
- maintain hidden control and queue worktrees plus `~/hourglass-vault` for
  `shared`;
- install fail-open Claude Code and Codex lifecycle hooks;
- register `~/hourglass-vault` as the Basic Memory project `hourglass` and
  expose exact-revision recall and receipt-bound feedback through `hgctl`;
- capture bounded turns and explicit observations in an atomic local outbox;
- commit and push only `queue/<machine-id>` and fast-forward only `shared`;
- request the trusted default-branch bootstrap workflow when a fresh repository
  has no `shared`, then wait under a fixed deadline;
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
  delivered/            # one local receipt per pushed event
  surfaces/              # bounded seven-day recall receipts; no queries
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
hgctl hook --client <claude|codex> --event <session-start|user-prompt|stop>
hgctl observe --client <claude|codex>
hgctl import <path> [--source <name>]
hgctl sync [--update]
hgctl context <repo-path> --client <claude|codex>
hgctl recall <query> --client <claude|codex>
hgctl feedback <surface-id> --client <claude|codex> \
  --outcome <used|irrelevant|stale|contradicted> --result <rank>
hgctl update
hgctl doctor
hgctl uninstall
hgctl version
```

Hooks always return success to the calling agent. A local disk, Git, network,
Basic Memory, or update failure is reported to diagnostics and retried by the
periodic sync path; it never blocks Claude Code or Codex.

## First run

Prerequisites are Git, an authenticated GitHub CLI for this private instance,
and Basic Memory. On macOS and Ubuntu, install the recall helper in its own
uv-managed environment:

```sh
# macOS
brew install git gh uv
uv tool install basic-memory

# Ubuntu
sudo apt-get update
sudo apt-get install -y git gh curl
curl -LsSf https://astral.sh/uv/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
uv tool install basic-memory
```

Run `gh auth login` once if GitHub CLI is not already authenticated. The active
account must be able to read the private Hourglass repository and dispatch its
Actions workflows. Basic Memory's isolated Python environment is replaceable
local index machinery; `hgctl` itself is one static Go binary and never invokes
a project venv.

Download the release asset matching the machine and verify the published
checksum:

```sh
case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) exit 1 ;; esac
case "$(uname -m)" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) exit 1 ;; esac
asset="hgctl_${os}_${arch}"
gh release download --repo x2x3studio/hgctl --pattern "$asset" --pattern checksums.txt
if command -v sha256sum >/dev/null; then
  grep "  ${asset}$" checksums.txt | sha256sum -c -
else
  grep "  ${asset}$" checksums.txt | shasum -a 256 -c -
fi
chmod 755 "$asset"
./"$asset" install --repo git@github.com:x2x3studio/hourglass.git
```

On the first machine only, bootstrap the current checkout of the old vault:

```sh
./"$asset" install \
  --repo git@github.com:x2x3studio/hourglass.git \
  --import /path/to/old-vault
```

Every later machine uses the first command without `--import`. Open
`~/hourglass-vault` in Obsidian as a view and leave Obsidian Git sync disabled.
If this is a fresh Hourglass repository, `install` first detects that `shared`
is absent, uses authenticated `gh` to dispatch `bootstrap.yml` from the
repository default branch, and polls for at most five minutes. The trusted
self-hosted workflow creates the initial product; `hgctl` only fetches it and
never commits or pushes `shared`. Concurrent first installs are safe because
the workflow is serialized and NOOPs after the branch appears.
`hgctl` asks Codex's official app-server to discover and trust only the three
exact user hooks it just installed; it never computes a trust hash or edits
Codex TOML itself. Installation reports failure if Codex cannot prove the hooks
are enabled and trusted, but only after the scheduler, imports, and initial Git
sync have completed safely. Background sync retries that exact trust operation
at most once every six hours; `hgctl doctor` repeats discovery read-only.

`install` has two phases:

1. **Read initialization:** clone the control branch, request bounded server-side
   bootstrap if `shared` is missing, create the shared and queue worktrees,
   fast-forward `shared`, configure Basic Memory, start the recovery scheduler,
   and then install the client adapters.
2. **Backfill:** after the recovery path is active, discover existing durable
   agent memory and optionally import an existing vault's current checkout.
   Imported Markdown enters the same machine queue as every later event; it
   never writes `shared` directly.

Imports are deterministic, bounded, resumable, and do not inspect source Git
history. A second machine initializes its own sources but does not replay the
global vault bootstrap.

Existing raw Claude Code and Codex session stores can be hundreds of megabytes
or more and have unstable schemas. They are deliberately not bulk-imported.
Prospective hooks capture a bounded user/assistant turn without tool output.

## Client integration

Each installed client receives `SessionStart`, `UserPromptSubmit`, and `Stop`
hooks:

`hgctl` configures only the clients present on that endpoint. Claude Code,
Codex Desktop/CLI, or both are valid; an absent optional client never makes
installation or diagnostics fail.

- `SessionStart` performs a bounded pull and tells the agent that Hourglass is
  available through the Basic Memory project `hourglass`.
- `UserPromptSubmit` atomically stages the bounded prompt for the current turn.
- `Stop` pairs the prompt with the bounded assistant response and emits one
  queue event.

Claude Code and current Codex releases share the same hook event format. Codex
trust is maintained through `initialize` -> `hooks/list` ->
`config/batchWrite` -> `hooks/list`; ambiguous, duplicate, modified, or
missing hooks fail closed.
Ubuntu setup also verifies systemd user lingering; if the account cannot enable
it directly, the installer fails with one exact `sudo loginctl enable-linger`
command instead of pretending background sync will survive logout.

## Recall

`recall` invokes `basic-memory tool search-notes` in local, entity-only FTS mode
using the persisted external project ID. Basic Memory supplies only ordered
candidate paths. `hgctl` rejects unprovable candidates, resolves every displayed
blob and note body from one exact indexed `shared` commit, skips only `Home.md`
and `Hourglass.canvas`, and stores a bounded seven-day surface receipt. It never
stores the query, scores, snippets, or rendered response.

An explicit verified empty lookup automatically queues `zero_hit`; an empty
SessionStart lookup does not. Agents can attach one terminal outcome to a
surfaced rank. The event ID deliberately excludes outcome and rank, so the first
assessment wins and identical retries replay the same bytes. Feedback queue
commits are v2-only, pending v1 evidence is delivered first, and expired local
feedback is pruned before sync.

Published feedback aggregates bind an exact memory path and Git blob. A closed,
canonical shard may conservatively reorder Basic Memory results by adjacent
swaps, with no card moving more than two positions. A missing shard means no
signal; any malformed required shard disables all feedback reranking for that
lookup without hiding the verified results.

The MVP deliberately does not register Basic Memory's full MCP server because
that surface also exposes write, edit, and delete tools; those would violate
Dream's single-writer boundary. Semantic indexing stays disabled and Basic
Memory's SQLite FTS cache remains disposable local state outside Git. After
`shared` advances, background sync runs an incremental reindex before recall.

## Updates

Release artifacts are built by GitHub Actions for macOS and Linux on amd64 and
arm64. `update` uses the required authenticated `gh` CLI, verifies the release
checksum, installs into a new version directory, and atomically retargets the
stable symlink.

The scheduler checks frequently during the MVP. A missing release or temporary
GitHub failure is a NOOP and does not affect sync.

Queue delivery also sends a best-effort GitHub repository dispatch so Dream can
run from the trusted default branch without executing workflow files from a
queue branch. The scheduled Dream run is the recovery path when a machine has
Git access but no authenticated `gh` command.

## One-time migration from the archived prototype

Remove the archived `kbctl` hook and scheduler with its own uninstall command
before installing `hgctl`. If that binary is already gone, remove only entries
whose command names `kbctl` and the legacy `com.chinaboard.kb.sync` LaunchAgent;
do not delete the old vault until its current checkout has been imported. The
new installer never claims or deletes an unrelated hook, Basic Memory project,
binary, or scheduler entry. Once import reaches the queue, the old vault is no
longer part of the live system.

The prototype also exposed Basic Memory's write-capable MCP server. Remove only
that old `basic-memory` client entry after confirming that it targets the
archived project (for Claude Code: `claude mcp remove basic-memory -s user`).
Keep the old Basic Memory project itself until Dream has published the imported
queue and the new `hourglass` project can recall it.

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

CI and release jobs target the trusted organization runner pool with labels
`self-hosted`, `Linux`, `X64`, and `x2x3studio-paas`.
