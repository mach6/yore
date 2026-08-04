# yore sandbox

A self-contained, four-container playground for exercising yore's full
cross-machine sync without touching your host or any real server:

| service  | what it is                                             | hostname   |
|----------|--------------------------------------------------------|------------|
| `server` | the sync server — stores **ciphertext only**           | (internal) |
| `zsh`    | a client with zsh + the yore hooks wired in            | `zsh-box`  |
| `bash`   | a client with bash + the yore hooks wired in           | `bash-box` |
| `fish`   | a client with fish + the yore hooks wired in           | `fish-box` |

Clients reach the server over an internal Docker network (plain HTTP — no TLS is
needed inside the sandbox; per-device request signing still protects writes).
Nothing is published to your host, and `down -v` removes every trace.

The client config (server URL, integration mode) is pre-seeded into each
container from the compose environment by `client-entrypoint.sh`, so `yore setup`
runs without prompts. No credential is seeded — config.json holds no secrets.
Enrolment is authorized by a **single-use token**, taken from `$YORE_TOKEN`.

## Bring it up

```sh
cd docker/sandbox
docker compose down -v          # ALWAYS reset first — see below
docker compose up -d --build
```

**Reset with `down -v`, not `down`.** `server-data` persists, and a group that
already has an active device refuses the bootstrap token by design — that is
what makes it a *first* credential rather than a standing one. Sandbox clients
are ephemeral, so their device keys die with the containers and nothing is left
to mint a token: a reused volume leaves an orphaned group and every `yore setup`
fails with `401 unauthorized`.

A convenience wrapper for running yore in a container (used below):

```sh
dc() { docker compose -f docker/sandbox/compose.yml exec -T "$@"; }
```

## Walkthrough — enroll two machines and sync a command E2E

```sh
# 1) zsh-box becomes the first device and bootstraps the history group (the HK).
#    The server's own token is accepted ONLY here, while the group is empty.
#    This also prints a RECOVERY PHRASE — the way back if every device is lost.
dc zsh yore setup --server http://server:8080 --token sandbox-token \
      --integration takeover --name zsh-box

# 2) Adding a machine needs a single-use token minted by an enrolled one.
TOKEN=$(dc zsh yore devices token)   # the token is the only thing on stdout

# 3) bash-box redeems it, registers as pending, and prints a verification code.
dc bash yore setup --server http://server:8080 --token "$TOKEN" \
      --integration takeover --name bash-box

# 4) From the already-enrolled zsh-box, confirm the code matches and approve.
#    `yore devices` opens the browser's devices pane: bash-box is at the top,
#    with its code; check it matches, press `a`, confirm.
docker compose -f docker/sandbox/compose.yml exec zsh yore devices

#    Scripting it instead? The pane is one client of the daemon's protocol —
#    newline-delimited JSON on a unix socket — and so is socat:
sock=/root/.config/yore/daemon.sock
ID=$(dc zsh sh -c "printf '{\"op\":\"devices\"}\n' | socat -t 30 - UNIX-CONNECT:$sock" \
       | jq -r '.devices.devices[] | select(.status=="pending") | .id')
dc zsh sh -c "printf '{\"op\":\"approve\",\"device_id\":\"$ID\"}\n' | socat -t 30 - UNIX-CONNECT:$sock"

# 5) Record a command on zsh-box and push it.
printf 'echo hello-from-zsh-box' | dc zsh yore record --cwd /root --exit 0
dc zsh yore sync

# 5) Pull on bash-box and deep-search across all hosts — the zsh command appears.
dc bash yore sync
dc bash yore search --headless --scope all hello-from-zsh
#   -> echo hello-from-zsh-box
```

## Prove it's end-to-end encrypted

The plaintext command must never appear in the server's database — the server
only ever sees ciphertext:

```sh
docker cp yore-sandbox-server-1:/data/yore.db /tmp/srv.db
grep -a hello-from-zsh-box /tmp/srv.db && echo "LEAK" || echo "ciphertext only ✓"
```

## Interactive shells

To poke at the real TUI (Ctrl-R search, `h`/`hs`, up-arrow), attach a terminal:

```sh
docker compose -f docker/sandbox/compose.yml exec zsh zsh      # zsh-box
docker compose -f docker/sandbox/compose.yml exec bash bash    # bash-box
docker compose -f docker/sandbox/compose.yml exec fish fish    # fish-box
```

