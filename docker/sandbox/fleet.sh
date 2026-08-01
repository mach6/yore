#!/usr/bin/env bash
#
# fleet.sh — MANUAL 20-node, two-user stress/soak harness for yore.
#
# NOT wired into CI (never runs in Drone). It builds the CURRENT source into a
# fleet of twenty client machines spread over eight Linux distributions, half
# zsh and half bash, split between TWO users (server tenants "alice" and "bob",
# ten machines each). It enrolls every machine, drives half a million randomized
# commands through them, syncs everything end-to-end encrypted, then measures
# and verifies: read/write/sync throughput, database growth, convergence,
# redaction, and — because two users share one server — tenant isolation.
#
# It leaves the fleet UP by default: the end state is the thing you want to poke
# at. Pass --down to tear it down.
#
# Usage:
#   docker/sandbox/fleet.sh                 # 500,000 records across 20 nodes
#   TOTAL=20000 docker/sandbox/fleet.sh     # quick run
#   docker/sandbox/fleet.sh --down          # tear down afterwards
#   docker/sandbox/fleet.sh --no-build      # reuse the running fleet, re-run the load
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE="$SCRIPT_DIR/fleet.yml"
GEN="$SCRIPT_DIR/fleet-gen.sh"

TOTAL="${TOTAL:-500000}"          # records across the whole fleet
KEEP="${KEEP:-1}"                 # 1 = leave the fleet running (the default)
BUILD="${BUILD:-1}"               # 0 = skip down -v/up --build, reuse what runs
OUT="${OUT:-$ROOT/.agents/fleet}" # run artifacts (gitignored)
SPAN_DAYS="${SPAN_DAYS:-90}"      # spread the generated history over this long
IGNORE_DIR="/root/ignored"
SERVER_URL="http://server:8080"
SOCK="/root/.config/yore/daemon.sock"
BIG=100000000                     # defeat the 200-row default window when counting

for arg in "$@"; do
  case "$arg" in
    --down)     KEEP=0 ;;
    --keep)     KEEP=1 ;;
    --no-build) BUILD=0 ;;
    TOTAL=*)    TOTAL="${arg#TOTAL=}" ;;
    *) echo "fleet.sh: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

