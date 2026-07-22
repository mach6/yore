# yore architecture

This is the design doc: how the pieces fit and why. Exact wire bytes — the sync
HTTP API and the E2E crypto scheme — live in **[`protocol.md`](protocol.md)**;
user-facing setup lives in the **[README](../README.md)**.

## Overview

`yore` is one static, **CGO-free** Go binary. A cobra subcommand selects which
role it plays:

| Role | Subcommands |
|---|---|
| Shell-hook fast path | `record`, `filter`, `export` |
| Search UIs | `search` (inline Ctrl-R TUI + `--headless`), `browse` (full-screen) |
| Background daemon | `daemon` (`run`/`stop`/`status`), `status`, `stop`, `sync` |
| Enrollment / devices | `setup`, `devices` (`approve`/`revoke`) |
| Sync server | `server` (`stop`), `healthcheck` |
| Setup / misc | `init`, `import`, `doctor`, `gen-id`, `version` |

Because everything is pure Go with `CGO_ENABLED=0`, it cross-compiles with no
toolchain to the tier-1 matrix: **linux/amd64, linux/arm64, darwin/amd64,
darwin/arm64, freebsd/amd64** (WSL runs the linux builds).

## Data flow & the sacred prompt path

```
                    redact gate (leading-space / ignore-dirs / secret rules)
                          │ drop (silent, exit 0)
shell hook ─(append,<1ms)─┴▶ spool file ─▶ ┌─ yore daemon (unix socket) ─────────┐
                                           │  owns local bbolt (this host only)  │
Ctrl-R / h / hs / browse ◀── unix sock ───▶│  RAM corpus (warm gob snapshot+tail)│◀─HTTPS─▶ yore server
   (thin TUI clients)                      │  RAM remote cache (never on disk)   │  (bbolt per tenant:
                                           │  background sync loop (push/pull)   │   ciphertext records,
                                           └─────────────────────────────────────┘   wrapped keys, pubkeys)
```

The prompt path is deliberately trivial and is the system's most important
invariant. On every command the shell hook runs `yore record`, which:

1. reads the command text on stdin (capped at 1 MiB);
2. drops it if empty;
3. runs the **redact gate** (below) — a rejection returns silently, exit 0;
4. appends one JSON line to this process's spool file and **fsyncs** it;
5. best-effort pokes the daemon over the unix socket (spawning one if absent),
   with tight deadlines (~150 ms worst case, and `record` runs backgrounded);
6. exits 0.

No database, no network, no crypto touches the prompt path. If the daemon is
dead the record still lands durably in the spool and is drained at the next
daemon start — recording survives a dead (or never-started) daemon.

## Packages (`internal/`)

Concise map by role. Leaf-contract packages import nothing else in the tree.

**Leaf contracts:**
- **rec** — the `Record` type. Its JSON is the encoding for both the spool and
  (as the sealed plaintext) the sync payload. ULID `id`; `type` `""`(command) or
  `"delete"`(tombstone).
- **proto** — the newline-delimited-JSON protocol over the daemon's unix socket.
- **wire** — the JSON types of the server's HTTP API.
- **config** — resolves the single state dir (`$YORE_DIR`, else `~/.config/yore/`)
  and all settings, applying defaults via accessor methods.

**Storage & capture:**
- **spool** — crash-safe, fsync'd, per-process (`<pid>.jsonl`) append files;
  `Drain` tolerates a torn final line and dedupes downstream by id.
- **store** — local bbolt (`data.db`). Holds only this host's stream. Single-owner
  (an exclusive file lock; a competing opener gets `ErrLocked`). Idempotent
  appends keyed by record id; assigns per-stream `seq`; tombstones for deletes;
  `BackupTo` is a hot online snapshot.

**Runtime & search:**
- **daemon** — the only process that opens the store; see below.
- **match** — whitespace-split substring terms (smart-case) with an incremental
  prefix-reuse `Filter`, plus a subsequence **fuzzy** matcher.
- **tui/theme**, **tui/hl** — adaptive lipgloss styles; a best-effort shell-command
  syntax classifier layered under match highlighting.
