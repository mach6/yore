#!/usr/bin/env bash
#
# stress.sh — MANUAL stress/soak harness for yore's 3-container sandbox.
#
# NOT wired into CI (never runs in Drone). It rebuilds the CURRENT binary into a
# fresh sandbox, hammers two clients with lots of history — normal commands,
# secrets that MUST be redacted, leading-space and ignore-dir commands that MUST
# be dropped, and agent-tagged batches — syncs everything end-to-end encrypted,
# then verifies correctness and reports timings. Every check prints ✓/✗ and the
# script exits non-zero if any check fails. It always tears the sandbox down
# (even on failure) unless --keep / KEEP=1 is given.
#
# Usage:
#   docker/sandbox/stress.sh                # N=5000 per host (a real run)
#   N=150 docker/sandbox/stress.sh          # quick smoke
#   docker/sandbox/stress.sh --keep         # leave the sandbox up for a post-mortem
#   make stress N=10000                     # via the Makefile
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration & paths
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
COMPOSE="$SCRIPT_DIR/compose.yml"

N="${N:-5000}"                 # records generated per host (default: a real run)
KEEP="${KEEP:-0}"              # 1 = do not tear the sandbox down at the end
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    N=*)    N="${arg#N=}" ;;
    *) echo "stress.sh: unknown argument: $arg" >&2; exit 2 ;;
  esac
done

# Unique per-run markers so a leak / a match is unambiguous and greppable, and so
# stale data (there shouldn't be any — we start fresh) can never satisfy a check.
RUN="$(date +%s)"
ZMARK="ZHOST${RUN}"            # appears in EVERY command stored by zsh-box
BMARK="BHOST${RUN}"            # appears in EVERY command stored by bash-box
SECRET="SEKRETMARKER"         # embedded in every secret; MUST never reach any db

BIGLIMIT=100000000            # defeat the 200-row default window when counting

# Per-host category sizes (fractions of N). Floored at 1 so every code path is
# exercised even at very small N. NORM (the bulk) is the remainder.
frac() { local v=$(( N / 20 )); [ "$v" -lt 1 ] && v=1; echo "$v"; }
SEC="$(frac)"                  # secrets            (dropped by redaction)
SP="$(frac)"                   # leading-space cmds (dropped)
IG="$(frac)"                   # ignore-dir cmds    (dropped; zsh-box only)
TC="$(frac)"                   # claude-code tagged (stored)
TS="$(frac)"                   # stress-agent tagged(stored)
TA="$(frac)"                   # aider tagged       (stored)

# Bulk (normal) counts differ per host: only zsh-box carries the tag + ignore
# batches, so bash-box devotes more of N to normal commands.
Z_NORM=$(( N - SEC - SP - IG - TC - TS - TA ))
B_NORM=$(( N - SEC - SP ))
[ "$Z_NORM" -lt 1 ] && Z_NORM=1
[ "$B_NORM" -lt 1 ] && B_NORM=1

# Stored (non-dropped) expectations per host.
Z_STORED=$(( Z_NORM + TC + TS + TA ))
B_STORED=$(( B_NORM ))

IGNORE_DIR="/root/ignored"     # configured into zsh-box's ignore_dirs

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
if [ -t 1 ]; then G=$'\033[32m'; R=$'\033[31m'; B=$'\033[1m'; Z=$'\033[0m'
else G=""; R=""; B=""; Z=""; fi

PASS=0
FAIL=0
ok()   { printf '  %s✓%s %s\n' "$G" "$Z" "$1"; PASS=$((PASS+1)); }
bad()  { printf '  %s✗%s %s\n' "$R" "$Z" "$1"; FAIL=$((FAIL+1)); }
step() { printf '\n%s==> %s%s\n' "$B" "$1" "$Z"; }
note() { printf '     %s\n' "$1"; }

dc() { docker compose -f "$COMPOSE" exec -T "$@"; }

