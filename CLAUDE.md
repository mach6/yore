# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Sensitive Files

**Never read or display `.env` files, or any `config.json` holding secrets.** They contain tokens and keys. Read them silently only when strictly needed; never cat/grep/print their contents or echo them into output. To edit `.env`, use the Edit tool with exact old/new strings without reading it first, or ask the user to make the change.

yore's own secret material — device private keys (`~/.config/yore/device.key`), the auth token — lives under `~/.config/yore/`, never in the repo. Never write keys, tokens, or real history into the working tree or tests.

## Project Overview

`yore` is a cross-machine, end-to-end-encrypted shell-history tool (module `yore`): it records every command (zsh + bash), keeps this host's history in a local bbolt store, and — pointed at a self-hosted sync server — makes all machines' history searchable everywhere, with the server only ever holding ciphertext. One static, **CGO-free** Go binary is the client, the background daemon, the TUIs, the importer, **and** the sync server, dispatched by subcommand (cobra CLI).

Core design invariants (do not break):
- **Prompt latency is sacred.** The shell hook does one O_APPEND spool write + a best-effort daemon poke — no db, network, or crypto on the prompt path.
- **`data.db` holds ONLY this host's history, and no plaintext but this host's is ever written to disk.** Other hosts' history is decrypted into the daemon's RAM and stays there. The *ciphertext* it was decrypted from is cached in `remote.db` (`internal/rstore`) alongside the per-host pull cursor — byte-for-byte what the server already holds, unreadable without this device's keys — because a RAM-only cursor made every daemon start re-pull every machine's archive from seq 0. The cache is derived: deleting it costs one re-pull.
- **E2E with no master-key replication.** Per-device keypairs → one wrapped History Key → auto-rotating epoch DEKs → records. Every hot path (search, decrypt, enroll, revoke) is O(1) in how much history has accumulated — nothing degrades as the archive grows over years.
- **Single-directory footprint.** All client state under `~/.config/yore/`; nothing scattered in `$HOME`.
- **The server stores ciphertext + device public keys only.** Mutating sync requests are signed per-device (a leaked bearer token can't push garbage or revoke devices).

## Go Conventions

- **Errors** propagate up the stack; decryption/tamper failures abort loudly and are never silently skipped.
- **Context** on anything that does I/O or blocks (syncer, server handlers, enrollment).
- **bbolt is single-owner:** the daemon owns the local db; other processes fall back to direct access via the file lock. Never open two writers.
- **CGO-free:** the binary must build with `CGO_ENABLED=0` on every tier-1 target. No cgo dependencies.
- **AAD binds ciphertext** to `recordID|hostID|seq|keyID`; keep that contract when touching crypto.

## Commands

Requires Go 1.26+.

```bash
make build              # -> ./bin/yore  (CGO-free static binary)
make test               # go test with gotestfmt: a coverage pass + a -race pass
make lint               # golangci-lint (covers gofmt/goimports/vet)
make coverage-check     # enforce the coverage floor via go-covercheck (run after `make test`)
make drone              # run the Drone pipeline locally (needs the drone CLI + docker)
make release            # cross-compile the tier-1 matrix -> dist/
make docker             # build the server image (docker/Dockerfile)
```

Always `gofmt`, `goimports`, and `go vet` your changes — `make lint` gates all three.

## Testing

Go tests use **testify `require`**, table-driven, with **no `t.Fatal`/`t.Error`/`t.Fail`** and `t.TempDir()` for any transient storage. Server tests use `httptest`; crypto tests assert round-trips + tamper/wrong-AAD failures; init-script output is golden-tested (`internal/shell/testdata/`, regenerate with `go test ./internal/shell -run TestInitGolden -update`). Coverage is enforced via `mach6/go-covercheck` (`.go-covercheck.yml`).

## CI/CD

Drone (`.drone.yml`): `yamllint` → `golangci-lint` → `go test` (gotestfmt, coverage + race) → `go-covercheck` → build the server Docker image and, on `main`/tags, push it to `$DOCKER_REGISTRY/yore`. yore is CGO-free, so CI uses off-the-shelf `golang:1.26-alpine`/`golangci-lint` images (no custom builder image needed).

## Workspace Rules

- **No transient files in the working tree.** Debug scripts, scratch output, throwaway test data → `.agents/` (gitignored). Never litter the repo.
- **No planning references in code or comments.** No roadmap/phase numbers, token IDs, group names, or session notes in source — a comment explains the code to a contributor who has never seen the plan. Plan docs do not belong in the repo at all: roadmaps, phases, and open issues live in the issue tracker.
- **Docs are maintained, and each has one tone.** Update them when you change a capability. `README.md` is user-facing — what it does and how to use it, no internals. `docs/architecture.md` is a design doc for engineers — how the pieces fit and why, not what the code makes obvious. `docs/protocol.md` is the exact formats. Docs describe what is true now: never a changelog, a roadmap, or a list of known issues, and never a reference to any particular person, machine, or filesystem.

## Reference Projects

Two projects are useful prior art. Neither is part of yore's build, and neither one's location is recorded here — ask if you need a checkout.

- **Atuin** ([atuin.sh](https://atuin.sh/)) — "magical shell history", the well-known Rust shell-history sync tool, and the closest prior art to yore. Consult it for protocol, sync, and UX design decisions.
- **suvadu** ([suvadu.sh](https://suvadu.sh/)) — a Rust shell-history replacement storing structured history in SQLite and exposing it as queryable "shared memory" for AI agents. Reference for structured-history modeling and agent-facing query design.

## Shortcuts & Personas

- **"Gopher"** = Go architecture mindset. Adopt when writing or reviewing Go structure: idiomatic patterns, interface design, package boundaries, error handling, naming, concurrency. Trigger: "put Gopher on it", "Gopher review".
- **"Steve"** = the team's best engineer. Adopt when reviewing or writing code and the user says "be Steve", "Steve review", "put Steve on it". Steve writes perfect, readable, terse code and refactors large swaths for the good of the project; his changes always work. As a reviewer he is sharp and honest — he finds the real issues others miss (correctness bugs, races, leaks, hidden coupling, dead code, needless complexity), cites them precisely (`file:line`), rates severity, and gives a terse fix or the corrected code. He does not rubber-stamp, pad, or hedge; he praises only what's genuinely good and says plainly when something is wrong — including in his own prior work.
