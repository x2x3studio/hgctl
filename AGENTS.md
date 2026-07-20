# AGENTS.md - hgctl engineering contract

`hgctl` must remain a small transport binary. It serves agents; it is not a
memory server, search engine, or local Dream implementation.

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
  embeddings, or MCP server.
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

The companion Hourglass repository owns the branch and event contract in
`protocol/v1.md`. Keep the Go event encoder compatible with that version. New
fields may be added; changing required fields, IDs, branch ownership, or cursor
semantics requires a new protocol version.

## Verification

Every behavior change needs focused tests. At minimum cover:

- deterministic IDs and import batching;
- UTF-8 and size bounds;
- atomic outbox and concurrent hook calls;
- prompt/response pairing and retry;
- machine identity persistence across hostname changes;
- Git queue idempotence and failed-push recovery;
- hook config merge/uninstall without damaging unrelated entries;
- scheduler label/path stability;
- update checksum and atomic symlink replacement;
- fail-open behavior when optional commands are absent.

Run `gofmt`, `go test ./...`, and `go vet ./...` before committing.
