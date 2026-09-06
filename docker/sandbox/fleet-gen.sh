#!/usr/bin/env bash
#
# fleet-gen.sh; randomized history generator, run INSIDE one fleet node.
#
# fleet.sh pipes this to `bash -s` in each of the twenty containers (one exec
# per node, never one per command) with its parameters in the environment. It
# writes N records through `yore record`: the exact call the shell hook makes;
# choosing each command from a weighted mix of categories with randomized text,
# directory, exit status, duration, session, and timestamp.
#
# Every generated command carries a trailing "#<MARK>-<n>" comment naming its
# node and its position, so a match is unambiguous, a count is a grep, and no
# two records are textually identical; headless search dedupes identical
# commands, so a corpus with repeats cannot be counted. Secrets additionally
# embed SEKRETMARKER, which must never survive into ANY database.
#
# Categories (weighted; the tallies are printed at the end, so the verifier
# checks against what was actually rolled rather than an assumed ratio):
#   norm    ordinary commands, exit 0                        stored
#   fail    ordinary commands, non-zero exit                 stored
#   agent   tagged as claude-code / aider / a freeform tag   stored, tagged
#   secret  valid-shape credentials                          stored REDACTED
#   space   leading space (histignorespace)                  DROPPED
#   ignore  run inside the configured ignore_dir             DROPPED
#
# Environment:
#   N            records to generate
#   MARK         this node's unique marker
#   SEED         RANDOM seed (per node, so nodes differ but a run is repeatable)
#   IGNORE_DIR   directory configured in ignore_dirs
#   SPAN_DAYS    spread timestamps over this many days back from now
#
# Output: "GEN <key>=<value>" lines on stdout; tallies and the in-container
# timing of the record loop itself (measured from /proc/uptime, which every
# distro and busybox has, unlike date +%N).
set -u

: "${N:?}" "${MARK:?}"
SEED="${SEED:-1}"
IGNORE_DIR="${IGNORE_DIR:-/root/ignored}"
SPAN_DAYS="${SPAN_DAYS:-90}"

RANDOM=$SEED

# --- corpus ----------------------------------------------------------------
# Fragments assembled into plausible command lines. Enough variety that the
# store sees long and short commands, quoting, pipes, unicode, and paths.
hosts=(build-01 db-prod web-7 staging edge-eu bastion runner-3)
dirs=(/root /root/src /root/src/api /root/src/web /var/log /etc /tmp /root/go/src/yore /srv/data "/root/dir with spaces")
files=(main.go server.go README.md Makefile config.toml notes.md handler_test.go schema.sql .env.example deploy.yaml)
words=(alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima)
branches=(main develop feature/sync hotfix/crash release/1.4 topic/redaction)
pkgs=(curl jq ripgrep tmux htop postgresql redis nginx openssl git)

