# AGENTS.md - hgctl engineering contract

`hgctl` must remain a small transport binary. It serves agents; it is not a
memory server, search engine, or local Dream implementation.

The Hourglass Project consists of exactly two repositories: this endpoint
binary and the companion `x2x3studio/hourglass` control-plane repository.
Changes must preserve that ownership boundary rather than treating either
repository as the whole Project.

## Repository language

Keep all source code, comments, tests, documentation, workflows, filenames,
repository metadata, and commit messages in English. Use ASCII escapes and
language-neutral code points when a test needs non-ASCII data; never add
Chinese literals to this repository.

## Invariants

- Use Go and the standard library. Do not add Python, a virtual environment, a
  package-manager runtime, or a resident application daemon.
- Support macOS and Ubuntu. Do not assume hostname is stable identity.
- Use `~/.local/bin/hgctl` as the only command path placed in hooks or a
  scheduler.
- Use exactly one macOS LaunchAgent label:
  `com.x2x3studio.hgctl.sync`.
- Hooks are fail-open, bounded, and safe under concurrent calls.
- Only `sync` writes Git. It pushes the endpoint queue and fast-forwards the
  shared worktree; it never commits to `main` or `shared`.
- Keep local outbox writes atomic. Use an OS-released advisory lock for Git
  sync so process death cannot leave a logical deadlock.
- Never scan source Git history during import. Never bulk-copy raw session
  transcript stores or tool output.
- Basic Memory is an external recall dependency. Do not recreate its index,
  embeddings, or MCP server. Reindex its disposable cache only after `shared`
  actually advances.
- Recall trusts only Basic Memory's entity paths, then resolves content and
  blobs from one exact indexed `shared` commit. Never surface cached snippets.
- Store bounded seven-day recall receipts without queries or prose. Feedback
  is receipt-bound, first-writer-wins, and may reorder a result by at most two
  positions through exact card-version aggregates.
- Wake Dream with a best-effort repository dispatch after a queue push; the
  scheduled workflow is the recovery path, not a second publisher.
- Maintain Codex hook trust only through the official app-server. A failed
  install attempt is visible but cannot strand imports or Claude capture;
  background repair is persisted and low-frequency.
- Auto-update must be atomic, checksum-verified, and unable to break the
  currently running binary.

## Configuration safety

Preserve unrelated Claude Code, Codex, Basic Memory, LaunchAgent, and systemd
configuration. Setup and uninstall identify only entries whose command is the
stable `hgctl` path. Repeated setup is idempotent.

Uninstall removes integration and scheduler files but preserves
`~/hourglass-vault`, outbox data, and machine identity unless a future explicit
purge command says otherwise.

## Protocol

The companion Hourglass repository owns the branch contract and the single
closed `hourglass.event/v1` wire protocol in `protocol/event.md`. Its event
kinds are exactly `observation`, `turn`, `import_batch`, and receipt-bound
`feedback`; neither envelope nor payload extensions are accepted. One queue
commit may mix any valid kinds while retaining the shared count and byte
limits. Unsupported event schemas are invalid, not deferred compatibility
work. Changing fields, IDs, branch ownership, or cursor semantics requires an
explicit coordinated change across both Project repositories.

## Verification

Every behavior change needs focused tests. At minimum cover:

- deterministic IDs and import batching;
- UTF-8 and size bounds;
- atomic outbox and concurrent hook calls;
- prompt/response pairing and retry;
- machine identity persistence across hostname changes;
- Git queue idempotence and failed-push recovery;
- exact shared commit/tree/blob recall and Basic Memory JSON validation;
- feedback identity, receipt expiry, first-writer-wins, and bounded reranking;
- mixed-kind queue batching, feedback expiry, and interrupted recovery;
- hook config merge/uninstall without damaging unrelated entries;
- Codex hook discovery and trust through the official app-server only;
- scheduler label/path stability;
- update checksum and atomic symlink replacement;
- fail-open behavior when optional commands are absent.

Run `gofmt`, `go test ./...`, and `go vet ./...` before committing.
