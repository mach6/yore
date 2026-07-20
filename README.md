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

If you don't need those properties, Atuin is excellent and more mature. If you
do, that's exactly what `yore` is for.

---

## Install

```bash
git clone <your-fork> && cd yore
make build          # -> ./bin/yore   (run `source ~/.gobrew` first if needed)
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

That installs:

- **capture hooks** that record each command with its exit code, duration,
  working directory, host, and session — adding effectively zero latency to your
  prompt (a single backgrounded write);
- **`Ctrl-R`** → the inline search TUI, with whatever you'd started typing as the
  initial query; the command you pick is inserted at your prompt (it does **not**
  auto-run — review, then Enter);
- **`h`** → the full-screen history browser;
- **`hs <query>`** → search from the command line; in a pipe it prints plain
  matching lines (`hs docker | grep build`).

Pass `yore init zsh --no-aliases` if you don't want `h`/`hs`.

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
| Search and recall a command | `Ctrl-R`, type, `Enter` to insert |
| Cycle search scope (this host / all / session / dir) | `Ctrl-R` again inside the search |
| Browse, filter, inspect, get stats | `h` |
| Grep history in a script | `hs <query>` \| … or `yore search --headless <query>` |
| See daemon / sync status | `yore status` |

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
docker stack deploy -c docker/stack.yml yore
```

See [`docker/stack.yml`](docker/stack.yml). Point your proxy at the service and
give it a hostname; the health endpoint is `GET /v1/health`.

### 2. Enroll your first machine

```bash
yore setup            # asks for the server URL + token; generates this
                      # machine's keypair and bootstraps the History Key
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
server can't reorder, replay, or substitute blobs. The local database and the
device key file are `0600`; treat full disk access to any one machine as access
to that machine's readable history, the same as `~/.zsh_history` today.

---

## Configuration

`~/.config/yore/config.json` (all keys optional):

```json
{
  "server_url": "https://yore.example.com",
  "token": "…",
  "key_epoch": "24h",
  "daemon_idle": "30m",
  "sync_interval": "5m",
  "ignore_patterns": ["my-internal-secret-[a-z0-9]+"],
  "ignore_dirs": ["/home/me/secret-project"],
  "record_space_prefixed": false
}
```

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
make test           # unit + integration tests
make vet
make bench          # store/matcher/crypto benchmarks
```

The codebase is a micro-package layout under `internal/`: `rec`/`proto`/`wire`
(shared contracts), `store`+`spool` (local bbolt + crash-safe capture),
`daemon` (RAM corpus + unix-socket server), `match`/`tui` (search + browse),
`cryptobox` (E2E core), `server`+`syncer` (sync), `redact` (secrets gate),
`importer`, `shell` (hook scripts). See [`docs/`](docs/) for architecture notes.