# now_ms / elapsed use the host's GNU date (nanosecond precision).
now_ms() { echo $(( $(date +%s%N) / 1000000 )); }
rate() { # rate <count> <ms>  ->  "<n>/s"
  local n="$1" ms="$2"
  if [ "$ms" -le 0 ]; then echo "n/a"; else
    awk "BEGIN{printf \"%.0f/s\", $n*1000.0/$ms}"; fi
}

# ---------------------------------------------------------------------------
# Teardown (default: always, even on failure; suppressed by --keep)
# ---------------------------------------------------------------------------
teardown() {
  local rc=$?
  if [ "$KEEP" = "1" ]; then
    step "Leaving sandbox UP (--keep). Tear down with:"
    note "docker compose -f $COMPOSE down -v"
  else
    step "Teardown"
    docker compose -f "$COMPOSE" down -v >/dev/null 2>&1 || true
    # Verify the host is left clean: no yore-sandbox containers remain.
    local left
    left="$(docker ps -a --filter name=yore-sandbox --format '{{.Names}}' || true)"
    if [ -z "$left" ]; then
      ok "host clean: no yore-sandbox containers remain (docker ps)"
    else
      bad "host NOT clean, leftover containers: $left"
    fi
  fi
  return "$rc"
}
trap teardown EXIT

# ===========================================================================
# 1. Fresh sandbox
# ===========================================================================
step "Fresh sandbox (down -v; up -d --build) — N=$N per host"
docker compose -f "$COMPOSE" down -v >/dev/null 2>&1 || true
docker compose -f "$COMPOSE" up -d --build

# Wait for the server healthcheck (probed from inside a client — nothing is
# published to the host).
printf '     waiting for server health'
healthy=0
for _ in $(seq 1 60); do
  if dc zsh yore healthcheck --url http://server:8080/v1/health >/dev/null 2>&1; then
    healthy=1; break
  fi
  printf '.'; sleep 1
done
printf '\n'
if [ "$healthy" = "1" ]; then ok "server healthy"; else
  bad "server never became healthy"; exit 1
fi

# ===========================================================================
# 2. Enroll two devices
# ===========================================================================
step "Enroll zsh-box (bootstrap) + bash-box (pending -> approved)"

dc zsh yore setup --server http://server:8080 --token sandbox-token \
     --integration takeover --name zsh-box >/dev/null
ok "zsh-box enrolled (first device, history group bootstrapped)"

TOKEN=$(dc zsh yore devices token 2>/dev/null | tr -d '\r\n')
dc bash yore setup --server http://server:8080 --token "$TOKEN" \
     --integration takeover --name bash-box >/dev/null
ok "bash-box registered (pending)"

# Approving is a keypress in the devices view, which a script cannot press. The
# view is one client of the daemon's protocol — newline-delimited JSON on a unix
# socket — and so is this: `devices` to find the pending machine, `approve` to
# admit it. socat and jq both run INSIDE the container (the client image carries
# them), so the harness still needs nothing on the host but docker.
dsock() { # dsock <request-json> [jq-filter]
  dc zsh sh -c "printf '%s\n' '$1' | socat -t 30 - UNIX-CONNECT:/root/.config/yore/daemon.sock${2:+ | jq -r '$2'}"
}
PENDING_ID="$(dsock '{"op":"devices"}' '.devices.devices[] | select(.status=="pending") | .id' | head -1 | tr -d '\r')"
if [ -z "$PENDING_ID" ]; then bad "could not find pending device id"; exit 1; fi
note "pending device id: $PENDING_ID"
dsock "{\"op\":\"approve\",\"device_id\":\"$PENDING_ID\"}" >/dev/null
ok "bash-box approved from zsh-box"

