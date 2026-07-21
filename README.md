# yore

**Your shell history — every machine, encrypted, searchable, fast.**

`yore` records every command you run (zsh and bash), keeps it in a rich TUI you
reach with `Ctrl-R` or the `h`/`hs` aliases, and — when you point it at a sync
server you host — makes all of your machines' history available everywhere,
end-to-end encrypted so the server never sees a single command in the clear.

One static Go binary is the client, the background daemon, the TUI, the
importer, **and** the sync server. No agents, no runtime deps, CGO-free.

---

## Why another shell-history tool?

`yore` exists for a specific threat model that Atuin and hiSHtory don't serve:

- **The server never holds plaintext.** History is sealed client-side. The
  server stores ciphertext blobs and device public keys — nothing it can read.
- **No master key to copy between machines.** Each device has its own keypair.
  A shared *History Key* is distributed **wrapped** per-device; enrolling a new
  machine is a one-time approval from an existing one, and revoking a machine
  takes effect without re-encrypting a single record.
- **Your other machines' history never touches this machine's disk.** The local
  database holds only *this* host's commands. Remote history is fetched into the
  daemon's RAM on demand and never persisted.
- **Nothing scattered across your home directory.** Everything lives under one
  directory: `~/.config/yore/`.
- **Built for years of history.** Every hot path — search, decrypt, enrolling a
  device — is O(1) in how much history you've accumulated. It doesn't rot as the
  archive grows.
- **One source of truth.** By default yore *replaces* your shell's native
  history: no second, unredacted `~/.zsh_history` on disk, and `!N` still works —
  against yore's redacted history. (Atuin leaves native history recording
  alongside its own store.)
- **A captured token can't tamper.** Every mutating sync request is signed with
  the device's key, so a leaked bearer token (e.g. from a TLS-inspecting
  corporate proxy) can't push garbage or revoke your devices — only the device's
  own private key can.

If you don't need those properties, Atuin is excellent and more mature. If you
do, that's exactly what `yore` is for.

---

## Install

Requires Go 1.26+.

```bash
git clone <your-fork> && cd yore
make build          # -> ./bin/yore
sudo install -m755 bin/yore /usr/local/bin/yore
```

Cross-compiled binaries for all supported platforms:

```bash
make release        # -> dist/yore-{linux,darwin,freebsd}-{amd64,arm64}
```

Supported: **Linux** (amd64/arm64, includes WSL), **macOS** (Intel/Apple
Silicon), **FreeBSD** (amd64). Native Windows and PowerShell are on the roadmap,
not yet supported.

---

## Shell setup

Add one line to your shell rc file:

```bash
# ~/.zshrc
eval "$(yore init zsh)"

# ~/.bashrc
eval "$(yore init bash)"
```

Both install **capture hooks** (recording each command with its exit code,
duration, working directory, host, and session — a single backgrounded write,
so effectively zero prompt latency) and, depending on the **integration mode**,
rebind your history keys.

### Integration modes

Pick a mode at `yore setup` (`--integration`), or `yore init --mode <mode>`; it
is stored in `config.integration` (default **takeover**):

- **takeover** (default) — *yore is the single source of truth.* Your shell's
  own persistent history is turned off (no unredacted `~/.zsh_history` on disk),
  its in-memory list is seeded from yore, and new commands are gated through
  yore's redaction. So **`!N`, `!!`, and Up work against yore's history, and it's
  secret-free**. `Ctrl-R` and Up open the search TUI; `h`/`hs` are added.
- **coexist** — record *alongside* your untouched native history. `Ctrl-R` is
  rebound to yore and `h`/`hs` are added; native `!N` keeps working against
  native history. (This is how Atuin behaves by default.)
- **capture** — record only; no keybinding or alias changes.

zsh gets the exact takeover experience via `zshaddhistory`; bash's history hooks
are coarser, so its in-memory gate is best-effort. Source `yore init` **after**
your own `HISTFILE`/`SAVEHIST` settings so takeover wins.

### The keys

- **`Ctrl-R`** → the inline search TUI, seeded with whatever you'd started typing;
  the command you pick is inserted at your prompt — review, then Enter (set
  `enter_executes: true` to run on Enter instead).
- **Up arrow** → yore search (takeover), or native scroll (coexist, unless you
  set `bind_up_arrow`).