The rc files already `eval "$(yore init …)"` — or `yore init fish | source`, as
fish has no `eval` — so hooks are live. In takeover mode the shell's own history
file is disabled and yore is the single source of truth (so `!N`, up-arrow, and
Ctrl-R all read from yore). fish announces this itself on startup with "fish is
running in private mode, history will not be persisted", which is `fish_private_mode`
doing exactly that job.

### Syntax-checking the generated hook

```sh
dc fish sh -c "yore init fish --mode takeover > /tmp/i.fish && fish -n /tmp/i.fish"
```

`fish -n -` does **not** work: unlike most tools fish has no `-` convention for
stdin and reports "Error reading script file '-'". Write the script to a file, or
pipe into `fish -n /dev/stdin`.

### Scripting an interactive fish

fish's `fish_preexec`/`fish_postexec` events fire only from the interactive
reader, so neither `fish -c 'cmd'` nor piping into `fish -i` records anything —
both skip the reader entirely, and a test built on either will silently observe
nothing while looking like it passed. Drive a real pty instead:

```sh
dc fish sh -c "apk add --no-cache util-linux >/dev/null
               printf 'echo hi\nfalse\nexit\n' > /tmp/c.txt
               script -qec 'fish -i' /dev/null < /tmp/c.txt"
```

## Tear it down (removes containers, network, and the server volume)

```sh
docker compose -f docker/sandbox/compose.yml down -v
```

## Stress test (`stress.sh`) — MANUAL, not CI

`stress.sh` is a self-contained stress/soak harness that automates the whole
walkthrough at scale and then *verifies* it. It is **manual only** — it is never
run by CI (it is not referenced in `.drone.yml`). Run it when you want to confirm
yore stays correct and fast under a real load, or after touching recording,
redaction, sync, or the daemon.

```sh
make stress               # N=5000 records per host (a real run)
make stress N=10000       # heavier
make stress N=150         # quick smoke
make stress KEEP=1        # leave the sandbox up afterwards for a post-mortem
# or call it directly:
N=5000 docker/sandbox/stress.sh
docker/sandbox/stress.sh --keep
```

### What it does

1. **Fresh sandbox** — `down -v` then `up -d --build`, so it always exercises the
   *current* source, then waits for the server's `/v1/health` (probed from inside
   a client; nothing is published to your host).
2. **Enrolls** zsh-box (bootstrap) and bash-box (pending → approved from
   zsh-box), and seeds an `ignore_dirs` entry plus a short `backup_interval` into
   the client config before any recording.
3. **Generates load** — one `docker exec` per host runs a shell loop that records
   `N` commands via `yore record` (never one exec per command). The mix carries
   unique, greppable markers:
   - **normal** commands (the bulk — a positive control that must be stored),
   - **secrets** in valid shapes (github token, AWS secret, URL userinfo, PEM
     header, `TOKEN=…`), each embedding `SEKRETMARKER` — must be redacted,
   - **leading-space** and **ignore-dir** commands — must be dropped,
   - **agent-tagged** batches (`CLAUDECODE=1`, `YORE_TAG=stress-agent`,
     `AIDER_MODEL=…`) — must be tagged.
4. **Syncs** both hosts (two rounds for full convergence).

### What it verifies (each a ✓/✗; non-zero exit if any fails)

| check | how |
|-------|-----|
| **Redaction / E2E at scale** (headline) | `docker cp` each client's `data.db` **and** the server's `yore.db` out, `grep -a` for `SEKRETMARKER` → **zero** hits everywhere (redaction dropped the secrets; the server holds ciphertext only). |
| **Positive control** | the normal `<HOSTMARKER>` commands are present via `yore search --headless`. |
| **Convergence** | from each host, a deep `--scope all` search finds the *other* host's commands; counts match the expected non-dropped totals (±tolerance). |
| **Dropped-count sanity** | stored ≈ generated − (secrets + space-prefixed + ignore-dir); the numbers are printed. |
| **Tags** | `yore search --headless --tag claude-code` (and `stress-agent`, `aider`) match the planted counts. |
| **Performance** | times the bulk-record loop, `yore sync`, and a deep `--scope all` search at full volume; prints ms and records/sec. |
| **Health** | exactly **one** `yore daemon` *process* per client (`pgrep -f '[y]ore daemon'` — the `[y]` trick avoids pgrep matching its own shell, and counts processes, not threads), **zero** zombies (from `/proc/<pid>/stat`), and a rolling backup exists under `~/.config/yore/backups/`. |

