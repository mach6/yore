# yore — bash shell integration ({{.Mode}} mode).
# Emitted by `yore init bash`; load with:  eval "$(yore init bash)"
# Sourcing repeatedly is safe and produces no output during normal operation.

# Only run in interactive shells; no-op (and never error) everywhere else.
[[ $- == *i* ]] || return 0

# Idempotence: initialize at most once per shell.
[[ -n ${__YORE_INITED-} ]] && return 0
__YORE_INITED=1

# One stable session id per shell. Exported so it survives `exec` and subshells;
# only generated when unset so a re-exec'd shell keeps the same id.
if [[ -z ${YORE_SESSION-} ]]; then
	export YORE_SESSION="$(command {{.Bin}} gen-id 2>/dev/null)"
fi

# bash-preexec provides zsh-like preexec/precmd hooks. A user's own copy always
# wins; only load our vendored copy when nothing has installed it yet. It is
# sourced from a heredoc in its own source context, so its top-level `return`
# cannot abort the remainder of this script.
if [[ -z ${bash_preexec_imported:-}${__bp_imported:-} ]] && ! type -t __bp_install >/dev/null 2>&1; then
	source /dev/stdin <<'__YORE_BASH_PREEXEC_EOF__'
{{.Preexec}}
__YORE_BASH_PREEXEC_EOF__
fi

# preexec runs immediately before the command executes. It performs ZERO
# process spawns: it only records the command text and a start timestamp.
# EPOCHREALTIME exists on bash >= 5; on bash 4 the start stays empty and the
# duration is simply omitted.
__yore_preexec() {
	__yore_cmd=$1
	__yore_start=${EPOCHREALTIME-}
}

