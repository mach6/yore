# yore architecture

One binary, selected by subcommand (via cobra): client, background daemon, TUIs,
importer, and sync server. Everything is pure Go and CGO-free, so it cross-
compiles to linux/darwin/freebsd × amd64/arm64 with no toolchain.

## Data flow

```
                    redact gate (secrets / ignore-dirs / leading-space)
                          │ drop
shell hook ─(append,<1ms)─┴▶ spool file ─▶ ┌─ yore daemon (unix socket) ─────────┐
                                           │  owns local bbolt (this host only)  │
Ctrl-R / h / hs ◀── unix socket ──────────▶│  RAM corpus (warm gob snapshot)     │◀─HTTPS─▶ yore server
   (thin TUI clients)                      │  RAM remote cache (never on disk)   │  (bbolt: ciphertext
                                           │  background sync loop (push/pull)   │   records, wrapped
                                           └─────────────────────────────────────┘   keys, device pubkeys)
```

The prompt path is `record` → redact gate → one fsync'd spool append → a
best-effort daemon poke, then exit 0. No DB, network, or crypto on that path;
recording survives the daemon being down (the spool is drained at next start).

## Packages (`internal/`)

**Leaf contracts** (import nothing else in the tree):
- **rec** — the `Record` type; its JSON is the on-wire/on-disk encoding.
- **proto** — the newline-delimited-JSON protocol over the daemon's unix socket.
- **wire** — the JSON types of the server's HTTP API (see `protocol.md`).
- **config** — resolves the single state dir (`~/.config/yore/`) and settings.

**Storage & capture:**
- **spool** — crash-safe, fsync'd, per-process append files handed to the store.
- **store** — local bbolt; holds only this host's stream. Single-owner (the
  daemon). Idempotent batched appends keyed by record ULID; tombstones for deletes.

**Runtime & search:**
- **daemon** — the only process that opens the store. Loads the corpus into RAM
  (from a warm gob snapshot, then folds in the tail via `store.Since`), serves
  search over the socket, debounces spool ingestion, re-snapshots periodically,
  and (when sync is configured) runs the push/pull loop + holds the RAM remote
  cache. Search filtering runs daemon-side over an append-only corpus with a
  lock-free read path.
- **match** — the matcher: whitespace-split substring terms with smart-case and
  an incremental (prefix-reuse) filter, plus a subsequence **fuzzy** matcher.
  Shared by the daemon and headless search.
- **tui/theme** — one pre-built lipgloss style set (adaptive light/dark).
- **tui/hl** — a best-effort shell-command syntax classifier (command / flag /
  string / path / operator / variable), layered *under* match highlighting.
- **tui/search** — the inline Ctrl-R panel. **tui/browse** — the full-screen
  browser (hosts / table / detail / stats / devices panes).

**Security & sync:**
- **cryptobox** — the E2E core (device X25519 + Ed25519 keypairs, History Key,
  epoch data keys, record AEAD). See "Key hierarchy" below.
- **reqsign** — the shared request-signing contract: the canonical string a
  device's Ed25519 key signs on every mutating sync request (so a captured token
  can't push or revoke). Used identically by client and server.
- **redact** — the recording gate: never spool/store/sync a command that carries
  a secret. Runs on live capture, on import, and on the shell-history gate.
- **server** — the sync server; stores only ciphertext + device public keys.
- **syncer** — the client engine: encrypt+push local records, pull+decrypt
  remote ones, and the device enroll / approve / revoke+rotate primitives.

**Integration:**
- **shell** — the embedded zsh/bash hook scripts (+ vendored bash-preexec).
- **importer** — zsh (extended-history, unmetafy, multiline) and bash parsers.
- **cli** — the cobra command tree; ships shell completions. Each subcommand's
  logic is a `runXxx` returning a process exit code (`internal/cli/root.go`).

## The daemon socket protocol (`proto`)

Clients speak newline-JSON over `~/.config/yore/daemon.sock`. Ops:

| Op | Purpose |
|---|---|
| `ping` | liveness + reset idle timer + nudge spool ingest |
| `record` | deliver one record for immediate (durable) ingest |
| `query` | search (scope, sort, fuzzy, tag, dedupe, paging) |
| `hosts` | per-host live-record counts (browse sidebar) |
| `delete` | tombstone one record by id |
| `devices` | list enrolled devices (proxied to the syncer) |
| `approve` / `revoke` | approve a pending device / revoke+rotate |
| `sync` | force a synchronous push/pull cycle |
| `status` | daemon status |
| `shutdown` | graceful exit (also `yore daemon stop`) |

The daemon auto-spawns on first use (detached, `Setsid`), idle-exits after
`daemon_idle` (default 30m), and writes a warm snapshot on shutdown so the next
start paints instantly.

## Search model

Every query is `match → scope filter → optional executor(tag) filter → sort →
dedupe → window`. Scopes:

- **local** (shallow) — this host only; always available, offline-safe.
- **all** / **host** (deep) — merges the RAM remote cache; a deep query nudges a
  background sync so the next query is richer. Offline, deep degrades to local.
