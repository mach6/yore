package cli

import "os"

// executorTag identifies which agent/tool ran a command, so history can be
// filtered by "what I typed" vs "what an agent ran". Resolution order:
//  1. an explicit --tag flag (handled by the caller),
//  2. $YORE_TAG (a manual override the user can export),
//  3. auto-detection from well-known agent environment markers.
//
// It returns "" for an ordinary interactive command.
func executorTag() string {
	if v := os.Getenv("YORE_TAG"); v != "" {
		return v
	}
	for _, m := range executorMarkers {
		if os.Getenv(m.env) != "" {
			return m.tag
		}
	}
	return ""
}

// executorMarkers maps a distinctive environment variable to an executor tag,
// most specific first. These are set by the agent/tool in the shell it drives.
var executorMarkers = []struct{ env, tag string }{
	{"CLAUDECODE", "claude-code"},
	{"CLAUDE_CODE_ENTRYPOINT", "claude-code"},
	{"CURSOR_TRACE_ID", "cursor"},
	{"AIDER_MODEL", "aider"},
	{"REPLIT_USER", "replit"},
	{"CODESPACES", "codespaces"},
	{"TERM_PROGRAM_GHOSTTY_AGENT", "agent"}, // generic fallback marker
}