- **tui/search** — the inline Ctrl-R panel. **tui/browse** — the full-screen
  browser (hosts / table / detail / stats / devices panes).

**Security & sync:**
- **cryptobox** — the E2E core (device X25519+Ed25519 keys, History Key, epoch
  DEKs, record AEAD). See "Key hierarchy" below and `protocol.md` for the bytes.
- **reqsign** — the shared canonical string + headers a device's Ed25519 key
  signs on every mutating sync request. One source of truth for client & server.
- **redact** — the recording gate: never spool/store/sync a command carrying a
  secret. Runs on live capture, on import, and on the shell-history gate.
- **server** — the multi-tenant sync server; stores only ciphertext + device
  public keys. See "The sync server" below.
- **syncer** — the client engine: encrypt+push local records, pull+decrypt
  remote ones, and the device enroll / approve / revoke+rotate primitives.

**Integration:**
- **shell** — the embedded zsh/bash hook scripts (+ vendored bash-preexec),
  rendered per integration mode via `text/template`.
- **importer** — zsh (extended-history, unmetafy, multiline) and bash parsers.
- **cli** — the cobra command tree; each subcommand is a `runXxx` returning a
  process exit code; ships shell completions.

## The daemon

A long-lived background process, auto-spawned on first use (detached, `Setsid`)
and idle-exiting after `daemon_idle` (default 30m). It is the sole owner of the
local bbolt store and the authority for all search.

- **RAM corpus.** On start it loads the live search corpus from a warm **gob
  snapshot** (`corpus.snap`) and folds in the store tail above the snapshot's
  position via `store.Since`; a missing/corrupt snapshot falls back to a full
  `store.All()` cold load. The corpus is append-only and guarded by an RWMutex;
  readers snapshot the slice header under RLock and scan lock-free. A snapshot is
  re-written every 5 min and once more on shutdown, so restarts paint instantly.
- **RAM-only remote cache.** Other hosts' history is pulled as ciphertext,
  decrypted, and held in RAM **only** — never written to disk — and re-pulled
  from empty cursors each daemon lifetime.
- **Debounced ingest.** A spool "wake" starts a 75 ms straggler window, then one
  fsync'd `IngestSpool` drain folds new rows into the corpus.
- **Concurrency.** One goroutine per socket connection (serves that connection's
  requests in order, so its per-connection `match.Filter` stays single-threaded);
  a single ingest goroutine; the main goroutine owns the idle timer and shutdown.
- **The unix-socket op table** (`~/.config/yore/daemon.sock`, 0600, one JSON
  object per line, one Response per Request):

| Op | Purpose |
|---|---|
| `ping` | liveness + reset idle timer + nudge spool ingest |
| `record` | spool one record (fsync) + nudge ingest — same durability as the CLI |
| `query` | search (scope, sort, fuzzy, tag, dedupe, paging) |
| `hosts` | per-host live-record counts (browse sidebar); warms the remote cache |
| `delete` | tombstone one record by id (syncs as a tombstone) |
| `devices` | list enrolled devices (proxied to the syncer) |
| `ticket` | mint a single-use enrollment ticket (proxied to the syncer) |
| `approve` / `revoke` | approve a pending device / revoke+rotate keys |
| `sync` | force a **synchronous** push/pull cycle (backs `yore sync`) |
| `status` | daemon status (pid, uptime, local rows, remote state, version) |
| `shutdown` | graceful exit (also `yore daemon stop` / `yore stop`) |

Exact request/response JSON for these ops is in `protocol.md`.

## Search model

Every query is `match → scope filter → optional executor(tag) filter → sort →
dedupe → window`. Matching is substring (default, smart-case) or **fuzzy**
(subsequence); the per-connection incremental filter is used for substring,
skipped for fuzzy. Scopes:

- **local** (shallow) — this host only; always available, offline-safe.
- **all** / **host** (deep) — merge the RAM remote cache. A deep query (and
  opening `browse`) nudges a background sync (`syncWake`) so the *next* query is
  richer; the request itself never blocks on the network. Offline, deep
  degrades to whatever is already cached (or just local).
