# yore architecture

How yore is built and why it is built that way. Exact wire formats (the sync
HTTP API, the crypto scheme, the daemon socket protocol) are in
[`protocol.md`](protocol.md). Setup and usage are in the [README](../README.md).

## Overview

yore is one static, CGO-free Go binary. A cobra subcommand selects the role it
plays:

| Role | Subcommands |
|---|---|
| Shell-hook fast path | `record`, `filter`, `export`, `hook <agent>` |
| Search UIs | `search` (inline Ctrl-R panel, plus `--headless`), `browse`, `stats`, `agents`, `devices` |
| Background daemon | `daemon run\|stop\|status`, `status`, `stop`, `sync` |
| Enrollment | `setup`, `recover`, `devices token` |
| Sync server | `server`, `healthcheck` (the container's probe, not a command you type) |
| Agent integration | `init`, `uninit`, `mcp-serve` |
| Misc | `import`, `tag`, `doctor`, `get-config`, `set-config`, `gen-id`, `version` |

Pure Go with `CGO_ENABLED=0` means it cross-compiles with no toolchain to the
tier-1 matrix: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64,
freebsd/amd64.

## Data flow

```
                    redact gate (leading-space / ignore-dirs / secret rules)
                          │ drop (silent, exit 0)
shell hook ─(append,<1ms)─┴▶ spool file ─▶ ┌─ yore daemon (unix socket) ─────────┐
                                           │  owns local bbolt (this host only)  │
Ctrl-R / hb / hs ◀──── unix socket ──────▶ │  RAM corpus (warm snapshot + tail)  │◀─HTTPS─▶ yore server
   (thin TUI clients)                      │  remote plaintext: RAM only         │  (ciphertext,
                                           │  background sync loop (push/pull)   │   wrapped keys,
                                           └─────────────────────────────────────┘   public keys)
```

### The prompt path is the load-bearing invariant

Everything else in the design bends to keep the shell prompt fast. On every
command the hook runs `yore record`, which:

1. reads the command text on stdin (capped at 1 MiB), dropping it if empty;
2. runs the redact gate, where a rejection returns silently with exit 0;
3. writes one JSON line to a `.tmp` spool file, fsyncs it, and renames it into
   place, so a concurrent drain can neither read a half-written record nor
   unlink a file still being written;
4. best-effort pokes the daemon over its unix socket (spawning one if absent)
   with tight deadlines, ~150 ms worst case, and backgrounded;
5. exits 0.

No database, no network, and no crypto run on the prompt path. If the daemon is
dead the record still lands durably in the spool and is drained at the next
daemon start, so recording survives a daemon that is down or was never started.

## Packages (`internal/`)

Leaf-contract packages import nothing else in the tree.

**Leaf contracts**

- **rec**: the `Record` type. Its JSON is the encoding for both the spool and,
  as the sealed plaintext, the sync payload. ULID `id`; `type` is `""` for a
  command, `"delete"` for a tombstone, `"tag"` for a user-tag operation, or
  `"prompt"` for one agent prompt.
- **proto**: the newline-delimited JSON protocol over the daemon's unix socket.
- **wire**: the JSON types of the server's HTTP API.
- **config**: resolves the state directory (`$YORE_DIR`, else
  `~/.config/yore/`) and all settings, applying defaults through accessors.

**Storage and capture**

- **spool**: crash-safe fsync'd handoff files published by rename. `Drain`
  tolerates a torn line in an older file, sweeps abandoned temps, and dedupes by
  record id.
- **store**: the local bbolt database (`data.db`), holding only this host's
  stream. Single-owner via an exclusive file lock; a competing opener gets
  `ErrLocked`. Appends are idempotent by record id, assign a per-stream `seq`,
  and delete via tombstone. `BackupTo` takes a hot online snapshot. A `meta`
  bucket carries facts only the lock holder may write: `host_id`, `hostname`,
  `last_uploaded_seq`, `revoked_by_server`, `schema`.

**Runtime and search**

- **daemon**: the only process that opens the store.
- **match**: whitespace-split substring terms with smart-case and an
  incremental prefix-reuse filter, plus a subsequence fuzzy matcher.
- **tui/theme**, **tui/hl**: adaptive lipgloss styles; a best-effort shell
  syntax classifier layered under match highlighting.
- **tui/search**: the inline Ctrl-R panel. **tui/browse** is the full-screen
  browser. **tui/keyhelp** holds one binding table per UI, rendered two ways.
- **risk**: a deterministic rule-based command classifier, advisory only.
- **mcp**: the local read-only MCP server over the daemon's query layer.

**Security and sync**

- **cryptobox**: the encryption core: device keypairs, the History Key, epoch
  data keys, per-record sealing.
- **reqsign**: the canonical string and headers a device signs on every sync
  request. One implementation shared by client and server.
- **redact**: the recording gate. Never spool, store, or sync a credential.
- **server**: the multi-tenant sync server. Ciphertext and public keys only.
- **syncer**: the client engine: encrypt and push local records, pull and
  decrypt remote ones, and the enroll / approve / revoke primitives.

**Integration**

- **shell**: the embedded zsh, bash, and fish hook scripts (plus vendored
  bash-preexec), rendered per integration mode with `text/template`.
- **importer**: zsh (extended history, unmetafy, multiline) and bash parsers.
- **cli**: the cobra command tree; each subcommand is a `runXxx` returning an
  exit code. Also ships shell completions.

## The daemon

A long-lived background process, auto-spawned on first use (detached, `Setsid`)
and idle-exiting after `daemon_idle` (default 30m). It is the sole owner of the
local store and the authority for all search.

Idle exit is invisible to whatever is on screen: a client treats a lost
connection as a reconnect, spawns a daemon if none is listening, and resends.
The one operation never resent is minting an enrollment token, because a retry
would leave a second live invitation standing on the server when only the first
was ever shown to anyone.

**RAM corpus.** On start the daemon loads the live search corpus from a warm gob
snapshot (`corpus.snap`) and folds in the store tail above the snapshot position
via `store.Since`; a missing or corrupt snapshot falls back to a full cold load.
The corpus is append-only under an RWMutex: readers snapshot the slice header
under RLock and then scan lock-free. The snapshot is rewritten every 5 minutes
and again on shutdown, so restarts paint instantly.

**Remote history: plaintext in RAM, ciphertext on disk.** Other hosts' history is
decrypted into RAM only and never written to disk. The ciphertext it was
decrypted from *is* cached, in `remote.db` (`internal/rstore`), alongside the
per-host pull cursor. That is byte-for-byte what the server already holds and is
unreadable without this device's keys, so it does not weaken the invariant, and
it is what makes startup incremental. With RAM-only cursors, every daemon
lifetime re-downloaded and re-decrypted every other machine's entire archive from
seq 0, several times a day once the idle timeout recycled the process.

`remote_keep` (default 50,000) caps how much of each host's tail is retained, in
the cache and in RAM alike, so neither grows without bound. The cache is derived
data: deleting it costs one full re-pull. It is reset when the configured server
URL changes, because the cached ciphertext then belongs to a different group.
Nothing else in the sync config invalidates it: a certificate pin, a rotation
cadence, or a retention bound all describe how to reach the *same* archive, and
dropping the cache for one of those would spend a full re-pull on a transport
edit, which is the exact cost the cache exists to avoid.

**Memory cost.** The corpus is the daemon's whole footprint and is linear in
records: roughly 620 bytes of live heap per record (a 304-byte `rec.Record`,
~200 bytes of size-class-rounded strings, and the parallel command header), with
resident memory around 3× live because Go's collector keeps that much headroom
by default.

```
daemon RSS ≈ 2.5 KB × (this host's records + remote_keep × other hosts)
```

Measured on a ten-machine group holding 470,000 records: 1.1 GB. `remote_keep`
bounds the second term; the first is unbounded, since your own history grows
forever, at roughly 75 MB of RSS per 30,000 commands.

**Prompt index.** Agent prompt text lives on its own record, so the daemon keeps
a `promptID → text` index, fed by the startup store scan, local ingest, and
remote pull, and rejoins each query row with its prompt text on the way out.

**Ingest.** A spool wake starts a 75 ms straggler window, then one fsync'd
`IngestSpool` drain folds the new rows into the corpus.

**Concurrency.** One goroutine per socket connection, serving that connection's
requests in order so its `match.Filter` stays single-threaded; one ingest
goroutine; the main goroutine owns the idle timer and shutdown.

**Socket operations** (`~/.config/yore/daemon.sock`, mode 0600, one JSON object
per line, one response per request):

| Op | Purpose |
|---|---|
| `ping` | liveness, reset the idle timer, nudge spool ingest |
| `record` | spool one record (fsync) and nudge ingest |
| `query` | search: scope, sort, fuzzy, executor and tag filters, dedupe, paging |
| `hosts` | per-host live record counts; warms the remote cache |
| `tags` | known user tags with how many commands carry each |
| `delete` | tombstone one record by id |
| `devices` | list enrolled devices |
| `token` / `tokens` / `revoketk` | mint, list, and cancel enrollment tokens |
| `approve` / `revoke` | approve a pending device / revoke and rotate keys |
| `sync` | force a synchronous push/pull cycle |
| `status` | pid, uptime, local rows, remote state, version |
| `shutdown` | graceful exit |

## Search model

Every query is `match → scope filter → optional executor/tag filter → sort →
dedupe → window`. Matching is substring (smart-case) by default or fuzzy
(subsequence); the incremental per-connection filter applies to substring only.

Scopes:

- **local**: this host only. Always available, offline-safe.
- **all** / **host**: merge the RAM remote cache. A deep query, or opening the
  browser, nudges a background sync so the *next* query is richer; the request
  itself never blocks on the network. Offline, a deep query degrades to whatever
  is already cached.
- **session** / **cwd**: filter by shell session id or exact working directory.
- **workspace**: commands run anywhere under the current git repo (walk up for
  `.git`; local only).

Sort is recency (descending `start_ms`, ties by descending `seq`) or frecency
(frequency × bucketed recency, with a ×2 boost for the same cwd, which
inherently collapses to one row per command). Recency can additionally dedupe,
newest winning.

The default window is 200 rows. `limit: 0` takes that default; `limit: -1`
(`proto.LimitAll`) asks for every match. The corpus is already in RAM and all
matches are sorted before windowing, so `LimitAll` costs serialization rather
than work. It is what a browser of the whole archive should ask for, and what
the top-N callers (Ctrl-R, MCP, headless search) deliberately do not.

## The browser (`internal/tui/browse`)

One Bubble Tea program with four screens: the tiled browse panes, stats, the
agent explorer, and devices. `yore stats`, `yore agents`, and `yore devices` open
the same program directly on one screen. Four subsystems carry all of it, and
each exists so that a behavior is written once rather than once per screen.

**Geometry** (`panes.go`) is the single place that decides how the screen is
carved up. It resolves each view's pane rectangles in absolute screen cells plus
the draggable seams between them; renderers size themselves from that geometry
and `mouse.go` hit-tests against it, so the two can never disagree about where a
pane is. Browse tiles three panes, the agent explorer five. Zoom expands the
focused pane to the whole frame, with a variant that keeps a companion detail
pane beside it for the lists whose whole point is one. Seam positions are held
as a per-mille fraction of the axis they cut, so the ratio survives a terminal
resize, and are persisted only when a drag *settles*: one write per resize,
always a layout the user stopped on. Per-mille rather than percent
because at 140 columns one percent is 1.4 cells, coarse enough that a dragged
seam visibly snaps away from the pointer.

**Columns** (`columns.go`) generalize five row tables (browse results, the
explorer's prompts and commands, and the devices view's machines and tokens)
into one list of specs per table saying what a column is called, how wide it is,
how it draws a cell, and how it orders two rows. Layout, header, row rendering,
and the columns pane are loops over those specs. They generalize because they
share a shape: fixed metadata columns, then one flexible column at the end taking
what is left and carrying the text. `colTableDef` captures that, so the width
arithmetic and shedding rules are written once; what stays per-table is the cell
gap, the flexible column's floor, the order columns give up width in, and the
table's natural order.

Sorting is client-side (every matching row is already in RAM) and **stable**
over a slice in the table's natural order, so that order stays the tiebreak
underneath whatever was asked for on top. Unknown sorts below every
known value for exit status and duration: a row still running has no outcome,
and a command whose timing was never reported is not a fast command. Because a
table's default *is* its natural order, any other order is applied to a
**copy**: the unfiltered prompt list is the aggregate itself, and
reordering it in place would rewrite what every other pane reads from. The
cursor is carried across a re-sort by record id, not index.

Three different things can take a column off screen (the user hid it, the rows
have nothing to put in it, or the pane was too narrow and it was shed), and the
pane names which, because hiding a column the data gate already closed would
otherwise look broken. Those gates are one-way: the pane can hide a column but
cannot force a blank one back on, and the flexible column cannot be hidden at
all.

**Persisted layout.** Column choices and seam positions go to `ui.toml`, never
to `config.toml`: the hand-edited settings file is not something a keystroke
should rewrite. `ui.toml` is written whole on every save, so seams and every
table's columns travel in one struct through one callback. Columns are stored by
**name** under a per-table key, because a file that outlives releases cannot
hold indexes. Loading is fail-safe: an unknown name is ignored, a sort naming an
unsortable column is dropped, a `hidden` entry for a column that must always
show is refused. A table still at its defaults writes nothing, so an untouched
`ui.toml` inherits whatever the default later becomes.

**Key help** (`internal/tui/keyhelp`) drives two renderings from one binding
table per UI, so they cannot drift: a one-line contextual footer always on
screen, and the full grouped panel behind `?`. A test parses the key handlers'
case clauses out of the package's own source and fails if a key is handled but
described nowhere, or described but no longer handled. That is what lets the
panel be trusted as the answer to "what can I press here". The footer names the
keys for the state the UI is actually in and follows focus; the panel is
exhaustive for one view and includes mouse gestures, since nothing else on screen
says the panes are draggable. The Ctrl-R panel carries the same pair with one
forced difference: a filter box cannot spend `?` on help, because searching for
`?` has to work.

### Selection is held by identity

`space` marks the row under the cursor and `ctrl+a` is a master-checkbox
tri-state, clearing the selection if everything currently showing is already
checked and otherwise checking everything shown, where "shown" means after the
query, period, and search have narrowed it, so a second press undoes the first
rather than being a one-way ratchet.

The set is held **by record id**, not table index, so a resort or an earlier
delete in the same batch can never carry a mark onto a different row's data. It
is cleared on every path that changes which records the table shows, but a
refresh the *app* initiates behaves the opposite way: the warm loop and post-sync
re-query keep the selection and merely prune marks for rows that are no longer
there. Clearing on those would make `space` in a freshly opened browser appear to
undo itself a beat later.

Delete and tag are the two consumers. Both act on the checked set when one exists
and fall back to the cursor row otherwise, and both prompts name which is about
to happen. Neither has a batch call in the daemon protocol, so a bulk action is N
round trips over one connection, run off the event loop so the UI keeps
redrawing. Every call is attempted regardless of an earlier failure (a row
already tombstoned cannot be un-tombstoned by giving up) and the result reports
both counts. They differ afterwards because they differ in what happens to the
rows: a deleted row leaves the table, so its id is dropped and only failures
remain ready to retry, while a tagged row survives, so the selection carries
forward and tagging one batch twice needs one selection.

Running off the event loop keeps the UI alive, which cuts both ways: the loop
goes on handling every message for the length of the batch. Keys are swallowed
while one is in flight, because a table the batch is halfway through changing is
a snapshot that is already wrong. The two that survive are the ones about the run
itself: `esc` stops it and `ctrl+c` leaves. A confirmed batch is unbounded, since
the table holds every matching row, so neither is optional; the flash counts the
round trips as they come back and names the key that stops them. A stop takes
effect between calls, never during one, so the call in flight is always seen
through to its answer and the closing flash can give both numbers. What the run
already did stands, because a tombstone is not the browser's to take back, and
the rows it never reached are untouched and still checked, which is what makes a
stopped run resumable with the key that started it. Query results are
the harder half. The sequence counter orders *deliveries*, not the daemon state
each query observed, so an answer computed before a delete and delivered after it
carries a higher number than anything applied so far and would be accepted, rows
and all. A delete therefore remembers what it removed and filters those ids out
of any answer no newer than the query that was in flight when it ran. The first
answer from a query issued after the delete reflects the tombstones, because the
call had returned before it was asked, and releases them.

### A UI may filter only if it can disclose

Both interactive search UIs hide agent-run commands by default
(`hide_agent_commands`, toggled per session). One agent prompt can produce forty
tool invocations, which bury a morning of the user's own work in a table whose
promise is "what happened here, newest first". That history is not lost: it
belongs in the agent explorer, grouped under the prompt that caused it.

The filter is applied **server-side**, since filtering after `Limit` would spend
the row budget on rows the UI is about to drop. Hiding a whole category of
history is only acceptable if the UI says so, which is the rule this design
turns on: the daemon returns how many rows the filter dropped, the count is on
screen whenever something is held back, the **empty state names it** (a search
whose only matches are an agent's must not answer "no matches", which would be a
lie about the user's own history), and the key that undoes it is advertised
exactly when something is hidden. With a period selected the browse table drops
the count rather than adjusting it, because the period narrows client-side over
rows the daemon already dropped and the note should not quote a number it cannot
stand behind. `--headless` never hides anything at all: it feeds scripts, which
want the whole archive and have no status line to be told what was withheld.

The tag and executor filters are server-side too, and each has two keys: one
adopts the highlighted row's value, and one opens a typed box seeded with the
filter in force, which reaches values no visible row carries. The box does not
filter as you type, unlike the search field: every change is a round trip, and a
half-typed tag matches nothing, so the table would empty out under each prefix on
the way to the name meant.

One period (Today, 7d, 30d, 90d, All) drives every screen. "Today" is the
calendar day in local time, not a rolling 24 hours, which would fold yesterday
evening into this morning's hour-of-day buckets. The daemon's query protocol
carries no time field, so the browse table filters the rows it got back, and it
got back every row matching the query, so the period narrows the whole timeline
rather than a slice of it.

### The agent explorer

Groups agent commands by the prompt that caused them. An executor sidebar and a
hosts pane each filter every other pane, and both filters are held by *name*, not
row index, so an agent or machine that drops out of the period releases the
filter rather than handing it to whoever inherits the row.

What the details pane describes is chosen by whichever list pane focus last
landed on, and it *sticks* when focus lands on the details pane itself.
Otherwise the pane's two reasons to exist work against each other: the only route
to a command's full record is to focus the pane, and focusing it would swap the
subject on the way. The prompt list spends a column on the host only when its
rows can disagree about it: several machines in the sample *and* no host filter
up. Filtered to one, that column repeats a name the pane title, the hosts
bullet, and the header line all already carry.

`/` filters the focused pane's list with the same matcher the browse view and
Ctrl-R use, and the two lists keep **separate** queries, so tabbing between panes
never silently re-points one pane's filter at another's rows. What is filtered is
the *view*, not the aggregate: a command filter narrows the command pane while
the prompt's counts and duration still describe the prompt, because rewriting
those would make the details pane and the prompt list disagree about the same
prompt. A filtered count is marked as one (`PROMPTS  1/1  /tower`), since
otherwise a narrowed list is indistinguishable from a quiet period. Esc backs out
one visible thing at a time, and the footer names whichever is next.

### Stats, color, and glyphs

The activity heatmap, daily trend, and hour-of-day histogram span the full width,
and a wider terminal buys *more history* rather than more whitespace. The heatmap
and daily trend deliberately ignore the period. They show the shape of activity
around the window the other panels summarize, so narrowing to Today must not
blank them, and both scale to the peak of what they actually draw, so a spike
outside the visible window cannot flatten the bars on screen. The histogram does
respect the period and, on Today, leaves hours that have not happened yet blank
rather than drawing them as zero.

Horizontally the ranked lists (top programs, commands, and directories, then by
executor, by host, by tag) shed from the right until they fit, which is the
order they earn their width in. The tag column is gated on the data as well as
on the width: it is drawn only once something is tagged, because a permanent
sixth column would cost the other five their width on an archive that has never
carried a tag. (The browse table's own tags column is gated the same way.) It
also **counts differently from every list beside it**: a row lands in one bucket
per tag it carries, so those counts do not partition the total the way a row's
single host or executor does.

Vertically the screen fits charts whole or not at all: each is offered the rows
it needs and declines if taking them would starve the ranked columns, so a short
terminal loses a chart cleanly instead of showing one with its axis sliced off.
The heatmap has a compact fallback, weeks folded into one row of totals, because
the full graph needs ten rows and a stock 80×24 terminal has never had them.

Four rules govern color and glyphs. A terminal has very few levers for hierarchy,
and a lever spent twice is a distinction the eye cannot find:

- **Chart ink is its own single-hue ramp**, deliberately not the UI accent, which
  would otherwise make everything with ink the color of everything selectable.
  Intensity rides on lightness as well as glyph height, and the heatmap draws a
  solid block rather than a `░▒▓` density ramp, the least portable glyphs in the
  box-drawing set, where a font that renders `▒` and `▓` alike silently collapses
  two levels.
- **Headings come in exactly three ranks**: accent+bold for a pane name, bold for
  a heading inside a pane, dim for the chrome below both.
- **State is a glyph, not a color**, so it survives colorblindness and
  screenshots. Exit status is `·` unknown, `✓` ok, `✗N` failed. `·` is the
  same glyph everywhere a value is missing (an unreported duration, an untimed
  imported command, an empty heatmap day), and it is one cell wide in every
  terminal, which the em-dash it replaced was not. Risk is a
  glyph-first ramp shared verbatim with the MCP output (`⛔ ⚠ ▲ • ✓`), shown for
  **every** command including clean ones, since a row that appears only on a hit
  cannot be told from a rule that never ran. The prompt details pane has no risk
  row: a prompt is not a command.
- **Identity is a hue.** Hostnames and executor names hash into a fixed 8-hue
  palette, red and green excluded because those belong to exit status, so the
  same identity is the same color everywhere it appears. Command text is
  syntax-lit wherever it is command text, with match highlighting layered on top
  and always winning, but never over prose, since shell coloring over English
  paints arbitrary words as flags.

**Sample honesty.** The stats and agent screens aggregate the whole archive, so
the header normally reads "all history". It derives that from the response itself
(the daemon returned fewer rows than it matched) rather than from a compiled-in
ceiling, so the claim stays true whatever any caller asks for. If a sample ever
does arrive short, the header says so and how far back it reaches; without that,
every window wider than the sample shows identical numbers and the period tabs
read as broken when they are working exactly as intended.

### Devices

Two stacked tables, machines over tokens, with every action asking first: approve
a pending machine (the prompt quotes its verification code, so the out-of-band
check is in front of the person answering), revoke one and rotate the group's
keys, mint an enrollment token, copy the one just minted, cancel an unclaimed
one, refetch both lists. The machine list opens on what needs attention, pending
above active above revoked, and the token list newest-first. An open token's
`expires` column counts *down*.

**Nothing on this screen moves unless asked.** A machine running `yore setup`
elsewhere shows up here as pending, so it is tempting to poll. But this is also
the screen holding a minted token's plaintext, which exists nowhere else and is
there to be read and copied off. A list that reorders itself under a cursor, or a
redraw during a mouse selection, costs more than the wait it saves.

Minting is one token at a time, because a press mints a real standing invitation:
the key is dead while a mint is in flight and while a minted token is still on
screen. The guard clears when the request *lands*, not when it succeeds, so one
failed mint cannot disable the key for the session. The copy comes from the held
value rather than the screen, because a narrow pane clips the banner and leaves a
secret that can be read but not selected, and there is no second chance to fetch
it. Cancelling a token rotates nothing: it let no one in, which is what
separates it from revoking a device.

Device management lives here and nowhere else. The CLI once had a second
implementation of approve and revoke, and two code paths for one dangerous
operation is two sets of confirmation rules to keep honest. `yore devices token`
stays, because minting an enrollment credential is something you pipe, not
manage.

## Recording and redaction

`yore record`, `yore import`, and the shell-history gate `yore filter` all run
the same ordered gate before anything is persisted:

1. **Leading-space opt-out**: a command starting with space or tab is skipped
   unless `record_space_prefixed` is set.
2. **Ignore-dirs**: a command whose cwd is at or under an `ignore_dirs` prefix
   (segment-aware) is skipped.
3. **Ignore-patterns**: a command matching one of the user's regexes is
   skipped. Like `ignore_dirs`, this is the user saying "never record this", so
   the record is dropped rather than masked.
4. **Secret rules** (`internal/redact`); see below.

Rules load from the editable, seeded `~/.config/yore/redact.yml`. Each is a name,
a Go regexp, optional cheap literal hints (a hot-path pre-filter, so the regexp
only runs if a hint is present), and an optional case-fold flag. Built-ins anchor
on the *shape* of a credential: AWS access and secret keys, GitHub, Slack,
Google, Stripe, OpenAI/Anthropic, Hugging Face, npm, PyPI and SendGrid tokens,
PEM blocks, JWTs, URL userinfo, password flags for common tools, generic
`token=`/`secret=`/`password=` assignments, and the prose form of the same
("the api key is …"), because this gate covers agent **prompts** as well as
commands and a prompt states a secret in a sentence.

**A secret rule redacts; it does not reject.** The credential is replaced with a
marker naming the rule that caught it, and everything else is kept:

```
export DB_PASSWORD=⟪redacted:generic-token-assign⟫
mysql -uroot -p⟪redacted:mysql-password⟫ appdb
```

Dropping the whole record was the older behavior and it was wrong in practice:
the commands most worth remembering are often exactly the ones with a token in
them, and an entry that silently vanished is indistinguishable from one never
run. The marker is deliberately not valid shell, so a redacted command recalled
onto the prompt fails loudly rather than running wrong.

Only the credential goes. Rules mark it with a `(?P<secret>…)` capture group, so
a rule that matches a wide context still blanks only the password; a rule with
no such group (a whole-value shape like an AWS key) has its entire match
replaced. Spans from all rules are collected against the *original* text and
overlaps merged, so markers never nest and never get re-matched. Redaction is
idempotent, which matters because records cross the gate more than once
(capture, then import).

One path is still all-or-nothing. `yore filter`, the gate for the shell's own
history, can only accept or reject, because zsh gives `zshaddhistory` no way to
rewrite the line. A command yore would redact is therefore dropped from the
shell's history entirely. yore's own redacted copy is still there to search.

Redaction is **fail-safe**: a missing, unreadable, unparseable, or empty
`redact.yml` falls back to the compiled-in built-ins, never to "redact nothing",
and an individual invalid regexp is skipped with a warning while the rest stay
active. `yore setup` seeds the file without clobbering edits, which means a file
seeded before a rule shipped keeps missing it. `yore doctor` therefore reports
any built-in the file lacks rather than silently re-adding it, since a rule may
be absent because it was deliberately deleted.

## Executors and tags

These are two different axes and yore keeps them apart everywhere. An
**executor** is an attribute of a record (which agent ran the command) captured
once from the environment and never edited. A **tag** is a label somebody
applied, and can be added and removed at will. They were once one field, which
meant a UI could not tell "an agent ran this" from "I called this a refactor",
and labelling a command hid which agent had run it. They are separate all the
way down to the on-disk format.

**Executor** resolution: an explicit `--executor`, else `$YORE_EXECUTOR`
(`$YORE_TAG` is the older name and still works), else auto-detection from agent
environment markers (`CLAUDECODE` → `claude-code`, `CURSOR_TRACE_ID` → `cursor`,
`AIDER_MODEL` → `aider`, and so on), else empty, meaning interactive.

**User tags** are freeform and a record can carry several. A tag record adds or
removes a named label on a command or a session and rides the same encrypted
stream as commands, remapped by name on sync: names are the identity, so there
is no id reconciliation. The daemon folds tag records into an in-RAM index
(`command|session → {tags}`) and resolves each row's effective tags at query
time: command tags ∪ session tags ∪ `auto_tags` (cwd-prefix rules from config,
applied at read time, so there is no cost on the record path and rules apply
retroactively).

Tagging a session is the common case: it covers every command that shell has
already run and every one it runs afterwards. `yore tag list` counts
**commands**, not associations. One session tag over a day's work reads as that
day's work, not as the single `tag add` that created it, which means the listing
resolves the corpus and so also shows `auto_tags` rules. Executors are never
listed as tags.

**Removing a tag takes off only what the record carries.** In the browser
`Ctrl+T` adds and `Ctrl+X` removes, each over the checked set when one exists
and the cursor row otherwise. A removal submits one `tag_op: "remove"` record
per target, since the protocol has no batch call, and the bulk path first
narrows the checked set to the rows that actually carry the tag, so the count it
reports is the number that *changed*, not the number asked about.

What comes off is the **command-level** association, which is all a
command-level record can express. A tag a row inherits from its session or from
an `auto_tags` rule is not that command's to drop: the daemon resolves it again
on the next query and it returns. Those come off with `yore tag rm --session`,
or by editing the rule.

## Agent capture

An agent whose commands run in a non-interactive shell (Claude Code's Bash tool
is `zsh -c …`) is never seen by the rc hooks, so each agent gets native hooks
installed by `yore init <agent>`.

The general shape, using Claude Code as the reference: **PreToolUse** stamps each
command's start time, **PostToolUse** and **PostToolUseFailure** pipe the
finished command to yore, and **UserPromptSubmit** pipes each prompt. Exit status
comes from an explicit `exit_code` when the payload carries one, else from which
event fired. Duration comes from payload timing when present, otherwise from the
delta against the PreToolUse start-stamp, which is how agent commands get real
durations at all, since most agents' payloads carry no timing. So agent commands
carry the same outcome data as shell ones, and success rates, `what_failed`, and
risk-of-failures all work on them.

Per agent:

| Agent | Mechanism | Notes |
|---|---|---|
| Claude Code | hooks in `settings.json` | full exit status; duration from the start-stamp |
| Devin CLI | hooks + MCP in its unified `config.json` | outcome is a boolean `success`; the merge preserves the auth that shares the file, and never reads it out |
| Cursor | `afterShellExecution` + `beforeSubmitPrompt` hooks | carries a duration but no exit code, so exit is unknown; its prompt hook always continues, so it never blocks a prompt |
| OpenCode | a JS plugin, not command hooks | adapts `tool.execute.after` and `message.part.updated`; carries a real exit code |
| Codex | Claude-Code-shaped hooks in `config.toml` | no separate failure event and no documented exit field, so exit is best-effort |

All hooks run through the same redaction gate as the shell path, so a secret in a
*prompt* is masked exactly like one in a command. Every installer is additive and
idempotent, preserves unrelated configuration, and is verified by `yore doctor`.
`--project` writes to the project-local config instead of the user-global one.

`yore uninit <agent>` reverses any of them, removing only yore's own hooks and
MCP registration (matched by the exact command string the installer wrote) and
leaving every other key in place. Emptied blocks are pruned so the file is left
as it was found; a config hand-edited past recognition reports "nothing to
remove" rather than guessing. Shells are not agents: the `eval "$(yore init
zsh)"` line comes out of the rc file by hand.

**Prompts are records.** The prompt hook spools one `type == "prompt"` record
holding the text and writes only that record's **id** to a per-session state
file; the command hooks read the id and stamp `prompt_id` onto each command. A
stored command row therefore holds the id and nothing else, and the daemon
rejoins the two at query time, so consumers just read `prompt` on a row and never
see the join.

Storing the text once rather than on every command that quotes it is what keeps
prompt tracing cheap: one prompt drives roughly sixteen commands in practice, so
the inlined alternative pays 16× the bytes on disk, on the wire, and in the
daemon's heap, for a field the search path never indexes. It also makes a prompt
that triggered **no** commands representable at all: as a field on its commands,
such a prompt would have nowhere to live. `sync_prompts = false` keeps prompt
records on the machine that recorded them while their commands still sync.

## MCP server (`internal/mcp`)

`yore mcp-serve` is a local, read-only [Model Context
Protocol](https://modelcontextprotocol.io) server over stdio JSON-RPC, with no
network port. It is a thin adapter over the daemon's query layer (it dials the
unix socket and issues queries, and never opens the store) so it inherits both
the single-writer guarantee and, more importantly, the cross-machine RAM corpus.
Tools take a `scope` of `local` or `all`, and `all` answers over every enrolled
device while the server still holds only ciphertext. That is what lets an agent
ask "have I run this migration anywhere?", a question a single-machine tool
cannot answer.

It exposes twelve tools (`search_commands`, `recent_commands`, `command_status`,
`session_history`, `list_sessions`, `get_stats`, `get_prompts`, `suggest_next`,
`what_failed`, `find_agent_session`, `replay_agent_session`, `assess_risk`) and
eight context resources (`yore://history/recent`, `…/failures/recent`,
`…/stats/today`, `…/risk/summary`, `…/agents/activity`, `…/agents/sessions`,
`…/context/project`, `…/history/session/<id>`).

Nothing is pushed. A tool runs only when the model calls it, and a resource
enters context only when the client attaches it.

## Risk (`internal/risk`)

A deterministic rule-based classifier for how dangerous a shell command is:
`safe` < `low` < `medium` < `high` < `critical`, each verdict carrying a category
and a one-line reason. No model, no network, no state, so the same command always
gets the same answer, and the answer can always be explained.

**It is advisory and never blocks anything.** This is worth stating plainly,
because a tool that rates danger invites the assumption that it prevents it. The
classifier is not consulted anywhere in an execution path. That is a choice, not
a missing capability: the PreToolUse hook already receives the command before it
runs and could adjudicate. It deliberately doesn't: a recorder that can veto is
a recorder that can wedge a session, and the prompt-latency invariant makes the
same argument for shells.

There are exactly three consumers and all three only *display* a verdict: the
risk row in the browser's command details panes, the `assess_risk` MCP tool, and
the `risk/summary` resource.

**Reaching a verdict** takes four steps. A command that cannot execute anything
short-circuits to safe: empty, a comment, an alias definition, or a bare
`echo`/`printf`, the last only when it holds no `| & ; > <`, backtick, or `$(`,
so `echo $(rm -rf x)` does not slip through. Then the user's `ignore` patterns
are tried, and a match returns safe while naming the pattern that silenced it.
Otherwise the line is parsed and every rule is scanned, highest severity winning,
ties keeping the first. Finally, anything the line hands to an interpreter is
assessed the same way, recursively.

**A command is judged by what it runs, not by what it contains** (`parse.go`).
This is the distinction the whole classifier rests on: `grep -rn "rm -rf" docs/`
and `sh -c 'rm -rf /'` carry the same eight characters and only one of them
deletes anything. Before any rule runs, the line is lexed into words and cut
into command *segments* wherever a shell would start a new command: a pipe, a
`;`, a `&&`, a `$(…)` even inside double quotes, a `find -exec`. Each segment
resolves the command word it would actually execute, with `sudo`-style wrappers
stepped over so `sudo rm -rf /` is an `rm`, quoted text kept as inert data,
redirection targets pulled out, and **its own flags kept to itself**, so the
`-r` in `grep -rn` is not available to an `rm` three words away. Rules then ask
"is `kill` the command here?" rather than "does this string contain kill".

Two consequences follow. A segment carrying `--help`, `--version`, or `--dry-run`
is inert, so `npm install --dry-run` rates nothing. And quoted text handed to
something that will execute it (`sh -c`, `python -c`) is re-assessed as the
command it becomes, to a depth of three; the same text handed to `grep`, which
executes nothing, is not.

**The built-in rules** number 65 across four levels. Levels mean something
specific, and that is what keeps the ramp useful rather than uniformly alarming:
**critical** is irreversible, **high** is undoable only with effort or changes
what code runs, **medium** has real but ordinarily recoverable side effects,
**low** reaches off the machine. Because severity wins over order, `sudo npm
install` is high (package-install), not medium (privilege).

| Level | Covers |
|---|---|
| ⛔ critical | recursive forced `rm`; force/mirror/delete pushes and history rewrites; `DROP`/`TRUNCATE`/unfiltered `DELETE`; raw disk writes, `mkfs`, `shred`; `terraform destroy` and auto-approved applies; `kubectl delete namespace`; cloud resource deletion; recursive object-store wipes; fork bombs and shells bound to sockets |
| ⚠ high | package installs and removals; publishing to a registry; fetch piped into an interpreter; running a local script; ephemeral runners (`npx`, `uvx`); world-writable or setuid `chmod`, recursive `chown`; `git clean -f`, `find -delete`, `rsync --delete`; container and cluster teardown; partition-table edits; `crontab -r`; account changes; reading private key material; firewall teardown; a container given the host; `shutdown`/`reboot` |
| ▲ medium | `sudo`, `su`, `pkexec`; container stop/remove; `kill`/`pkill`; recoverable git surgery (`reset`, `restore`, `stash drop`, `rebase`, `amend`); `truncate` and bare `>` redirects; service stop/disable; piping local output into a request body; `terraform apply`, `kubectl apply`, `helm install`; running an executable out of the working directory |
| • low | network reach (`curl`, `wget`, `ssh`, `scp`, `nc`); `git push`; a credential typed into the environment |

Rules live in Go rather than regexps wherever being right matters more than being
uniform, which is most of them. `chmod` is the clearest case: only the group and
other digits can grant a write bit, so `644`, `755`, and `600` must not be
flagged while `chmod -R 777` must, and no single pattern gets both ends of that
right. `git push --force` carves out `--force-with-lease`, which RE2 cannot
express without negative lookahead and which would otherwise rate the *safe* form
as the most dangerous thing in the table. SQL is matched by content, but only
where SQL would actually execute (handed to a database client, or typed as the
whole line) so `git commit -m "drop table support"` is a commit message.

**What it does not see.** The parser resolves command position, not semantics. A
command behind an alias, a Makefile target, or a variable holding a program name
trips nothing; neither does one carried inside `docker exec web …` or
`ssh host …`, where the remote command is an argument this classifier does not
follow. That is the deliberate trade: a classifier wrong in an obvious,
inspectable direction beats one wrong subtly, and `risk.toml` is where a team
closes the gaps that matter to them.

**Rules are extensible.** `~/.config/yore/risk.toml` adds `[[rule]]` entries with
a Go regexp, a level, and an optional category and reason, plus a top-level
`ignore` list. User rules are appended after the built-ins and severity wins, so
a rule can only ever *escalate* a command; de-escalation is what `ignore` is for,
and it goes all the way to safe. Loading is fail-safe exactly like `redact.yml`,
so a typo cannot switch risk assessment off. `yore doctor` is where a skipped
rule gets named, since the browser's alt-screen and the MCP server's
stdout-owned transport have nowhere to say it. The browser and `assess_risk` load
the same file, so the TUI and an agent always agree about the same command.

## Shell integration modes

`config.integration` (default `takeover`, overridable per `yore init --mode`)
controls how deeply the emitted hooks take over the shell.

- **takeover**: yore is the single source of truth. For zsh and bash the
  shell's persistent history is disabled, its in-memory list is seeded from yore
  at startup (one `fc -R` / `history -r`), and additions are gated by yore's
  redaction, so `!N` and Up-arrow work against yore-consistent, secret-free
  history. zsh is exact; bash is coarser, since multiline collapses to one line.
  fish has neither problem to solve, having no `!N` expansion and no separate
  in-memory list, so takeover there is just `fish_private_mode`, which fish
  checks on every write rather than caching at shell start, so setting it from a
  sourced init script still takes effect.
- **coexist**: record alongside the untouched native history; rebind Ctrl-R and
  add the aliases.
- **capture**: record only; no keybinding or alias changes.

**Mechanics.** zsh installs `zshaddhistory` (calling `yore filter`, returning
nonzero to drop) and rebinds `^R`, and optionally Up. bash uses vendored
bash-preexec for capture and a best-effort gate. fish needs no third-party shim,
since `--on-event fish_preexec` and `fish_postexec` are native, and gets
duration free from `$CMD_DURATION`, where zsh and bash have to timestamp in
preexec and subtract in precmd. fish `bind` takes literal escape sequences
rather than key names, so one binding works unchanged across fish versions,
mirroring why zsh and bash bind raw sequences too.

Two support subcommands back this: `yore filter` (reads a command on stdin, exits
1 to drop; zsh and bash only, since fish has no native history to gate) and
`yore export --shell` (the history seed). Neither is on the prompt fast path:
`filter` runs synchronously in single-digit milliseconds, and `export` runs once
per shell start and is best-effort, printing nothing and exiting 0 if no daemon
is running.

Scoped aliases (unless `--no-aliases`): `hb` for the browser and `hs` for
searching this host, with `hsa` (all hosts), `hss` (session), `hsc` (cwd), and
`hsw` (workspace). yore never rebinds `h`.

## Key hierarchy and encryption

```
device X25519 + Ed25519 keypairs   per machine; private halves never leave it
  │ X25519 seals ─▶ History Key (HK)   one 32B symmetric key per group, stored
  │                                     only as per-device wrapped blobs
  │ HK wraps    ─▶ epoch data keys      32B, one per epoch (default 24h)
  │ DEK seals   ─▶ history records      XChaCha20-Poly1305, AAD-bound
  └ Ed25519 signs ─▶ every sync request
```

- **Read** costs one asymmetric HK unwrap per daemon lifetime; every data key and
  record then opens symmetrically. Constant in history age.
- **Enroll** is one wrap: an existing device wraps HK for the newcomer's public
  key.
- **Revoke** rotates to a new HK and re-wraps the few data keys and HK for
  surviving devices. **Records are never re-encrypted**, so this is also constant
  in history age.
- **AAD** binds every sealed record to `recordID|hostID|seq|keyID`, so a
  compromised server cannot reorder, replay, or substitute blobs undetected.
  Decryption failure is fatal to a pull, never silently skipped.
- **No shared secret.** A device authenticates with its Ed25519 key on every
  request, reads included; there is no bearer token to capture. The server's
  configured token is only an enrollment token for an *empty* group. Once any
  device is active it enrolls nothing, and every later machine needs a single-use
  token minted by one already enrolled.
- **Recovery.** Per-device keys mean losing every device would otherwise lose the
  archive for good, so bootstrap also seals HK to a key derived (Argon2id) from a
  one-time recovery phrase, shown once and never stored. The server holds only
  the salt, the recovery public keys, and that wrap.
- **Optional certificate pinning** (`yore setup --pin`) hardens against a
  TLS-inspecting proxy and fails closed. Fail-closed is the right default and a
  bad failure mode to diagnose blind: the pin covers the server's *key*, not its
  certificate, so an ACME renewal with a fresh key breaks sync while local
  history keeps working. A mismatch is therefore a typed error carrying both
  digests, and `yore doctor` reports it as its own diagnosis, with both
  remedies, since re-pinning from an intercepted network would pin the
  interceptor.

Exact byte layouts, domain-separation strings, and the `device.key` format are in
[`protocol.md`](protocol.md).

**Enrollment timeouts.** `yore setup` budgets each stretch of talking to the
server separately rather than putting one deadline around the whole run. The run
stops to ask for a token, and fetching one means walking to another machine, so a
single deadline opened before the question expired while the user was answering
it, and the enrollment that followed failed the instant the token was pasted in,
reported as the server *refusing* it. A timeout is now named as a timeout, since
no amount of minting fresh tokens fixes a server that never answered.

## Sync

Streams are append-only, per-host, and client-sequenced; merge is a set union by
record ULID; deletes are appended tombstones. Sync is therefore conflict-free and
eventually consistent by construction.

The daemon's sync loop, started only when a server is configured, is driven by:

- an initial warm sync ~2 s after startup, so deep search is warm quickly;
- a periodic tick every `sync_interval` (default 5m);
- a nudge from a deep read or from opening the browser;
- **experimental** `push_debounce` (default off): a duration arms a coalesced
  push shortly after new records are ingested, so cross-host propagation is
  seconds rather than up to `sync_interval`. Off by default because an agent
  firing bursts of commands would push about once per debounce for its whole run.
  The nudge fires only while the server is reachable. Offline, new records stay
  in the local store and go out in a batch when the next periodic tick
  reconnects.

`yore sync` (and `S` in the TUIs) runs a synchronous cycle and reports the real
outcome. Every cycle is push then pull, serialized by a mutex so the periodic
loop and an explicit sync never overlap.

**Push** uploads local records above the persisted `last_uploaded_seq` watermark
in ascending batches bounded by **both** ≤1000 records **and** ≤8 MiB of encoded
body, advancing the watermark by exactly what the server acked. Size is a bound
because size is what the server actually limits: a thousand records carrying long
prompts or heredocs is megabytes, and a body over the server's 10 MiB cap fails
*every* retry, so the watermark never advances and sync wedges permanently. If
the server rejects a batch anyway, the client halves it and retries, terminating
at a single record so a genuinely bad request surfaces as an error rather than
looping forever.

**Pull** is split in two so ciphertext can be banked before anything is spent on
crypto: `PullCiphertext` walks each other host's stream from its persisted
cursor, and `OpenRecords` decrypts. Each cycle writes the new sealed records and
the advanced cursor to `remote.db` *first*, so a process that dies mid-cycle
resumes rather than re-downloading. On the first cycle of a daemon's life the
cache is decrypted into RAM, which needs the server to unwrap the keys, so a
cold start still has no remote history while offline.

A decryption failure during a live pull is fatal, as always. One while decrypting
the *cache* is not treated as tampering: the only way that file can hold records
we cannot open is if it outlived the group it belongs to, and it is derived data,
so it is thrown away and re-pulled rather than wedging sync on a stale cache.

**Schema version.** `data.db`'s meta bucket carries a `schema` key (currently 1).
There is no upgrade path yet, so the version does exactly one thing today: a
build that finds a version it does not know refuses to open the database rather
than misreading the one copy of this machine's history. A store written before
the key existed is stamped rather than refused: the layout never changed, so an
unversioned store *is* version 1. Recording it now is what makes a migration
possible later. A refused open still releases the lock, and `yore record` never
opens the store at all, so commands keep spooling meanwhile.

**Being revoked** is its own state, separate from "unreachable" because retrying
cannot fix it. On the first `device_revoked` response the daemon records the
fact, detaches and deletes `remote.db`, drops the pull cursors, and stops
syncing for the rest of the process; `yore sync` and `yore status` say so
instead of reporting success. Remote history already decrypted into RAM is left
for the rest of that session, since the user is looking at it, and it is gone at
the next start, which finds an empty cache and can obtain no key.

The record lives in `data.db`'s meta bucket rather than a file of its own,
because the daemon holds that database under its write lock, so the fact is not a
loose marker in the state directory inviting deletion. It makes the purge survive
a process killed mid-way, a restored backup, or a machine that comes back up
offline and can never be told again.

It is a record of the last thing the server said, **not a permanent verdict**. A
revoked start is therefore not latched: it gets exactly one attempt, because
nothing in the enrollment path can reach into `data.db` while the daemon holds
the lock, so the only evidence that can retire a revocation is the server serving
this device again. Enroll the machine afresh and the next cycle clears it.
Re-pointing at a different server clears it too, since that revocation described
the old group, while editing the *transport* to the same server does not.

## The sync server

Single-tenant or multi-tenant, never a mix. Each tenant is an isolated bbolt file
owned solely by the server process; the identity that signed a request selects
the tenant every handler operates on.

- **Device → tenant.** The auth middleware finds the tenant holding the signing
  device's record (ids are globally unique ULIDs, so at most one matches) and
  caches the mapping. Enrollment routes by token, recovery by the tenant that
  holds recovery material. Bootstrap tokens are compared constant-time against
  every configured token with no early break, so a match leaks nothing about
  which or how many tenants exist. No match is a 401. A request that somehow
  reaches a handler with no bound tenant fails 500 rather than touch another
  tenant's data.
- **One mode or the other.** A server is configured **either** with a single
  token, hosting one tenant whose database is `--db`, **or** with named tenants
  from `$YORE_TOKENS_FILE` at `<dir(--db)>/tenants/<name>.db`, in which case
  nothing is created at `--db` and the path only roots `tenants/` and `backups/`.
  Both together is refused, and so is neither: the tokens file is not an addition
  to a mandatory "default" tenant a multi-tenant operator never asked for, and a
  server with no configured token has no way in at all. Names are
  `[A-Za-z0-9_-]+`; `default` is reserved. Duplicate tokens are refused at
  startup, since two tenants sharing one would be indistinguishable.
- **Storage** is ciphertext and device public keys only. The client and wire
  protocol are unchanged by multi-tenancy.
- **Per-tenant rolling backups.** With `$YORE_BACKUP_INTERVAL` > 0 (default 1h,
  `"0"` disables) one goroutine writes a consistent online snapshot of every
  tenant database, temp file plus atomic rename, pruning to the newest
  `$YORE_BACKUP_KEEP` (default 3). Pruning also collects abandoned `.tmp-*.db`
  files: a process killed mid-snapshot leaves a full-size copy of its own
  database behind, and on a restarting server those accumulate until they fill
  the volume.
- Exactly one server replica, since bbolt is single-owner. TLS terminates at your
  reverse proxy.
- **Liveness is not readiness.** Reads come out of bbolt's mmap and need no
  write, so a server whose volume has filled keeps serving every GET while every
  push fails, so the archive stays readable and quietly stops recording.
  `GET /v1/ready` commits a probe transaction and answers 503 when it cannot, and
  `yore doctor` calls it, so "server reachable" can no longer be printed for a
  server that has stopped accepting history. It is deliberately kept off the
  container healthcheck: a full disk is not a restartable fault, and restarting
  on it only costs the reads that still worked. `GET /v1/health` is the shallow
  probe that backs the healthcheck.
- **A 500 always names its cause in the server log.** The response is generic by
  design and the access log carries only the status, so the error path logs the
  underlying cause with tenant, method, and path. Without it a failed write
  transaction is invisible from both ends of the connection.

## Durability and ops

- **Local rolling backups.** The daemon writes a consistent snapshot of `data.db`
  to `~/.config/yore/backups/data-<unixMillis>.db` every `backup_interval`
  (default 1h, `"0"` disables), keeping the newest `backup_keep` (default 3),
  using the same temp-file, atomic-rename, and prune scheme as the server.
- **What backups cost.** Each one is a full copy, so `backup_keep` is a
  multiplier, not a margin: peak disk is roughly `(backup_keep + 1) × db size`,
  and every interval rewrites a whole database. At the default, a 300 MB store
  occupies ~1.2 GB and rewrites 300 MB an hour. Size a server volume for the
  multiple, not the database. A backup is smaller than its `data.db` because it
  writes only used pages, without bbolt's ~20% allocation slack.
- **Bounded daemon log.** `daemon.log` rotates once it would exceed
  `log_max_size` (default 5MB, `"0"` for unbounded), keeping `log_keep` old
  segments. `log_silent` (default true) suppresses logging entirely and creates
  no file, so a fresh install writes no log; set it false for debugging. A
  log-open failure is non-fatal: the daemon runs without logging rather than
  refusing to start.
- **Warm snapshots** (`corpus.snap`) keep restarts instant. They are derived data
  and always rebuildable, so a bad snapshot just triggers a full load.

## Single-directory footprint

All client state lives under `~/.config/yore/` (or `$YORE_DIR`), so uninstall is
one `rm -rf`. The one deliberate exception is secret material: where a usable OS
keyring exists the device key is stored there instead, so a full uninstall also
drops the `yore` keyring entries. `$YORE_SECRET_BACKEND=file` forces everything
back into the directory.

| Path | What |
|---|---|
| `config.toml` | settings (0600) |
| `ui.toml` | pane sizes and column choices the browser remembers (0600) |
| `redact.yml` | editable, seeded secret-redaction rules (0600) |
| `risk.toml` | optional user risk rules and ignores |
| `data.db` | local store, this host's history only, plus the `meta` bucket |
| `device.key` | device identity, in the OS keyring where one is usable, else this file (0600, refused if group or other readable) |
| `spool/<pid>.jsonl` | crash-safe capture handoff, drained by the daemon |
| `daemon.sock` | daemon control socket (0600) |
| `corpus.snap` | warm-start corpus snapshot (derived) |
| `remote.db` | other hosts' history as ciphertext, plus their pull cursors (derived) |
| `agent-prompts/` | one file per agent session holding the current prompt's id: the handoff between an agent's prompt hook and its tool hooks, which are separate processes |
| `agent-cmd-starts/` | per-command start stamps written by an agent's PreToolUse hook, consumed once by the hook that records the finished command |
| `daemon.log` | bounded daemon log and rotated segments |
| `backups/` | rolling local `data.db` snapshots |

## Config

`~/.config/yore/config.toml` is a plain [TOML](https://toml.io) file, read and
written by hand or through `yore get-config` and `yore set-config`, the one
authoritative accessor pair. Nothing else parses the file: the emitted shell
integration, for instance, asks `yore get-config enter_executes` at call time
rather than grepping.

| Key | Default | Meaning |
|---|---|---|
| `server_url` | none | sync server base URL; empty means local-only |
| `token_file` | none | path to a file holding an enrollment token, for externally managed setups |
| `server_pin` | none | pinned server TLS SPKI (base64 SHA-256); set by `setup --pin` |
| `integration` | `takeover` | `takeover` \| `coexist` \| `capture` |
| `key_epoch` | `24h` | data-key epoch width |
| `daemon_idle` | `30m` | daemon idle timeout before exit |
| `sync_interval` | `5m` | periodic push/pull tick |
| `push_debounce` | off | **experimental** coalesced push-on-record delay |
| `sync_prompts` | `true` | upload agent prompt records; `false` keeps prompt text on the recording machine while its commands still sync. Not retroactive |
| `remote_keep` | `50000` | records cached and held in RAM per *other* host, newest first; negative means unlimited |
| `auto_deepen` | `true` | let deep reads nudge a background sync |
| `enter_executes` | `false` | Ctrl-R Enter puts the result on the prompt; `true` runs it outright |
| `bind_up_arrow` | `false` | also bind Up to the search TUI |
| `hide_agent_commands` | `true` | keep agent-run commands out of the interactive search UIs |
| `keymap` | `emacs` | `emacs` \| `vim` TUI key style |
| `ignore_patterns` | none | regexes whose matching commands are **dropped**, not redacted |
| `ignore_dirs` | none | cwd prefixes whose commands are never recorded |
| `record_space_prefixed` | `false` | record leading-space commands too |
| `auto_tags` | none | cwd-prefix → tag rules, applied at query time |
| `capture_spool_only` | `false` | capture writes to the spool only, never poking or spawning the daemon |
| `backup_interval` | `1h` | local backup cadence; `"0"` disables |
| `backup_keep` | `3` | local backups retained |
| `log_max_size` | `5MB` | `daemon.log` rotation threshold; `"0"` for unbounded |
| `log_keep` | `1` | rotated log segments kept |
| `log_silent` | `true` | suppress daemon logging entirely |

Defaults are applied the plain-Go way: `config.Load` starts from
`config.Defaults()` and decodes the file over it, so an omitted key keeps its
default and an explicit value, including `false` or `0`, overrides it. Booleans
that default to true (`auto_deepen`, `sync_prompts`, `log_silent`,
`hide_agent_commands`) are written without `omitempty` so an explicit `false`
round-trips; there are no `*bool` "was it set?" fields. String-backed durations
and sizes are stored verbatim and parsed by typed accessors that fall back to the
default on a malformed value.

Layout the browser writes back lives in `ui.toml`, not here: `config.Save`
marshals the whole struct, and a TUI that rewrote and reformatted the user's
settings file every time a pane moved would be a poor neighbour. Same directory,
same mode, different concern.

Server-side settings are environment variables rather than `config.toml`:
`$YORE_TOKEN` / `$YORE_TOKEN_FILE` **or** `$YORE_TOKENS_FILE` (mutually
exclusive), `$YORE_BACKUP_INTERVAL`, `$YORE_BACKUP_KEEP`.

## Invariants

Do not break these:

- The prompt path never does database, network, or crypto work, and recording
  survives the daemon being down.
- `data.db` holds only this host's history. Other hosts' **plaintext** exists
  only in daemon RAM and is never written to disk; the ciphertext it came from is
  cached in `remote.db`, which is exactly what the server holds and is unreadable
  without this device's keys.
- Streams are append-only, per-host, client-sequenced; merge is a ULID set union
  and deletes are tombstones, so sync is conflict-free.
- Every hot path (search, decrypt, enroll, revoke, and daemon startup) is
  constant in history age, bounded by `remote_keep` rather than by how much
  history the group has ever accumulated.
- Prompt text is stored once, on its own record; the daemon rejoins it at query
  time.
- The server only ever holds ciphertext, wrapped keys, and device public keys.
  Mutating requests are per-device signed and tenants never share a database.
- All client state lives under `~/.config/yore/`.
</content>
</invoke>
