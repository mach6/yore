# Contributing to yore

Thanks for your interest in yore. This guide covers how to build, test, and land
a change so it passes CI on the first try.

## Prerequisites

- **Go 1.26+** (yore is CGO-free — no C toolchain needed).
- For the full local CI dry-run: **Docker** and the **`drone`** CLI.
- Tooling the `make` targets expect on your `PATH`:
  - [`golangci-lint`](https://golangci-lint.run/) — the lint gate.
  - [`gotestfmt`](https://github.com/GoTestTools/gotestfmt) — human-readable test
    output (`go install github.com/gotesttools/gotestfmt/v2/cmd/gotestfmt@latest`).
  - [`go-covercheck`](https://github.com/mach6/go-covercheck) — the coverage-floor
    gate.
  - `goimports` — import grouping (also run by the lint gate).

## Build & run

```bash
make build      # -> ./bin/yore  (CGO-free static binary)
make release    # cross-compile the tier-1 matrix -> dist/
```

`make release` builds linux/{amd64,arm64}, darwin/{amd64,arm64}, and
freebsd/amd64. WSL runs the linux binaries; native Windows is not yet supported.

## The `make` targets

| Target | What it does |
|---|---|
| `make build` | Build the CGO-free binary to `./bin/yore`. |
| `make test` | `go test` via gotestfmt: a coverage pass (writes `coverage.out`) then a `-race` pass. |
| `make lint` | `golangci-lint run` (covers gofmt, goimports, and vet). |
| `make coverage-check` | Enforce the repo-wide coverage floor with go-covercheck (run **after** `make test`). |
| `make release` | Cross-compile the tier-1 platform matrix into `dist/`. |
| `make drone` | Run the Drone pipeline locally (needs the `drone` CLI + Docker). Scope it with `make drone steps=lint,test`. |
| `make docker` | Build the server image from `docker/Dockerfile`. |
| `make bench` | Store / matcher / crypto benchmarks. |
| `make stress` | End-to-end harness against the 3-container sandbox: records, redacts, syncs, and verifies. `N=150` is a ~20 s smoke once the images are cached (a couple of minutes the first time, when it compiles yore into the client image); the default `N=5000` is a real run. |
| `make fleet` | The same at scale: 20 machines, 8 distributions, 2 users on one multi-tenant server. Minutes, and it pulls ~2 GB of base images the first time. |

Before pushing, the quick loop is:

```bash
make lint && make test && make coverage-check
```

## CI gates

CI (`.drone.yml`) runs, in order, and every gate must be green:

1. **yamllint** — YAML is linted with `.yamllint.yml` (120-col lines, no trailing
   whitespace, newline at EOF). Keep any YAML you add clean.
2. **golangci-lint** — **zero** issues. The enabled linters are in `.golangci.yml`
   (errcheck, govet, staticcheck, unused, ineffassign, gocritic, misspell, nilerr,
   prealloc, revive, unconvert, unparam).
3. **go test** — the coverage pass and the `-race` pass (both via gotestfmt).
4. **go-covercheck** — the repo-wide coverage floor in `.go-covercheck.yml`. Don't
   lower the floor to pass; add tests. Ratchet it up when coverage rises.

## Testing conventions

- Use **testify `require`** — table-driven where it fits.
- **No `t.Fatal`, `t.Error`, or `t.Fail`** — assert with `require`.
- Use **`t.TempDir()`** for any transient storage; never write into the working
  tree or `$HOME`.
- Server tests use `httptest`; crypto tests assert round-trips **and**
  tamper/wrong-AAD failures; the shell init-script output is golden-tested
  (`internal/shell/testdata/`, regenerate with
  `go test ./internal/shell -run TestInitGolden -update`).

## Code & workspace hygiene

These are enforced by review (see `CLAUDE.md`):

- **No transient files in the working tree.** Debug scripts, scratch output, and
  throwaway data go in `.agents/` (gitignored). Never litter the repo.
- **No planning references in code or comments** — no roadmap/phase numbers,
  token IDs, or session notes. A comment explains the code to someone who has
  never seen the plan; planning lives under `docs/`.
- **Never commit secrets or real history.** Device keys, tokens, and history live
  under `~/.config/yore/`, never in the repo or tests. `*.db` and `.env` are
  gitignored — keep it that way.
- **Errors propagate**; decryption/tamper failures must abort loudly, never be
  silently skipped. Anything doing I/O or blocking takes a `context.Context`.
- **Stay CGO-free.** The binary must build with `CGO_ENABLED=0` on every tier-1
  target; no cgo dependencies.

## Keep the docs current

When you change a capability, update it in the same PR:

- **`README.md`** — the user-facing feature set and the comparison table.
- **`docs/architecture.md`** — design and rationale.
- **`docs/protocol.md`** — the sync HTTP API.

Explain design and rationale, not what the code makes obvious.

## Manual end-to-end testing

For a full multi-machine round-trip without touching your real `~/.config/yore`,
use the containerized sandbox in [`docker/sandbox/`](docker/sandbox/): a sync
server plus a zsh client and a bash client, all in containers. See its
[`README.md`](docker/sandbox/README.md) for the compose workflow.

**Run `make stress N=150` before merging anything that touches recording,
redaction, the daemon, sync, or the shell integration.** It takes about twenty
seconds, tears itself down afterwards, and it is the only thing that exercises
the CLI the way a user does. Unit tests do not: the harness has twice been the
thing that noticed a subcommand had been removed from under it, and both times
only because someone ran it. Nothing runs it for you — neither harness is in CI
(they need Docker and minutes, and `.drone.yml` deliberately stays fast).

For a change to sync, multi-user isolation, or anything whose behavior depends
on scale, `make fleet TOTAL=20000` covers ground the 3-container sandbox cannot:
twenty machines across eight Linux distributions, split between two users on one
multi-tenant server, with cross-user isolation and per-record accounting.

## Submitting a change

1. Branch off `main`.
2. Make the change with tests and doc updates.
3. Run `make lint && make test && make coverage-check` (or `make drone` for the
   full pipeline) until green.
4. If you touched recording, redaction, the daemon, sync, or the shell
   integration, run `make stress N=150` as well.
5. Open a PR with a clear description of the behavior change and its rationale.
   Call out any change to a **default** explicitly — an existing user who never
   set the key gets the new behavior on upgrade, and that belongs in the PR
   description whether or not the code change looks small.

By contributing you agree that your contributions are licensed under the
project's [MIT License](LICENSE).