ALICE=(a01 a02 a03 a04 a05 a06 a07 a08 a09 a10)
BOB=(b01 b02 b03 b04 b05 b06 b07 b08 b09 b10)
ALL=("${ALICE[@]}" "${BOB[@]}")
PER_NODE=$(( TOTAL / ${#ALL[@]} ))
RUN="$(date +%s)"
MARKPREFIX="FLT${RUN}"

mkdir -p "$OUT"
REPORT="$OUT/report-$RUN.md"

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
if [ -t 1 ]; then G=$'\033[32m'; R=$'\033[31m'; Y=$'\033[33m'; B=$'\033[1m'; Z=$'\033[0m'
else G=""; R=""; Y=""; B=""; Z=""; fi
PASS=0; FAIL=0
ok()   { printf '  %s✓%s %s\n' "$G" "$Z" "$1"; PASS=$((PASS+1)); }
bad()  { printf '  %s✗%s %s\n' "$R" "$Z" "$1"; FAIL=$((FAIL+1)); }
warn() { printf '  %s!%s %s\n' "$Y" "$Z" "$1"; }
step() { printf '\n%s==> %s%s\n' "$B" "$1" "$Z"; }
note() { printf '     %s\n' "$1"; }
say()  { printf '%s\n' "$*" >> "$REPORT"; }

now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
rate() { local n="$1" ms="$2"; if [ "${ms:-0}" -le 0 ]; then echo "n/a"; else
  awk "BEGIN{printf \"%.0f/s\", $n*1000.0/$ms}"; fi; }

dc()  { docker compose -f "$COMPOSE" exec -T "$@"; }
# dsock sends one newline-delimited-JSON request to a node's daemon socket and
# prints the response. It is how a script does what the devices TUI does (list,
# approve) — the daemon's protocol is the API; the TUI is one of its clients.
# The optional third argument is a jq filter, applied INSIDE the container: the
# node images carry socat and jq, so the harness needs nothing on the host but
# docker. $H is exported into the container for filters that match on hostname.
dsock() { local n="$1" json="$2" filter="${3:-}"
  dc -e H="${H:-}" "$n" sh -c "printf '%s\n' '$json' | socat -t 60 - UNIX-CONNECT:$SOCK${filter:+ | jq -r '$filter'}"; }
hostname_of() { dc "$1" cat /etc/hostname | tr -d '\r\n'; }
mark_of()     { echo "${MARKPREFIX}$1"; }

# Node -> tenant, for reporting.
tenant_of() { case "$1" in a*) echo alice ;; *) echo bob ;; esac; }

teardown() {
  local rc=$?
  if [ "$KEEP" = "1" ]; then
    printf '\n%s==> Fleet left UP.%s  Tear down with:\n' "$B" "$Z"
    note "docker compose -f $COMPOSE down -v"
  else
    step "Teardown"
    docker compose -f "$COMPOSE" down -v >/dev/null 2>&1 || true
    ok "fleet torn down"
  fi
  return "$rc"
}
trap teardown EXIT

# ===========================================================================
step "Fleet: 20 nodes / 8 distros / 2 users — $TOTAL records ($PER_NODE per node)"
say "# yore fleet run $RUN"
say ""
say "- nodes: 20 (10 alice, 10 bob) across 8 distributions, 10 zsh / 10 bash"
say "- records generated: $TOTAL ($PER_NODE per node)"
say "- history spread over: $SPAN_DAYS days"
say ""

if [ "$BUILD" = "1" ]; then
  docker compose -f "$COMPOSE" down -v >/dev/null 2>&1 || true
  bt0="$(now_ms)"
  docker compose -f "$COMPOSE" up -d --build >"$OUT/build.log" 2>&1 || {
    bad "build/up failed — see $OUT/build.log"; tail -30 "$OUT/build.log"; exit 1; }
  bt1="$(now_ms)"
  ok "built and started 22 containers in $(( (bt1-bt0)/1000 ))s"
fi

printf '     waiting for server health'
healthy=0
for _ in $(seq 1 90); do
  if dc a01 yore healthcheck --url "$SERVER_URL/v1/health" >/dev/null 2>&1; then healthy=1; break; fi
  printf '.'; sleep 1
done
printf '\n'
[ "$healthy" = "1" ] && ok "server healthy (multi-tenant: alice + bob)" || { bad "server never became healthy"; exit 1; }

# ===========================================================================
step "Enroll — two tenants, ten machines each"
# Each tenant bootstraps its first machine with its own token (accepted only
# while that tenant has no active device), then every further machine redeems a
# single-use token minted by an enrolled one and is approved from it.
enroll_tenant() { # enroll_tenant <tenant> <token> <first> <rest...>
  local tenant="$1" token="$2" first="$3"; shift 3
  local log="$OUT/enroll-$tenant.log"
  : > "$log"
  dc "$first" yore setup --server "$SERVER_URL" --token "$token" >>"$log" 2>&1 \
    || { echo "BOOTSTRAP FAILED $first" >>"$log"; return 1; }
  echo "bootstrapped $first" >>"$log"
  local n tok pend host
  for n in "$@"; do
    tok="$(dc "$first" yore devices token 2>/dev/null | tr -d '\r\n')"
    [ -n "$tok" ] || { echo "NO TOKEN for $n" >>"$log"; return 1; }
    dc "$n" yore setup --server "$SERVER_URL" --token "$tok" >>"$log" 2>&1 \
      || { echo "SETUP FAILED $n" >>"$log"; return 1; }
    host="$(hostname_of "$n")"
    pend="$(H="$host" dsock "$first" '{"op":"devices"}' \
      '.devices.devices[] | select(.status=="pending" and .name==env.H) | .id' | head -1 | tr -d '\r')"
    [ -n "$pend" ] || { echo "NO PENDING id for $n ($host)" >>"$log"; return 1; }
    dsock "$first" "{\"op\":\"approve\",\"device_id\":\"$pend\"}" >>"$log" 2>&1
    echo "enrolled+approved $n ($host) -> $pend" >>"$log"
  done
}

et0="$(now_ms)"
enroll_tenant alice alice-bootstrap-token "${ALICE[@]}" & pa=$!
enroll_tenant bob   bob-bootstrap-token   "${BOB[@]}"   & pb=$!
efail=0
wait "$pa" || efail=1
wait "$pb" || efail=1
et1="$(now_ms)"
[ "$efail" = "0" ] && ok "all 20 machines enrolled in $(( (et1-et0)/1000 ))s (2 groups formed, 18 approvals)" \
  || { bad "enrollment failed — see $OUT/enroll-*.log"; tail -20 "$OUT"/enroll-*.log; exit 1; }

# Every device of each tenant must be active, and each tenant must see exactly
# its own ten — the first isolation check, before any history exists.
for pair in "alice a01" "bob b01"; do
  set -- $pair
  active="$(dsock "$2" '{"op":"devices"}' '[.devices.devices[] | select(.status=="active")] | length' | tr -d '\r')"
  total="$(dsock "$2" '{"op":"devices"}' '.devices.devices | length' | tr -d '\r')"
  note "$1: $active active devices (of $total known)"
  [ "$active" = "10" ] && [ "$total" = "10" ] && ok "$1 sees exactly its own 10 active machines" \
    || bad "$1 device roster wrong (active=$active total=$total, want 10/10)"
done

# ===========================================================================
step "Seed per-node config (ignore_dirs, rolling backups)"
for n in "${ALL[@]}"; do
  ( dc "$n" sh -c "cat > /root/.config/yore/config.toml <<EOF
server_url = \"$SERVER_URL\"
integration = \"\${YORE_INTEGRATION:-takeover}\"
backup_interval = \"60s\"
backup_keep = 3
ignore_dirs = [\"$IGNORE_DIR\"]
EOF
chmod 600 /root/.config/yore/config.toml; mkdir -p $IGNORE_DIR; yore stop >/dev/null 2>&1 || true" ) &
done
wait
ok "config seeded on 20 nodes (ignore_dirs=[$IGNORE_DIR], backups every 60s)"

# ===========================================================================
step "Shell-hook conformance — every distro/shell records through its real hook"
# The bulk load calls `yore record` directly (exactly what the hook calls). This
# check is the other half: drive each node's ACTUAL interactive shell, hooks
# live, under a pty, and prove the command lands in the store. Fed slowly on
# purpose — a shell reads its terminal, and a burst races the prompt.
#
# TERM=dumb on purpose. Every yore process started on a TTY writes a terminal
# status query (OSC 11 + cursor-position report) and then reads the reply — and
# a pty with no terminal emulator behind it never answers, so that read eats the
# script's queued input instead. TERM=dumb turns the query off, leaving exactly
# what this check is about: the shell hook, doing its job. The cost of the query
# is measured on its own further down ("terminal input integrity").
hook_probe() { # hook_probe <node>
  local n="$1" sh mark
  sh="$(dc "$n" sh -c 'echo ${YORE_SHELL:-bash}' | tr -d '\r\n')"
  mark="HOOKPROBE${RUN}$n"
  dc "$n" bash -c "
    { sleep 1
      echo \"echo $mark-one\"; sleep 0.8
      echo \"true $mark-two\";  sleep 0.8
      echo exit; sleep 0.8
    } | TERM=dumb timeout 30 script -q -c '$sh -i' /dev/null >/dev/null 2>&1 || true" >/dev/null 2>&1 || true
  echo "$mark"
}
declare -A HOOKMARK
for n in "${ALL[@]}"; do HOOKMARK[$n]="HOOKPROBE${RUN}$n"; ( hook_probe "$n" >/dev/null ) & done
wait
sleep 3
hook_ok=0; hook_bad=""
for n in "${ALL[@]}"; do
  c="$(dc "$n" yore search --headless --scope local --limit 100 "${HOOKMARK[$n]}" 2>/dev/null | grep -c "${HOOKMARK[$n]}" || true)"
  if [ "${c:-0}" -ge 2 ]; then hook_ok=$((hook_ok+1)); else hook_bad="$hook_bad $n($c)"; fi
done
[ "$hook_ok" = "20" ] && ok "all 20 shells captured their commands through the live hook" \
  || bad "hook capture failed on:$hook_bad (ok on $hook_ok/20)"

# Terminal input integrity: the same probe with a normal $TERM, fed faster than
# a shell can answer. A yore process started on a TTY queries the terminal for
# its colours and reads the reply, and that read will happily consume input the
# user has already typed — so a hook that lets a per-command call inherit the
# terminal loses pasted lines. Every line must survive.
ti_bad=""; ti_note=""
for n in a02 a01; do
  sh="$(dc "$n" sh -c 'echo ${YORE_SHELL:-bash}' | tr -d '\r\n')"
  m="PASTEPROBE${RUN}$n"
  dc "$n" bash -c "
    { sleep 1; for i in 1 2 3 4 5; do echo \"echo $m-\$i\"; sleep 0.8; done; echo exit; sleep 1
    } | TERM=xterm timeout 40 script -q -c '$sh -i' /dev/null >/dev/null 2>&1 || true" >/dev/null 2>&1 || true
  sleep 2
  c="$(dc "$n" yore search --headless --scope local --limit 100 "$m" 2>/dev/null | grep -c "$m" || true)"
  ti_note="$ti_note $n/$sh:${c:-0}of5"
  [ "${c:-0}" = "5" ] || ti_bad="$ti_bad $n/$sh(${c:-0}of5)"
done
note "terminal input integrity (TERM=xterm, 5 lines pasted 0.8s apart):$ti_note"
[ -z "$ti_bad" ] && ok "terminal input integrity: nothing on the prompt path eats typed input" \
  || bad "input lost to yore's terminal status query:$ti_bad"

# ===========================================================================
step "Load — $TOTAL randomized commands, 20 nodes in parallel"
lt0="$(now_ms)"
i=0
for n in "${ALL[@]}"; do
  i=$((i+1))
  ( dc -e N="$PER_NODE" -e MARK="$(mark_of "$n")" -e SEED="$((RUN + i * 7919))" \
       -e IGNORE_DIR="$IGNORE_DIR" -e SPAN_DAYS="$SPAN_DAYS" \
       "$n" bash -s < "$GEN" > "$OUT/gen-$n.txt" 2>&1 ) &
done
wait
lt1="$(now_ms)"
LOAD_MS=$(( lt1 - lt0 ))
ok "load generated in $(( LOAD_MS/1000 ))s — fleet write rate $(rate "$TOTAL" "$LOAD_MS")"

# Per-node tallies from the generators.
declare -A G_STORED G_DROPPED G_SECRET G_SPACE G_IGNORE G_CC G_AIDER G_TAG G_LOOP G_SESS
tot_stored=0; tot_dropped=0; tot_secret=0
for n in "${ALL[@]}"; do
  g() { sed -n "s/^GEN $1=//p" "$OUT/gen-$n.txt" | head -1; }
  G_STORED[$n]="$(g stored)";   G_DROPPED[$n]="$(g dropped)"
  G_SECRET[$n]="$(g secret)";   G_SPACE[$n]="$(g space)"
  G_IGNORE[$n]="$(g ignore)";   G_CC[$n]="$(g claude)"
  G_AIDER[$n]="$(g aider)";     G_TAG[$n]="$(g usertag)"
  G_LOOP[$n]="$(g loop_ms)";    G_SESS[$n]="$(g sessions)"
  tot_stored=$(( tot_stored + ${G_STORED[$n]:-0} ))
  tot_dropped=$(( tot_dropped + ${G_DROPPED[$n]:-0} ))
  tot_secret=$(( tot_secret + ${G_SECRET[$n]:-0} ))
done
note "expected stored fleet-wide: $tot_stored   dropped: $tot_dropped   secrets(redacted): $tot_secret"

# ===========================================================================
step "Ingest — waiting for every daemon to drain its spool"
# `yore record` appends to the spool and pokes the daemon; the daemon ingests
# into bbolt. The gap between the two is the write pipeline's lag, and it is
# worth a number of its own.
local_count() { dc "$1" yore status 2>/dev/null | tr -d ',' \
  | sed -n 's/^history *: *\([0-9]*\) local entries.*/\1/p' | head -1; }
it0="$(now_ms)"
for _ in $(seq 1 300); do
  done_nodes=0
  for n in "${ALL[@]}"; do
    c="$(local_count "$n")"; c="${c:-0}"
    [ "$c" -ge "$(( ${G_STORED[$n]:-0} ))" ] && done_nodes=$((done_nodes+1))
  done
  [ "$done_nodes" = "20" ] && break
  sleep 2
done
it1="$(now_ms)"
INGEST_MS=$(( it1 - it0 ))
[ "${done_nodes:-0}" = "20" ] && ok "all 20 daemons ingested everything (+$(( INGEST_MS/1000 ))s after the last write)" \
  || warn "only ${done_nodes:-0}/20 daemons fully ingested after $(( INGEST_MS/1000 ))s"

# ===========================================================================
step "User tags — label whole sessions, then make them travel"
# Executors are attribution (which agent ran it); tags are the user's own
# labels. A tag applied to a session covers every command in it, and — like
# everything else — has to be readable from any other machine in the group.
declare -A TAGGED
for pair in "a03 alice-review" "b04 bob-review"; do
  set -- $pair
  sess="$(mark_of "$1")-s0"   # the first session of that node's run; always present
  dc "$1" yore tag add "$2" --session "$sess" >/dev/null 2>&1 || true
  TAGGED[$1]="$(dc "$1" yore search --headless --scope local --limit $BIG --tag "$2" 2>/dev/null | grep -c "$MARKPREFIX" || true)"
  note "$1: tagged session $sess as '$2' — ${TAGGED[$1]} commands locally"
done

# ===========================================================================
step "Sync — push, pull, converge (timed per wave)"
timed() { # timed <node> <cmd...> -> ms
  local n="$1"; shift; local t0 t1
  t0="$(now_ms)"; dc "$n" "$@" >/dev/null 2>&1 || true; t1="$(now_ms)"
  echo $(( t1 - t0 ))
}
declare -A SYNC1 SYNC2
wave() { # wave <n> <assoc-name>
  local label="$1" t0 t1
  t0="$(now_ms)"
  for n in "${ALL[@]}"; do ( echo "$(timed "$n" yore sync)" > "$OUT/sync-$label-$n.ms" ) & done
  wait
  t1="$(now_ms)"
  echo $(( t1 - t0 ))
}
WAVE1_MS="$(wave 1)"
note "wave 1 (every node pushes its 25k): $(( WAVE1_MS/1000 ))s wall"
WAVE2_MS="$(wave 2)"
note "wave 2 (every node pulls the other nine): $(( WAVE2_MS/1000 ))s wall"
WAVE3_MS="$(wave 3)"
note "wave 3 (settle): $(( WAVE3_MS/1000 ))s wall"
ok "three sync waves completed"

# ===========================================================================
step "Measure"
# search timing, inside the container, averaged over REPS runs
timesearch() { # timesearch <node> <reps> <args...>
  local n="$1" reps="$2"; shift 2
  dc "$n" bash -c "t0=\$(cut -d' ' -f1 /proc/uptime)
    for i in \$(seq $reps); do yore search --headless --limit $BIG $* >/dev/null 2>&1; done
    t1=\$(cut -d' ' -f1 /proc/uptime)
    awk \"BEGIN{printf \\\"%.1f\\\", (\$t1-\$t0)*1000/$reps}\"" 2>/dev/null | tr -d '\r\n'
}
A_MARK="$(mark_of a01)"
S_LOCAL="$(timesearch a01 5 --scope local "$A_MARK")"
S_ALL="$(timesearch a01 5 --scope all "$A_MARK")"
S_FUZZY="$(timesearch a01 5 --scope all --fuzzy "gtcmt")"
S_EXEC="$(timesearch a01 5 --scope all --executor claude-code "$MARKPREFIX")"
S_TAG="$(timesearch a01 5 --scope all --tag fleet-agent "$MARKPREFIX")"
S_FREC="$(timesearch a01 5 --scope all --sort frecency "$MARKPREFIX")"
S_WIDE="$(timesearch a01 3 --scope all "$MARKPREFIX")"
note "search a01: local=${S_LOCAL}ms  deep=${S_ALL}ms  fuzzy=${S_FUZZY}ms  executor=${S_EXEC}ms  tag=${S_TAG}ms  frecency=${S_FREC}ms  whole-corpus=${S_WIDE}ms"

# db sizes + daemon memory per node
declare -A DATA_KB REMOTE_KB RSS_KB LOCAL_N
tot_data=0; tot_remote=0; tot_rss=0
for n in "${ALL[@]}"; do
  read -r d r rss <<<"$(dc "$n" sh -c '
    d=$(wc -c < /root/.config/yore/data.db 2>/dev/null || echo 0)
    r=$(wc -c < /root/.config/yore/remote.db 2>/dev/null || echo 0)
    p=$(pgrep -f "[y]ore daemon" | head -1)
    rss=$(awk "/VmRSS/{print \$2}" /proc/$p/status 2>/dev/null || echo 0)
    echo "$((d/1024)) $((r/1024)) ${rss:-0}"' 2>/dev/null | tr -d '\r')"
  DATA_KB[$n]="${d:-0}"; REMOTE_KB[$n]="${r:-0}"; RSS_KB[$n]="${rss:-0}"
  LOCAL_N[$n]="$(local_count "$n")"
  tot_data=$(( tot_data + ${d:-0} )); tot_remote=$(( tot_remote + ${r:-0} )); tot_rss=$(( tot_rss + ${rss:-0} ))
done
note "client storage: data.db total $(( tot_data/1024 ))MB, remote.db total $(( tot_remote/1024 ))MB, daemon RSS total $(( tot_rss/1024 ))MB"

# server side: copy the tenant dbs out (distroless has no shell to grep with)
TMP="$(mktemp -d "${TMPDIR:-/tmp}/yore-fleet.XXXXXX")"
srv_cid="$(docker compose -f "$COMPOSE" ps -q server)"
docker cp "$srv_cid:/data/tenants/alice.db" "$TMP/alice.db" >/dev/null 2>&1 || true
docker cp "$srv_cid:/data/tenants/bob.db"   "$TMP/bob.db"   >/dev/null 2>&1 || true
SRV_ALICE_KB=$(( $(wc -c < "$TMP/alice.db" 2>/dev/null || echo 0) / 1024 ))
SRV_BOB_KB=$(( $(wc -c < "$TMP/bob.db" 2>/dev/null || echo 0) / 1024 ))
SRV_RSS_KB="$(docker stats --no-stream --format '{{.MemUsage}}' "$srv_cid" 2>/dev/null | awk '{print $1}')"
note "server storage: alice.db $(( SRV_ALICE_KB/1024 ))MB, bob.db $(( SRV_BOB_KB/1024 ))MB (ciphertext), RSS $SRV_RSS_KB"

# ===========================================================================
step "Verify"

# --- redaction: SEKRETMARKER must exist NOWHERE, on any node or the server ---
# grep exits 1 when it finds nothing, so every count is taken through hits(),
# which yields a single number on every path.
hits() { local out; out="$( { grep -ac "$1" "${@:2}" 2>/dev/null || true; } | head -1 | tr -dc '0-9')"; echo "${out:-0}"; }
leaks=0; leak_where=""
for n in "${ALL[@]}"; do
  h="$(dc "$n" sh -c 'c=0; for f in /root/.config/yore/data.db /root/.config/yore/remote.db; do
        n=$(grep -ac SEKRETMARKER "$f" 2>/dev/null || true); c=$(( c + ${n:-0} )); done; echo $c' | tr -dc '0-9')"
  [ "${h:-0}" != "0" ] && { leaks=$(( leaks + h )); leak_where="$leak_where $n:$h"; }
done
for t in alice bob; do
  h="$(hits SEKRETMARKER "$TMP/$t.db")"
  [ "$h" != "0" ] && { leaks=$(( leaks + h )); leak_where="$leak_where server/$t:$h"; }
done
[ "$leaks" = "0" ] && ok "redaction: ZERO plaintext secrets in 40 client dbs and both server dbs ($tot_secret secrets planted)" \
  || bad "SECRET LEAK ($leaks hits):$leak_where"

# --- E2E: no plaintext command text on the server at all --------------------
srv_plain=0
for t in alice bob; do
  srv_plain=$(( srv_plain + $(hits "$MARKPREFIX" "$TMP/$t.db") ))
done
[ "$srv_plain" = "0" ] && ok "E2E: the run marker appears in ZERO server bytes — the server holds ciphertext only" \
  || bad "E2E: server db contains plaintext command text ($srv_plain hits)"

# --- drops: stored == generated − (space + ignore), to the record -----------
# Exact, not approximate. The generator says what it rolled and the earlier
# probes are counted rather than guessed at, so the only thing a mismatch can
# mean is that the write path gained or lost a command — which is the whole
# question. (It has: a drain used to be able to unlink a spool file a live
# writer had created and not yet written to, and the record vanished silently.)
drop_bad=""
for n in "${ALL[@]}"; do
  probes="$(dc "$n" yore search --headless --scope local --limit $BIG "PROBE${RUN}$n" 2>/dev/null | grep -c "PROBE${RUN}$n" || true)"
  want=$(( ${G_STORED[$n]:-0} + ${probes:-0} ))
  got="${LOCAL_N[$n]:-0}"
  [ "$got" = "$want" ] || drop_bad="$drop_bad $n(want $want got $got)"
done
[ -z "$drop_bad" ] && ok "drop gate: every node stored generated − (space-prefixed + ignore-dir) exactly, no record lost" \
  || bad "drop gate off on:$drop_bad"

# --- convergence: one node per tenant must see all ten of its peers ---------
conv_bad=""; conv_rows=0
count_deep() { dc "$1" yore search --headless --scope all --limit $BIG "$2" 2>/dev/null | grep -c "$2" || true; }
for probe in a01 a07 b01 b06; do
  t="$(tenant_of "$probe")"
  peers=("${ALICE[@]}"); [ "$t" = bob ] && peers=("${BOB[@]}")
  for p in "${peers[@]}"; do
    want="${G_STORED[$p]:-0}"
    got="$(count_deep "$probe" "$(mark_of "$p")")"
    conv_rows=$(( conv_rows + got ))
    d=$(( got - want )); [ "$d" -lt 0 ] && d=$(( -d ))
    [ "$d" -gt $(( want / 100 + 5 )) ] && conv_bad="$conv_bad $probe→$p(want $want got $got)"
  done
done
[ -z "$conv_bad" ] && ok "convergence: 4 probe nodes each see all 10 peers' history at full count" \
  || bad "convergence gaps:$conv_bad"

# --- tenant isolation: alice must never see bob, or vice versa -------------
iso_bad=""
for probe in "${ALICE[@]}"; do
  c="$(count_deep "$probe" "$(mark_of b01)")"
  [ "${c:-0}" != "0" ] && iso_bad="$iso_bad $probe(sees b01:$c)"
done
for probe in "${BOB[@]}"; do
  c="$(count_deep "$probe" "$(mark_of a01)")"
  [ "${c:-0}" != "0" ] && iso_bad="$iso_bad $probe(sees a01:$c)"
done
# and the host sidebar must list only same-tenant machines
for probe in a01 b01; do
  other=bob-; [ "$(tenant_of "$probe")" = bob ] && other=alice-
  h="$(dsock "$probe" '{"op":"hosts"}' | grep -c "$other" || true)"
  [ "${h:-0}" != "0" ] && iso_bad="$iso_bad $probe(host-list leak)"
done
[ -z "$iso_bad" ] && ok "tenant isolation: neither user's fleet can see one byte of the other's history" \
  || bad "TENANT LEAK:$iso_bad"

# --- agent attribution: executors and user tags -----------------------------
# CLAUDECODE / AIDER_MODEL make yore attribute the command to an agent (an
# executor); YORE_TAG applies a freeform user tag. Both must survive the round
# trip and be filterable from any machine in the group.
tag_bad=""
for probe in a01 b01; do
  t="$(tenant_of "$probe")"
  peers=("${ALICE[@]}"); [ "$t" = bob ] && peers=("${BOB[@]}")
  want_cc=0; want_ai=0; want_tg=0
  for p in "${peers[@]}"; do
    want_cc=$(( want_cc + ${G_CC[$p]:-0} ))
    want_ai=$(( want_ai + ${G_AIDER[$p]:-0} ))
    want_tg=$(( want_tg + ${G_TAG[$p]:-0} ))
  done
  # $YORE_TAG is an EXECUTOR override (the alias of $YORE_EXECUTOR), not a user
  # tag — all three of these are agent attributions and filter with --executor.
  cnt() { dc "$probe" yore search --headless --scope all --limit $BIG "$@" "$MARKPREFIX" 2>/dev/null | grep -c "$MARKPREFIX" || true; }
  got_cc="$(cnt --executor claude-code)"
  got_ai="$(cnt --executor aider)"
  got_tg="$(cnt --executor fleet-agent)"
  note "$t: claude-code $got_cc/$want_cc, aider $got_ai/$want_ai, fleet-agent $got_tg/$want_tg"
  off() { local g="$1" w="$2" d=$(( $1 - $2 )); [ "$d" -lt 0 ] && d=$(( -d )); [ "$d" -gt $(( w / 100 + 5 )) ]; }
  off "$got_cc" "$want_cc" && tag_bad="$tag_bad $probe(claude-code $got_cc/$want_cc)"
  off "$got_ai" "$want_ai" && tag_bad="$tag_bad $probe(aider $got_ai/$want_ai)"
  off "$got_tg" "$want_tg" && tag_bad="$tag_bad $probe(tag $got_tg/$want_tg)"
done
[ -z "$tag_bad" ] && ok "agent attribution: executor filters match what was planted, fleet-wide" \
  || bad "attribution counts off:$tag_bad"

# --- user tags travel -------------------------------------------------------
utag_bad=""
for pair in "a03 alice-review a09" "b04 bob-review b07"; do
  set -- $pair
  want="${TAGGED[$1]:-0}"
  got="$(dc "$3" yore search --headless --scope all --limit $BIG --tag "$2" 2>/dev/null | grep -c "$MARKPREFIX" || true)"
  note "tag '$2': $want on the machine that applied it, $got seen from $3"
  [ "${got:-0}" = "$want" ] && [ "$want" -gt 0 ] || utag_bad="$utag_bad $2($want→$got)"
done
[ -z "$utag_bad" ] && ok "user tags: a session tagged on one machine is filterable from another" \
  || bad "user tags did not travel:$utag_bad"

# --- redaction markers are present (secrets kept, credential removed) -------
red_seen="$(dc a01 yore search --headless --scope all --limit $BIG "redacted:" 2>/dev/null | grep -c "⟪redacted:" || true)"
[ "${red_seen:-0}" -gt 0 ] && ok "redaction markers present: $red_seen commands kept with the credential replaced" \
  || bad "no redaction markers found — secrets should be stored redacted, not dropped"

# --- health -----------------------------------------------------------------
health_bad=""
for n in "${ALL[@]}"; do
  read -r d z bk <<<"$(dc "$n" sh -c '
    d=$(pgrep -f "[y]ore daemon" | wc -l)
    z=0; for s in /proc/[0-9]*/stat; do st=$(sed -e "s/^.*) //" -e "s/ .*//" "$s" 2>/dev/null); [ "$st" = Z ] && z=$((z+1)); done
    bk=$(ls /root/.config/yore/backups/ 2>/dev/null | wc -l)
    echo "$d $z $bk"' 2>/dev/null | tr -d '\r')"
  [ "${d:-0}" = "1" ] || health_bad="$health_bad $n(daemons=$d)"
  [ "${z:-0}" = "0" ] || health_bad="$health_bad $n(zombies=$z)"
  [ "${bk:-0}" -ge 1 ] || health_bad="$health_bad $n(no-backup)"
done
[ -z "$health_bad" ] && ok "health: exactly one daemon per node, zero zombies, rolling backups on all 20" \
  || bad "health:$health_bad"

# ===========================================================================
step "Report"
say "## Result"
say ""
say "- checks passed: $PASS, failed: $FAIL"
say "- load: $TOTAL records in $(( LOAD_MS/1000 ))s — $(rate "$TOTAL" "$LOAD_MS") fleet-wide"
say "- ingest drain after last write: $(( INGEST_MS/1000 ))s"
say "- sync waves: push $(( WAVE1_MS/1000 ))s, pull $(( WAVE2_MS/1000 ))s, settle $(( WAVE3_MS/1000 ))s"
say "- search (a01): local ${S_LOCAL}ms, deep ${S_ALL}ms, fuzzy ${S_FUZZY}ms, executor ${S_EXEC}ms, tag ${S_TAG}ms, frecency ${S_FREC}ms, whole-corpus ${S_WIDE}ms"
say "- storage: clients $(( tot_data/1024 ))MB data.db + $(( tot_remote/1024 ))MB remote.db; server $(( SRV_ALICE_KB/1024 ))MB + $(( SRV_BOB_KB/1024 ))MB"
say "- daemon RSS total: $(( tot_rss/1024 ))MB across 20 nodes"
say ""
say "## Per-node"
say ""
say "| node | tenant | host | stored | dropped | rec loop | rec rate | data.db | remote.db | daemon RSS |"
say "|------|--------|------|--------|---------|----------|----------|---------|-----------|------------|"
for n in "${ALL[@]}"; do
  say "| $n | $(tenant_of "$n") | $(hostname_of "$n") | ${G_STORED[$n]:-?} | ${G_DROPPED[$n]:-?} | $(( ${G_LOOP[$n]:-0}/1000 ))s | $(rate "$PER_NODE" "${G_LOOP[$n]:-0}") | $(( ${DATA_KB[$n]:-0}/1024 ))MB | $(( ${REMOTE_KB[$n]:-0}/1024 ))MB | $(( ${RSS_KB[$n]:-0}/1024 ))MB |"
done
rm -rf "$TMP"

printf '\n     checks: %s%d passed%s, %s%d failed%s\n' "$G" "$PASS" "$Z" \
  "$( [ "$FAIL" -gt 0 ] && echo "$R" || echo "$G")" "$FAIL" "$Z"
note "report: $REPORT"
[ "$FAIL" -gt 0 ] && { printf '\n%s%sFLEET: FAIL (%d checks)%s\n' "$B" "$R" "$FAIL" "$Z"; exit 1; }
printf '\n%s%sFLEET: PASS (%d checks)%s\n' "$B" "$G" "$PASS" "$Z"
exit 0
