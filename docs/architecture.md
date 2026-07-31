# yore architecture

This is the design doc: how the pieces fit and why. Exact wire bytes — the sync
HTTP API and the E2E crypto scheme — live in **[`protocol.md`](protocol.md)**;
user-facing setup lives in the **[README](../README.md)**.

## Overview

`yore` is one static, **CGO-free** Go binary. A cobra subcommand selects which
role it plays:

| Role | Subcommands |
|---|---|
| Shell-hook fast path | `record`, `filter`, `export` |
| Search UIs | `search` (inline Ctrl-R TUI + `--headless`), `browse` (full-screen), `stats` / `agents` (the browser, opened on one of its screens) |
| Background daemon | `daemon` (`run`/`stop`/`status`), `status`, `stop`, `sync` |
| Enrollment / devices | `setup`, `devices` (the browser's devices pane; `token` mints an enrollment credential) |
| Sync server | `server` (`stop`), `healthcheck` |
| Setup / misc | `init`, `uninit`, `import`, `doctor`, `gen-id`, `version` |

Because everything is pure Go with `CGO_ENABLED=0`, it cross-compiles with no
toolchain to the tier-1 matrix: **linux/amd64, linux/arm64, darwin/amd64,
darwin/arm64, freebsd/amd64** (WSL runs the linux builds).

## Data flow & the sacred prompt path

```
                    redact gate (leading-space / ignore-dirs / secret rules)
                          │ drop (silent, exit 0)
shell hook ─(append,<1ms)─┴▶ spool file ─▶ ┌─ yore daemon (unix socket) ─────────┐
                                           │  owns local bbolt (this host only)  │
Ctrl-R / h / hs / browse ◀── unix sock ───▶│  RAM corpus (warm gob snapshot+tail)│◀─HTTPS─▶ yore server
   (thin TUI clients)                      │  RAM remote cache (never on disk)   │  (bbolt per tenant:
                                           │  background sync loop (push/pull)   │   ciphertext records,
                                           └─────────────────────────────────────┘   wrapped keys, pubkeys)
```

The prompt path is deliberately trivial and is the system's most important
invariant. On every command the shell hook runs `yore record`, which:

1. reads the command text on stdin (capped at 1 MiB);
2. drops it if empty;
3. runs the **redact gate** (below) — a rejection returns silently, exit 0;
4. appends one JSON line to this process's spool file and **fsyncs** it;
5. best-effort pokes the daemon over the unix socket (spawning one if absent),
   with tight deadlines (~150 ms worst case, and `record` runs backgrounded);
6. exits 0.

No database, no network, no crypto touches the prompt path. If the daemon is
dead the record still lands durably in the spool and is drained at the next
daemon start — recording survives a dead (or never-started) daemon.

## Packages (`internal/`)

Concise map by role. Leaf-contract packages import nothing else in the tree.

**Leaf contracts:**
- **rec** — the `Record` type. Its JSON is the encoding for both the spool and
  (as the sealed plaintext) the sync payload. ULID `id`; `type` `""`(command),
  `"delete"`(tombstone), or `"tag"`(a user-tag add/remove).
- **proto** — the newline-delimited-JSON protocol over the daemon's unix socket.
- **wire** — the JSON types of the server's HTTP API.
- **config** — resolves the single state dir (`$YORE_DIR`, else `~/.config/yore/`)
  and all settings, applying defaults via accessor methods.

**Storage & capture:**
- **spool** — crash-safe, fsync'd, per-process (`<pid>.jsonl`) append files;
  `Drain` tolerates a torn final line and dedupes downstream by id.
- **store** — local bbolt (`data.db`). Holds only this host's stream. Single-owner
  (an exclusive file lock; a competing opener gets `ErrLocked`). Idempotent
  appends keyed by record id; assigns per-stream `seq`; tombstones for deletes;
  `BackupTo` is a hot online snapshot. The `meta` bucket carries this host's
  identity (`host_id`, `hostname`), the sync watermark (`last_uploaded_seq`), the
  revocation record (`revoked_by_server`), and the layout version (`schema`).

**Runtime & search:**
- **daemon** — the only process that opens the store; see below.
- **match** — whitespace-split substring terms (smart-case) with an incremental
  prefix-reuse `Filter`, plus a subsequence **fuzzy** matcher.
- **tui/theme**, **tui/hl** — adaptive lipgloss styles; a best-effort shell-command
  syntax classifier layered under match highlighting.
- **tui/search** — the inline Ctrl-R panel. **tui/browse** — the full-screen
  browser: three tiled browse panes plus the stats, agent-explorer, and devices
  screens. See "The browser" below.
- **risk** — a deterministic, rule-based command classifier (`safe…critical`),
  advisory only: it labels history, it never gates execution. See "Risk" below.
- **mcp** — the local, read-only MCP server (`yore mcp-serve`) over the daemon
  query layer; exposes history to coding agents. See "MCP server" below.

**Security & sync:**
- **cryptobox** — the E2E core (device X25519+Ed25519 keys, History Key, epoch
  DEKs, record AEAD). See "Key hierarchy" below and `protocol.md` for the bytes.
- **reqsign** — the shared canonical string + headers a device's Ed25519 key
  signs on every mutating sync request. One source of truth for client & server.
- **redact** — the recording gate: never spool/store/sync a command carrying a
  secret. Runs on live capture, on import, and on the shell-history gate.
- **server** — the multi-tenant sync server; stores only ciphertext + device
  public keys. See "The sync server" below.
- **syncer** — the client engine: encrypt+push local records, pull+decrypt
  remote ones, and the device enroll / approve / revoke+rotate primitives.

**Integration:**
- **shell** — the embedded zsh/bash hook scripts (+ vendored bash-preexec),
  rendered per integration mode via `text/template`.
- **importer** — zsh (extended-history, unmetafy, multiline) and bash parsers.
  Bash history is only timestamped when the writing shell had `HISTTIMEFORMAT`
  set (it emits a `#<epoch>` line per command); otherwise the file is bare
  command lines and every record lands with `StartMs == 0`, meaning *no known
  time*. `yore import` warns once per such file, and the TUI renders a zero
  timestamp as `—` (`theme.Unknown`) rather than as the Unix epoch.
- **cli** — the cobra command tree; each subcommand is a `runXxx` returning a
  process exit code; ships shell completions.

## The daemon

A long-lived background process, auto-spawned on first use (detached, `Setsid`)
and idle-exiting after `daemon_idle` (default 30m). It is the sole owner of the
local bbolt store and the authority for all search.

- **RAM corpus.** On start it loads the live search corpus from a warm **gob
  snapshot** (`corpus.snap`) and folds in the store tail above the snapshot's
  position via `store.Since`; a missing/corrupt snapshot falls back to a full
  `store.All()` cold load. The corpus is append-only and guarded by an RWMutex;
  readers snapshot the slice header under RLock and scan lock-free. A snapshot is
  re-written every 5 min and once more on shutdown, so restarts paint instantly.
- **RAM-only remote plaintext, cached ciphertext.** Other hosts' history is
  decrypted into RAM **only** — plaintext is never written to disk. The
  *ciphertext* it was decrypted from is cached in `remote.db`
  (`internal/rstore`), alongside the per-host pull cursor. That is what the
  server already holds and this machine cannot read without its keys, so the
  invariant is untouched — and it is what makes startup incremental: cursors
  used to be RAM-only, so every daemon lifetime re-downloaded and re-decrypted
  every other machine's entire archive from seq 0, several times a day once the
  30-minute idle timeout recycled the process. `remote_keep` (default 50 000)
  caps how much of each host's tail is retained, in the cache and in RAM both,
  so neither grows without bound as years of history accumulate. The cache is
  derived: deleting it costs one full re-pull and nothing else, and it is reset
  whenever the configured **server URL** changes — the cached ciphertext then
  belongs to a stranger whose key hierarchy has nothing to do with the new one's.
  Nothing else in `syncConf` (`sameServerAs`) invalidates it: a certificate pin
  added or cleared, a rotation cadence, a retention bound all describe how to
  reach the same archive, and dropping the cache for one of those would spend a
  full re-pull of every machine's history on a transport edit — the exact cost the
  cache exists to avoid.
- **Prompt index.** Agent prompt text is stored once, on its own record (see
  below), so the daemon keeps a `promptID → prompt` index — fed by the local
  store scan at startup, local ingest, and remote pull — and rejoins each query
  row with its prompt text on the way out.
- **Debounced ingest.** A spool "wake" starts a 75 ms straggler window, then one
  fsync'd `IngestSpool` drain folds new rows into the corpus.
- **Concurrency.** One goroutine per socket connection (serves that connection's
  requests in order, so its per-connection `match.Filter` stays single-threaded);
  a single ingest goroutine; the main goroutine owns the idle timer and shutdown.
- **The unix-socket op table** (`~/.config/yore/daemon.sock`, 0600, one JSON
  object per line, one Response per Request):

| Op | Purpose |
|---|---|
| `ping` | liveness + reset idle timer + nudge spool ingest |
| `record` | spool one record (fsync) + nudge ingest — same durability as the CLI |
| `query` | search (scope, sort, fuzzy, executor + freeform-tag filter, dedupe, paging) |
| `hosts` | per-host live-record counts (browse sidebar); warms the remote cache |
| `tags` | list known user tags with how many commands carry each (scoped) |
| `delete` | tombstone one record by id (syncs as a tombstone) |
| `devices` | list enrolled devices (proxied to the syncer) |
| `tokens` / `revoketk` | list enrollment tokens with their states / cancel an unclaimed one (proxied) |
| `token` | mint a single-use enrollment token (proxied to the syncer) |
| `approve` / `revoke` | approve a pending device / revoke+rotate keys |
| `sync` | force a **synchronous** push/pull cycle (backs `yore sync`) |
| `status` | daemon status (pid, uptime, local rows, remote state, version) |
| `shutdown` | graceful exit (also `yore daemon stop` / `yore stop`) |

Exact request/response JSON for these ops is in `protocol.md`.

## Search model

Every query is `match → scope filter → optional executor(tag) filter → sort →
dedupe → window`. Matching is substring (default, smart-case) or **fuzzy**
(subsequence); the per-connection incremental filter is used for substring,
skipped for fuzzy. Scopes:

- **local** (shallow) — this host only; always available, offline-safe.
- **all** / **host** (deep) — merge the RAM remote cache. A deep query (and
  opening `browse`) nudges a background sync (`syncWake`) so the *next* query is
  richer; the request itself never blocks on the network. Offline, deep
  degrades to whatever is already cached (or just local).
- **session** / **cwd** — filter by shell session id or exact working directory.
- **workspace** — commands run anywhere under the current git repo (walk up for
  `.git`; local-only).

Sort is **recency** (descending `start_ms`, ties by descending `seq`) or
**frecency** (frequency × bucketed-recency weight with a same-cwd ×2 boost, which
inherently collapses to one row per command). Recency sort can also `dedupe`
(newest wins). The default window is 200 rows; `limit: 0` takes that default and
`limit: -1` (`proto.LimitAll`) asks for **every** match. The daemon already holds
the corpus in RAM and sorts all matches before windowing, so `LimitAll` costs
serialization, not work — it is what a browser of the archive should ask for, and
what the top-N callers (Ctrl-R, MCP, headless search) deliberately do not.

## The browser (`internal/tui/browse`)

`yore browse` (the `hb` alias) is one Bubble Tea program with four screens: the
tiled **browse** panes, the full-screen **stats** screen (`s`), the **agent
explorer** (`a`), and **devices** (`D`). `yore stats`, `yore agents`, and `yore
devices` open the same program directly on one of those screens; Esc drops
through to the browse table from any of them.

Devices are managed *only* there. The view is two stacked panes — **MACHINES**
over **TOKENS** — with `tab` between them, `z` to zoom either, and every action
asking first:

| pane | key | effect |
|---|---|---|
| machines | `a` | approve a pending machine — the prompt quotes its verification code, so the out-of-band check is in front of the person answering |
| machines | `x` | revoke it and rotate the group's keys |
| tokens | `n` | mint an enrollment token, shown once (see below) |
| tokens | `y` | copy the one just minted |
| tokens | `x` | cancel an unclaimed token |

The CLI had a second implementation of the machine actions (`devices
approve|revoke` plus a printed list); two code paths for one dangerous operation
is two sets of confirmation rules to keep honest, so the CLI one is gone. `yore
devices token` stays, because minting an enrollment credential is something you
pipe, not something you manage.

**Why tokens are on screen at all.** An open enrollment token admits a machine
to everything the group can read, and until it was listed nothing told you one
existed. Each row carries its state — `open`, `claimed` (by which device),
`expired`, `revoked` — computed by the server so no client re-derives "expired"
from its own clock. A newly minted token's plaintext is displayed until
dismissed and held nowhere else: the server kept only `sha256(token)`, so that
is genuinely the one moment it exists, and it is never written to `ui.toml`, the
log, or anywhere on disk. `y` copies it from that held value rather than from the
screen, which matters because a narrow or short pane clips the banner and leaves
a secret that can be read but not selected — and because the whole point of the
banner is that there is no second chance to fetch it. The copy goes out the same
OSC 52 + local-tool path the browse table's `y` uses, so it works over SSH. An
armed confirmation still wins the key: mint a token, arm a cancel, and `y`
answers the destructive question the footer is showing. Cancelling a token
rotates nothing — it let no one in — which is what separates it from revoking a
device.

**Panes and geometry.** `panes.go` is the single place that decides how the
screen is carved up: it resolves each view's pane rectangles in absolute screen
cells, plus the draggable seams between them. Renderers size themselves from
that geometry and `mouse.go` hit-tests against it, so the two can never disagree
about where a pane is. Browse tiles three panes (host sidebar; the command
table over the detail pane); the agent explorer tiles five — the executor
sidebar over a content-sized host list over the details pane down the left, the
prompt list over its command pane on the right. Every pane carries a title with its count or cursor
position, and `Tab` cycles focus within the active view.

- **Zoom** (`z`) expands the focused pane to the whole frame. Focus and zoom move
  together, so `Tab` while zoomed swaps which pane fills the screen rather than
  dropping back to the tiles; Esc unzooms before it leaves the view.
- **Mouse** (cell-motion reporting, enabled in `Run`): click focuses a pane, the
  wheel scrolls whatever the pointer is over without moving focus, and dragging a
  seam resizes the panes either side of it. The trade-off is that the terminal's
  own text selection needs the usual Shift modifier while the browser is open.
- **Remembered layout.** Seam positions are stored as a fraction of the axis they
  cut, so the ratio survives a terminal resize, and persisted to `ui.toml` when a
  drag *settles* — one write per resize, and the file always holds a layout the
  user stopped on. The unit is per-mille, not percent: at 140 columns one percent
  is 1.4 cells, coarse enough that a dragged seam would visibly snap away from
  the pointer. The zero value means "never dragged", so browse keeps its
  long-standing default layout until a seam is actually moved.

**Keys are described once** (`internal/tui/keyhelp`, driven by each UI's
`keys.go`). One table of bindings feeds two renderings: the one-line footer that
is always on screen, and the full grouped panel behind `?`. They cannot drift
from each other, and a test holds them both to the dispatcher — it parses the key
handlers' case clauses out of the package's own source and fails if a key is
handled but described nowhere, or described but no longer handled. That check is
the reason the panel can be trusted as the answer to "what can I press here".

The two renderings answer different questions, so they are sized differently:

- **The footer is terse and contextual.** It names a handful of keys for the
  state the UI is *actually* in — the browse table's arrows move a selection, the
  detail pane's scroll, the sidebar's pick a host, so the hint that names them
  follows focus. A modal (delete confirmation, the tag prompt, the filter box)
  swallows nearly all input, so its footer lists what is left rather than keys
  that would not fire. While the filter box has focus the footer does not offer
  `?` at all, because there `?` is a character being typed.
- **The panel is exhaustive for one view.** It lists every binding that does
  something *there*, grouped, in as many columns as the terminal affords. The
  devices pane handles its keys before the global ones ever run, so it advertises
  none of them — and it is the one screen where `q` means "back" rather than
  "quit". Mouse gestures are listed too: nothing else on screen says the panes
  are draggable. On a short terminal the list scrolls and says so, rather than
  being silently cut off — a truncated list of keys is a wrong one.

The panel is a modal: it covers the view it describes, so keys that would act on
rows the user can no longer see do not fire through it. The footer occupies
exactly one row in every state, which is what lets `applyLayout` budget for it
without re-measuring.

The `Ctrl-R` panel (`internal/tui/search`) carries the same two renderings, with
one difference forced by what it is: a filter box cannot spend `?` on help,
because a history search for `?` has to work. Its key list is on `⌥/` — which
sits with the panel's other alt-modified toggles — and on `?` in vim's normal
sub-mode, where nothing is being typed. Its hints are the last pieces on the
status line and are dropped whole when the terminal is too narrow, so a warning
like "daemon unreachable" always outranks them.

**Agent commands are hidden from both interactive search UIs by default**
(`hide_agent_commands`; `A` in the browser, `⌥a` in the Ctrl-R panel, per
session). One agent prompt can produce forty tool invocations, which bury a
morning of the user's own work in a table whose whole promise is "what happened
here, newest first" and crowd out the handful of commands worth recalling. That
history is not lost — the agent explorer is where it belongs, grouped under the
prompt that caused it rather than interleaved with commands nobody typed.

The filter is `QueryReq.HumanOnly`, applied **server-side**. Filtering after
`Limit` would spend the row budget on rows the UI is about to drop, so a machine
where an agent ran all morning would answer a full request with a handful of
rows.

Hiding a whole category of history is only acceptable if the UI says it is doing
it, so the daemon returns `QueryResp.HiddenAgents` — how many rows matched
everything else and were dropped by the filter. Both UIs spend it the same way:

- the count is on screen (`3 agent commands hidden`) whenever the filter is
  holding something back — the browser's status bar, the panel's status line;
- the **empty state names it**, which is the case this design exists for: a
  search whose only matches are an agent's must not answer "no matches", because
  that is a lie about the user's own history. It reads
  `3 agent commands hidden — A shows them` (`⌥a` in the panel), which matters
  most in Ctrl-R, whose entire job is recall;
- the key that undoes it is advertised **exactly when something is hidden** —
  precisely when someone might be wondering where a command they remember
  running went. In the browser it joins the footer; in the panel it takes the
  scope hint's slot, because the hints shed from the end on a narrow terminal
  and this is the one that matters then.

The two UIs differ in one place. The browse table also has the period tabs, and
with a period selected the count is **dropped, not adjusted**: it is counted
across everything the query matched, and the period narrows further,
client-side, over rows the daemon has already dropped — so the note states the
filter without quoting a number it cannot stand behind. The panel has no period,
so its count is always exact.

Asking for one executor is asking for agent commands, so the two filters never
both apply: `t` wins in the browser, and an explicit `--executor` both opens the
panel unfiltered and makes `⌥a` a no-op. Neither key writes config — a keystroke
that rewrote the setting would make an experiment permanent.

**`--headless` never hides anything.** It feeds scripts, which want the whole
archive and have no status line to be told what was withheld. The rule is that a
UI may filter only if it can disclose; the scripted path cannot, so it does not.

**Columns** (`columns.go`). The TUI has three row tables — the browse view's
results, the explorer's prompt list, and that prompt's command list — and each is
one list of specs saying what a column is called, how wide it is, how it draws a
cell, and how it orders two rows. The layout, the header, the row renderers and
the columns pane are loops over those specs. Each table used to carry its own
hand-kept trio (a width struct, a header builder, a row builder) that had to agree
about the set and the order, which is the kind of agreement that lasts until
someone adds a column — and there were three such trios drifting independently.

They generalize because they are the same shape underneath: some fixed metadata
columns, then one **flexible** column at the end taking what is left and carrying
the text (a command, or a prompt). `colTableDef` captures that, so the width
arithmetic and the shedding rules are written once rather than three times in three
slightly different ways. What stays per-table is what genuinely differs: the cell
gap (1 column in the browse table, 2 in the explorer), the flexible column's
floor, the order columns give up width in, and the order the table opens in.

`c` raises the **COLUMNS pane** over the table it is aimed at — in the explorer,
whichever list pane has focus, with the sidebar and details pane aiming at the
prompt list the view hangs off, the same rule `/` uses. `space` shows or hides a
column; `s` sorts by it, and `s` again reverses, so the key that picks a column is
the key that flips it. Each table keeps its **own** choices: hiding the session in
the prompt list says nothing about the browse table's host.

Sorting is client-side — every matching row is already in memory
(`proto.LimitAll`) — and **stable** over a slice that arrives in the table's
natural order, so that order stays the tiebreak underneath whatever was asked for
on top: equal durations keep their time order. Unknown sorts below every known
value for both exit status and duration: a row still running has no outcome, and a
command whose timing was never reported is not a fast command. Each table's default
*is* its natural order, which makes the sort free until it is changed — and means
any other order must be applied to a **copy**, because the unfiltered prompt list
is `prompts.prompts` itself and a prompt's command list is `p.cmds` itself.
Reordering those in place would rewrite the aggregate every other pane reads from.
`applyPromptFilter` and `visibleCmds` own that copy, and the re-sort path goes
through them rather than sorting behind their backs. The cursor is carried by
record ID across a re-sort: the row's index moves, the user's place in their
history does not.

Three things can take a column off screen and they are not the same thing: the
user hid it, the rows have nothing to put in it (no executor, no tags, one host in
scope, no agent reporting timing), or the pane is too narrow and it was shed. The
pane names which, because pressing `space` on a column the data gate closed would
otherwise look broken. Those gates are one-way — the pane can hide a column but
cannot force a blank one back on, since a column of empty cells costs width and
answers nothing. The flexible column cannot be hidden at all: a table of metadata
with no commands or prompts in it is not a history.

Both choices **persist to `ui.toml`**, beside the dragged seams and for the same
reason: a table you reshaped to read something is a table you want to find that way
next time, and re-hiding the same three columns every session is exactly the chore
a remembered layout exists to remove. They go to `ui.toml` and never to
`config.toml` — the hand-edited settings file is not something a keystroke should
rewrite, which is the line `A` (a genuine session toggle, seeded by
`hide_agent_commands`) sits on the other side of.

`ui.toml` is rewritten **whole** on every save, so the seams and every table's
columns travel in one `Prefs` struct through one callback. An earlier
splits-only callback would have erased the column choices each time a seam moved;
`prefsFromUI`/`uiFromPrefs` are a matched pair for that reason, and are tested as
one, because whatever either drops the other silently deletes from the file.

Columns are stored **by name** under a per-table key (`[columns.browse]`,
`[columns.prompts]`, `[columns.commands]`), because a file that outlives releases
cannot hold indexes — index 3 would come to mean a different column the moment one
is added. Loading is fail-safe the way `redact.yml` and `risk.toml` are: a name
this build does not have is ignored, a sort naming an unsortable column is dropped,
a `hidden` entry for a column that must always show is refused. A stale or
hand-mangled file can leave a table unremembered, never unusable. A table still at
its defaults writes nothing at all, so an untouched `ui.toml` grows no `[columns]`
section and inherits whatever the default later becomes.

The sorted column carries an arrow in its header where the cell is wide enough to
hold one, and the sort is named in words as well — in the browse status bar, and in
the explorer on the pane title, which is the only place those panes can say it.
`exit` is four columns wide with no room for a glyph, and a distinction carried by
color alone is no distinction.

**The two value filters.** The browse table filters on a tag and on an executor,
both server-side (`QueryReq.Tag` / `.Executor`), and each has two keys. `t` and
`e` adopt the highlighted row's value — one keystroke, and the common case, since
the reason you want a filter is usually the row you are looking at. But adopting
from the cursor can only ever reach values already on screen, so `T` and `E` open
a typed box on the search line instead: it starts on the filter in force, or
failing that the row's own value, which makes `T` a strict superset of `t` (Enter
alone does what `t` does) and still reaches a tag no visible row carries. An empty
box clears that axis, so a filter can also come off without hunting for a row that
happens to carry it.