# Configure ignore-dir + a short backup interval into each host's config BEFORE
# recording (setup wrote config.toml; we extend it while no daemon holds it yet).
# zsh-box additionally gets an ignore_dirs entry so the ignore-dir path is
# exercised. Values are the same ones the sandbox always uses, so a full rewrite
# is safe.
write_cfg() {  # write_cfg <service> <ignore_dirs_toml_array>
  dc "$1" sh -c "cat > /root/.config/yore/config.toml <<EOF
server_url = \"http://server:8080\"
integration = \"takeover\"
backup_interval = \"2s\"
backup_keep = 3
ignore_dirs = $2
EOF
chmod 600 /root/.config/yore/config.toml"
}
write_cfg zsh  "[\"$IGNORE_DIR\"]"
write_cfg bash "[]"
ok "config seeded: zsh-box ignore_dirs=[$IGNORE_DIR], backups on (2s) both hosts"

# ===========================================================================
# 3. Generate load — ONE docker exec per host running a bash loop
# ===========================================================================
# The generator (below) is fed on stdin to `bash -s`; parameters arrive via -e
# env vars. It emits a "STRESS_BULK_MS=<n>" line timing just the bulk (normal)
# record loop, measured inside the container from /proc/uptime (no dependency on
# GNU date's %N inside busybox).
GEN='
set -u
uptime_s() { cut -d" " -f1 /proc/uptime; }
rec() { printf "%s" "$1" | yore record --cwd "${2:-/root}" --exit 0 --session "$SESSION" >/dev/null 2>&1; }

# --- bulk: normal commands (positive control; MUST be stored) ---
t0=$(uptime_s)
i=0; while [ "$i" -lt "$NORM" ]; do
  rec "echo run-$i-$HMARK"
  i=$((i+1))
done
t1=$(uptime_s)
echo "STRESS_BULK_MS=$(awk "BEGIN{printf \"%d\", ($t1-$t0)*1000}")"

# --- agent-tagged batches (stored, tagged; zsh-box only) ---
i=0; while [ "$i" -lt "$TC" ]; do CLAUDECODE=1        rec "echo cc-$i-$HMARK"; i=$((i+1)); done
i=0; while [ "$i" -lt "$TS" ]; do YORE_TAG=stress-agent rec "echo sa-$i-$HMARK"; i=$((i+1)); done
i=0; while [ "$i" -lt "$TA" ]; do AIDER_MODEL=gpt-x    rec "echo ai-$i-$HMARK"; i=$((i+1)); done

# --- secrets: valid-shape, each embeds SEKRETMARKER; MUST be redacted ---
i=0; while [ "$i" -lt "$SEC" ]; do
  case $(( i % 5 )) in
    0) rec "echo ghp_SEKRETMARKERxxxxxxxxxxxxxxxxxxxx$i" ;;
    1) rec "export AWS_SECRET_ACCESS_KEY=SEKRETMARKER0000000000000000000000000000" ;;
    2) rec "curl https://u:SEKRETMARKERpw$i@example.internal/" ;;
    3) rec "echo -----BEGIN SEKRETMARKER PRIVATE KEY-----" ;;
    4) rec "TOKEN=SEKRETMARKERtoken$i" ;;
  esac
  i=$((i+1))
done

# --- leading-space commands (histignorespace; MUST be dropped) ---
i=0; while [ "$i" -lt "$SP" ]; do rec " echo space-$i-$HMARK"; i=$((i+1)); done

# --- ignore-dir commands (recorded with --cwd in the ignored dir; MUST drop) ---
i=0; while [ "$i" -lt "$IG" ]; do rec "echo ignored-$i-$HMARK" "$IGNORE_DIR"; i=$((i+1)); done
'

step "Generate load on zsh-box (bulk=$Z_NORM, tags=$TC/$TS/$TA, secrets=$SEC, space=$SP, ignore=$IG)"
zt0="$(now_ms)"
zsh_out="$(dc -e HMARK="$ZMARK" -e SESSION="zsess-$RUN" \
     -e NORM="$Z_NORM" -e TC="$TC" -e TS="$TS" -e TA="$TA" \
     -e SEC="$SEC" -e SP="$SP" -e IG="$IG" -e IGNORE_DIR="$IGNORE_DIR" \
     zsh bash -s <<<"$GEN")"
zt1="$(now_ms)"
ZSH_BULK_MS="$(printf '%s\n' "$zsh_out" | sed -n 's/^STRESS_BULK_MS=//p')"
note "zsh-box generation wall: $(( zt1 - zt0 )) ms; bulk-record loop: ${ZSH_BULK_MS:-?} ms"

