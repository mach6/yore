# yore — zsh shell integration ({{.Mode}} mode).
# Emitted by `yore init zsh`; load with:  eval "$(yore init zsh)"
# Sourcing repeatedly is safe and produces no output during normal operation.

# Idempotence: initialize at most once per shell.
[[ -n ${__YORE_INITED-} ]] && return
typeset -g __YORE_INITED=1

zmodload zsh/datetime   # provides $EPOCHREALTIME
autoload -Uz add-zsh-hook

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

add-zsh-hook preexec _yore_preexec
add-zsh-hook precmd _yore_precmd
{{- if .Takeover}}

# --- History takeover: yore is the single source of truth ---------------------
# Disable the shell's OWN persistent history so no unredacted ~/.zsh_history is
# written (yore's store is the durable, redacted source). The in-memory list is
# then seeded from yore and gated by yore's redaction, so `!N` and Up work
# against yore-consistent, secret-free history. Source yore AFTER any of your
# own HISTFILE/SAVEHIST settings so this wins.
SAVEHIST=0
unsetopt inc_append_history share_history 2>/dev/null
(( HISTSIZE < 100000 )) && HISTSIZE=100000
if [[ -o interactive ]]; then
	# Seed the in-memory history from yore (best effort; never blocks startup).
	# =(...) is a real temp file, which `fc -R` needs.
	fc -R =(command {{.Bin}} export --shell 2>/dev/null) 2>/dev/null
fi
# Redaction gate: keep the shell's in-memory history consistent with yore's
# store — anything yore would drop (secret / ignored dir / space-prefixed) is
# not added to the shell history either. The filter exits 1 to drop, and a
# zshaddhistory function returning non-zero skips the entry.
_yore_addhistory() {
	emulate -L zsh
	command {{.Bin}} filter --cwd "$PWD" <<< "${1%$'\n'}"
}
add-zsh-hook zshaddhistory _yore_addhistory
{{- end}}
{{- if .Bindings}}

# Whether accepting a Ctrl-R result should run it immediately (Atuin parity).
# Default: run the picked command on Enter; opt out with enter_executes:false to
# insert it for review instead. Read from config.json at call time — NOT at
# source time — so toggling "enter_executes" takes effect without re-sourcing.
# Honors $YORE_DIR.
_yore_enter_executes() {
	local f="${YORE_DIR:-$HOME/.config/yore}/config.json"
	[[ -r $f ]] && grep -q '"enter_executes"[[:space:]]*:[[:space:]]*false' "$f" && return 1
	return 0
}

# Interactive search widget. The TUI draws on /dev/tty, so this command
# substitution captures only the final selection printed to stdout. A missing
# binary or a cancelled search (non-zero exit) leaves the buffer untouched.
# With enter_executes on, a successful selection runs immediately via
# accept-line instead of being left on the line for review.
_yore_search_widget() {
	local selected
	selected=$(command {{.Bin}} search --query "$BUFFER" 2>/dev/null)
	if (( $? == 0 )) && [[ -n $selected ]]; then
		BUFFER=$selected
		CURSOR=$#BUFFER
		if _yore_enter_executes; then
			zle accept-line
			return
		fi
	fi
	zle reset-prompt
}
{{- if not .Takeover}}
# Coexist mode: bind Up to search only if the user opts in (config bind_up_arrow).
_yore_bind_up_arrow() {
	local f="${YORE_DIR:-$HOME/.config/yore}/config.json"
	[[ -r $f ]] && grep -q '"bind_up_arrow"[[:space:]]*:[[:space:]]*true' "$f"
}
{{- end}}
if [[ -o interactive ]]; then
	zle -N _yore_search_widget
	bindkey '^r' _yore_search_widget
	{{- if .Takeover}}
	# Takeover: Up arrow opens yore search (native scroll still on Down/^N).
	bindkey '^[[A' _yore_search_widget  # Up (normal cursor keys)
	bindkey '^[OA' _yore_search_widget  # Up (application cursor keys)
	{{- else}}
	if _yore_bind_up_arrow; then
		bindkey '^[[A' _yore_search_widget
		bindkey '^[OA' _yore_search_widget
	fi
	{{- end}}
fi
{{- end}}
{{- if .Aliases}}

# Convenience aliases (Options.Aliases). `h` opens the browser and drops the
# command you pick onto your NEXT prompt (print -z), like hs — Enter inserts, y
# copies to the clipboard. `hs` searches (the common `history` / `history | grep`
# pattern), with scoped siblings hsa (all hosts), hss (this session), hsc (this
# cwd), and hsw (this git repo). You may already use h/hs as aliases, so remove
# any first at runtime, and escape each function name (\h, \hs, \hsa, …) so zsh
# does not alias-expand it at PARSE time — an unescaped hs() would abort sourcing
# the whole script with "defining function based on alias".
unalias h hs hsa hss hsc hsw 2>/dev/null
\h() {  # browse; drop the picked command onto the next prompt (print -z), like hs
	local __f __sel
	__f=$(mktemp) || return
	command {{.Bin}} browse --accept-file "$__f"
	__sel=$(cat "$__f"); command rm -f "$__f"
	[[ -n $__sel ]] && print -z -- "$__sel"
}
\_yore_hs() {  # $1 = scope, rest = query
	local scope=$1; shift
	if [[ -t 1 ]]; then
		# Interactive: capture the pick and push it onto the editor buffer stack
		# (print -z) so it lands on the NEXT prompt, editable — same as the Ctrl-R
		# and Up widgets. The TUI still draws in color: it renders on /dev/tty, so
		# capturing its stdout here does not strip styling.
		local __sel
		__sel=$(command {{.Bin}} search --scope "$scope" --query "$*") && [[ -n $__sel ]] && print -z -- "$__sel"
	else
		command {{.Bin}} search --headless --scope "$scope" "$*"
	fi
}
\hs()  { _yore_hs local "$@"; }
\hsa() { _yore_hs all "$@"; }
\hss() { _yore_hs session "$@"; }
\hsc() { _yore_hs cwd "$@"; }
\hsw() { _yore_hs workspace "$@"; }
{{- end}}