Unlike the search field it does not filter as you type: every change is a round
trip to the daemon, and a half-typed tag matches nothing, so the table would empty
out under each prefix on the way to the name you meant. For the same reason `esc`
abandons the box and leaves the filter alone, where in the search field the text
typed so far *is* the filter and there is nothing to abandon. The prompt names the
axis (`filter tag ❯`) because three different boxes share that one line.

**The time window.** One period (`1`–`5`: Today / 7d / 30d / 90d / All, default
All) drives every screen, with its tabs pinned to the same top-right corner
everywhere. "Today" is the **calendar** day in local time, not a rolling 24
hours — the tab says today, and a rolling window would fold yesterday evening
into this morning's hour-of-day buckets. The daemon's query protocol carries no
time field, so the browse table filters the rows it got back — and it got back
every row matching the query (`proto.LimitAll`), so the period narrows the whole
timeline rather than a slice of it. The status bar names the window and how much
it hides.

**The agent explorer** groups agent commands by the prompt that caused them (see
"Recording & redaction" below for how that trace is captured). Picking an
executor in the sidebar filters the prompt,
command, and details panes to its work; the filter is held by executor *name*,
not row index, so an agent that drops out of the period releases the filter
rather than silently handing it to whoever inherits its row.

**What the details pane is describing** is chosen by the list pane focus last
landed on — the prompt's aggregate from the prompt list or the sidebar, the
selected command's path/time/duration/exit from the command pane — and it *sticks*
when focus lands on the details pane itself. Reading the focused pane instead, as
it first did, meant the pane's own two reasons to exist worked against each other:
the only route to a command's full record is to focus the pane and press `z`, and
focusing it swapped the prompt's record in on the way. It also left the arrows
driving the command cursor while the prompt was on screen. The title names the
subject (`DETAILS  command`) so a pane that outlives the focus that chose it still
says what it is holding.