step "Generate load on bash-box (bulk=$B_NORM, secrets=$SEC, space=$SP)"
bt0="$(now_ms)"
bash_out="$(dc -e HMARK="$BMARK" -e SESSION="bsess-$RUN" \
     -e NORM="$B_NORM" -e TC=0 -e TS=0 -e TA=0 \
     -e SEC="$SEC" -e SP="$SP" -e IG=0 -e IGNORE_DIR="$IGNORE_DIR" \
     bash bash -s <<<"$GEN")"
bt1="$(now_ms)"
BASH_BULK_MS="$(printf '%s\n' "$bash_out" | sed -n 's/^STRESS_BULK_MS=//p')"
note "bash-box generation wall: $(( bt1 - bt0 )) ms; bulk-record loop: ${BASH_BULK_MS:-?} ms"

# ===========================================================================
# 4. Sync (twice around for full convergence), timing each cycle
# ===========================================================================
step "Sync both hosts (2 rounds)"
timed_sync() { # timed_sync <service> <label>  -> echoes ms
  local s="$1" ms t0 t1
  t0="$(now_ms)"; dc "$s" yore sync >/dev/null; t1="$(now_ms)"
  ms=$(( t1 - t0 )); note "$2: ${ms} ms" >&2; echo "$ms"
}
SYNC_ZSH_MS="$(timed_sync zsh 'zsh-box sync (push all)')"
SYNC_BASH_MS="$(timed_sync bash 'bash-box sync (push+pull)')"
timed_sync zsh 'zsh-box sync (pull bash)' >/dev/null
timed_sync bash 'bash-box sync (settle)' >/dev/null
ok "sync cycles completed"

# ===========================================================================
# 5. Verify
# ===========================================================================
step "Verify"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/yore-stress.XXXXXX")"
zsh_cid="$(docker compose -f "$COMPOSE" ps -q zsh)"
bash_cid="$(docker compose -f "$COMPOSE" ps -q bash)"
srv_cid="$(docker compose -f "$COMPOSE" ps -q server)"

# --- 5a. Redaction / E2E at scale (the headline check) ---------------------
docker cp "$zsh_cid:/root/.config/yore/data.db" "$TMP/zsh.db"  >/dev/null 2>&1 || true
docker cp "$bash_cid:/root/.config/yore/data.db" "$TMP/bash.db" >/dev/null 2>&1 || true
docker cp "$srv_cid:/data/yore.db" "$TMP/server.db"            >/dev/null 2>&1 || true
leaks=0
for db in zsh bash server; do
  if [ ! -f "$TMP/$db.db" ]; then bad "could not copy $db db"; leaks=$((leaks+1)); continue; fi
  hits="$(grep -a -c "$SECRET" "$TMP/$db.db" 2>/dev/null || true)"
  [ -z "$hits" ] && hits=0
  if [ "$hits" -eq 0 ]; then note "$db.db: 0 hits for $SECRET"; else
    note "$db.db: ${R}$hits${Z} hits for $SECRET"; leaks=$((leaks+hits)); fi
done
if [ "$leaks" -eq 0 ]; then
  ok "redaction/E2E: ZERO '$SECRET' in every client db AND the server db (server holds ciphertext only)"
else
  bad "redaction/E2E: $SECRET leaked ($leaks hits) — see above"
fi

# helper: count headless search matches for a marker (dedupe-safe: markers unique)
count_search() { # count_search <service> <scope> <marker> [extra-args...]
  local s="$1" scope="$2" marker="$3"; shift 3
  dc "$s" yore search --headless --scope "$scope" --limit "$BIGLIMIT" "$@" "$marker" \
    2>/dev/null | grep -c "$marker" || true
}

