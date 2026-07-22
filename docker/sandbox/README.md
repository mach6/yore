# yore sandbox

A self-contained, three-container playground for exercising yore's full
cross-machine sync without touching your host or any real server:

| service  | what it is                                             | hostname   |
|----------|--------------------------------------------------------|------------|
| `server` | the sync server — stores **ciphertext only**           | (internal) |
| `zsh`    | a client with zsh + the yore hooks wired in            | `zsh-box`  |
| `bash`   | a client with bash + the yore hooks wired in           | `bash-box` |

Clients reach the server over an internal Docker network (plain HTTP — no TLS is
needed inside the sandbox; per-device request signing still protects writes).
Nothing is published to your host, and `down -v` removes every trace.

The client config (server URL, integration mode) is pre-seeded into each
container from the compose environment by `client-entrypoint.sh`, so `yore setup`
runs without prompts. No credential is seeded — config.json holds no secrets.
Enrolment is authorized by a **single-use ticket**, taken from `$YORE_TICKET`.

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
to mint a ticket: a reused volume leaves an orphaned group and every `yore setup`
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
dc zsh yore setup --server http://server:8080 --ticket sandbox-token \
      --integration takeover --name zsh-box

# 2) Adding a machine needs a single-use ticket minted by an enrolled one.
TICKET=$(dc zsh yore devices ticket)   # the ticket is the only thing on stdout

# 3) bash-box redeems it, registers as pending, and prints a verification code.
dc bash yore setup --server http://server:8080 --ticket "$TICKET" \
      --integration takeover --name bash-box

# 4) From the already-enrolled zsh-box, confirm the code matches and approve.
#    (approve prompts y/N — feed 'y' since we exec with -T)
dc zsh yore devices                       # shows bash-box pending + its code
printf 'y\n' | dc zsh yore devices approve <BASH_DEVICE_ID>

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
```

The rc files already `eval "$(yore init …)"`, so hooks are live. In takeover
mode the shell's own history file is disabled and yore is the single source of
truth (so `!N`, up-arrow, and Ctrl-R all read from yore).

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