The pane scrolls, because it holds a whole record and a tiled pane is shorter than
one: focused, `↑`/`↓`/`g`/`G` move its body — the same keys that scroll the browse
view's detail pane — and the title carries `↓ more` while there is more below,
since the pane has no scrollbar and a record cut off at the last visible row reads
as a record that ends there. The wrapped command or prompt text is capped at
`infoBodyCap` lines while the pane is only being glanced at, so a long one cannot
push the metadata rows out; focused, the cap lifts, because an ellipsis would hide
the very text the user came to read. A scroll offset belongs to the record it was
scrolled into, so moving to another command or prompt rewinds it.

The command pane's exit column is the same 4 wide as the browse table's, so the same value is the
same width in both.

The explorer's sample is cross-host (the query is scope-all), and the HOSTS
pane under the executor sidebar filters it by machine: one row per host with
its agent-command count, an "all hosts" row on top, the bullet on the active
stop. Picking a host works exactly like picking an executor — the cursor
selects, and the work panes (executors, prompts, commands, details) narrow to
that machine — while the stats screen, which shares the sample, stays
unfiltered. `H` walks the same rows from anywhere in the explorer: all hosts,
each host in name order, back to all. The filter is held by host*name* with the
same release rule as the executor filter. The pane's own rows never narrow, and
its counts ignore the explorer's filters — it is the map of where the filter
can go, not a view of where it is — and the pane is always present, so the
layout never depends on the data. It sizes itself to its content until the seam
above it is dragged; from then on the dragged proportion wins, held as a
fraction of the top-left region so it survives moving the main seams too. `A` is
the same ring on the other axis, walking the executor sidebar — all agents, each
executor in turn, back to all. Either key on a sample with only one stop says so
rather than appearing to do nothing, since "all" and the single child describe the
same work. (`A` means this only here. In the browse table it hides and shows
agent commands, that view's one agent-shaped question; in the explorer every row
is agent work already, so the useful question is *which* agent.)

The prompt pane spends a column on the host only when its rows can disagree about
it: several machines in the sample *and* no host filter up. Filtered to one, every
cell would repeat a name the pane title, the HOSTS bullet, and the header line all
already carry, so those ten columns go to the prompt text instead. The table does
therefore reshape as `H` walks the ring — that is the point, since the column
exists to tell hosts apart and a chosen host leaves none to tell apart.