- **session** / **cwd** — filter by shell session id or exact working directory.
- **workspace** — commands run anywhere under the current git repo (walk up for
  `.git`; local-only).

Sort is **recency** (descending `start_ms`, ties by descending `seq`) or
**frecency** (frequency × bucketed-recency weight with a same-cwd ×2 boost, which
inherently collapses to one row per command). Recency sort can also `dedupe`
(newest wins). The default window is 200 rows.

## Recording & redaction

`yore record`, `yore import`, and the shell-history gate (`yore filter`) all run
the same ordered gate before anything is persisted:

1. **Leading-space opt-out** (`histignorespace`): a command starting with space
   or tab is skipped, unless `record_space_prefixed` is true.
2. **Ignore-dirs**: if the command's cwd is at or under a configured
   `ignore_dirs` prefix (segment-aware) it is skipped.
3. **Secret rules** (`internal/redact`): rules load from the editable, seeded
   `~/.config/yore/redact.yml`; each rule is a name + Go regexp + optional cheap
   literal `hints` (a hot-path pre-filter — the regex only runs if a hint is
   present) + optional case-`fold`. Built-ins anchor on the *shape* of a secret
   (AWS AKIA/ASIA + secret-key, GitHub `ghp_`/`github_pat_`, Slack `xox*`, PEM,
   JWT, URL userinfo, tool password flags for openssl/gpg/sshpass/mysql/mongo/
   smbclient/curl/wget, and generic `token=`/`secret=`/`password=` assignments)
   plus any user `ignore_patterns`.

A rejected command is dropped silently (never spooled) — the gate never explains
why, since that would itself leak that a secret was typed. Redaction is
**fail-safe**: a missing, unreadable, unparseable, or empty `redact.yml` falls
back to the compiled-in built-ins (never "redact nothing"); an individual invalid
regex is skipped with a warning while the rest stay active. `yore setup` seeds
`redact.yml` from the built-ins without ever clobbering edits.

**Executor tagging.** Each record carries a `tag` naming what ran it: an explicit
`--tag`, else `$YORE_TAG`, else auto-detection from agent env markers
(`CLAUDECODE`/`CLAUDE_CODE_ENTRYPOINT` → `claude-code`, `CURSOR_TRACE_ID` →
`cursor`, `AIDER_MODEL` → `aider`, etc.), else `""` (interactive). Search can
filter by tag, separating "what I typed" from "what an agent ran".

An agent whose commands run in a *non-interactive* shell (Claude Code's Bash
tool is `zsh -c …`) is never seen by the rc hooks, so `yore init claude-code`
installs Claude Code hooks: **PostToolUse** pipes each Bash command to
`yore hook claude-code`, and **UserPromptSubmit** pipes each prompt to
`yore hook claude-prompt`. The prompt hook writes the session's current prompt to
a per-session state file; the command hook reads it and stamps `prompt_id` +
`prompt` onto the record, so every command is traced to the prompt that triggered
it. The browser's **prompt explorer** (`p`) groups on `prompt_id` — one row per
prompt, `Enter` drilling into the exact command sequence it produced. Both hooks
go through the same redaction gate as the shell path (a secret-bearing
command or prompt is dropped). `yore init claude-code` writes ~/.claude/settings.json
(or, with --project, ./.claude/settings.json), merging without disturbing other settings.

## Shell integration modes

`config.integration` (default `takeover`, overridable per `yore init --mode`)
controls how deeply the emitted hooks take over the shell, all via `yore init`:

- **takeover** — yore is the single source of truth. The shell's persistent
  history is disabled (no unredacted `~/.zsh_history`); its in-memory list is
  seeded from yore (`yore export --shell`, one `fc -R` / `history -r` at startup)
  and gated by yore's redaction (`yore filter` from zsh's `zshaddhistory`, best-
  effort in bash), so `!N` / up-arrow work against yore-consistent, secret-free
  history. zsh is exact; bash is coarser (multiline collapses to one line).
- **coexist** — record alongside the untouched native history; rebind Ctrl-R and
  add the aliases. Native `!N` works against native history.
- **capture** — record only; no keybinding or alias changes.

