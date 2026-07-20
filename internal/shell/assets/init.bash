# yore — bash shell integration.
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

# Whether accepting a Ctrl-R result should run it immediately (Atuin parity).
# Honors $YORE_DIR. NOTE: bash reads this at source time to choose the Ctrl-R
# binding below (bash `bind -x` cannot itself accept the line), so a change to
# "enter_executes" in config.json takes effect only in a new shell / re-source.
# zsh, by contrast, re-reads it on every keypress.
_yore_enter_executes() {
	local f="${YORE_DIR:-$HOME/.config/yore}/config.json"
	[[ -r $f ]] && grep -q '"enter_executes"[[:space:]]*:[[:space:]]*true' "$f"
}

# Ctrl-R: interactive search. The TUI draws on /dev/tty, so this command
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
{{- if .Aliases}}

# Convenience aliases (Options.Aliases). Drop any pre-existing h/hs aliases
# (the common `history` / `history | grep` pattern) so our versions win and a
# leftover `hs` alias can't break the function definition below.
unalias h hs 2>/dev/null
alias h='{{.Bin}} browse'
hs() {
	if [[ -t 1 ]]; then
		command {{.Bin}} search --query "$*"
	else
		command {{.Bin}} search --headless "$*"
	fi
}
{{- end}}