**Filtering the explorer.** `/` filters the focused pane's list — prompts by
their text, commands by the command — matching with the same `internal/match`
query the browse view and `Ctrl-R` use, highlights included. The two lists keep
**separate** queries: tabbing between panes must not silently re-point one
pane's filter at another pane's rows, so each `/` opens on its own query (ready
to edit rather than retype). The sidebar and the details pane have no list of
their own, so `/` there aims at the prompts.

- **What is filtered is the view, not the aggregate.** A command filter narrows
  the command pane only; the prompt's counts, duration and modal path still
  describe the prompt. Rewriting those to match a search would make the details
  pane and the prompt list disagree about the same prompt.
- **A filtered count is marked as one.** The pane's border reads
  `PROMPTS  1/1  /tower`: without the marker a narrowed list is
  indistinguishable from a quiet period, and the query is the only thing on
  screen that accounts for the missing rows. Filtered to nothing, the pane says
  which query found nothing rather than rendering as empty.
- **Esc backs out one visible thing at a time** — the zoom, then the focused
  list's filter, then the view. Each level is on screen, so each gets its own
  Esc, and the footer names whichever one is next.

The filter is orthogonal to the period tabs and the executor sidebar: all three
narrow independently, and re-aggregating for a new window or executor re-applies
the text filter rather than dropping it.

**The stats graphs** — the activity heatmap, the daily trend, and the hour-of-day
histogram — all span the full width, and a wider terminal buys *more history*
rather than more whitespace: the heatmap draws `(width − 4) / 2` week columns
(two-cell days, so a cell reads as a square, up to two years) under a month
ruler, and the daily trend draws one column per day for as many days as there
are columns. The heatmap and the daily trend deliberately ignore the period:
they exist to show the shape of activity *around* the window the other panels
summarize, so narrowing to Today must not blank them. Both scale to the peak of
what they actually draw, so a spike outside the visible window cannot flatten the
bars on screen. The hour-of-day histogram does respect the period, widens its
fixed 24 buckets to fill (the remainder going to the leftmost, so the row ends
flush), and on Today leaves the hours that have not happened yet **blank** rather
than drawing them as zero — "it isn't 11pm yet" is not "nothing ran at 11pm".

Vertically the screen fits charts **whole or not at all**: each is offered the
rows it needs and declines if taking them would starve the ranked columns, so a
short terminal loses a chart cleanly instead of showing one with its axis sliced
off. The heatmap has a compact form — the week folded into a single row of
per-week totals — which it falls back to before dropping, because the full graph
needs ten rows and a stock 80×24 terminal has never had them: the one view that
shows years at a glance would otherwise be invisible at the default size.

**What the colors mean.** Chart ink is its own ramp (one hue, four intensity
steps) and is deliberately *not* the UI accent: every bar, gauge and heat cell
used to render in accent blue, which on the stats screen meant everything with
ink was the color of everything selectable, so the accent distinguished nothing.
Intensity rides on lightness as well as on glyph height, and the heatmap draws a
solid block rather than a `░▒▓` density ramp — those are the least portable
glyphs in the box-drawing set, and a font that renders `▒` and `▓` alike
silently collapses two of the four levels. A day with no activity keeps its own
dim `·`, so "none" and "a little" never look the same.

Headings come in exactly three ranks, because a terminal has very few levers for
hierarchy and a rank that shares one is a rank the eye cannot find: accent+bold
for a pane's name (in its border), bold for a heading inside a pane, dim for the
chrome below both (column headers, field labels). Column headers are lowercase
and pane names uppercase throughout, and the two details panes — the browse one
and the explorer's, a keystroke apart over the same record — use one set of field
names in one label column.

A command's outcome gets a distinct **glyph** per state (`·` unknown, `✓` ok,
`✗N` failed), not a shared glyph in three colors: success and unknown once
differed by color alone, which put the distinction out of reach of anyone who
cannot separate dim grey from green, and out of reach of a screenshot.

**Identity is a hue.** Hostnames and executor names hash into the same fixed
8-hue palette (red and green excluded — those belong to exit status), so the
same *who* is the same color everywhere it appears: the sidebar, the prompt
table, the ranked stats lists, the details rows. `(you)` stays plain — the user
is not an agent identity to pick out of a lineup.

**Command text is syntax-lit wherever it is command text** — the table rows,
the explorer's command pane, both details panes' wrapped command body, and the
"Top commands"/"Top programs" stats lists all classify through the same
`tui/hl` pass, with match highlighting layered on top (matches always win).
Never over prose: a prompt is English, and shell coloring over English paints
arbitrary words as flags. Paths dim their directory and keep the leaf normal,
so the eye lands on the name rather than the prefix.

**Risk is a glyph-first ramp** shared verbatim with the MCP output (`⛔`
critical, `⚠` high, `▲` medium, `•` low, `✓` safe), shown as a `Risk` row in the
two command-level details panes — for every command, clean ones included. A row
that appears only on a hit cannot be told from a rule that never ran, so the
clean verdict is stated: `Risk  ✓ safe`. The category is dropped when it merely
repeats the level, so a safe command reads once rather than `safe (safe)`; a
command silenced by the user's `ignore` list keeps its `(ignored)` and says why
it went quiet. Critical borrows the exit red and safe the exit green — the same
`✓` means the same thing on the `Exit` and `Risk` rows — medium the match amber;
high is the palette's one ink of its own, an orange seated between them, so the
ramp reads as five levels while spending one new color.

The prompt details pane has no `Risk` row: a prompt is not a command, and there
is nothing to assess until it triggers one.

What the ramp is ranking, and how a user's `risk.toml` reshapes it, is the
"Risk" section below. The row is a label on history, not a guard: nothing the
browser shows has any bearing on what an agent is allowed to run.

**Sample honesty.** The stats and agent screens aggregate the whole archive, so
the header normally reads "all history". It derives that from the response
itself — `Total > len(Rows)` means the daemon returned less than it matched — not
from a compiled-in ceiling, so the claim stays true whatever any caller asks for.
If a sample ever does arrive short, the header says so and how far back it
actually reaches: without that, every window wider than the sample's reach shows
identical numbers and the period tabs read as broken when they are working
exactly as intended.

## Recording & redaction

`yore record`, `yore import`, and the shell-history gate (`yore filter`) all run
the same ordered gate before anything is persisted:

1. **Leading-space opt-out** (`histignorespace`): a command starting with space
   or tab is skipped, unless `record_space_prefixed` is true.
2. **Ignore-dirs**: if the command's cwd is at or under a configured
   `ignore_dirs` prefix (segment-aware) it is skipped.
3. **User ignore-patterns**: a command matching one of the user's own
   `ignore_patterns` regexes is skipped. Like `ignore_dirs`, this is the user
   saying "never record this", so the record is dropped, not masked.
4. **Secret rules** (`internal/redact`): rules load from the editable, seeded
   `~/.config/yore/redact.yml`; each rule is a name + Go regexp + optional cheap
   literal `hints` (a hot-path pre-filter — the regex only runs if a hint is
   present) + optional case-`fold`. Built-ins anchor on the *shape* of a secret
   (AWS AKIA/ASIA + secret-key, GitHub `ghp_`/`github_pat_`, Slack `xox*`,
   Google `AIza`, Stripe `sk_live_`, OpenAI/Anthropic `sk-`, Hugging Face,
   npm, PyPI, SendGrid, PEM, JWT, URL userinfo, tool password flags for
   openssl/gpg/sshpass/mysql/mongo/smbclient/curl/wget, generic
   `token=`/`secret=`/`password=` assignments, and the prose form of the same
   — `the api key is …` — because this gate covers agent **prompts** as well as
   commands, and a prompt states a secret in a sentence).

**A secret rule redacts; it does not reject.** The credential is replaced with a
marker naming the rule that caught it and everything else is kept:

```
export DB_PASSWORD=⟪redacted:generic-token-assign⟫
mysql -uroot -p⟪redacted:mysql-password⟫ appdb
```

Dropping the whole record was the older behaviour and it was wrong in practice:
the commands most worth remembering are often exactly the ones with a token in
them, and an entry that silently vanished is indistinguishable from one that was
never run. The marker is deliberately not valid shell, so a redacted command
recalled into the prompt fails loudly rather than running wrong.

Only the credential goes. Rules mark it with a `(?P<secret>…)` capture group, so
a rule that matches a wide context (`mysql -u root -p<pw>` matches from the
program name) still only blanks the password; a rule with no such group — a
whole-value shape like an AWS key — has its entire match replaced. Spans from
all rules are collected against the *original* text and overlaps merged, so
markers never nest and never get re-matched; redaction is idempotent, which
matters because records cross the gate more than once (capture, then import).

One path is still all-or-nothing: `yore filter`, the gate for the *shell's own*
history, can only accept or reject — zsh gives `zshaddhistory` no way to rewrite
the line — so a command yore would redact is dropped from the shell's history
entirely. yore's own copy, redacted, is still there to search.

Redaction is **fail-safe**: a missing, unreadable, unparseable, or empty
`redact.yml` falls back to the compiled-in built-ins (never "redact nothing"); an
individual invalid regex is skipped with a warning while the rest stay active.
`yore setup` seeds `redact.yml` from the built-ins without ever clobbering edits
— which means a file seeded before a rule shipped keeps missing it, silently, so
`yore doctor` reports any built-in the file lacks rather than re-adding it (a
rule may be absent because it was deliberately deleted).