- **`!N` / `!!` / `!$`** → native shell history expansion — still works, against
  yore's history in takeover and native history in coexist.
- **`h`** → the full-screen browser; the command you pick is dropped onto your
  next prompt to edit (zsh) — `Enter` inserts it, `y` copies it to the clipboard.
  **`hs <query>`** → CLI search (plain matching lines when piped, e.g. `hs docker
  | grep build`). Scoped siblings: **`hsa`** (all hosts), **`hss`** (this
  session), **`hsc`** (this cwd), and **`hsw`** (this git repo). Omit them all
  with `yore init zsh --no-aliases`.

### Shell completions

The CLI is built with cobra, so it ships completions for every command, flag, and
even dynamic values (device ids, hostnames). Install them once:

```bash
# zsh — write to a directory on your $fpath, e.g.
yore completion zsh > "${fpath[1]}/_yore"

# bash
yore completion bash | sudo tee /etc/bash_completion.d/yore >/dev/null

# fish
yore completion fish > ~/.config/fish/completions/yore.fish
```

`yore completion --help` prints per-shell instructions. Completions cover flag
values (`--scope`, `--sort`, `--format`), shell names for `init`, and live
device ids for `yore devices approve|revoke`.

### What gets recorded

Everything except what looks sensitive. `yore` never records:

- commands matching a built-in **secrets** pattern (AWS/GitHub/Slack tokens,
  `--password`/credential flags, connection-string URLs with inline passwords,
  PEM blocks, JWTs, `TOKEN=…`/`SECRET=…` assignments, …) or any regex you add;
- commands you start with a leading space (the `histignorespace` convention);
- commands run under a directory you list in `ignore_dirs`.

The same filter runs on **import**, so bulk-loading years of `~/.zsh_history`
won't drag old credentials into the store (or, later, to your server).

---

## Import your existing history

```bash
yore import auto                       # finds ~/.zsh_history, ~/.bash_history, …
yore import --format zsh ~/.zsh_history
```

Import is idempotent — run it as many times as you like; only new entries land.
It reports how many secret-bearing lines it skipped.

---

## Everyday use

| You want to… | Do this |
|---|---|
| Search and recall a command | `Ctrl-R` (or Up, in takeover), type, `Enter` to insert |
| Re-run an event by number | `!N`, `!!`, `!$` — native, works against yore's history |
| Cycle search scope (host / all / session / dir / repo) | `Ctrl-R` again inside the search |
| Rank by frequency×recency, or fuzzy match | `Alt-f` / `Alt-z` inside the search |
| Browse, filter, inspect, get stats, manage devices | `h` — `Enter` recalls the pick to your prompt, `y` copies it (then `D` for devices) |
| See only what an agent ran | `yore search --tag claude-code` |
| Grep history in a script | `hs <query>` \| … or `yore search --headless <query>` |
| See daemon / sync status, or diagnose | `yore status` / `yore doctor` |
| Stop the background daemon / server | `yore stop` / `yore server stop` |

The background daemon starts itself on first use and idles out when unused. It
holds the searchable corpus in RAM (loaded from a warm snapshot for an instant
first search) and does all filtering, so search stays instant into six figures
of history.

---

## Multi-machine sync (optional)

### 1. Run the server

The server is the same binary. It stores only ciphertext. Deploy it wherever
your machines can reach it (it's built for a Docker Swarm; TLS terminates at
your existing reverse proxy).

```bash
# create the auth token secret
openssl rand -base64 32 | docker secret create yore_token -
docker build -f docker/Dockerfile -t yore:latest .
docker stack deploy -c docker/swarm/stack.yml yore
```

See [`docker/swarm/stack.yml`](docker/swarm/stack.yml). Point your proxy at the
service and give it a hostname; the health endpoint is `GET /v1/health`. For a
local, non-Swarm playground (server + a zsh and a bash client, all in
containers) see [`docker/sandbox/`](docker/sandbox/).

### 2. Enroll your first machine

```bash
yore setup            # asks for the server URL + token and your integration
                      # mode; generates this machine's keypair and bootstraps
                      # the History Key. Add --pin to pin the server's TLS cert.
```

### 3. Enroll another machine

