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

It also gives your **AI coding agents** a memory: it captures what Claude Code,
Cursor, OpenCode, Codex, and the Devin CLI run (traced to the prompt that caused
it) and serves your cross-machine history back to them over **MCP** — E2E,
nothing leaving your devices.

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
- **Other machines' plaintext never touches this disk** — only the ciphertext
  the server already holds is cached locally, and it's decrypted into the
  daemon's RAM.
- **Secrets never get recorded, and never sync** — a default-on, editable
  redaction gate masks the credential and keeps the command, so you still have
  the history without ever storing the key.
- **A captured request can't tamper** — every request, reads included, is signed
  with the device's key (safe against a TLS-inspecting proxy).
- **One source of truth** (optional takeover mode): no second, unredacted
  `~/.zsh_history`, and `!N` / `!!` / Up still work — against yore's history.

[Atuin](https://atuin.sh) is more mature and a great choice if you don't need
this specific key model. [suvadu](https://suvadu.sh) is excellent if you want the
structured, agent-queryable history and only ever work on one machine.

## How it compares

| | **yore** | **Atuin** | **suvadu** | **plain history** |
|---|---|---|---|---|
| Sync across machines | Yes | Yes | No — 100% local | No |
| E2E (server can't read history) | Yes | Yes | n/a — no server | n/a |
| No shared master key (per-device keys) | Yes | No — one key you copy | n/a | n/a |
| Per-device revocation | Yes | No | n/a | n/a |
| Single-use enrollment (no standing token) | Yes | No | n/a | n/a |
| Recovery phrase if every device is lost | Yes | n/a — copy the key | n/a | n/a |
| Other hosts' plaintext never stored locally | Yes — ciphertext only | No — replicated to each | n/a | n/a |
| Per-device request signing (incl. reads) | Yes | No | n/a | n/a |
| Secrets in the OS keyring | Yes | No | n/a | n/a |
| Self-hostable server | Yes | Yes | n/a | No |
| Multi-tenant server | Yes | Yes | n/a | n/a |
| Secrets redaction | Yes — default-on, **masks** the value | Opt-in filter | Yes — default-on, **masks** the value | No |
| …and it covers agent prompts too | Yes | n/a | n/a | n/a |
| …and it gates sync | Yes | — | n/a | n/a |
| Exit code, duration, cwd, session per command | Yes | Yes | Yes | No |
| Cross-shell (zsh + bash) | Yes | Yes | Yes | Yes |
| Rich interactive TUI | Yes | Yes | Yes | No (basic `Ctrl-R`) |
| Agent/executor tagging | Yes | No | Yes — wider agent coverage | No |
| Prompt → the commands it triggered | Yes | No | Yes | No |
| AI agents query your history (MCP) | Yes — cross-machine | Yes — cross-machine | Yes — local only | No |
| History-aware risk assessment for agents | Yes | — | Yes | No |
| Single static binary | Yes | Yes | Yes | n/a |

Blank cells (`—`) aren't guessed. **Atuin** does E2E sync, but with a **single key
you copy to each machine** (no per-device keys or revocation) and **replicates
full history to every machine**; it also ships an MCP server, so the
agent-facing half is no longer a yore differentiator — the key model is.
**suvadu** is the closest thing to yore's agent story and is very good at it, but
it is deliberately local-only: no sync, no server, nothing cross-machine. Plain
history is a plaintext file with no sync.

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
yore devices token                   # on an enrolled machine
yore setup --token <token>           # on the new machine: registers PENDING
yore devices                         # back on the enrolled one: pick it, press
                                     # a, and check the code matches
```

`yore devices` also lists every enrollment token and what became of it — open,
claimed (by which machine), expired, or revoked — so an outstanding invitation
into your history is something you can see and cancel (`x`), not something you
wait out. `n` mints one there and `y` copies it; it is shown once and never
again, because the server keeps only its hash.

Lost every machine? `yore recover` asks for the recovery phrase and re-enrols
this one. Client secrets (the device key) live in your **OS keyring**, falling
back to a `0600` file on headless servers and in containers.

Multi-tenant hosting, server backups, and revocation:
[`docker/swarm/stack.yml`](docker/swarm/stack.yml) and
[`docs/protocol.md`](docs/protocol.md). A local container playground is in
[`docker/sandbox/`](docker/sandbox/).

### Uninstall

Un-wire any agents you set up, remove the `eval "$(yore init …)"` line from your
`~/.zshrc` / `~/.bashrc` (reopen your shell), then:

```bash
yore uninit claude-code       # …and cursor / opencode / codex / devin
rm -f /usr/local/bin/yore     # or wherever you installed it
rm -rf ~/.config/yore         # all of yore's state
```

`uninit` removes only yore's hooks and MCP entry from each agent's config,
leaving everything else — including the agent's own auth — exactly as it was.

## Highlights

- **`Ctrl-R` search** — scopes (local / all / host / session / cwd / git-repo),
  frecency and fuzzy matching, syntax highlighting; Enter runs the pick by default
  (`enter_executes`).
- **`hb` browser** — hosts / commands / details panes, plus full-screen **stats**
  (`s`: KPIs, top programs/commands/dirs, per-host, and three full-width graphs —
  a contribution heatmap with a month ruler, a daily trend, and an hour-of-day
  histogram, all of which show *more history* on a wider terminal — with period
  tabs), an **agent explorer** (`a`: five panes — an executor sidebar and a
  HOSTS pane that each filter everything, every prompt, the exact commands the
  highlighted prompt triggered, and a details pane that keeps whichever record
  you pointed it at — focus it to scroll or `z` it to read the whole thing; `H` cycles the
  host filter from anywhere and `A` the agent, and the host column appears only
  when the rows can disagree about it), and
  **devices** (`D`). Any pane can
  be **expanded to the full terminal** (`z`) or **resized by dragging its
  border** with the mouse — and the sizes you pick are remembered between runs;
  the wheel scrolls whatever the pointer is over. One **time window** (`1`-`5`:
  Today / 7d / 30d / 90d / All) drives every view, including the command table,
  with its tabs in the same top-right corner everywhere. `yore stats` and
  `yore agents` open straight on those two screens. Separate `exec` and `tags`
  columns, `e`/`t` filter by the row's executor/tag and `E`/`T` by any you type,
  `H` cycles the host scope, `c` opens a **columns pane** to show, hide, and sort
  by any column of whichever list you are in — the results table, the explorer's
  prompts, or its commands, each keeping its own choices — `Ctrl+T` tags a row,
  `S` sync-now; Enter recalls, `y` copies. (yore
  leaves your own `h` alone.) Pane sizes you drag **and the columns you show,
  hide, and sort by** persist to `~/.config/yore/ui.toml` — kept out of your
  hand-edited `config.toml`, and stored by column *name* so the file survives
  upgrades.
  Commands are **syntax-highlighted everywhere they appear** — the table, both
  details panes, the top-commands stats — hosts *and* executors carry stable
  identity hues, and the details panes flag risky commands (`Risk  ⚠ high
  (script-exec)`) using the same rules as the MCP `assess_risk` tool.
- **`hs` + scoped `hsa`/`hss`/`hsc`/`hsw`** search aliases; `yore search
  --headless` for scripts and pipes.
- **Freeform tags** — label commands and sessions (`yore tag add refactor`,
  filter `yore search --tag refactor`); a record can carry several, `auto_tags`
  labels by directory, and they sync E2E. Tagging a session covers the work that
  shell has already done and everything it does next. `yore tag list` counts
  commands, not labellings. Which agent ran a command is a *separate* axis —
  `--executor`, never `--tag`.
- **AI-agent capture** — records which agent ran what (`--executor claude-code`);
  `yore init claude-code | cursor | opencode | codex | devin` installs each one's
  native hooks/plugin so even their non-interactive shells are captured,
  prompt-traced, with exit status and — via a PreToolUse start-stamp — real
  command durations even when the agent's payload omits timing.
  `yore uninit <agent>` cleanly reverses it (removing only yore's hooks + MCP,
  preserving the agent's own config), and `yore doctor` shows what's wired up.
- **Agent memory over MCP** — the same `init` registers a local, read-only MCP
  server so an agent can query your history back — search, failures, prompts,
  stats, and a history-aware `assess_risk` (*"run 3× across your machines, 1
  failed"*) — **across every machine you own**. `yore doctor` verifies it.
  Risk rules are yours to extend: `~/.config/yore/risk.toml` adds `[[rule]]`
  patterns and an `ignore` list on top of the built-ins, fail-safe like
  `redact.yml`, and the browser and `assess_risk` read the same file.
- **Secrets redaction** from an editable, fail-safe `~/.config/yore/redact.yml`;
  runs on capture, on import, and on the history seed. It **masks the credential
  and keeps the command** — `export DB_PASSWORD=⟪redacted:generic-token-assign⟫`
  — so you keep the history and the marker says which rule took the value. It
  gates agent prompts as well as commands. (Two things still drop a record
  outright, because you asked for it: `ignore_dirs` and `ignore_patterns`.)
- **Import** your existing history idempotently (`yore import auto`). Zsh's
  extended history carries a timestamp per entry; **bash only does when
  `HISTTIMEFORMAT` is set** in the shell that wrote the file, so a default
  `~/.bash_history` imports with no times at all (shown as `—`, not as 1970).
  `yore import` says so when it happens. The dates are not recoverable after the
  fact — set `HISTTIMEFORMAT` to get them on future entries.
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
| Browse, get stats, watch agents, manage devices | `hb` — `s` stats, `a` agents, `D` devices; `Enter` recalls, `y` copies |
| Read your own history without an agent's noise in it | nothing — `hb` and `Ctrl-R` hide agent commands by default (`A` / `⌥a` shows them; `hide_agent_commands` sets the default) |
| See every key that works on the screen you're on | `?` in `hb` · `⌥/` inside `Ctrl-R` search |
| Jump straight to stats or the agent explorer | `yore stats` · `yore agents` |
| See an agent's prompt and the commands it triggered | `yore agents` — `Tab` cycles the five panes, the sidebar filters to one agent |
| Find one prompt, or one command an agent ran | `/` in `yore agents` — filters whichever list has focus; `Esc` clears it |
| See one machine's agent work | the HOSTS pane in `yore agents`, or `H` to cycle the host filter from anywhere (`A` cycles the agent) |
| Narrow any view to a time window | `1`-`5` — Today (the calendar day) / 7d / 30d / 90d / All |
| Give one pane the whole screen | `z` (again to restore) |
| Resize the panes | drag the border between them with the mouse (remembered in `~/.config/yore/ui.toml`) |
| Keep a table's columns and sort between runs | nothing to do — `c` writes them to `ui.toml` as you make them |
| Read a command/prompt that's cut off with `…` | `←`/`→` scroll the selected row horizontally |
| Capture what Claude Code / Devin runs + let it query history (MCP) | `yore init claude-code` \| `yore init devin` (once) |
| See only what an agent ran | `yore search --executor claude-code` |
| Tag commands/sessions and filter by tag | `yore tag add refactor` · `yore search --tag refactor` · `T` in the browser |
| Sort by duration, or hide a column you don't need | `c` in any list — `s` sorts by the highlighted column, `space` hides it |
| Sort an agent's prompts by how many commands they ran | `a`, then `c` on the prompt list, `s` on `cmds` |
| See what your tags actually cover | `yore tag list` (counts commands; `--scope all` for every machine) |
| Let an agent check a command's risk / history | it calls the `assess_risk` MCP tool |
| Grep history in a script | `hs <query>` \| … or `yore search --headless <query>` |
| Force a sync now | `yore sync` (or `S` in `hb`) |
| Add another machine | `n` in `yore devices` (or `yore devices token`), then `yore setup --token …` there, then approve it with `a` |
| Approve or revoke a machine | `yore devices` — `a` approves, `x` revokes, both ask first |
| See which enrollment tokens are outstanding | `yore devices`, tokens pane — open / claimed (by which machine) / expired / revoked; `x` cancels an open one |
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