**Executors and tags are two different axes**, and yore keeps them apart
everywhere: an executor is an *attribute* of a record — which agent ran the
command — captured once from the environment and never edited; a tag is a
*label* somebody applied, and can be added and removed at will. They were once
one field (`Record.Tag`, resolved into one merged set), which meant a UI could
not tell "an agent ran this" from "I called this a refactor", and labelling a
command hid which agent had run it. They are now separate all the way down to
the format: the executor is its own `executor` key on disk and in the sealed
payload, next to the `tag_name`/`tag_desc`/`tag_op` keys a user-tag record uses.

**Executor.** Each record names what ran it: an explicit `--executor`, else
`$YORE_EXECUTOR` (`$YORE_TAG` is the older name and still works), else
auto-detection from agent env markers (`CLAUDECODE`/`CLAUDE_CODE_ENTRYPOINT` →
`claude-code`, `CURSOR_TRACE_ID` → `cursor`, `AIDER_MODEL` → `aider`, etc.), else
`""` (interactive). `yore search --executor claude-code` separates "what I typed"
from "what an agent ran"; in the browser, `e` filters by the selected row's
executor and `A` hides agent commands wholesale.

**User tags** are freeform labels, and a record can carry several. A `tag` record
(`Type == "tag"`) adds or removes a named label on a command (`target_id`) or a
session, and rides the same E2E stream as commands: sealed per record, remapped
by name on sync (names are the identity, so no id reconciliation), server
ciphertext-only. The daemon folds tag records into an in-RAM index
(`command|session → {tags}`, fed by both local ingest and remote pull) and
resolves each row's **effective tags** at query time — command tags ∪ session
tags ∪ `auto_tags` (cwd-prefix rules from config, applied at read time so there
is no record-path cost and rules apply retroactively). `yore tag
add/rm/list/create`; `yore search --tag <name>`, or `t` in the browser. Tagging a
session is the common case: it covers every command that shell has run and every
one it runs afterwards.

`yore tag list` counts **commands**, not associations — one session tag over a
day's work reads as that day's work, not as the single `tag add` that created it
— which means the listing resolves the corpus and so also shows `auto_tags`
rules. `--scope all` counts every host instead of this one. Executors are never
listed as tags, and `--tag claude-code` matches nothing.

An agent whose commands run in a *non-interactive* shell (Claude Code's Bash
tool is `zsh -c …`) is never seen by the rc hooks, so `yore init claude-code`
installs four Claude Code hooks: **PreToolUse** stamps each Bash command's start
time (`yore hook claude-code-pre`), **PostToolUse** pipes each *successful* Bash
command to `yore hook claude-code`, **PostToolUseFailure** pipes each *failed*
one to `yore hook claude-code-failure`, and **UserPromptSubmit** pipes each
prompt to `yore hook claude-prompt`. The command hooks record the command's
**exit status** — an explicit `tool_response.exit_code` when present, else the
event decides (PostToolUse → 0, PostToolUseFailure → nonzero) — and its
**duration**: a payload timing (`tool_start_time`/`tool_end_time`) wins when
present, otherwise the delta from the PreToolUse start-stamp (Claude Code's own
payload carries no tool timing — this is how agent commands get real durations at
all). So agent commands carry the same outcome data as shell ones (success rates,
`what_failed`, risk of failed commands all work).

**Prompts are records.** The prompt hook spools one `Type == "prompt"` record
holding the text, and writes only that record's **id** to a per-session state
file; the command hooks read the id and stamp `prompt_id` onto each command they
record. A stored command row therefore holds the id and nothing else, and the
daemon rejoins the two at query time from its prompt index, so consumers just
read `prompt` on a command row and never see the join.

Storing the text on its own record rather than on every command that quotes it
is what keeps prompt tracing cheap: one prompt drives ~16 commands in practice,
so the inlined alternative pays 16× the bytes on disk, on the wire, and in the
daemon's heap — for a field the search path never indexes. It also makes a
prompt that triggered **no** commands representable at all; as a field on its
commands, a prompt that caused none would have nowhere to live.
`sync_prompts = false` keeps prompt records on the machine that recorded them
while their commands still sync (the `prompt_id` then resolves to no text
elsewhere).

The start-stamp is keyed by session+command under `agent-cmd-starts/` and
consumed once. That `prompt_id` is what the browser's agent explorer groups on
(see "The browser" above). All hooks go through the same redaction gate as the
shell path — a secret in a *prompt* is masked exactly like one in a command.
`yore init claude-code` writes ~/.claude/settings.json (or, with --project,
./.claude/settings.json), merging without disturbing other settings.

The **Devin CLI** integrates the same way — `yore init devin` merges into Devin's
unified `config.json` (the global `~/.config/devin/config.json` by default, or
`./.devin/config.json` with `--project`), binding PreToolUse/PostToolUse/
UserPromptSubmit to Devin's `exec` tool (`yore hook devin{,-pre,-prompt}`, tagged
`devin`) under the `hooks` key and registering yore's MCP server under
`mcpServers` — the same file that holds Devin's auth, which the merge preserves
untouched (and never reads out). Devin reports outcome as a boolean
`tool_response.success` (mapped to exit 0/1) and, like Claude Code, no timing —
so the PreToolUse start-stamp supplies the duration.

**Cursor** capture works the same way via `yore init cursor`, which installs
Cursor's `afterShellExecution` + `beforeSubmitPrompt` hooks into
`~/.cursor/hooks.json` (correlated by `conversation_id` → session `cursor-<id>`)
and registers the MCP server in `~/.cursor/mcp.json`. Cursor's payload carries
the command and a duration but no exit code, so Cursor commands record duration
and prompt tracing with an unknown exit; its prompt hook always emits
`{"continue": true}` so it never blocks a prompt.

**OpenCode** uses a JS plugin rather than command hooks, so `yore init opencode`
writes `~/.config/opencode/plugins/yore.js` (or `.opencode/plugins/` with
--project). The plugin adapts OpenCode's `tool.execute.after` (bash) and
`message.part.updated` events and pipes JSON to `yore hook opencode` /
`opencode-prompt` — capturing command, cwd (`args.workdir`), **exit code**
(`output.metadata.exit`), and the triggering prompt, correlated by session
(`opencode-<id>`). It also registers the MCP server under the `mcp` key of
`~/.config/opencode/opencode.json` (a local stdio server).

**Codex** (OpenAI's CLI) has Claude-Code-shaped hooks, so `yore init codex`
appends `[[hooks.PostToolUse]]` (matcher `^Bash$`) and `[[hooks.UserPromptSubmit]]`
blocks to `~/.codex/config.toml` (running `yore hook codex` / `codex-prompt`).
The payload shares `claudeHookInput`'s shape (`session_id`, `cwd`,
`tool_input.command`, `tool_response`, `prompt`); Codex has no separate failure
event and doesn't document its `tool_response` exit field, so exit is
best-effort (`tool_response.exit_code` when present, else unknown). It also
appends an `[mcp_servers.yore]` block registering the MCP server.

All MCP/hook installers are additive and idempotent, preserve unrelated config,
and are verified by `yore doctor` (per-agent capture + MCP registration checks).

**`yore uninit <agent>`** reverses any of them. It removes only yore's own hooks
and MCP registration — matched by the exact command string the installer wrote —
and leaves every other key in place, so an agent's own settings (and, for Devin,
the auth that shares the file) survive. Emptied blocks, events, and maps are
pruned so the file is left as it was found; a config that has been hand-edited
past recognition reports "nothing to remove" rather than guessing. A missing file
is a no-op and a malformed one is left untouched. Shells are not agents: the
`eval "$(yore init zsh)"` line comes out of your rc file by hand.

## MCP server (`internal/mcp`)

`yore mcp-serve` is a local, read-only [Model Context Protocol](https://modelcontextprotocol.io)
server — stdio JSON-RPC, no network port — that lets a coding agent query the
history it is creating. It is a thin adapter over the daemon's query layer (it
dials the unix socket and issues `OpQuery`; it never opens the store), so it
inherits the single-writer guarantee and, crucially, the **cross-machine** RAM
corpus: tools take a `scope` (`local`/`all`), and `all` answers over every
enrolled device while the sync server still holds only ciphertext — a question
like *"have I run this migration anywhere?"* a single-machine tool cannot answer.
It exposes tools (`search_commands`, `command_status`, `what_failed`,
`get_prompts`, `replay_agent_session`, `assess_risk`, …) and context resources
(`yore://history/recent`, `…/risk/summary`, …) over `resources/list` and
`resources/read`. Nothing is pushed: a tool runs only when the model calls it,
and a resource enters context only when the client attaches it — in Claude Code,
an explicit `@` reference. `yore init claude-code` registers the server in
`~/.claude.json` (or `./.mcp.json` with `--project`).

**Risk** backs the `assess_risk` tool — which also reports how often the command
has run across your machines and whether it succeeded — and the `risk/summary`
resource. See the next section.

## Risk (`internal/risk`)

A deterministic, rule-based classifier for how dangerous a shell command is:
`safe` < `low` < `medium` < `high` < `critical`, each verdict carrying a category
and a one-line reason. No model, no network, no state — the same command always
gets the same answer, and the answer can always be explained.

**It is advisory, and never blocks anything.** This is the property to be clear
about, because a tool that rates danger invites the assumption that it prevents
it. The classifier is not consulted anywhere in an execution path. Note that
this is a choice, not a missing capability: the `PreToolUse` hook `yore init`
installs for Claude Code and Devin already receives the command *before* it
runs, and could adjudicate. It deliberately doesn't — it stamps a start time so
`PostToolUse` can report a real duration, then exits 0 without printing, like
every other capture hook. A recorder that can veto is a recorder that can wedge
a session, and the prompt-latency invariant makes the same argument for shells.

