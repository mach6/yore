# yore architecture

One binary, selected by subcommand: client, daemon, TUI, importer, sync server.
Everything is pure Go and CGO-free.

## Data flow

```
shell hook ─(append, <1ms)─▶ spool file ─▶ ┌─ yore daemon (unix socket) ──────┐
                                           │  owns local bbolt (this host)     │
Ctrl-R / h / hs ◀── unix socket ──────────▶│  + RAM corpus (warm snapshot)     │◀─HTTPS─▶ yore server
   (thin TUI clients)                      │  + RAM remote cache (never disk)  │  (bbolt: ciphertext
                                           └── push local / pull deltas ───────┘   records, wrapped
                                                                                    keys, device pubkeys)
```

## Packages (`internal/`)

Leaf contracts (import nothing else in the tree):
- **rec** — the `Record` type; JSON is the on-wire/on-disk encoding.
- **proto** — the newline-JSON protocol spoken over the daemon's unix socket.
- **wire** — the JSON types of the server's HTTP API.
- **config** — resolves the single state dir (`~/.config/yore/`) and settings.

Storage & capture:
- **spool** — crash-safe, fsync'd, per-process append files handed off to the store.
- **store** — local bbolt; holds only this host's stream. Single-owner (the
  daemon). Idempotent batched appends keyed by record ULID; tombstones for deletes.

Runtime:
- **daemon** — the only process that opens the store. Loads the corpus into RAM
  (from a warm gob snapshot, then folds in the tail), serves search over the
  socket, debounces spool ingestion, re-snapshots periodically. Search filtering
  runs daemon-side over an append-only corpus with a lock-free read path.
- **match** — the substring/smart-case matcher and its incremental filter, shared
  by the daemon and headless search.
- **tui/theme**, **tui/search** (inline Ctrl-R), **tui/browse** (full-screen).

Security & sync:
- **cryptobox** — the E2E core (device keypairs, History Key, epoch data keys,
  record AEAD). See "Key hierarchy" below.
- **redact** — the recording gate: never spool/store/sync a command that carries
  a secret. Runs on live capture and on import.
- **server** — the sync server. Stores only ciphertext + device public keys.
- **syncer** — the client engine: encrypt+push local records, pull+decrypt
  remote ones, and the device enrollment/revocation primitives.

Integration:
- **shell** — the embedded zsh/bash hook scripts (+ vendored bash-preexec).
- **importer** — zsh (extended-history, unmetafy, multiline) and bash parsers.
- **cli** — subcommand dispatch; wires the above together.

## Key hierarchy (cryptobox)

```
device X25519 keypair    per machine, private half never leaves it
  │ seals ──▶ History Key (HK)    one 32B symmetric key, only ever stored
  │                               as per-device wrapped blobs on the server
  │ wraps ──▶ epoch Data Keys     per device, per time-epoch, wrapped under HK
  │ seals ──▶ history records     each sealed with its epoch's data key
```

- **Read**: one asymmetric HK unwrap per daemon lifetime, then all data keys and
  records open symmetrically (sub-microsecond). Cost is O(1) in history age.
- **Enroll**: an existing device wraps HK for the newcomer's public key — one wrap.
- **Revoke**: rotate to HK2, re-wrap the data keys (symmetric, fast) and HK for
  surviving devices; **records are never re-encrypted**.
- **AAD**: every sealed record is bound to `recordID|hostID|seq|keyID`, so the
  server cannot reorder, replay, or substitute blobs undetected.

## Invariants

- The prompt path does one fsync'd spool append plus a best-effort daemon poke —
  no DB, network, or crypto work. Recording survives the daemon being down.
- The local DB holds only this host's history. Remote history is decrypted into
  daemon RAM only, re-pulled per daemon lifetime, never written to disk.
- Streams are append-only and per-host with client-assigned sequence numbers;
  merge is a set union by record ULID. Deletions are appended tombstones. There
  is nothing to conflict, so sync is eventually consistent by construction.
- All client state lives under `~/.config/yore/`; uninstall is one `rm -rf`.
```