### Teardown

By default the harness always tears the sandbox down (via an `EXIT` trap, even on
failure) and confirms `docker ps` is left clean. Pass `--keep` / `KEEP=1` to leave
it running for inspection.

## The fleet (`fleet.yml` + `fleet.sh`) — twenty machines, two users

The four-container sandbox above is the one you poke at by hand. `fleet.yml` is
the same idea at the scale yore is actually meant for: **twenty client machines,
eight Linux distributions, ten zsh and ten bash, split between TWO users** who
share one server and must never see each other's history.

| tenant  | machines | distributions                                                   |
|---------|----------|-----------------------------------------------------------------|
| `alice` | `a01`…`a10` | alpine 3.20, alpine edge, debian 12, ubuntu 24.04, fedora 41, almalinux 9, arch, opensuse leap |
| `bob`   | `b01`…`b10` | the same eight, differently paired with shells                  |

Two users means two **server tenants**: `fleet-tokens.json` maps a name to a
bootstrap token, the server hosts each in its own database under
`/data/tenants/<name>.db`, and requests route by device (or, at enrollment, by
token). Nothing about a tenant is visible to the other — which is a property the
harness checks rather than assumes.

One `Dockerfile.node` builds every node: the base image is a build arg and the
package step picks the distro's package manager, so the yore binary (CGO-free,
static) is built once and dropped into all eight. Each image also carries
`socat` and `jq`, which is how the harness drives the daemon: its protocol is
newline-delimited JSON on a unix socket, so a script can list and approve
devices the same way the TUI does.

```sh
make fleet                 # 500,000 randomized commands across 20 nodes
make fleet TOTAL=20000     # a quick pass over the same ground
make fleet DOWN=1          # tear it down at the end (default: leave it up)
docker/sandbox/fleet.sh --no-build   # re-run the load on the fleet already up
```

### What it does

1. **Builds and starts** 22 containers, then waits for the server's health probe
   from inside a node (nothing is published to the host).
2. **Enrolls all twenty machines**: each tenant bootstraps its first machine with
   its own token, then every further machine redeems a single-use token minted by
   an enrolled one and is approved from it — 2 groups formed, 18 approvals.
3. **Seeds config** per node (`ignore_dirs`, rolling backups).
4. **Checks the real shell hooks** on every distro/shell pair: a pty-driven
   interactive zsh or bash, hooks live, must land its commands in the store.
5. **Generates the load** — one exec per node running `fleet-gen.sh`, a weighted
   random mix of ordinary commands, failures, agent-run commands, secrets,
   space-prefixed commands and ignored-directory commands, with randomized
   text, cwd, exit status, duration, session, and timestamps spread over 90 days.
6. **Waits for every daemon to drain its spool**, timing the gap.
7. **Syncs** in three waves (push, pull, settle), timing each.
8. **Measures**: record throughput per node, search latency (local, deep, fuzzy,
   executor-filtered, tag-filtered, frecency, whole-corpus), database sizes on
   every client and both tenants, daemon RSS.
9. **Verifies**: zero plaintext secrets anywhere; no plaintext at all on the
   server; the drop gate exact; full convergence across each tenant's ten
   machines; **neither user able to see one byte of the other's history**; agent
   attribution and user tags surviving the round trip; one daemon per node, no
   zombies, backups rolling.

Every check prints ✓/✗ and a per-node table lands in `.agents/fleet/` (gitignored)
along with each generator's tallies.

### Notes for anyone extending it

- **Counting requires unique commands.** `yore search --headless` dedupes
  identical command text (`internal/cli/search.go` sets `Dedupe: true`), so the
  generator makes every command unique with a trailing `#<node>-<n>` comment.
  Counting deduped output against a generated total otherwise comes up short.
- **`$YORE_TAG` is an executor**, an alias of `$YORE_EXECUTOR` — filter it with
  `--executor`, not `--tag`. User tags come from `yore tag add`.
- **The pty probes run with `TERM=dumb`.** Any yore process started on a TTY
  writes a terminal status query and reads the answer; a pty with no terminal
  emulator behind it never answers, and that read consumes the input the harness
  had queued. `TERM=dumb` turns the query off. The harness measures the cost of
  the query separately and reports it.