# --- 5b. Positive control: the normal commands ARE stored locally ----------
z_local="$(count_search zsh local "$ZMARK")"
b_local="$(count_search bash local "$BMARK")"
note "zsh-box local stored (marker $ZMARK): $z_local (expected $Z_STORED)"
note "bash-box local stored (marker $BMARK): $b_local (expected $B_STORED)"
if [ "$z_local" -gt 0 ] && [ "$b_local" -gt 0 ]; then
  ok "positive control: normal commands present on both hosts (nothing silently dropped)"
else
  bad "positive control: a host stored ZERO matching commands"
fi

# --- 5c. Convergence: each host sees the OTHER host's commands via deep search
z_from_bash="$(count_search bash all "$ZMARK")"
b_from_zsh="$(count_search zsh all "$BMARK")"
note "bash-box sees zsh-box commands (deep, marker $ZMARK): $z_from_bash (expected ~$Z_STORED)"
note "zsh-box sees bash-box commands (deep, marker $BMARK): $b_from_zsh (expected ~$B_STORED)"
conv_ok=1
within() { # within <got> <expected> <tol>
  local got="$1" exp="$2" tol="$3" d=$(( $1 - $2 )); [ "$d" -lt 0 ] && d=$(( -d ))
  [ "$d" -le "$tol" ]
}
TOL=$(( N / 100 + 2 ))
within "$z_from_bash" "$Z_STORED" "$TOL" || conv_ok=0
within "$b_from_zsh" "$B_STORED" "$TOL" || conv_ok=0
if [ "$conv_ok" = "1" ]; then
  ok "convergence: cross-host deep counts match expected non-dropped totals (±$TOL)"
else
  bad "convergence: cross-host deep counts diverge from expected (±$TOL)"
fi

# --- 5d. Dropped-count sanity ---------------------------------------------
# stored (local) ≈ generated − (secrets + space + ignore)
z_gen=$(( Z_NORM + TC + TS + TA + SEC + SP + IG ))
b_gen=$(( B_NORM + SEC + SP ))
z_dropped=$(( SEC + SP + IG ))
b_dropped=$(( SEC + SP ))
note "zsh-box : generated $z_gen − dropped $z_dropped (sec=$SEC space=$SP ignore=$IG) = expect stored $Z_STORED; got $z_local"
note "bash-box: generated $b_gen − dropped $b_dropped (sec=$SEC space=$SP) = expect stored $B_STORED; got $b_local"
if within "$z_local" "$Z_STORED" "$TOL" && within "$b_local" "$B_STORED" "$TOL"; then
  ok "dropped-count sanity: stored ≈ generated − dropped on both hosts (±$TOL)"
else
  bad "dropped-count sanity: stored count off by more than ±$TOL"
fi

# --- 5e. Agent attribution -------------------------------------------------
# $CLAUDECODE / $AIDER_MODEL / $YORE_TAG all name the agent that RAN the
# command, which yore records as its executor. Executors are not user tags:
# --tag filters labels the user applied with `yore tag`, and would find none of
# these.
exec_count() { dc zsh yore search --headless --scope local --limit "$BIGLIMIT" --executor "$1" 2>/dev/null | grep -c "$ZMARK" || true; }
n_cc="$(exec_count claude-code)"
n_sa="$(exec_count stress-agent)"
n_ai="$(exec_count aider)"
note "executor claude-code : $n_cc (planted $TC)"
note "executor stress-agent: $n_sa (planted $TS)"
note "executor aider       : $n_ai (planted $TA)"
if within "$n_cc" "$TC" "$TOL" && within "$n_sa" "$TS" "$TOL" && within "$n_ai" "$TA" "$TOL"; then
  ok "attribution: claude-code/stress-agent/aider executor counts match planted (±$TOL)"
else
  bad "attribution: an executor count diverges from planted"
fi

