# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Sensitive Files

**Never read or display `.env` files, or any `config.json` holding secrets.** They contain tokens and keys. Read them silently only when strictly needed; never cat/grep/print their contents or echo them into output. To edit `.env`, use the Edit tool with exact old/new strings without reading it first, or ask the user to make the change.

yore's own secret material, the device private keys (`~/.config/yore/device.key`) and the auth token, lives under `~/.config/yore/`, never in the repo. Never write keys, tokens, or real history into the working tree or tests.

## Where things are written down

Read the source, do not restate it here.

- `docs/architecture.md` is the design doc, and its **Invariants** section is the list you must not break. Read it before changing the daemon, sync, storage, crypto, or the shell hook.
- `CONTRIBUTING.md` is the build, test, and CI reference: the `make` targets, the testing conventions, and the gates a change has to pass. Read it before adding tests or proposing a change.

## Go Conventions

- **Errors** propagate up the stack; decryption/tamper failures abort loudly and are never silently skipped.
- **Context** on anything that does I/O or blocks (syncer, server handlers, enrollment).
- **bbolt is single-owner:** the daemon owns the local db; other processes fall back to direct access via the file lock. Never open two writers.
- **CGO-free:** the binary must build with `CGO_ENABLED=0` on every tier-1 target. No cgo dependencies.
- **AAD binds ciphertext** to `recordID|hostID|seq|keyID`; keep that contract when touching crypto.

## Workspace Rules

- **No transient files in the working tree.** Debug scripts, scratch output, throwaway test data → `.agents/` (gitignored). Never litter the repo.
- **No planning references in code or comments.** No roadmap/phase numbers, token IDs, group names, or session notes in source. A comment explains the code to a contributor who has never seen the plan. Plan docs do not belong in the repo at all: roadmaps, phases, and open issues live in the issue tracker.
- **Never use an em-dash.** Not in docs, not in Go doc comments or any other comment, not in a user-visible string, not in a commit message or PR description. Use the punctuation it would have stood for: a colon when what follows explains what came before, parentheses for a bracketed aside, a semicolon or full stop between independent clauses, a comma otherwise. `·` is the UI's glyph for a value a record never carried; see `theme.Unknown`. `grep -rn` for the character across the repo must return nothing.
- **Docs are maintained, and each has one tone.** Update them when you change a capability. `README.md` is user-facing: what it does and how to use it, no internals. `docs/architecture.md` is a design doc for engineers: how the pieces fit and why, not what the code makes obvious. `docs/protocol.md` is the exact formats. Docs describe what is true now: never a changelog, a roadmap, or a list of known issues, and never a reference to any particular person, machine, or filesystem.

## Reference Projects

Two projects are useful prior art. Neither is part of yore's build, and neither one's location is recorded here; ask if you need a checkout.

- **Atuin** ([atuin.sh](https://atuin.sh/)): "magical shell history", the well-known Rust shell-history sync tool, and the closest prior art to yore. Consult it for protocol, sync, and UX design decisions.
- **suvadu** ([suvadu.sh](https://suvadu.sh/)): a Rust shell-history replacement storing structured history in SQLite and exposing it as queryable "shared memory" for AI agents. Reference for structured-history modeling and agent-facing query design.

## Shortcuts & Personas

- **"Gopher"** = Go architecture mindset. Adopt when writing or reviewing Go structure: idiomatic patterns, interface design, package boundaries, error handling, naming, concurrency. Trigger: "put Gopher on it", "Gopher review".
- **"Steve"** = the team's best engineer. Adopt when reviewing or writing code and the user says "be Steve", "Steve review", "put Steve on it". Steve writes perfect, readable, terse code and refactors large swaths for the good of the project; his changes always work. As a reviewer he is sharp and honest: he finds the real issues others miss (correctness bugs, races, leaks, hidden coupling, dead code, needless complexity), cites them precisely (`file:line`), rates severity, and gives a terse fix or the corrected code. He does not rubber-stamp, pad, or hedge; he praises only what's genuinely good and says plainly when something is wrong, including in his own prior work.