# precmd runs after the command finishes, before the next prompt is drawn.
__yore_precmd() {
	local exit=$?   # the user's exit status — must be captured first
	[[ -n ${__yore_cmd-} ]] || return "$exit"
	local -a dur=()
	if [[ -n ${__yore_start-} && -n ${EPOCHREALTIME-} ]]; then
		# EPOCHREALTIME is <secs>.<6-digit-usecs>; stripping every non-digit
		# yields microseconds regardless of the locale's decimal separator.
		local now=${EPOCHREALTIME//[^0-9]/} start=${__yore_start//[^0-9]/}
		dur=(--duration-ms "$(( (now - start) / 1000 ))")
	fi
	# Command text goes in on stdin (printf), never argv: immune to quoting
	# quirks and E2BIG. Backgrounded and disowned so the prompt never waits and
	# no job-control notice ever prints. The array expansion is nounset-safe.
	printf '%s' "$__yore_cmd" | command {{.Bin}} record \
		--exit "$exit" ${dur[@]+"${dur[@]}"} \
		--session "$YORE_SESSION" --cwd "$PWD" &>/dev/null &
	disown
	__yore_cmd=
	__yore_start=
	return "$exit"   # leave $? untouched for any of the user's own precmds
}

precmd_functions+=(__yore_precmd)
preexec_functions+=(__yore_preexec)
{{- if .Takeover}}

# --- History takeover: yore is the single source of truth ---------------------
# No persistent native ~/.bash_history (yore's store is the durable, redacted
# source); seed the in-memory list from yore, and drop secrets from it. Source
# yore AFTER your own HISTFILE settings so this wins. bash's history hooks are
# coarser than zsh's, so the in-memory gate is best-effort.
unset HISTFILE
history -c
history -r <(command {{.Bin}} export --shell --format bash 2>/dev/null) 2>/dev/null
# Redaction gate: if yore would drop the just-entered command (secret / ignored
# dir / space-prefixed), delete it from bash history too. Runs in preexec, where
# the command is already in the history list.
__yore_bash_gate() {
	command {{.Bin}} filter --cwd "$PWD" <<< "$1" \
		|| history -d "$(HISTTIMEFORMAT= history 1 | awk '{print $1}')" 2>/dev/null
}
preexec_functions+=(__yore_bash_gate)
{{- end}}
{{- if .Bindings}}

# Whether accepting a Ctrl-R result should run it immediately (Atuin parity).
# Default: run the picked command on Enter; opt out with enter_executes:false to
# insert it for review instead. Honors $YORE_DIR. NOTE: bash reads this at source
# time to choose the Ctrl-R binding below (bash `bind -x` cannot itself accept the
# line), so a change to "enter_executes" in config.json takes effect only in a new
# shell / re-source. zsh, by contrast, re-reads it on every keypress.
_yore_enter_executes() {
	local f="${YORE_DIR:-$HOME/.config/yore}/config.json"
	[[ -r $f ]] && grep -q '"enter_executes"[[:space:]]*:[[:space:]]*false' "$f" && return 1
	return 0
}

# Interactive search widget. The TUI draws on /dev/tty, so this command
# substitution captures only the final selection printed to stdout. A missing
# binary or a cancelled search (non-zero exit) leaves the line untouched —
# except under enter_executes, where a cancelled/empty search blanks the line so
# the auto-appended Return (see the binding below) runs nothing, never a stale
# partial query.
__yore_search() {
	local selected
	selected=$(command {{.Bin}} search --query "$READLINE_LINE" 2>/dev/null)
	if (( $? == 0 )) && [[ -n $selected ]]; then
		READLINE_LINE=$selected
		READLINE_POINT=${#READLINE_LINE}
	elif _yore_enter_executes; then
		READLINE_LINE=
		READLINE_POINT=0
	fi
}
# `bind -x` runs the widget but cannot itself run accept-line, so
# execute-on-enter is done with a two-part key macro: a private ESC-prefixed
# sequence (\eyore) runs the widget to set the line, then an appended Return
# (\C-m) accepts it. The insert-only binding omits the Return. This choice is
# fixed here at source time (see the _yore_enter_executes note above).
bind -x '"\eyore": __yore_search'
if _yore_enter_executes; then
	bind '"\C-r": "\eyore\C-m"'
else
	bind '"\C-r": "\eyore"'
fi
{{- if .Takeover}}
# Takeover: Up arrow opens yore search.
if _yore_enter_executes; then
	bind '"\e[A": "\eyore\C-m"'
else
	bind '"\e[A": "\eyore"'
fi
{{- else}}
# Coexist: bind Up to search only if the user opts in (config bind_up_arrow).
_yore_bind_up_arrow() {
	local f="${YORE_DIR:-$HOME/.config/yore}/config.json"
	[[ -r $f ]] && grep -q '"bind_up_arrow"[[:space:]]*:[[:space:]]*true' "$f"
}
if _yore_bind_up_arrow; then
	if _yore_enter_executes; then
		bind '"\e[A": "\eyore\C-m"'
	else
		bind '"\e[A": "\eyore"'
	fi
fi
{{- end}}
{{- end}}
{{- if .Aliases}}

# Convenience aliases (Options.Aliases). `hb` opens the browser; Enter prints the
# command you pick to stdout and y copies it to the clipboard (bash can't refill
# the prompt the way zsh's print -z does). `hs` searches (the `history | grep`
# pattern), with scoped siblings hsa (all hosts), hss (this session), hsc (this
# cwd), and hsw (this git repo). yore does NOT touch `h` — keep your own (e.g.
# h=history). Drop any pre-existing collisions so our versions win and a leftover
# alias can't break the function definitions below.
unalias hb hs hsa hss hsc hsw 2>/dev/null
hb() { command {{.Bin}} browse; }   # Enter prints the pick to stdout; y copies
_yore_hs() {  # $1 = scope, rest = query
	local scope=$1; shift
	if [[ -t 1 ]]; then
		command {{.Bin}} search --scope "$scope" --query "$*"
	else
		command {{.Bin}} search --headless --scope "$scope" "$*"
	fi
}
hs()  { _yore_hs local "$@"; }
hsa() { _yore_hs all "$@"; }
hss() { _yore_hs session "$@"; }
hsc() { _yore_hs cwd "$@"; }
hsw() { _yore_hs workspace "$@"; }
{{- end}}