gen_cmd() {
  local w=${words[RANDOM % ${#words[@]}]} f=${files[RANDOM % ${#files[@]}]}
  local d=${dirs[RANDOM % ${#dirs[@]}]} h=${hosts[RANDOM % ${#hosts[@]}]}
  local b=${branches[RANDOM % ${#branches[@]}]} p=${pkgs[RANDOM % ${#pkgs[@]}]}
  case $((RANDOM % 26)) in
    0)  cmd="ls -la $d" ;;
    1)  cmd="cd $d && git status --short" ;;
    2)  cmd="git commit -m \"fix $w handling in $f\"" ;;
    3)  cmd="git rebase -i origin/$b" ;;
    4)  cmd="grep -rn '$w' $d --include='*.go'" ;;
    5)  cmd="rg --hidden -S '$w.*$w' $d | head -50" ;;
    6)  cmd="go test ./... -run Test$w -race" ;;
    7)  cmd="make build && ./bin/yore status" ;;
    8)  cmd="docker compose -f $d/compose.yml up -d --build" ;;
    9)  cmd="kubectl -n $w get pods -o wide | grep -v Running" ;;
    10) cmd="ssh $h 'tail -n 200 /var/log/syslog | grep -i $w'" ;;
    11) cmd="curl -sS https://api.internal/$w/status | jq '.items[] | {id, state}'" ;;
    12) cmd="psql -h $h -c \"select count(*) from $w where created_at > now() - interval '1 day'\"" ;;
    13) cmd="tar czf /tmp/$w-\$(date +%F).tgz $d" ;;
    14) cmd="rsync -avz --delete $d/ $h:/srv/$w/" ;;
    15) cmd="awk '{s+=\$2} END {print s}' /var/log/$w.log" ;;
    16) cmd="sed -i 's/$w/$w-2/g' $d/$f" ;;
    17) cmd="apt-get install -y $p" ;;
    18) cmd="systemctl restart $p && systemctl status $p --no-pager" ;;
    19) cmd="python3 -c 'import json,sys; print(len(json.load(sys.stdin)))' < $d/$f" ;;
    20) cmd="for i in \$(seq 1 10); do echo \"$w-\$i\"; done | sort -u" ;;
    21) cmd="echo 'héllo wörld → $w ✓ 日本語' > $d/$f" ;;
    22) cmd="find $d -name '*.go' -newer $d/$f -print0 | xargs -0 gofmt -l" ;;
    23) cmd="journalctl -u $p --since '2 hours ago' | grep -c $w" ;;
    24) cmd="vim $d/$f" ;;
    *)  cmd="echo $w-$RANDOM" ;;
  esac
}

gen_secret() {
  case $((RANDOM % 5)) in
    0) cmd="echo ghp_SEKRETMARKERxxxxxxxxxxxxxxxxxx$RANDOM" ;;
    1) cmd="export AWS_SECRET_ACCESS_KEY=SEKRETMARKER0000000000000000000000000000" ;;
    2) cmd="curl https://svc:SEKRETMARKERpw$RANDOM@api.internal/v1/deploy" ;;
    3) cmd="echo -----BEGIN SEKRETMARKER PRIVATE KEY-----" ;;
    *) cmd="TOKEN=SEKRETMARKERtoken$RANDOM ./deploy.sh" ;;
  esac
}

# --- timeline --------------------------------------------------------------
# Spread the run over SPAN_DAYS so the store holds a plausible history rather
# than N records in the same millisecond: stats, time filters, and the daemon's
# ordering all get something real to work on.
now_ms=$(( $(date +%s) * 1000 ))
span_ms=$(( SPAN_DAYS * 86400 * 1000 ))
step_ms=$(( span_ms / (N > 0 ? N : 1) ))
(( step_ms < 1 )) && step_ms=1

n_norm=0; n_fail=0; n_agent=0; n_secret=0; n_space=0; n_ignore=0
n_cc=0; n_aider=0; n_tag=0

session="$MARK-s0"
sess_left=$(( 20 + RANDOM % 180 ))
sess_n=0

