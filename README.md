# yore

**Your shell history — every machine, end-to-end encrypted, searchable, fast.**

<!-- demo: paste the asciinema embed here, e.g.
[![asciicast](https://asciinema.org/a/REPLACE.svg)](https://asciinema.org/a/REPLACE) -->
<!-- (repo stays binary-free per house rules; a committed GIF works too) -->

---

`yore` records every command you run (zsh and bash), keeps this machine's history
in a fast local store, and — pointed at a sync server you host — makes all of
your machines' history searchable everywhere. It is **end-to-end encrypted**: the
server only ever holds ciphertext and can never read a single command.

It's for anyone who lives across several machines and wants one searchable
history — without trusting a server, a proxy, or a copied master key with the
plaintext of everything they've ever typed. Reach it with `Ctrl-R`, the `hb`/`hs`
aliases, or the CLI. One static, CGO-free Go binary is the client, the background
daemon, the TUIs, the importer, **and** the sync server. No agents, no runtime
dependencies.

## Why yore

yore is built for a specific threat model that most history tools don't serve:

- **The server never holds plaintext.** History is sealed client-side; the server
  stores ciphertext blobs and device public keys — nothing it can read.
- **No master key to copy between machines.** Each device has its own keypair. The
  shared *History Key* is distributed **wrapped** per-device; enrolling a machine
  is a one-time approval from an existing one, and revoking a machine takes effect
  **without re-encrypting a single record**.
- **Per-device revocation.** Lose a laptop and revoke just that device; everything
  else keeps working.
- **Your other machines' history never touches this disk.** The local database
  holds only *this* host's commands. Remote history is fetched into the daemon's
  RAM on demand and never persisted.
- **Secrets never get recorded — and never sync.** A default-on, editable
  redaction gate drops commands that carry credentials before they're ever
  written, so they can't leak into your store or up to your server.
- **A captured token can't tamper.** Every mutating sync request is signed with
  the device's private key, so a leaked bearer token (e.g. from a TLS-inspecting
  corporate proxy) can't push garbage or revoke your devices.
- **One source of truth.** By default yore *replaces* your shell's native history:
  no second, unredacted `~/.zsh_history` on disk — and `!N` / `!!` / Up still
  work, against yore's redacted history.
- **Built to last years.** Every hot path — search, decrypt, enroll, revoke — is
  O(1) in how much history you've accumulated. Nothing rots as the archive grows.

Everything lives under one directory (`~/.config/yore/`); uninstall is one
`rm -rf`.

## How it compares

Honest note up front: **[Atuin](https://atuin.sh) is more mature and a great
choice** if you don't need yore's specific model. yore's differentiator is the
cryptography and data-locality story below — E2E where the server can't read your
history, per-device keys with no shared master key, per-device revocation, and
keeping other machines' history off this disk.

| | **yore** | **Atuin** | **hiSHtory** | **mcfly** | **plain history** |
|---|---|---|---|---|---|
| Sync across machines | Yes | Yes | Yes | No | No |
| E2E (server can't read history) | Yes | Yes | Yes | n/a | n/a |
| No shared master key (per-device keys) | Yes | No — one key you copy | No — one secret you copy | n/a | n/a |
| Per-device revocation | Yes | No | No | n/a | n/a |
| Other hosts' history never stored locally | Yes | No — replicated to each | No — replicated to each | n/a | n/a |
| Secrets redaction | Yes (default-on) | Opt-in filter | — | No | No |
| …and it gates sync | Yes | — | — | n/a | n/a |
| Per-device request signing | Yes | No | No | n/a | n/a |
| Self-hostable server | Yes | Yes | Yes | No | No |
| Multi-tenant server | Yes | Yes (multi-user) | Yes (multi-user) | n/a | n/a |
| Cross-shell (zsh + bash) | Yes | Yes | Yes | Yes | Yes |
| Rich interactive TUI | Yes | Yes | Yes | Yes | No (basic `Ctrl-R`) |
| Agent/executor tagging | Yes | No | No | No | No |
| Single static binary | Yes | Yes | Yes | Yes | n/a |

Cells marked `—` are left blank rather than guessed: a capability that isn't a
well-established fact of that tool's current behavior. Notably, Atuin and
hiSHtory both do E2E sync, but with a **single key/secret you copy to each
machine** (no per-device keys or revocation) and **replicate the full history to
every machine's local database**. mcfly is **local-only** — no sync, no server,
no encryption. Plain shell history is a **plaintext** file with no sync.

## Quickstart

### 1. Install

Requires **Go 1.26+**.

```bash
git clone <your-fork> && cd yore
make build          # -> ./bin/yore
sudo install -m755 bin/yore /usr/local/bin/yore
```

Cross-compiled binaries for every supported platform:

```bash
make release        # -> dist/yore-{linux,darwin,freebsd}-{amd64,arm64}
```

Supported: **Linux** (amd64/arm64, includes WSL), **macOS** (Intel/Apple
Silicon), **FreeBSD** (amd64). Native Windows/PowerShell is not yet supported.

### 2. Shell setup

Add one line to your shell rc file:

```bash
# ~/.zshrc
eval "$(yore init zsh)"

# ~/.bashrc
eval "$(yore init bash)"
```

Source it **after** your own `HISTFILE`/`SAVEHIST` settings so yore wins. This
installs the capture hooks and, depending on the integration mode, rebinds your
history keys and adds the `hb`/`hs` aliases. That's the whole single-machine
setup — you have a fast, redacted, searchable local history now.

### 3. (Optional) Sync across machines

Sync is opt-in and needs a server you host. In short:

```bash
# on a server your machines can reach (same binary; stores only ciphertext)
openssl rand -base64 32 | docker secret create yore_token -
docker build -f docker/Dockerfile -t yore:latest .
docker stack deploy -c docker/swarm/stack.yml yore

# on your first machine
yore setup          # server URL + token + integration mode; bootstraps keys

# on each additional machine
yore setup                        # registers it as PENDING, prints a code
# …then, on an already-enrolled machine:
yore devices approve <id>         # confirm the code, approve — it syncs from here
```

Full deployment, multi-tenant, and enrollment details are in
[Multi-machine sync](#multi-machine-sync-optional) below.

## Features

### Capture & history

- **Cross-shell capture** for **zsh** and **bash**, recording each command with
  its exit code, duration, working directory, host, and session.
- **Sub-1 ms prompt path.** The hook does one append to a crash-safe spool plus a
  best-effort daemon poke — no database, network, or crypto on the prompt path.
  Recording survives the daemon being down (the spool drains at next start).
- **Integration modes** (`config.integration`, default **takeover**), chosen at
  `yore setup --integration` or `yore init --mode`:
  - **takeover** (default) — *yore is the single source of truth.* Your shell's
    persistent history is turned off (no unredacted `~/.zsh_history` on disk), its
    in-memory list is seeded from yore, and new commands are gated through yore's
    redaction — so **`!N`, `!!`, `!$`, and Up work against yore's history, and
    it's secret-free**. zsh gets this exactly via `zshaddhistory`; bash's in-memory
    gate is coarser and best-effort.
  - **coexist** — record *alongside* your untouched native history. `Ctrl-R` is
    rebound to yore and `hb`/`hs` are added; native `!N` keeps working against
    native history.
  - **capture** — record only; no keybinding or alias changes.
- **Import** existing history idempotently, with redaction applied so old
  credentials never get dragged in:

  ```bash
  yore import auto                       # finds ~/.zsh_history, ~/.bash_history, …
  yore import --format zsh ~/.zsh_history
  ```

  Re-run it as often as you like; only new entries land, and it reports how many
  secret-bearing lines it skipped.

### Search & TUI

- **Inline `Ctrl-R` search** — a fast panel seeded with whatever you'd started
  typing. The pick **runs immediately on Enter** by default (`enter_executes`;
  set it `false` to insert at the prompt for review instead).
  - **Scopes**: local (this host) / all hosts / host / session / cwd / workspace
    (anywhere under the current git repo) — cycle scope right inside the search.
  - **Ranking**: recency (default) or **frecency** (frequency × recency).
  - **Matching**: substring (smart-case) or **fuzzy** (subsequence).
  - **Syntax highlighting** of command rows, layered under match highlighting.
- **Full-screen browser** (`hb`) with hosts / table / detail / stats / devices
  panes, a **tag column** and `t` **tag filter**, and **`S` sync-now**. `Enter`
  recalls the pick to your prompt to edit; `y` copies it to the clipboard; `D`
  opens the devices pane.
- **Aliases** (omit with `yore init … --no-aliases`):
  - **`hb`** — the browser. (yore leaves `h` alone, so your own `h=history`
    survives.)
  - **`hs <query>`** — CLI search; prints plain matching lines when piped, e.g.
    `hs docker | grep build`. Scoped siblings: **`hsa`** (all hosts), **`hss`**
    (this session), **`hsc`** (this cwd), **`hsw`** (this git repo).
- **Scriptable search**: `yore search --headless <query>` prints matching commands
  as plain lines (with `--scope`, `--sort`, `--fuzzy`, `--tag`, `--limit`,
  `--no-host`).
- **Executor / agent tagging.** Each record is tagged with what ran it —
  auto-detected agent environments (e.g. `CLAUDECODE`, Cursor, aider) or your own
  `$YORE_TAG` / `--tag`. Filter with `yore search --tag claude-code` or the
  browser's `t`, so you can separate "what I typed" from "what an agent ran".
- **Shell completions** for **bash, zsh, and fish** via cobra — covering flag
  values (`--scope`, `--sort`, `--format`), shell names, and live device ids for
  `yore devices approve|revoke`:

  ```bash
  yore completion zsh  > "${fpath[1]}/_yore"
  yore completion bash | sudo tee /etc/bash_completion.d/yore >/dev/null
  yore completion fish > ~/.config/fish/completions/yore.fish
  ```

- **`vim` or `emacs` keymap** for the TUIs (`keymap` config).

The background daemon auto-starts on first use and idles out when unused. It holds
the searchable corpus in RAM (loaded from a warm snapshot for an instant first
search) and does all filtering, so search stays instant into six figures of
history.

### Secrets redaction

yore never records anything that looks sensitive. It drops:

- commands matching a **secrets** rule — AWS/GitHub/Slack tokens,
  `--password`/credential flags, connection-string URLs with inline passwords,
  PEM blocks, JWTs, `TOKEN=…`/`SECRET=…` assignments, and any regex you add;
- commands you start with a leading space (the `histignorespace` convention);
- commands run under a directory you list in `ignore_dirs`.

The rules live in an editable **`~/.config/yore/redact.yml`**, seeded from the
built-ins on `yore setup` and yours to tune. Each rule is a `name`, a Go
`pattern` (regexp), optional literal `hints` (a fast pre-filter), and an optional
`fold` flag (case-insensitive hints):

```yaml
rules:
  - name: my-internal-secret
    pattern: "my-internal-secret-[a-z0-9]+"
    hints:
      - my-internal-secret
    fold: false
```

It is **fail-safe**: if the file is missing, unreadable, unparseable, or left
with no patterns, yore falls back to the compiled-in built-ins — a typo can never
silently switch redaction off (one invalid pattern is skipped; the rest keep
working). The **same gate runs on live capture, on import, and on the
shell-history seed**, so secrets never reach your store or your server. `yore
doctor` reports which rules loaded and whether it fell back.

### Security & encryption

- **Per-device keypairs.** Each device holds an X25519 (encryption) and Ed25519
  (signing) keypair; the private halves never leave the machine.
- **One wrapped History Key.** A single 32-byte symmetric History Key exists only
  as blobs **wrapped to each enrolled device's public key** — there is no master
  key to copy around.
- **Auto-rotating epoch data keys**, each wrapped once under the History Key;
  every record is sealed (XChaCha20-Poly1305) with its epoch's key.
- **Enrollment & revocation.** Approving a new device is one key-wrap from an
  existing device; revoking rotates the keys (re-wrapping for surviving devices)
  **without re-encrypting a single record**, so cost is independent of history
  size.
- **Tamper-evident.** Every sealed record is bound by authenticated data to its
  exact position (`recordID|hostID|seq|keyID`), so a compromised server can't
  reorder, replay, or substitute blobs undetected.
- **Per-device request signing.** On top of the bearer token, every *mutating*
  request is signed with the device's Ed25519 key (with a nonce + timestamp
  anti-replay), so a captured token can't push or revoke — useful against
  TLS-inspecting proxies, which see ciphertext and the token but no private key.
- **Optional TLS cert pinning** (`yore setup --pin`) — fail-closed against
  interception.
- Local database and device key file are `0600`. Treat full disk access to a
  machine as access to that machine's readable history — the same as
  `~/.zsh_history` today.

Full crypto and endpoint details are in [`docs/protocol.md`](docs/protocol.md).

### Sync & server

- **Eventual-consistency sync**: append-only per-host streams with
  client-assigned sequence numbers; merge is a set-union by record ULID; deletes
  are tombstones — conflict-free by construction. The client pushes above a
  watermark and pulls other hosts' streams via delta cursors.
- **Remote history lives only in daemon RAM** — fetched and decrypted on demand,
  **never written to this machine's disk**, re-pulled per daemon lifetime.
- **Sync cadence**: a background loop (`sync_interval`, default `5m`), plus **`S`
  sync-now** in the browser and `yore sync` on the CLI. An experimental,
  opt-in `push_debounce` coalesces a push shortly after recording.
- **Shallow vs deep search**: local-host results are always instant and
  offline-safe; all-hosts results are pulled into RAM, decrypted, and cached for
  the daemon's lifetime — fetched lazily when you widen scope. Offline, deep scope
  simply reports remote is unavailable; nothing breaks.
- **Multi-tenant server**: one server can host several isolated tenants — each its
  own devices, keys, and history in a **separate database file**, selected by
  which bearer token a request matches (`$YORE_TOKENS_FILE`). The client and wire
  protocol are unchanged.
- **Backups & logs**: rolling local-db snapshots (`backup_*` config) and a
  bounded, silenceable daemon log (`log_*` config); the server writes rolling
  per-tenant snapshots too.

### Portability & ops

- **One CGO-free static binary** for client, daemon, TUIs, importer, and server.
- **Cross-platform**: linux/macOS/FreeBSD on amd64/arm64 (WSL runs the linux
  builds).
- **O(1)-in-history-age hot paths** — search, decrypt, enroll, and revoke don't
  degrade as years of history accumulate.
- **Diagnostics**: `yore doctor` (environment + which redaction rules loaded) and
  `yore status` (daemon + store + sync). Stop things with `yore stop` /
  `yore server stop`.

## Everyday use

| You want to… | Do this |
|---|---|
| Search and recall a command | `Ctrl-R` (or Up, in takeover), type, `Enter` |
| Re-run an event by number | `!N`, `!!`, `!$` — native, against yore's history |
| Cycle search scope (host / all / session / dir / repo) | `Ctrl-R` again inside the search |
| Rank by frequency×recency, or fuzzy-match | frecency / fuzzy toggles inside the search |
| Browse, filter, inspect, get stats, manage devices | `hb` — `Enter` recalls, `y` copies, `D` for devices |
| See only what an agent ran | `yore search --tag claude-code` |
| Grep history in a script | `hs <query>` \| … or `yore search --headless <query>` |
| See daemon / sync status, or diagnose | `yore status` / `yore doctor` |
| Force a sync now | `yore sync` (or `S` in `hb`) |
| Stop the daemon / server | `yore stop` / `yore server stop` |

## Multi-machine sync (optional)

### 1. Run the server

The server is the same binary and stores only ciphertext. Deploy it wherever your
machines can reach it (built for a Docker Swarm; TLS terminates at your existing
reverse proxy).

```bash
openssl rand -base64 32 | docker secret create yore_token -
docker build -f docker/Dockerfile -t yore:latest .
docker stack deploy -c docker/swarm/stack.yml yore
```

See [`docker/swarm/stack.yml`](docker/swarm/stack.yml). Point your proxy at the
service and give it a hostname; the health endpoint is `GET /v1/health`. For a
local, non-Swarm playground (server + a zsh and a bash client, all in
containers) see [`docker/sandbox/`](docker/sandbox/).

#### Multiple tenants (optional)

One server can host several **isolated tenants** — each its own group of machines
with their own devices, keys, and history in a **separate database file**. The
client and wire protocol are unchanged: a machine sends its bearer token, and the
server routes by which token it matches.

- `$YORE_TOKEN` / `$YORE_TOKEN_FILE` is the **default** tenant (its db is `--db`,
  e.g. `/data/yore.db`) — a single-token server behaves exactly as before.
- `$YORE_TOKENS_FILE` points at a JSON object of **named** tenants
  (`name → token`); each shards to `<dir(--db)>/tenants/<name>.db`. Names are
  restricted to `[A-Za-z0-9_-]+`, and `default` is reserved.

  ```json
  { "alice": "<token>", "bob": "<token>" }
  ```

Generate a token per tenant the same way (`openssl rand -base64 32`). A tenant
never sees another tenant's data.

#### Server backups (optional)

The server writes rolling per-tenant snapshots to
`<dir(--db)>/backups/<tenant>/data-<unixMillis>.db` (atomic; safe to copy):

- `$YORE_BACKUP_INTERVAL` — Go duration between snapshots (default `1h`; `0`
  disables).
- `$YORE_BACKUP_KEEP` — snapshots retained per tenant, newest first (default `3`).

### 2. Enroll your first machine

```bash
yore setup            # asks for the server URL + token and your integration
                      # mode; generates this machine's keypair and bootstraps
                      # the History Key. Add --pin to pin the server's TLS cert.
```

If the server is unreachable or rejects the token, nothing is saved.

### 3. Enroll another machine

```bash
# on the new machine
yore setup                    # registers it as PENDING, prints a verification code

# on a machine that's already enrolled
yore devices                  # shows the pending device + its code
yore devices approve <id>     # confirm the code matches, then approve
```

After approval the new machine syncs automatically. To remove a machine:

```bash
yore devices revoke <id>      # revokes and rotates keys; old records are NOT
                              # re-encrypted — the revoked machine just loses access
```

## Configuration

`~/.config/yore/config.json` (all keys optional):

```json
{
  "server_url": "https://yore.example.com",
  "token": "…",
  "server_pin": "…",
  "integration": "takeover",
  "keymap": "emacs",
  "enter_executes": true,
  "bind_up_arrow": false,
  "key_epoch": "24h",
  "daemon_idle": "30m",
  "sync_interval": "5m",
  "push_debounce": "0",
  "backup_interval": "1h",
  "backup_keep": 3,
  "log_max_size": "5MB",
  "log_keep": 1,
  "log_silent": false,
  "ignore_patterns": ["my-internal-secret-[a-z0-9]+"],
  "ignore_dirs": ["/home/me/secret-project"],
  "record_space_prefixed": false
}
```

- `integration` — `takeover` (default) / `coexist` / `capture`.
- `server_pin` — base64 SHA-256 of the server's TLS cert; set by `setup --pin`.
- `keymap` — `emacs` (default) or `vim` for the TUIs.
- `enter_executes` — run the picked command on Enter (default); `false` inserts it
  at the prompt for review.
- `bind_up_arrow` — in coexist mode, also bind Up to search.
- `key_epoch` — how often a new epoch data key is minted (default `24h`).
- `daemon_idle` — idle timeout before the daemon exits (default `30m`).
- `sync_interval` — background push/pull cadence (default `5m`).
- `push_debounce` — experimental; a duration enables a coalesced push shortly
  after recording (default off).
- `backup_interval` / `backup_keep` — local `data.db` snapshot cadence and
  retention (`1h`, `3`; `"0"` disables).
- `log_max_size` / `log_keep` / `log_silent` — cap and rotate `daemon.log`
  (`5MB`, `1`; `"0"` size = unbounded; `log_silent` writes no file).
- `ignore_patterns` / `ignore_dirs` — extra never-record regexes and directories.
- `record_space_prefixed` — record even leading-space commands.

The auth token may also come from `$YORE_TOKEN` or `$YORE_TOKEN_FILE` (the server
reads `$YORE_TOKEN_FILE` for Swarm secrets). Override the whole state directory
with `$YORE_DIR`.

### Footprint

```
~/.config/yore/
├── config.json     settings
├── redact.yml      editable secret-redaction rules (seeded; fail-safe)
├── data.db         this host's history (bbolt)
├── device.key      this device's private key (0600)
├── corpus.snap     warm-start search snapshot (derived, rebuildable)
├── daemon.sock     daemon control socket
├── daemon.log      daemon log (size-capped/rotated; see log_* config)
├── backups/        rolling data.db snapshots (see backup_* config)
└── spool/          crash-safe capture queue
```

Uninstall = remove that directory.

## Docs & contributing

- [`docs/architecture.md`](docs/architecture.md) — the design: data flow,
  packages, daemon socket protocol, search model, key hierarchy, invariants.
- [`docs/protocol.md`](docs/protocol.md) — the full sync HTTP/JSON API.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — build, test, the CI gates, and
  conventions.
- [`LICENSE`](LICENSE) — MIT.

Contributions are welcome — please read [`CONTRIBUTING.md`](CONTRIBUTING.md)
first.
