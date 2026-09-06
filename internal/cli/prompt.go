package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/redact"
)

// Prompt tracing, shared by every agent integration (Claude Code, Codex,
// Cursor, OpenCode, Devin). Each agent exposes the same two moments under a
// different name, so both sides live here rather than five times over:
//
//   - the agent's "user submitted a prompt" hook calls recordPrompt, which
//     spools ONE rec.TypePrompt record holding the text and remembers its id
//     for this session;
//   - the agent's "a tool ran" hook calls stampPrompt, which tags the command
//     with that id alone.
//
// The text therefore exists once, however many commands the prompt causes, and
// a prompt that causes none is still recorded. The daemon rejoins the two at
// query time (see the daemon's prompt index).

// promptState is the current prompt for a session, written by the prompt hook
// so the separate tool-hook processes (different processes, no shared memory)
// can attach its id to the commands that follow. Only the id travels through
// here. The text lives on the prompt record, which went through the redaction
// gate; keeping a second, ungated copy in this file would put the very thing
// the gate exists to catch on disk in plaintext.
type promptState struct {
	ID string `json:"id"`
	Ms int64  `json:"ms"`
}

// promptStatePath is where a session's current prompt id is held. Session ids
// are opaque; hash to a safe filename.
func promptStatePath(dir, session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(dir, "agent-prompts", hex.EncodeToString(sum[:])[:32]+".json")
}

// loadPromptState reads the current prompt for a session (ok=false if none).
func loadPromptState(dir, session string) (promptState, bool) {
	if session == "" {
		return promptState{}, false
	}
	b, err := os.ReadFile(promptStatePath(dir, session))
	if err != nil {
		return promptState{}, false
	}
	var ps promptState
	if json.Unmarshal(b, &ps) != nil || ps.ID == "" {
		return promptState{}, false
	}
	return ps, true
}

// recordPrompt is the shared body of every agent's prompt hook: it gates the
// text through redaction, spools it as a prompt record, and remembers its id
// for this session. Like every capture path it is best-effort and silent: a
// hook that errored or stalled would disrupt the agent it observes. The gate
// runs HERE as well as inside spoolRecord because only the reasons that DROP
// a record need catching before the id is remembered: a prompt we refuse to
// record must not leave commands pointing at a prompt that does not exist.
// Redaction is not one of those: a redacted prompt is still recorded, so
// spoolRecord masking it is enough.
func recordPrompt(dir, session, executor, cwd, text string) {
	text = strings.TrimSpace(text)
	if text == "" || session == "" {
		return
	}
	cfg, _ := config.Load(dir)
	filter, _ := redact.Load(dir, cfg.IgnorePatterns, cfg.IgnoreDirs)
	if filter.SkipDir(cwd) || filter.Ignored(text) {
		return
	}

	ps := promptState{ID: rec.NewID(), Ms: time.Now().UnixMilli()}
	b, err := json.Marshal(ps)
	if err != nil {
		return
	}
	path := promptStatePath(dir, session)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	if os.WriteFile(path, b, 0o600) != nil {
		return
	}

	spoolRecord(dir, rec.Record{
		ID:       ps.ID,
		Type:     rec.TypePrompt,
		Session:  session,
		Prompt:   text,
		Executor: executor,
		Cwd:      cwd,
		StartMs:  ps.Ms,
	})
}

// stampPrompt attaches the session's current prompt to a command record. Only
// the id travels: the text lives on the prompt record recordPrompt spooled.
// Best-effort: a session with no recorded prompt just leaves the command
// untraced.
func stampPrompt(dir, session string, r *rec.Record) {
	if ps, ok := loadPromptState(dir, session); ok {
		r.PromptID = ps.ID
	}
}