So there are exactly three consumers, and all three only *display* a verdict:
the `Risk` row in the browser's two command-level details panes, the
`assess_risk` MCP tool, and the `risk/summary` resource. An agent about to run a
destructive command touches none of them unless the model volunteers to ask
first, and nothing obliges it to — or stops it from proceeding when the answer
comes back `critical`.

**Reaching a verdict** takes four steps. A command that cannot execute anything
short-circuits to safe: empty, a `#` comment, an `alias …` definition, or a bare
`echo`/`printf` — the last only when it holds no `| & ; > <`, backtick, or `$(`,
so `echo $(rm -rf x)` does not slip through. Then the user's `ignore` patterns
are tried, and a match returns safe while naming the pattern that silenced it.
Otherwise the line is parsed (below) and every rule is scanned, highest severity
winning; ties keep the first, which is why the table is ordered most-severe-first
within a level. Finally, anything the line hands to an interpreter is assessed
the same way, recursively.

**A command is judged by what it runs, not by what it contains** (`parse.go`).
This is the distinction the whole classifier rests on: `grep -rn "rm -rf" docs/`
and `sh -c 'rm -rf /'` carry the same eight characters and only one of them
deletes anything. So before any rule runs, the line is lexed into words and cut
into command *segments* wherever a shell would start a new command — a pipe, a
`;`, a `&&`, a `$(…)` (even inside double quotes, where it still runs), a
`find -exec`. Each segment resolves the command word it would actually execute,
with `sudo`-style wrappers stepped over so `sudo rm -rf /` is an `rm`, quoted
text kept as inert data, redirection targets pulled out, and **its own flags kept
to itself** — the `-r` in `grep -rn` is not available to an `rm` three words
away. Rules then ask "is `kill` the command here?" rather than "does this string
contain kill", which is what lets `cat kill.txt` and `git commit -m "remove the
kill switch"` stay safe.

Two consequences are worth stating. A segment carrying `--help`, `--version`, or
`--dry-run` is *inert*: it announces what it would do, so `npm install --dry-run`
rates nothing. And quoted text handed to something that will execute it —
`sh -c '…'`, `python -c '…'` — is re-assessed as the command it becomes, to a
depth of three; the same text handed to `grep`, which executes nothing, is not.

**The built-in rules** (`rules.go`), 65 of them across four levels. Levels mean
something specific, and the meaning is what keeps the ramp useful rather than
uniformly alarming: **critical** is irreversible, **high** is undoable only with
effort or changes what code runs, **medium** has real but ordinarily recoverable
side effects, **low** reaches off the machine. Because severity wins over order,
`sudo npm install` is high (package-install), not medium (privilege).

| Level | Matches | Category |
|---|---|---|
| ⛔ critical | `rm` both recursive *and* forced (`-rf`, `-fr`, `-r -f`, long forms) | destructive |
| ⛔ critical | `git push --force`/`--mirror` — but not `--force-with-lease` | destructive |
| ⛔ critical | remote branch deletion (`git push --delete`, `git push origin :branch`) | destructive |
| ⛔ critical | `git reset --hard`, `git filter-branch`, `git reflog expire` | destructive |
| ⛔ critical | `DROP`, `TRUNCATE`, unfiltered `DELETE`/`UPDATE`, `FLUSHALL`, `.drop()` | destructive |
| ⛔ critical | raw disk writes (`> /dev/sd*`, `dd of=/dev/…`), `mkfs`, `wipefs`, `shred` | destructive |
| ⛔ critical | `terraform`/`tofu`/`pulumi destroy`, and any apply with `-auto-approve` | infra |
| ⛔ critical | `kubectl delete namespace`/`--all` | infra |
| ⛔ critical | cloud deletion — `aws`/`gcloud`/`az`/`doctl` `delete`/`terminate-*`/`rb` | cloud |
| ⛔ critical | recursive object-store wipes (`aws s3 rm --recursive`) | cloud |
| ⛔ critical | fork bomb; a shell bound to a socket (`nc -e`, `socat EXEC:`) | script-exec |
| ⚠ high | package installs — npm/yarn/pnpm/bun, pip/uv/poetry, cargo, go, gem, brew, apt/dnf/yum/pacman/apk/zypper/nix | package-install |
| ⚠ high | publishing — `npm publish`, `cargo publish`, `gem push`, `docker push`, `twine upload` | supply-chain |
| ⚠ high | fetch piped into an interpreter; `bash <(curl …)`; `eval "$(curl …)"`; anything piped into a shell | script-exec |
| ⚠ high | running a local script (`./x.sh`, `bash x.sh`, `. ./setup.sh`) | script-exec |
| ⚠ high | ephemeral runners — `npx`, `uvx`, `bunx`, `pipx run` | script-exec |
| ⚠ high | `chmod` granting group/world **write**, or setting setuid/setgid; `chown -R`; `setfacl` | permission |
| ⚠ high | `git clean -f`; `find -delete`; `rsync --delete`; package *removal* | destructive |
| ⚠ high | `docker`/`podman` prune, `volume rm`; `kubectl delete`/`drain`; `helm uninstall` | destructive |
| ⚠ high | partition-table edits (`fdisk`, `parted`, `sgdisk` — but not their `-l`) | destructive |
| ⚠ high | `crontab -r`, which deletes every scheduled job at once | destructive |
| ⚠ high | account changes — `useradd`, `userdel`, `usermod`, `passwd`, `visudo` | account |
| ⚠ high | reading or copying private key material (`~/.ssh/id_*`, `/etc/shadow`, `*.pem`, `.aws/credentials`); `gpg --export-secret-keys` | secret |
| ⚠ high | firewall teardown — `iptables -F`, `ufw disable`, `setenforce 0` | network |
| ⚠ high | a container given the host (`--privileged`, `-v /:/host`, the docker socket) | container |
| ⚠ high | `shutdown`, `reboot`, `init 0` | system |
| ▲ medium | `sudo`/`doas`; `su`, `pkexec` | privilege |
| ▲ medium | `docker`/`podman` `rm`/`kill`/`stop`/`down` | container |
| ▲ medium | `kill`, `killall`, `pkill` | process |
| ▲ medium | `git reset`, `restore`, `checkout --`, `stash drop`/`clear`, `branch -d`, `tag -d`, `gc`; `rebase`, `cherry-pick`, `commit --amend` | destructive |
| ▲ medium | `truncate`, and a bare `> file` redirect with no command producing content | destructive |
| ▲ medium | `systemctl stop`/`disable`/`mask`; `crontab -e`, `at` | system |
| ▲ medium | piping local output into a request body (`… \| curl -d @-`) | network |
| ▲ medium | `terraform apply`, `kubectl apply`, `helm install`, `ansible-playbook` | infra |
| ▲ medium | running an executable out of the working directory (`./configure`) | script-exec |
| • low | `curl`, `wget`, `ssh`, `scp`, `rsync`, `nc`, `telnet` | network |
| • low | `git push` | vcs |
| • low | a credential typed into the environment (`FOO_TOKEN=…`) | secret |

Rules live in Go rather than in regexps wherever being right matters more than
being uniform, which is most of them. `chmod` is the clearest case: only the
group and other digits can grant anyone a write bit, so `644`, `755`, and `600`
— the three most common modes there are — must not be flagged while `chmod -R
777` must, and no single pattern gets both ends of that right. `git push --force`
carves out `--force-with-lease`, which RE2 cannot express without negative
lookahead and which would otherwise rate the *safe* form as the most dangerous
thing in the table. SQL is matched by content, but only where SQL would actually
execute — handed to a database client, or typed as the whole line — so
`git commit -m "drop table support"` is a commit message and not a dropped table.

**What it does not see.** The parser resolves command position, not semantics.
A command behind an alias, a `Makefile` target, or a variable holding a program
name trips nothing; neither does one carried inside `docker exec web …` or
`ssh host …`, where the remote command is an argument this classifier does not
follow. This stays the deliberate trade — a classifier that is wrong in an
obvious, inspectable direction beats one that is wrong subtly — and `risk.toml`
is where a team closes the gaps that matter to them.

**Risk rules are the user's.** The built-in table ships compiled in;
`~/.config/yore/risk.toml` extends it — `[[rule]]` entries with a Go regexp, a
level, and an optional category/reason, plus a top-level `ignore` list that
neutralizes false positives by naming them. User rules are appended after the
built-ins and severity wins, so a rule can only ever *escalate* a command;
de-escalation is what `ignore` is for, and it goes all the way to safe. Loading
is fail-safe exactly like `redact.yml`: a missing file is silent, a broken file
or entry is skipped with a warning, and the built-ins are never lost — a typo
cannot switch risk assessment off. `yore doctor` is where a skipped rule gets
named (the browser's alt-screen and the MCP server's stdout-owned transport have
nowhere to say it), and the browser and `assess_risk` load the same file, so the
TUI and an agent asking about the same command always agree.

## Shell integration modes

`config.integration` (default `takeover`, overridable per `yore init --mode`)
controls how deeply the emitted hooks take over the shell, all via `yore init`:

- **takeover** — yore is the single source of truth. The shell's persistent
  history is disabled (no unredacted `~/.zsh_history`); its in-memory list is
  seeded from yore (`yore export --shell`, one `fc -R` / `history -r` at startup)
  and gated by yore's redaction (`yore filter` from zsh's `zshaddhistory`, best-
  effort in bash), so `!N` / up-arrow work against yore-consistent, secret-free
  history. zsh is exact; bash is coarser (multiline collapses to one line).