**Mechanics.** zsh installs `zshaddhistory` (calls `yore filter`, returns nonzero
to drop) and rebinds `^R` (and optionally Up, `bind_up_arrow`) to a widget that
runs the search TUI. bash uses vendored bash-preexec for capture and a best-
effort gate. Scoped convenience aliases (unless `--no-aliases`): `hb` (browse,
drop pick onto the next prompt), `hs` (search this host), and siblings `hsa`
(all hosts), `hss` (session), `hsc` (cwd), `hsw` (workspace). `yore` never
rebinds `h`. Two support subcommands back this: `yore filter` (reads a command
on stdin, exits 1 to drop) and `yore export --shell [--format zsh|bash]` (the
history seed) — neither is on the prompt fast path (`filter` runs synchronously
from `zshaddhistory` in single-digit ms; `export` runs once per shell start and
is best-effort: no daemon means it prints nothing and exits 0).

## Key hierarchy & E2E (summary)

```
device X25519 + Ed25519 keypairs   per machine; private halves never leave it
  │ X25519 seals ─▶ History Key (HK)   one 32B symmetric key per group, stored
  │                                     only as per-device wrapped blobs (no master)
  │ HK wraps    ─▶ epoch Data Keys      32B, one per epoch (default 24h), wrapped under HK
  │ DEK seals   ─▶ history records      each sealed XChaCha20-Poly1305, AAD-bound
  └ Ed25519 signs ─▶ mutating sync requests (reqsign)
```

- **Read**: one asymmetric HK unwrap per daemon lifetime; then every DEK and
  record opens symmetrically. Cost is **O(1) in history age**.
- **Enroll**: an existing device wraps HK for the newcomer's pubkey — one wrap.
- **Revoke**: rotate to a new HK, re-wrap the (few) DEKs and HK for surviving
  devices — **records are never re-encrypted**. Also O(1) in history age.
- **AAD** binds every sealed record to `recordID|hostID|seq|keyID`, so a
  compromised server cannot reorder, replay, or substitute blobs undetected.
  Decryption failure is fatal to a pull (never silently skipped).
- **No shared secret at all.** A device authenticates with its Ed25519 key on
  every request, reads included; there is no bearer token to capture. The
  server's configured token is only an *enrollment ticket for an empty group* —
  once any device is active it enrolls nothing, and every later machine needs a
  single-use ticket minted by one already enrolled.
- **Recovery.** Per-device keys mean losing every device would otherwise lose
  the archive for good, so bootstrap also seals HK to a key derived (Argon2id)
  from a one-time recovery phrase shown once and never stored. The server holds
  only the salt, the recovery public keys, and that wrap — still nothing that
  can decrypt history.
- **Request signing** (`reqsign`) means a captured request can't push
  garbage or revoke a device; optional TLS cert pinning (`yore setup --pin`)
  hardens against a TLS-inspecting proxy, fail-closed.

Exact byte layouts, domain-separation strings, and the device.key format are in
`protocol.md`.

## Sync & eventual consistency

Streams are **append-only, per-host, client-sequenced**; merge is a set-union by
record ULID; deletes are appended tombstones — so sync is conflict-free and
eventually consistent by construction.

The daemon's sync loop (started only when a server is configured) is driven by:

- an initial warm sync ~2 s after startup (so deep search is warm quickly);
- a periodic tick every `sync_interval` (default **5m**);
- `syncWake` — a deep read or opening `browse` nudges a background cycle;
- **EXPERIMENTAL** `push_debounce` (default **off**): setting a duration arms a
  coalesced push shortly after new records are ingested (`pushWake`), so
  cross-host propagation is seconds rather than up to `sync_interval`. Off by
  default because an agent firing command bursts would push about once per
  debounce for its whole run. The eager nudge fires only while the server is
  reachable — when it is offline, new records stay spooled in the local store
  and go out in a batch when the next periodic tick reconnects, rather than
  firing an eager push that would only fail.