- **session** / **cwd** — filter by shell session id or working directory.
- **workspace** — commands run anywhere under the current git repo (walk up for
  `.git`; local-only).

Sort is recency (newest-first) or **frecency** (frequency × recency with a
same-dir boost, which collapses to one row per command). Matching is substring
(default) or **fuzzy** (subsequence). Remote records live only in daemon RAM and
are re-pulled per daemon lifetime — never written to disk.

## Recording pipeline & redaction

`record` (the hook fast path) applies, in order: leading-space opt-out
(histignorespace), ignored-directory check, then the **redact** secret filter
(built-in patterns for AWS/GitHub/Slack tokens, credential flags, connection-
string URLs, PEM, JWT, `TOKEN=`/`SECRET=` assignments, plus user regexes). A
rejected command is silently dropped (never spooled). The same filter runs on
`import`, so bulk-loading old `~/.zsh_history` can't drag secrets in. Each record
is tagged with its **executor** (auto-detected agent env like `CLAUDECODE`, or
`$YORE_TAG`/`--tag`), so history can be filtered by "what I typed" vs "what an
agent ran".

## Shell integration modes

`config.integration` (default `takeover`) controls how deeply the emitted hooks
take over the shell's history, all via `yore init`:

- **takeover** — yore is the single source of truth. The shell's persistent
  history is disabled (no unredacted `~/.zsh_history`); its in-memory list is
  seeded from yore (`yore export --shell`, one `fc -R` / `history -r` at startup)
  and gated by yore's redaction (`yore filter` from zsh's `zshaddhistory`, or a
  `history -d` in bash), so `!N` / up-arrow work against yore-consistent,
  secret-free history. zsh is exact; bash's gate is best-effort.
- **coexist** — record alongside the untouched native history; rebind Ctrl-R,
  add `h`/`hs`. Native `!N` works against native history.
- **capture** — record only; no keybinding/alias changes.

Two subcommands support takeover: `yore filter` (the redaction gate — reads a
command on stdin, exits 1 to drop) and `yore export --shell [--format zsh|bash]`
(the history seed). Neither is on the prompt fast path; `filter` runs
synchronously from `zshaddhistory` (single-digit ms) and `export` once per shell
start.

## Key hierarchy (cryptobox)

```
device X25519 keypair    per machine, private half never leaves it
  │ seals ──▶ History Key (HK)    one 32B symmetric key, only ever stored
  │                               as per-device wrapped blobs on the server
  │ wraps ──▶ epoch Data Keys     per device, per time-epoch, wrapped under HK
  │ seals ──▶ history records     each sealed (XChaCha20-Poly1305) with its DEK
```

- **Read**: one asymmetric HK unwrap per daemon lifetime, then all data keys and
  records open symmetrically (sub-µs). Cost is O(1) in history age.
- **Enroll**: an existing device wraps HK for the newcomer's public key — one wrap.
- **Revoke**: rotate to HK2, re-wrap the data keys (symmetric, fast) and HK for
  surviving devices; **records are never re-encrypted**.
- **AAD**: every sealed record is bound to `recordID|hostID|seq|keyID`, so a
  compromised server cannot reorder, replay, or substitute blobs undetected.
- **Request signing** (`reqsign`): the device's Ed25519 key also signs every
  mutating sync request, so a captured bearer token (e.g. via a TLS-inspecting
  proxy) can't push garbage or revoke a device. Optional TLS cert pinning
  (`yore setup --pin`) hardens further, fail-closed, against such proxies.

## Sync protocol

Append-only per-host streams with **client-assigned** sequence numbers; merge is
a set-union by record ULID; deletions are appended tombstones — so sync is
eventually consistent by construction with nothing to conflict. The client
pushes local records above a persisted watermark and pulls other hosts'
streams via cursors, decrypting into the RAM cache. The full HTTP API (records
push/pull, host discovery, device lifecycle, key distribution + rotation) is
documented in **[`protocol.md`](protocol.md)**.

## Config (`~/.config/yore/config.json`)

`server_url`, `token` / `token_file`, `server_pin`, `integration`
(takeover|coexist|capture), `key_epoch` (24h), `daemon_idle` (30m),
`sync_interval` (5m), `auto_deepen`, `enter_executes`, `bind_up_arrow`, `keymap`
(emacs|vim), `ignore_patterns`, `ignore_dirs`, `record_space_prefixed`.

## Invariants

- The prompt path never does DB, network, or crypto work; recording survives the
  daemon being down.
- The local DB holds only this host's history. Remote history is decrypted into
  daemon RAM only, re-pulled per daemon lifetime, never written to disk.
- Streams are append-only, per-host, client-sequenced; merge is a ULID set union;
  deletes are tombstones — eventually consistent, conflict-free.
- The server only ever holds ciphertext, wrapped keys, and device public keys.
- All client state lives under `~/.config/yore/`; uninstall is one `rm -rf`.