- **coexist** — record alongside the untouched native history; rebind Ctrl-R and
  add the aliases. Native `!N` works against native history.
- **capture** — record only; no keybinding or alias changes.

**Mechanics.** zsh installs `zshaddhistory` (calls `yore filter`, returns nonzero
to drop) and rebinds `^R` (and optionally Up, `bind_up_arrow`) to a widget that
runs the search TUI. bash uses vendored bash-preexec for capture and a best-
effort gate. Scoped convenience aliases (unless `--no-aliases`): `hb` (browse,
drop pick onto the next prompt), `hs` (search this host), and siblings `hsa`
(all hosts), `hss` (session), `hsc` (cwd), `hsw` (workspace). `yore` never
rebinds `h`. Two support subcommands back this: `yore filter` (reads a command
on stdin, exits 1 to drop) and `yore export --shell [--format zsh|bash]` (the
history seed) — neither is on the prompt fast path (`filter` runs synchronously
from `zshaddhistory` in single-digit ms; `export` runs once per shell start and
is best-effort: no daemon means it prints nothing and exits 0).

## Key hierarchy & E2E (summary)

```
device X25519 + Ed25519 keypairs   per machine; private halves never leave it
  │ X25519 seals ─▶ History Key (HK)   one 32B symmetric key per group, stored
  │                                     only as per-device wrapped blobs (no master)
  │ HK wraps    ─▶ epoch Data Keys      32B, one per epoch (default 24h), wrapped under HK
  │ DEK seals   ─▶ history records      each sealed XChaCha20-Poly1305, AAD-bound
  └ Ed25519 signs ─▶ mutating sync requests (reqsign)
```

- **Read**: one asymmetric HK unwrap per daemon lifetime; then every DEK and
  record opens symmetrically. Cost is **O(1) in history age**.
- **Enroll**: an existing device wraps HK for the newcomer's pubkey — one wrap.
- **Revoke**: rotate to a new HK, re-wrap the (few) DEKs and HK for surviving
  devices — **records are never re-encrypted**. Also O(1) in history age.
- **AAD** binds every sealed record to `recordID|hostID|seq|keyID`, so a
  compromised server cannot reorder, replay, or substitute blobs undetected.
  Decryption failure is fatal to a pull (never silently skipped).
- **No shared secret at all.** A device authenticates with its Ed25519 key on
  every request, reads included; there is no bearer token to capture. The
  server's configured token is only an *enrollment token for an empty group* —
  once any device is active it enrolls nothing, and every later machine needs a
  single-use token minted by one already enrolled.
- **Recovery.** Per-device keys mean losing every device would otherwise lose
  the archive for good, so bootstrap also seals HK to a key derived (Argon2id)
  from a one-time recovery phrase shown once and never stored. The server holds
  only the salt, the recovery public keys, and that wrap — still nothing that
  can decrypt history.
- **Request signing** (`reqsign`) means a captured request can't push
  garbage or revoke a device; optional TLS cert pinning (`yore setup --pin`)
  hardens against a TLS-inspecting proxy, fail-closed. Fail-closed is the right
  default and a bad failure mode to diagnose blind — the pin outlives only the
  server's *key*, not its certificate, so an ACME renewal with a fresh key breaks
  sync while local history keeps working. A mismatch is therefore a typed
  `syncer.PinError` carrying both digests, and `yore doctor` reports it as its own
  diagnosis (with both remedies, since re-pinning from an intercepted network
  would pin the interceptor) instead of a generic connection failure.

**`yore setup` budgets each stretch of talking to the server separately** rather
than putting one deadline around the whole run. The run stops to ask for a token,
and fetching one means walking to another machine — a single deadline opened
before the question expired while the user was answering it, so the enrollment
that followed failed the instant the token was pasted in, and was reported as the
server *refusing* it. The prompt asks for the token and says nothing else: it
used to call itself "the server token for the first machine", which is true only
of the enrollment that forms the group and is wrong advice on every machine
after it. A timeout is now named as a timeout, since no amount of minting fresh
tokens fixes a server that never answered.

Exact byte layouts, domain-separation strings, and the device.key format are in
`protocol.md`.

## Sync & eventual consistency

Streams are **append-only, per-host, client-sequenced**; merge is a set-union by
record ULID; deletes are appended tombstones — so sync is conflict-free and
eventually consistent by construction.

The daemon's sync loop (started only when a server is configured) is driven by:

- an initial warm sync ~2 s after startup (so deep search is warm quickly);
- a periodic tick every `sync_interval` (default **5m**);
- `syncWake` — a deep read or opening `browse` nudges a background cycle;
- **EXPERIMENTAL** `push_debounce` (default **off**): setting a duration arms a
  coalesced push shortly after new records are ingested (`pushWake`), so
  cross-host propagation is seconds rather than up to `sync_interval`. Off by
  default because an agent firing command bursts would push about once per
  debounce for its whole run. The eager nudge fires only while the server is
  reachable — when it is offline, new records stay spooled in the local store
  and go out in a batch when the next periodic tick reconnects, rather than
  firing an eager push that would only fail.

`yore sync` (and `S` in the TUIs) runs a **synchronous** cycle via `OpSync` and
reports the real outcome. Every cycle is `Push` then pull, serialized by a mutex
so the periodic loop and an explicit sync never overlap.

**Push** uploads local records above a persisted watermark (`last_uploaded_seq`)
in ascending batches bounded by **both** ≤1000 records **and** ≤8 MiB of encoded
body, advancing the watermark by exactly what the server acked (a failed batch is
retried, never skipped). Size is a bound because size is what the server actually
limits: a thousand records carrying long prompts or heredocs is megabytes, and a
body over the server's 10 MiB cap fails *every* retry — the watermark never
advances and sync wedges permanently. If the server rejects a batch anyway (413,
or the 400 a truncated body decodes as), the client halves it and retries; the
halving terminates at a single record, so a genuinely bad request surfaces as an
error instead of looping forever. The server now answers an oversized body with a
real **413** rather than a generic 400.

**Pull** is split in two so the ciphertext can be banked before anything is spent
on crypto: `PullCiphertext` walks each other host's stream from its persisted
cursor and `OpenRecords` decrypts. Each cycle writes the new sealed records and
the advanced cursor to `remote.db` *first*, so a process that dies mid-cycle
resumes rather than re-downloading. On the first cycle of a daemon's life the
cache is decrypted into RAM (that needs the server, to unwrap the keys — so a
cold start still has no remote history while offline). A decryption failure
during a live pull is fatal, as always; one while decrypting the *cache* is not
treated as tampering — the only way that file can hold records we cannot open is
if it outlived the group it belongs to, and it is derived data, so it is thrown
away and re-pulled rather than wedging sync on a stale cache.

**Schema version.** `data.db`'s `meta` bucket carries a `schema` key
(`store.SchemaVersion`, currently 1). There is **no upgrade path yet** — that is
its own piece of work — so the version does exactly one thing today: a build that
finds a version it does not know refuses to open the database (`ErrSchemaNewer`)
instead of misreading the one copy of this machine's history. A store written
before the key existed is stamped rather than refused: the layout never changed,
so an unversioned store *is* version 1. Recording it now is what makes a
migration possible later; without it a future one would have to infer the format
from structure. A refused open still releases the lock, and `yore record` never
opens the store at all, so commands keep spooling durably meanwhile.

**Being revoked** is its own state (`remote: revoked`), separate from
"unreachable" because retrying cannot fix it. On the first `device_revoked`
response the daemon records the fact, detaches and **deletes** `remote.db`, drops
the pull cursors, and stops syncing for the rest of the process; `yore sync` and
`yore status` say so instead of reporting success. Remote history already
decrypted into RAM is left for the rest of that session — the user is looking at
it, and it is gone at the next start, which finds an empty cache and can obtain
no key.

The record lives in **`data.db`'s meta bucket** (`revoked_by_server`), not a file
of its own: the daemon holds that database under its write lock, so the fact is
not a loose marker in the state directory inviting deletion. It makes the purge
survive a process killed mid-way, a restored backup, or a machine that comes back
up offline and can never be told again — a start that reads it deletes the cache
before opening anything.

It is a record of the last thing the server said, **not a permanent verdict**. A
revoked start is therefore not latched: it gets exactly one attempt, because
nothing in the enrollment path can reach into `data.db` (the daemon owns the
lock), so the only evidence that can retire a revocation is the server serving
this device again. Enroll the machine afresh and the next cycle clears it;
a still-revoked one is simply refused again and stops. Re-pointing at a different
server clears it too — that revocation described the old group. Editing the
*transport* to the same server (a certificate pin) does not: the group is
unchanged, so the revocation stands and no cache is re-opened for it. See
docs/protocol.md for why the server only reveals revocation to a caller whose
signature verified.

## The sync server

Single-tenant or multi-tenant, never a mix. Each tenant is an isolated bbolt file
owned solely by the server process; the identity that signed a request selects
the tenant every handler operates on.

- **Device → tenant.** The auth middleware finds the tenant holding the signing
  device's record (ids are globally-unique ULIDs, so at most one matches) and
  caches the mapping. Enrollment routes by token, recovery by the tenant that
  has recovery material. Bootstrap tokens are still compared constant-time against
  every configured token (no early break — a match leaks nothing about which or
  how many tenants exist) and binds that tenant's db into the request context. No
  match → `401`. A request that somehow reaches a handler with no bound tenant
  fails `500` rather than touch another tenant's data (fail-closed; no shared db).
