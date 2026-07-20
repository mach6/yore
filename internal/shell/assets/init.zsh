# yore — zsh shell integration.
# Emitted by `yore init zsh`; load with:  eval "$(yore init zsh)"
# Sourcing repeatedly is safe and produces no output during normal operation.

# Idempotence: initialize at most once per shell.
[[ -n ${__YORE_INITED-} ]] && return
typeset -g __YORE_INITED=1

zmodload zsh/datetime   # provides $EPOCHREALTIME

# One stable session id per shell. Exported so it survives `exec` and subshells;
# only generated when unset so a re-exec'd shell keeps the same id.
if [[ -z ${YORE_SESSION-} ]]; then
	export YORE_SESSION="$(command {{.Bin}} gen-id 2>/dev/null)"
fi

# Two-phase capture state. Globals, __yore-prefixed; nothing else touches them.
typeset -g __yore_cmd=
typeset -g __yore_start=

# preexec runs immediately before the command executes. It performs ZERO
# process spawns: it only records the command text and a start timestamp.
_yore_preexec() {
	__yore_cmd=$1
	__yore_start=$EPOCHREALTIME
}

# precmd runs after the command finishes, before the next prompt is drawn.
_yore_precmd() {
	local exit=$?   # the user's exit status — must be captured first
	if [[ -n ${__yore_cmd-} ]]; then
		integer dur
		(( dur = (EPOCHREALTIME - __yore_start) * 1000 ))   # float secs -> int ms
		# Command text goes in on stdin (printf), never argv: immune to quoting
		# quirks and E2BIG. Fully backgrounded and disowned so the prompt never
		# waits and no job-control notice ever prints.
		printf '%s' "$__yore_cmd" | command {{.Bin}} record \
			--exit "$exit" --duration-ms "$dur" \
			--session "$YORE_SESSION" --cwd "$PWD" &>/dev/null &!
		__yore_cmd=
		__yore_start=
	fi
	return $exit   # leave $? untouched for any of the user's own precmds
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec _yore_preexec
add-zsh-hook precmd _yore_precmd

# Ctrl-R: interactive search. The TUI draws on /dev/tty, so this command
# substitution captures only the final selection printed to stdout. A missing
# binary or a cancelled search (non-zero exit) leaves the buffer untouched.
_yore_search_widget() {
	local selected
	selected=$(command {{.Bin}} search --query "$BUFFER" 2>/dev/null)
	if (( $? == 0 )) && [[ -n $selected ]]; then
		BUFFER=$selected
		CURSOR=$#BUFFER
	fi
	zle reset-prompt
}
if [[ -o interactive ]]; then
	zle -N _yore_search_widget
	bindkey '^r' _yore_search_widget
fi
{{- if .Aliases}}

# Convenience aliases (Options.Aliases). You may already use h/hs as aliases
# (the common `history` / `history | grep` pattern). Remove those first so our
# versions win at runtime, and escape the hs function name (\hs) so zsh does
# not alias-expand it at PARSE time — an unescaped hs() would abort sourcing
# the whole script with "defining function based on alias".
unalias h hs 2>/dev/null
alias h='{{.Bin}} browse'
\hs() {
	if [[ -t 1 ]]; then
		command {{.Bin}} search --query "$*"
	else
		command {{.Bin}} search --headless "$*"
	fi
}
{{- end}}