t0=$(cut -d' ' -f1 /proc/uptime)
i=0
while (( i < N )); do
  # A shell session is a run of commands, not one per command.
  if (( sess_left-- <= 0 )); then
    sess_n=$(( sess_n + 1 ))
    session="$MARK-s$sess_n"
    sess_left=$(( 20 + RANDOM % 180 ))
  fi

  start=$(( now_ms - span_ms + i * step_ms + RANDOM % (step_ms + 1) ))
  dur=$(( RANDOM % 4000 ))
  cwd=${dirs[RANDOM % ${#dirs[@]}]}
  exit_code=0
  cmd=""

  # Weighted category roll (out of 100).
  case $(( RANDOM % 100 )) in
    0|1|2|3)      # 4%  secret; stored, but the credential must be redacted
      gen_secret; n_secret=$(( n_secret + 1 ))
      printf '%s #%s-%d' "$cmd" "$MARK" "$i" | yore record --cwd "$cwd" --exit 0 \
        --duration-ms "$dur" --start-ms "$start" --session "$session" >/dev/null 2>&1
      ;;
    4|5|6)        # 3%  leading space; must be dropped
      gen_cmd; n_space=$(( n_space + 1 ))
      printf ' %s #%s' "$cmd" "$MARK" | yore record --cwd "$cwd" --exit 0 \
        --duration-ms "$dur" --start-ms "$start" --session "$session" >/dev/null 2>&1
      ;;
    7|8|9)        # 3%  inside the ignored directory; must be dropped
      gen_cmd; n_ignore=$(( n_ignore + 1 ))
      printf '%s #%s-%d' "$cmd" "$MARK" "$i" | yore record --cwd "$IGNORE_DIR" --exit 0 \
        --duration-ms "$dur" --start-ms "$start" --session "$session" >/dev/null 2>&1
      ;;
    1[0-9])       # 10% agent-run; stored and tagged
      gen_cmd; n_agent=$(( n_agent + 1 ))
      case $(( RANDOM % 3 )) in
        0) n_cc=$(( n_cc + 1 ))
           printf '%s #%s-%d' "$cmd" "$MARK" "$i" | CLAUDECODE=1 yore record \
             --cwd "$cwd" --exit 0 --duration-ms "$dur" --start-ms "$start" \
             --session "$session" >/dev/null 2>&1 ;;
        1) n_aider=$(( n_aider + 1 ))
           printf '%s #%s-%d' "$cmd" "$MARK" "$i" | AIDER_MODEL=gpt-x yore record \
             --cwd "$cwd" --exit 0 --duration-ms "$dur" --start-ms "$start" \
             --session "$session" >/dev/null 2>&1 ;;
        *) n_tag=$(( n_tag + 1 ))
           printf '%s #%s-%d' "$cmd" "$MARK" "$i" | YORE_TAG=fleet-agent yore record \
             --cwd "$cwd" --exit 0 --duration-ms "$dur" --start-ms "$start" \
             --session "$session" >/dev/null 2>&1 ;;
      esac
      ;;
    2[0-7])       # 8%  a command that failed; stored with its exit status
      gen_cmd; n_fail=$(( n_fail + 1 ))
      exit_code=$(( 1 + RANDOM % 130 ))
      printf '%s #%s-%d' "$cmd" "$MARK" "$i" | yore record --cwd "$cwd" --exit "$exit_code" \
        --duration-ms "$dur" --start-ms "$start" --session "$session" >/dev/null 2>&1
      ;;
    *)            # 72% ordinary command
      gen_cmd; n_norm=$(( n_norm + 1 ))
      printf '%s #%s-%d' "$cmd" "$MARK" "$i" | yore record --cwd "$cwd" --exit 0 \
        --duration-ms "$dur" --start-ms "$start" --session "$session" >/dev/null 2>&1
      ;;
  esac
  i=$(( i + 1 ))
done
t1=$(cut -d' ' -f1 /proc/uptime)

# Tallies for the verifier: what was actually rolled, and what must survive.
stored=$(( n_norm + n_fail + n_agent + n_secret ))
dropped=$(( n_space + n_ignore ))
printf 'GEN generated=%d\n' "$N"
printf 'GEN norm=%d\n'      "$n_norm"
printf 'GEN fail=%d\n'      "$n_fail"
printf 'GEN agent=%d\n'     "$n_agent"
printf 'GEN claude=%d\n'    "$n_cc"
printf 'GEN aider=%d\n'     "$n_aider"
printf 'GEN usertag=%d\n'   "$n_tag"
printf 'GEN secret=%d\n'    "$n_secret"
printf 'GEN space=%d\n'     "$n_space"
printf 'GEN ignore=%d\n'    "$n_ignore"
printf 'GEN stored=%d\n'    "$stored"
printf 'GEN dropped=%d\n'   "$dropped"
printf 'GEN sessions=%d\n'  "$(( sess_n + 1 ))"
printf 'GEN loop_ms=%d\n'   "$(awk "BEGIN{printf \"%d\", ($t1-$t0)*1000}")"