# --- 5f. Performance -------------------------------------------------------
# Deep search at full volume, timed on the host.
dt0="$(now_ms)"; deep="$(count_search bash all "$ZMARK")"; dt1="$(now_ms)"
DEEP_MS=$(( dt1 - dt0 ))
note "bulk record : zsh ${ZSH_BULK_MS:-?} ms ($(rate "$Z_NORM" "${ZSH_BULK_MS:-0}")), bash ${BASH_BULK_MS:-?} ms ($(rate "$B_NORM" "${BASH_BULK_MS:-0}"))"
note "sync        : zsh push ${SYNC_ZSH_MS} ms ($(rate "$Z_STORED" "$SYNC_ZSH_MS")), bash ${SYNC_BASH_MS} ms"
note "deep search : ${DEEP_MS} ms for $deep rows at full volume ($(rate "$deep" "$DEEP_MS"))"
ok "performance timings captured"

# --- 5g. Health: one daemon process per client, zero zombies, a backup -----
# Count daemon PROCESSES (not threads). The '[y]ore daemon' trick makes pgrep -f
# match the daemon's cmdline but NOT its own shell (whose cmdline would otherwise
# contain the literal pattern), so this counts exactly the daemon process(es).
daemon_count() { dc "$1" sh -c "pgrep -f '[y]ore daemon' | wc -l" | tr -d '[:space:]'; }
# Zombies via /proc (portable on busybox): field 3 of /proc/PID/stat is state.
zombie_count() {
  dc "$1" sh -c 'c=0; for s in /proc/[0-9]*/stat; do st=$(sed -e "s/^.*) //" -e "s/ .*//" "$s" 2>/dev/null); [ "$st" = Z ] && c=$((c+1)); done; echo $c' | tr -d '[:space:]'
}
health_ok=1
for s in zsh bash; do
  d="$(daemon_count "$s")"; zc="$(zombie_count "$s")"
  note "$s-box: yore daemon processes=$d, zombies=$zc"
  [ "$d" = "1" ] || { health_ok=0; }
  [ "$zc" = "0" ] || { health_ok=0; }
done
if [ "$health_ok" = "1" ]; then
  ok "health: exactly ONE yore daemon process per client, zero zombies"
else
  bad "health: daemon-process count != 1 or zombies present"
fi

# Backup file present (backup_interval=2s; first backup ~2s after daemon start).
backup_ok=0
for _ in $(seq 1 10); do
  nb="$(dc zsh sh -c 'ls /root/.config/yore/backups/data-*.db 2>/dev/null | wc -l' | tr -d '[:space:]')"
  if [ "${nb:-0}" -ge 1 ]; then backup_ok=1; break; fi
  sleep 1
done
if [ "$backup_ok" = "1" ]; then
  ok "backup: at least one rolling backup exists under ~/.config/yore/backups/"
else
  bad "backup: no backup file produced"
fi

rm -rf "$TMP"

# ===========================================================================
# 6. Summary
# ===========================================================================
step "Summary  (N=$N per host, run $RUN)"
printf '     checks: %s%d passed%s, %s%d failed%s\n' "$G" "$PASS" "$Z" \
  "$( [ "$FAIL" -gt 0 ] && echo "$R" || echo "$G")" "$FAIL" "$Z"
printf '     timings:\n'
printf '       bulk record : zsh %s ms (%s), bash %s ms (%s)\n' \
  "${ZSH_BULK_MS:-?}" "$(rate "$Z_NORM" "${ZSH_BULK_MS:-0}")" \
  "${BASH_BULK_MS:-?}" "$(rate "$B_NORM" "${BASH_BULK_MS:-0}")"
printf '       sync (scale): zsh push %s ms (%s), bash %s ms\n' \
  "$SYNC_ZSH_MS" "$(rate "$Z_STORED" "$SYNC_ZSH_MS")" "$SYNC_BASH_MS"
printf '       deep search : %s ms for %s rows (%s)\n' \
  "$DEEP_MS" "$deep" "$(rate "$deep" "$DEEP_MS")"

if [ "$FAIL" -gt 0 ]; then
  printf '\n%s%sSTRESS: FAIL (%d checks failed)%s\n' "$B" "$R" "$FAIL" "$Z"
  exit 1
fi
printf '\n%s%sSTRESS: PASS (%d checks)%s\n' "$B" "$G" "$PASS" "$Z"
exit 0
