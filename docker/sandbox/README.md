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

The client config (server URL, token, integration mode) is pre-seeded into each
container from the compose environment by `client-entrypoint.sh`, so `yore setup`
runs without prompts.

## Bring it up

```sh
cd docker/sandbox
docker compose up -d --build
```

A convenience wrapper for running yore in a container (used below):

```sh
dc() { docker compose -f docker/sandbox/compose.yml exec -T "$@"; }
```

## Walkthrough — enroll two machines and sync a command E2E

```sh
# 1) zsh-box becomes the first device and bootstraps the history group (the HK).
dc zsh yore setup --server http://server:8080 --token sandbox-token \
      --integration takeover --name zsh-box

# 2) bash-box registers as pending and prints a verification code.
dc bash yore setup --server http://server:8080 --token sandbox-token \
      --integration takeover --name bash-box

# 3) From the already-enrolled zsh-box, confirm the code matches and approve.
#    (approve prompts y/N — feed 'y' since we exec with -T)
dc zsh yore devices                       # shows bash-box pending + its code
printf 'y\n' | dc zsh yore devices approve <BASH_DEVICE_ID>

# 4) Record a command on zsh-box and push it.
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
