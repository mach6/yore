# yore

**Your shell history — every machine, end-to-end encrypted, searchable, fast.**

<!-- demo: paste the asciinema embed here, e.g.
[![asciicast](https://asciinema.org/a/REPLACE.svg)](https://asciinema.org/a/REPLACE) -->
<!-- (repo stays binary-free per house rules; a committed GIF works too) -->

---

`yore` records every command you run (zsh + bash) and makes your history
searchable on every machine you use — pointed at a sync server you host, and
**end-to-end encrypted** so the server only ever holds ciphertext. Reach it with
`Ctrl-R`, the `hb`/`hs` aliases, or `yore search`. It's one static, CGO-free Go
binary: client, background daemon, TUIs, importer, **and** sync server.

## Why yore

Built for a threat model most history tools don't serve:

- **The server can't read your history** — only ciphertext and device public keys.
- **No master key to copy.** Per-device keypairs; the shared key is wrapped per
  device. Enroll a machine by approving it from another; **revoke** one without
  re-encrypting a single record.
- **No standing credential at all.** A device authenticates with its own key on
  every request — there is no bearer token to leak. Adding a machine takes a
  **single-use enrollment token** minted by one already enrolled.
- **A recovery phrase, so per-device keys can't lock you out.** Shown once when
  you create the group; it is the way back if you lose every machine.
- **Other machines' history never touches this disk** — it's fetched into the
  daemon's RAM on demand, never persisted.
- **Secrets never get recorded, and never sync** — a default-on, editable
  redaction gate drops credential-bearing commands before they're written.
- **A captured request can't tamper** — every request, reads included, is signed
  with the device's key (safe against a TLS-inspecting proxy).
- **One source of truth** (optional takeover mode): no second, unredacted
  `~/.zsh_history`, and `!N` / `!!` / Up still work — against yore's history.

[Atuin](https://atuin.sh) is more mature and a great choice if you don't need
this specific model.

## How it compares

| | **yore** | **Atuin** | **hiSHtory** | **mcfly** | **plain history** |
|---|---|---|---|---|---|
| Sync across machines | Yes | Yes | Yes | No | No |
| E2E (server can't read history) | Yes | Yes | Yes | n/a | n/a |
| No shared master key (per-device keys) | Yes | No — one key you copy | No — one secret you copy | n/a | n/a |
| Per-device revocation | Yes | No | No | n/a | n/a |
| Single-use enrollment (no standing token) | Yes | No | No | n/a | n/a |
| Recovery phrase if every device is lost | Yes | n/a — copy the key | n/a — copy the secret | n/a | n/a |
| Other hosts' history never stored locally | Yes | No — replicated to each | No — replicated to each | n/a | n/a |
| Secrets redaction | Yes (default-on) | Opt-in filter | — | No | No |
| …and it gates sync | Yes | — | — | n/a | n/a |
| Per-device request signing (incl. reads) | Yes | No | No | n/a | n/a |
| Secrets in the OS keyring | Yes | No | No | n/a | n/a |
| Self-hostable server | Yes | Yes | Yes | No | No |
| Multi-tenant server | Yes | Yes | Yes | n/a | n/a |
| Cross-shell (zsh + bash) | Yes | Yes | Yes | Yes | Yes |
| Rich interactive TUI | Yes | Yes | Yes | Yes | No (basic `Ctrl-R`) |
| Agent/executor tagging | Yes | No | No | No | No |
| Single static binary | Yes | Yes | Yes | Yes | n/a |

Blank cells (`—`) aren't guessed. Atuin and hiSHtory both do E2E sync, but with a
**single key you copy to each machine** (no per-device keys or revocation) and
**replicate full history to every machine**. mcfly is local-only. Plain history
is a plaintext file with no sync.

## Quickstart

### Install (Go 1.26+)

```bash
git clone <your-fork> && cd yore
make build && sudo install -m755 bin/yore /usr/local/bin/yore
```

`make release` cross-compiles Linux / macOS / FreeBSD (amd64/arm64; WSL runs the
Linux build).

### Shell setup

Add one line to your shell rc, **after** your own `HISTFILE`/`SAVEHIST` settings:

```bash
# ~/.zshrc
eval "$(yore init zsh)"

# ~/.bashrc
eval "$(yore init bash)"
```

That's the whole single-machine setup — fast, redacted, searchable local history.
Choose how deeply it integrates with `yore init --mode takeover|coexist|capture`
(default `takeover`; details in [architecture.md](docs/architecture.md)).

### Sync across machines (optional)

Host the server (same binary, ciphertext only), then enroll each machine:

```bash
# on a server your machines can reach
openssl rand -base64 32 | docker secret create yore_token -
docker build -f docker/Dockerfile -t yore:latest .
docker stack deploy -c docker/swarm/stack.yml yore

# first machine: uses the server's own token, forms the group, prints a
# RECOVERY PHRASE — write it down, it is shown once and never stored
yore setup

# each other machine needs a single-use token from one already enrolled:
yore devices token                    # on an enrolled machine
yore setup --token <token>           # on the new machine: registers PENDING
yore devices approve <id>              # back on the enrolled one; confirm the code
```

Lost every machine? `yore recover` asks for the recovery phrase and re-enrols
this one. Client secrets (the device key) live in your **OS keyring**, falling
back to a `0600` file on headless servers and in containers.

Multi-tenant hosting, server backups, and revocation:
[`docker/swarm/stack.yml`](docker/swarm/stack.yml) and
[`docs/protocol.md`](docs/protocol.md). A local container playground is in
[`docker/sandbox/`](docker/sandbox/).

### Uninstall

Remove the `eval "$(yore init …)"` line from your `~/.zshrc` / `~/.bashrc`
(reopen your shell), then:

```bash
rm -f /usr/local/bin/yore     # or wherever you installed it
rm -rf ~/.config/yore         # all of yore's state
```

## Highlights

- **`Ctrl-R` search** — scopes (local / all / host / session / cwd / git-repo),
  frecency and fuzzy matching, syntax highlighting; Enter runs the pick by default
  (`enter_executes`).
- **`hb` browser** — hosts / table / detail panes, plus full-screen **stats**
  (`s`: KPIs, top programs/commands/dirs, by-executor, per-day + hourly
  histograms, period tabs), an **agent monitor** (`a`: per-agent commands,
  success rate, failures, avg duration), a **prompt explorer** (`p`: one row per
  agent prompt with executor, session, command count, status and duration —
  `Enter` drills into the exact commands that prompt triggered), and **devices**
  (`D`). A tag column and `t` filter, `S` sync-now; Enter recalls the pick, `y`
  copies. (yore leaves your own `h` alone.)
- **`hs` + scoped `hsa`/`hss`/`hsc`/`hsw`** search aliases; `yore search
  --headless` for scripts and pipes.
- **Freeform tags** — label commands and sessions with any tags you like
  (`yore tag add refactor`), filter with `yore search --tag refactor`. A record
  can carry several. The agent/executor is just an **auto-applied** tag, so an
  agent's refactor commands can read `claude-code` *and* `refactor` at once.
  `auto_tags` config tags commands by directory. Tags sync end-to-end.
- **Agent tracking** — commands an agent runs are auto-tagged so you can tell
  them from what you typed (`yore search --executor claude-code`). In your
  interactive shell, `CLAUDECODE` / Cursor / aider (or `$YORE_TAG`) auto-detect.
  For Claude Code — whose Bash tool runs a *non-interactive* shell the rc hooks
  never see — `yore init claude-code` installs its hooks (with exit status,
  duration, and prompt tracing) and registers the MCP server.
- **MCP server for your agents** — `yore init claude-code` (or `yore init
  cursor`) wires up a local, read-only MCP server so a coding agent can query
  your history back: search, failures, prompts, stats, and `assess_risk` —
  **across every machine you own** (end-to-end encrypted, nothing leaves your
  devices). Ask *"have I run this migration anywhere?"* or *"what failed in this
  project recently?"*. `yore doctor` verifies the hooks + MCP registration.
- **Risk checks** — a deterministic classifier rates a command `safe…critical`
  with a reason, exposed to agents via `assess_risk` (history-aware: *"run 3×
  across your machines, 1 failed"*).
- **Secrets redaction** from an editable, fail-safe `~/.config/yore/redact.yml`;
  runs on capture, on import, and on the history seed.
- **Import** your existing history idempotently (`yore import auto`).
- **Shell completions** (bash / zsh / fish); `vim` or `emacs` TUI keymaps.
- **Diagnostics**: `yore doctor`, `yore status`.

Configure via `~/.config/yore/config.toml` — a plain TOML file, or use
`yore get-config <key>` / `yore set-config <key> <value>` (every key + default is
in [architecture.md](docs/architecture.md)); all state lives under
`~/.config/yore/`.

## Everyday use

| You want to… | Do this |
|---|---|
| Search and recall a command | `Ctrl-R` (or Up, in takeover), type, `Enter` |
| Re-run an event by number | `!N`, `!!`, `!$` — native, against yore's history |
| Cycle search scope (host / all / session / dir / repo) | `Ctrl-R` again inside the search |
| Rank by frequency×recency, or fuzzy-match | frecency / fuzzy toggles inside the search |
| Browse, get stats, watch agents, manage devices | `hb` — `s` stats, `a` agents, `p` prompts, `D` devices; `Enter` recalls, `y` copies |
| See an agent's prompt and drill into its commands | `hb`, then `p`; `Enter` on a prompt |
| Capture what Claude Code runs + let it query history (MCP) | `yore init claude-code` (once) |
| See only what an agent ran | `yore search --executor claude-code` |
| Tag commands/sessions and filter by tag | `yore tag add refactor` · `yore search --tag refactor` |
| Let an agent check a command's risk / history | it calls the `assess_risk` MCP tool |
| Grep history in a script | `hs <query>` \| … or `yore search --headless <query>` |
| Force a sync now | `yore sync` (or `S` in `hb`) |
| Add another machine | `yore devices token`, then `yore setup --token …` there |
| Get back in after losing every machine | `yore recover` (needs the recovery phrase) |
| Diagnose / status | `yore doctor` / `yore status` |
| Stop the daemon / server | `yore stop` / `yore server stop` |

## Docs & contributing

- [`docs/architecture.md`](docs/architecture.md) — the design: data flow,
  packages, the daemon, search, integration modes, key hierarchy, config, and the
  single-directory footprint.
- [`docs/protocol.md`](docs/protocol.md) — the sync HTTP API, the crypto scheme
  (exact bytes), and the daemon socket protocol.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — build, test, the CI gates, conventions.
- [`LICENSE`](LICENSE) — MIT.