```bash
# on the new machine
yore setup            # registers it as PENDING and prints a verification code

# on a machine that's already enrolled
yore devices          # shows the pending device + its code
yore devices approve <id>     # confirm the code matches, then approve
```

After approval the new machine syncs automatically. To remove a machine:

```bash
yore devices revoke <id>      # revokes and rotates keys; old records are NOT
                              # re-encrypted, the revoked machine just loses access
```

### How search uses remote data

- **Shallow** (local host) results are always instant and work offline.
- **Deep** (all hosts) results are pulled from the server into the daemon's RAM,
  decrypted, and cached for the daemon's lifetime — fetched lazily when you
  widen the scope or when a shallow search comes up thin. Offline, deep scope
  simply reports that remote is unavailable; nothing breaks.

---

## Security model in one paragraph

Each device holds an X25519 keypair; the private key never leaves it. One 32-byte
symmetric **History Key** exists only as blobs wrapped to each enrolled device's
public key. History records are sealed with rotating per-device **epoch keys**,
each wrapped once under the History Key. Reading is a single asymmetric unwrap
(then everything is symmetric and sub-microsecond); enrolling a device is one
wrap; revoking is a key re-wrap — **records are never re-encrypted**, so cost is
independent of how much history you keep. Every sealed record is bound by
authenticated data to its exact position in its host's stream, so a compromised
server can't reorder, replay, or substitute blobs. On top of the bearer token,
every *mutating* request is **signed with the device's Ed25519 key** (with a
nonce + timestamp anti-replay), so a captured token can't push or revoke —
useful against TLS-inspecting proxies, which see ciphertext + the token but not
any private key. `yore setup --pin` additionally pins the server's TLS
certificate (fail-closed against interception). The local database and the
device key file are `0600`; treat full disk access to any one machine as access
to that machine's readable history, the same as `~/.zsh_history` today. Full
endpoint and crypto details are in [`docs/protocol.md`](docs/protocol.md).

---

## Configuration

`~/.config/yore/config.json` (all keys optional):

```json
{
  "server_url": "https://yore.example.com",
  "token": "…",
  "server_pin": "…",
  "integration": "takeover",
  "keymap": "emacs",
  "enter_executes": false,
  "bind_up_arrow": false,
  "key_epoch": "24h",
  "daemon_idle": "30m",
  "sync_interval": "5m",
  "ignore_patterns": ["my-internal-secret-[a-z0-9]+"],
  "ignore_dirs": ["/home/me/secret-project"],
  "record_space_prefixed": false
}
```

- `integration` — `takeover` (default) / `coexist` / `capture` (see Shell setup).
- `server_pin` — base64 SHA-256 of the server's TLS cert; set by `setup --pin`.
- `keymap` — `emacs` (default) or `vim` for the TUIs.
- `enter_executes` — run the picked command on Enter instead of inserting it.
- `bind_up_arrow` — in coexist mode, also bind Up to search.

The auth token may also come from `$YORE_TOKEN` or `$YORE_TOKEN_FILE` (the
server reads `$YORE_TOKEN_FILE` for Swarm secrets). Override the whole state
directory with `$YORE_DIR`.

### Footprint

```
~/.config/yore/
├── config.json     settings
├── data.db         this host's history (bbolt)
├── device.key      this device's private key (0600)
├── corpus.snap     warm-start search snapshot (derived, rebuildable)
├── daemon.sock     daemon control socket
└── spool/          crash-safe capture queue
```

Uninstall = remove that directory.

---

## Development

```bash
make test           # unit + integration tests (testify; go test ./...)
make vet
make bench          # store/matcher/crypto benchmarks
```

The codebase is a micro-package layout under `internal/`: `rec`/`proto`/`wire`
(shared contracts), `store`+`spool` (local bbolt + crash-safe capture),
`daemon` (RAM corpus + unix-socket server), `match`/`tui` (search + browse, with
`tui/hl` syntax highlighting), `cryptobox` (E2E core), `reqsign` (request
signing), `server`+`syncer` (sync), `redact` (secrets gate), `importer`, `shell`
(hook scripts), `cli` (cobra tree). Tests use testify (`require`, table-driven).

See [`docs/architecture.md`](docs/architecture.md) for the design and
[`docs/protocol.md`](docs/protocol.md) for the full sync API.