`yore sync` (and `S` in the TUIs) runs a **synchronous** cycle via `OpSync` and
reports the real outcome. Every cycle is `Push` then `PullOthers`, serialized by
a mutex so the periodic loop and an explicit sync never overlap. **Push** uploads
local records above a persisted watermark (`last_uploaded_seq`) in ascending
batches of ≤1000, advancing the watermark per acked batch (a failed batch is
retried, never skipped). **Pull** walks each other host's stream from an
in-RAM per-host cursor, decrypting into the remote cache; cursors are RAM-only,
so a fresh daemon re-pulls from `after=0`. Remote plaintext exists only in RAM.

## The sync server

Multi-tenant and sharded. Each tenant is an isolated bbolt file owned solely by
the server process; the identity that signed a request selects the tenant every
handler operates on.

- **Device → tenant.** The auth middleware finds the tenant holding the signing
  device's record (ids are globally-unique ULIDs, so at most one matches) and
  caches the mapping. Enrollment routes by ticket, recovery by the tenant that
  has recovery material. Bootstrap tokens are still compared constant-time against
  every configured token (no early break — a match leaks nothing about which or
  how many tenants exist) and binds that tenant's db into the request context. No
  match → `401`. A request that somehow reaches a handler with no bound tenant
  fails `500` rather than touch another tenant's data (fail-closed; no shared db).
- **Sharding.** The **default** tenant (the single `$YORE_TOKEN` / `--token` /
  `$YORE_TOKEN_FILE`) uses the server's `--db` path unchanged — a single-token
  server is exactly as before. Named tenants come from `$YORE_TOKENS_FILE` (a
  JSON `{"name":"token", …}`) and live at `<dir(--db)>/tenants/<name>.db`; names
  are restricted to `[A-Za-z0-9_-]+` and `default` is reserved. Duplicate tokens
  are rejected at startup (two tenants sharing a token would be indistinguishable).
- **Storage** is ciphertext + device public keys only; the server can never
  decrypt. **The client and wire protocol are unchanged by multi-tenancy.**
- **Per-tenant rolling backups.** With `$YORE_BACKUP_INTERVAL` > 0 (default 1h;
  `"0"` disables) a single goroutine writes a consistent online snapshot of every
  tenant db to `<dir(--db)>/backups/<tenant>/data-<unixMillis>.db` (temp file +
  atomic rename), pruning to the newest `$YORE_BACKUP_KEEP` (default 3).
- Exactly one server replica (bbolt is single-owner); TLS terminates at your
  reverse proxy. `GET /v1/health` is open and backs the container HEALTHCHECK.

## Durability & ops

- **Local rolling backups.** The daemon writes a consistent `store.BackupTo`
  snapshot of `data.db` to `~/.config/yore/backups/data-<unixMillis>.db` every
  `backup_interval` (default 1h; `"0"` disables), keeping the newest
  `backup_keep` (default 3) — same temp-file + atomic-rename + prune scheme as
  the server.
- **Bounded daemon log.** `~/.config/yore/daemon.log` rotates once it would
  exceed `log_max_size` (default 5MB; `"0"` = unbounded append), keeping
  `log_keep` old segments (default 1). `log_silent` (default **true**) suppresses
  logging entirely — no file is created — so a fresh install writes no
  `daemon.log`; set it `false` to get the rotating log for debugging. A log-open
  failure is non-fatal — the daemon runs without logging rather than refusing to
  start.
- **Warm snapshots** (`corpus.snap`) keep restarts instant; they are derived data,
  always rebuildable from `data.db`, so a bad snapshot just triggers a full load.

## Single-directory footprint

All client state lives under `~/.config/yore/` (or `$YORE_DIR`); uninstall is one
`rm -rf`. The one deliberate exception is secret material: when a usable OS
keyring exists, the device key is stored there instead of in the directory, so a
full uninstall also drops the `yore` keyring entries. `$YORE_SECRET_BACKEND=file`
forces everything back into the directory. The files:

| Path | What |
|---|---|
| `config.toml` | settings (0600) |
| `redact.yml` | editable, seeded secret-redaction rules (0600) |
| `data.db` | local bbolt store — this host's history only |
| `device.key` | device X25519+Ed25519 identity — kept in the **OS keyring** when one is usable, else this file (0600; refused if group/other-readable) |
| `spool/<pid>.jsonl` | crash-safe capture handoff, drained by the daemon |
| `daemon.sock` | daemon control socket (0600) |
| `corpus.snap` | warm-start corpus gob snapshot (derived) |
| `daemon.log` | bounded daemon log (+ rotated segments) |
| `backups/` | rolling local `data.db` snapshots |

## Config (`~/.config/yore/config.toml`)

A plain [TOML](https://toml.io) file. Read and written by hand, or through
`yore get-config <key>` and `yore set-config <key> <value>` — the one
authoritative accessor pair (`config.Get`/`config.Set`). Nothing else parses the
file: the emitted shell integration, for instance, asks `yore get-config
enter_executes` at call time rather than grepping. Zero values mean "use
default"; accessors apply defaults so callers never branch.

| Key | Default | Meaning |
|---|---|---|
| `server_url` | — | sync server base URL; empty = local-only |
| `token_file` | — | path to a file holding an enrollment ticket, for setups that manage it externally |
| `server_pin` | — | pinned server TLS SPKI (base64 SHA-256); set by `setup --pin` |
| `integration` | `takeover` | `takeover` \| `coexist` \| `capture` |
| `key_epoch` | `24h` | DEK epoch width |
| `daemon_idle` | `30m` | daemon idle timeout before exit |
| `sync_interval` | `5m` | periodic push/pull tick |
| `push_debounce` | off | **EXPERIMENTAL** coalesced push-on-record delay; empty/`0`/invalid = disabled |
| `auto_deepen` | `true` | let deep reads nudge a background sync |
| `enter_executes` | `true` | Ctrl-R Enter runs the result (vs. insert for review) |
| `bind_up_arrow` | `false` | also bind Up to the search TUI |
| `keymap` | `emacs` | `emacs` \| `vim` TUI key style |
| `ignore_patterns` | — | extra user secret regexes (never recorded) |
| `ignore_dirs` | — | cwd prefixes whose commands are never recorded |
| `record_space_prefixed` | `false` | record leading-space commands too |
| `capture_spool_only` | `false` | capture writes to the spool only — never pokes/spawns the daemon; the spool is drained the next time a daemon runs |
| `backup_interval` | `1h` | local db backup cadence; `"0"` disables |
| `backup_keep` | `3` | local db backups retained |
| `log_max_size` | `5MB` | daemon.log rotation threshold; `"0"` = unbounded append |
| `log_keep` | `1` | rotated daemon.log segments kept |
| `log_silent` | `true` | suppress daemon logging entirely (no `daemon.log`); set `false` to log for debugging |

Defaults are applied the plain-Go way: `config.Load` starts from `config.Defaults()`
and decodes the TOML file over it, so an omitted key keeps its default and an
explicit value — including `false` or `0` — overrides it. Booleans that default to
`true` (`auto_deepen`, `enter_executes`, `log_silent`) are written without
`omitempty` so an explicit `false` round-trips; there are no `*bool` "was it set?"
fields. String-backed durations/sizes are stored verbatim and parsed by typed
accessors that fall back to the default on a malformed value.

Server-side settings are env vars, not config.toml: `$YORE_TOKEN` /
`$YORE_TOKEN_FILE`, `$YORE_TOKENS_FILE`, `$YORE_BACKUP_INTERVAL`,
`$YORE_BACKUP_KEEP`.

## Invariants (do not break)

- The prompt path never does DB, network, or crypto work; recording survives the
  daemon being down (the spool is drained at next start).
- The local DB holds only this host's history. Remote history is decrypted into
  daemon RAM only, re-pulled per daemon lifetime, never written to disk.
- Streams are append-only, per-host, client-sequenced; merge is a ULID set union;
  deletes are tombstones — eventually consistent, conflict-free.
- Every hot path (search, decrypt, enroll, revoke) is **O(1) in history age**.
- The server only ever holds ciphertext, wrapped keys, and device public keys;
  mutating requests are per-device signed; tenants never share a db.
- All client state lives under `~/.config/yore/`; uninstall is one `rm -rf`.
</content>
</invoke>
