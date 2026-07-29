package cli

import "os"

// detectExecutor identifies which agent/tool ran a command, so history can be
// filtered by "what I typed" vs "what an agent ran". Resolution order:
//  1. an explicit --executor flag (handled by the caller),
//  2. $YORE_EXECUTOR (a manual override the user can export; $YORE_TAG is the
//     older name for it and still works),
//  3. auto-detection from well-known agent environment markers.
//
// It returns "" for an ordinary interactive command. The executor is not a user
// tag: it is captured once, from the environment, and never edited afterwards.
func detectExecutor() string {
	for _, env := range []string{"YORE_EXECUTOR", "YORE_TAG"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	for _, m := range executorMarkers {
		if os.Getenv(m.env) != "" {
			return m.name
		}
	}
	return ""
}

// executorMarkers maps a distinctive environment variable to an executor name,
// most specific first. These are set by the agent/tool in the shell it drives.
var executorMarkers = []struct{ env, name string }{
	{"CLAUDECODE", "claude-code"},
	{"CLAUDE_CODE_ENTRYPOINT", "claude-code"},
	{"CURSOR_TRACE_ID", "cursor"},
	{"AIDER_MODEL", "aider"},
	{"REPLIT_USER", "replit"},
	{"CODESPACES", "codespaces"},
	{"TERM_PROGRAM_GHOSTTY_AGENT", "agent"}, // generic fallback marker
}