- **Sharding — one mode or the other.** A server is configured **either** with a
  single token (`--token` / `$YORE_TOKEN` / `$YORE_TOKEN_FILE`), hosting one
  tenant whose db is `--db`, **or** with named tenants from `$YORE_TOKENS_FILE` (a
  JSON `{"name":"token", …}`) at `<dir(--db)>/tenants/<name>.db`. In the second
  mode nothing is created at `--db`; the path only roots `tenants/` and
  `backups/`. Both together is refused, and so is neither: the tokens file is not
  an addition to a mandatory "default" tenant that a multi-tenant operator never
  asked for, and a server with no configured token has no way in at all. Tenant
  names are `[A-Za-z0-9_-]+`; `default` is reserved because it names the backup
  directory of a single-token server's tenant. Duplicate tokens are rejected at
  startup (two tenants sharing a token would be indistinguishable).
- **Storage** is ciphertext + device public keys only; the server can never
  decrypt. **The client and wire protocol are unchanged by multi-tenancy.**
- **Per-tenant rolling backups.** With `$YORE_BACKUP_INTERVAL` > 0 (default 1h;
  `"0"` disables) a single goroutine writes a consistent online snapshot of every
  tenant db to `<dir(--db)>/backups/<tenant>/data-<unixMillis>.db` (temp file +
  atomic rename), pruning to the newest `$YORE_BACKUP_KEEP` (default 3).
- Exactly one server replica (bbolt is single-owner); TLS terminates at your
  reverse proxy. `GET /v1/health` is open and backs the container HEALTHCHECK.

## Durability & ops

- **Local rolling backups.** The daemon writes a consistent `store.BackupTo`
  snapshot of `data.db` to `~/.config/yore/backups/data-<unixMillis>.db` every
  `backup_interval` (default 1h; `"0"` disables), keeping the newest
  `backup_keep` (default 3) — same temp-file + atomic-rename + prune scheme as
  the server.
- **Bounded daemon log.** `~/.config/yore/daemon.log` rotates once it would
  exceed `log_max_size` (default 5MB; `"0"` = unbounded append), keeping
  `log_keep` old segments (default 1). `log_silent` (default **true**) suppresses
  logging entirely — no file is created — so a fresh install writes no
  `daemon.log`; set it `false` to get the rotating log for debugging. A log-open
  failure is non-fatal — the daemon runs without logging rather than refusing to
  start.
- **Warm snapshots** (`corpus.snap`) keep restarts instant; they are derived data,
  always rebuildable from `data.db`, so a bad snapshot just triggers a full load.

## Single-directory footprint

All client state lives under `~/.config/yore/` (or `$YORE_DIR`); uninstall is one
`rm -rf`. The one deliberate exception is secret material: when a usable OS
keyring exists, the device key is stored there instead of in the directory, so a
full uninstall also drops the `yore` keyring entries. `$YORE_SECRET_BACKEND=file`
forces everything back into the directory. The files:

| Path | What |
|---|---|
| `config.toml` | settings (0600) |
| `ui.toml` | pane-divider positions the browser remembers (0600) — deliberately **not** `config.toml`, which is the hand-edited settings file a TUI has no business rewriting every time a pane is dragged |
| `redact.yml` | editable, seeded secret-redaction rules (0600) |
| `data.db` | local bbolt store — this host's history only, plus a `meta` bucket for facts only the lock-holder may write (`host_id`, `hostname`, `last_uploaded_seq`, `revoked_by_server`, `schema`) |
| `device.key` | device X25519+Ed25519 identity — kept in the **OS keyring** when one is usable, else this file (0600; refused if group/other-readable) |
| `spool/<pid>.jsonl` | crash-safe capture handoff, drained by the daemon |
| `daemon.sock` | daemon control socket (0600) |
| `corpus.snap` | warm-start corpus gob snapshot (derived) |
| `remote.db` | other hosts' history as **ciphertext**, plus their pull cursors (derived; delete it and the next sync re-pulls) |
| `agent-prompts/` | one file per agent session holding the current prompt's **id** — the handoff between an agent's prompt hook and its tool hooks, which are separate processes |
| `daemon.log` | bounded daemon log (+ rotated segments) |
| `backups/` | rolling local `data.db` snapshots |

## Config (`~/.config/yore/config.toml`)

A plain [TOML](https://toml.io) file. Read and written by hand, or through
`yore get-config <key>` and `yore set-config <key> <value>` — the one
authoritative accessor pair (`config.Get`/`config.Set`). Nothing else parses the
file: the emitted shell integration, for instance, asks `yore get-config
enter_executes` at call time rather than grepping. Zero values mean "use
default"; accessors apply defaults so callers never branch.

Layout the browser *writes back* (dragged pane sizes) lives in a separate
`ui.toml`, not here — `config.Save` marshals the whole struct, and a TUI that
rewrote and reformatted the user's settings file every time a pane moved would
be a poor neighbour. Same directory, same 0600, different concern.

| Key | Default | Meaning |
|---|---|---|
| `server_url` | — | sync server base URL; empty = local-only |
| `token_file` | — | path to a file holding an enrollment token, for setups that manage it externally |
| `server_pin` | — | pinned server TLS SPKI (base64 SHA-256); set by `setup --pin` |
| `integration` | `takeover` | `takeover` \| `coexist` \| `capture` |
| `key_epoch` | `24h` | DEK epoch width |
| `daemon_idle` | `30m` | daemon idle timeout before exit |
| `sync_interval` | `5m` | periodic push/pull tick |
| `push_debounce` | off | **EXPERIMENTAL** coalesced push-on-record delay; empty/`0`/invalid = disabled |
| `sync_prompts` | `true` | upload agent prompt records; `false` keeps prompt text on the machine that recorded it (their commands still sync). Not retroactive — prompts already pushed stay on the server |
| `remote_keep` | `50000` | records cached and held in RAM per *other* host, newest first; a negative value means unlimited |
| `auto_deepen` | `true` | let deep reads nudge a background sync |
| `enter_executes` | `true` | Ctrl-R Enter runs the result (vs. insert for review) |
| `bind_up_arrow` | `false` | also bind Up to the search TUI |
| `hide_agent_commands` | `true` | keep agent-run commands out of the interactive search UIs (`A` in the browser, `⌥a` in the Ctrl-R panel, per session; both always say how many rows they are holding). `--headless` never hides anything |
| `keymap` | `emacs` | `emacs` \| `vim` TUI key style |
| `ignore_patterns` | — | user regexes whose matching commands are **dropped** (not redacted — this is "never record this", unlike a secret rule, which only costs the command its credential) |
| `ignore_dirs` | — | cwd prefixes whose commands are never recorded |
| `record_space_prefixed` | `false` | record leading-space commands too |
| `auto_tags` | — | cwd-prefix → tag rules (`/work=refactor,…`); tags matching commands at query time, and listed by `yore tag list` |
| `capture_spool_only` | `false` | capture writes to the spool only — never pokes/spawns the daemon; the spool is drained the next time a daemon runs |
| `backup_interval` | `1h` | local db backup cadence; `"0"` disables |
| `backup_keep` | `3` | local db backups retained |
| `log_max_size` | `5MB` | daemon.log rotation threshold; `"0"` = unbounded append |
| `log_keep` | `1` | rotated daemon.log segments kept |
| `log_silent` | `true` | suppress daemon logging entirely (no `daemon.log`); set `false` to log for debugging |

Defaults are applied the plain-Go way: `config.Load` starts from `config.Defaults()`
and decodes the TOML file over it, so an omitted key keeps its default and an
explicit value — including `false` or `0` — overrides it. Booleans that default to
`true` (`auto_deepen`, `enter_executes`, `log_silent`, `hide_agent_commands`) are written without
`omitempty` so an explicit `false` round-trips; there are no `*bool` "was it set?"
fields. String-backed durations/sizes are stored verbatim and parsed by typed
accessors that fall back to the default on a malformed value.

Server-side settings are env vars, not config.toml: `$YORE_TOKEN` /
`$YORE_TOKEN_FILE` **or** `$YORE_TOKENS_FILE` (mutually exclusive — see the sync
server above), `$YORE_BACKUP_INTERVAL`, `$YORE_BACKUP_KEEP`.

## Invariants (do not break)

- The prompt path never does DB, network, or crypto work; recording survives the
  daemon being down (the spool is drained at next start).
- `data.db` holds only this host's history. Other hosts' **plaintext** exists
  only in daemon RAM and is never written to disk; the ciphertext it came from
  is cached in `remote.db`, which is exactly what the server holds and is
  unreadable without this device's keys.
- Streams are append-only, per-host, client-sequenced; merge is a ULID set union;
  deletes are tombstones — eventually consistent, conflict-free.
- Every hot path (search, decrypt, enroll, revoke) is **O(1) in history age** —
  including daemon startup, which resumes from a persisted pull cursor rather
  than re-fetching every machine's archive, and is bounded by `remote_keep`
  rather than by how much history the group has ever accumulated.
- Prompt text is stored once, on its own record. Nothing copies it onto the
  commands a prompt caused; the daemon rejoins them at query time.
- The server only ever holds ciphertext, wrapped keys, and device public keys;
  mutating requests are per-device signed; tenants never share a db.
- All client state lives under `~/.config/yore/`; uninstall is one `rm -rf`.
</content>
</invoke>
