package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"yore/internal/rec"
)

// agentCursor is the executor tag stamped on commands captured from Cursor's
// hooks, matching the interactive auto-detect value ($CURSOR_TRACE_ID).
const agentCursor = "cursor"

// cursorHookInput is the subset of Cursor's hook payloads we consume. Cursor
// delivers the full JSON on stdin; unknown fields are ignored. Field names
// follow the documented Cursor hooks schema (docs.cursor.com/agent/hooks).
//
// Note: afterShellExecution carries no cwd and no exit code — only command,
// output, and duration — so recorded Cursor commands use workspace_roots[0] as
// the cwd and leave the exit status unknown rather than fabricating one.
type cursorHookInput struct {
	HookEventName  string   `json:"hook_event_name"`
	Command        string   `json:"command"`         // afterShellExecution
	Duration       *int64   `json:"duration"`        // afterShellExecution, milliseconds
	ConversationID string   `json:"conversation_id"` // correlates prompt <-> commands
	WorkspaceRoots []string `json:"workspace_roots"`
	Prompt         string   `json:"prompt"` // beforeSubmitPrompt
}

// cursorSession maps a Cursor conversation id to a yore session id (prefixed so
// it never collides with a shell session).
func cursorSession(conversationID string) string {
	if conversationID == "" {
		return ""
	}
	return "cursor-" + conversationID
}

// runHookCursor ingests a Cursor afterShellExecution payload and records the
// command it ran, tagged cursor. Like every hook it never blocks and always
// exits 0; an empty command or malformed JSON is a silent no-op.
func runHookCursor() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in cursorHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return
	}
	dir := stateDir()
	session := cursorSession(in.ConversationID)
	cwd := ""
	if len(in.WorkspaceRoots) > 0 {
		cwd = in.WorkspaceRoots[0]
	}
	r := rec.Record{
		Session: session,
		Cmd:     cmd,
		Cwd:     cwd,
		Tag:     agentCursor,
	}
	if in.Duration != nil && *in.Duration >= 0 {
		r.DurMs = in.Duration
	}
	// Exit status is not carried by Cursor's afterShellExecution, so it stays nil.
	stampPrompt(dir, session, &r)
	spoolRecord(dir, r)
}

// runHookCursorPrompt ingests a Cursor beforeSubmitPrompt payload and records it
// as the conversation's current prompt (for the shell-exec hook to attach).
//
// It MUST let the prompt through: Cursor treats this hook's stdout as the
// allow/deny decision, so it always prints {"continue": true} and exits 0 — even
// when it records nothing (empty prompt, or a secret-bearing one it drops).
func runHookCursorPrompt() {
	// Always allow the prompt, whatever happens below.
	defer fmt.Println(`{"continue": true}`)

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in cursorHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	cwd := ""
	if len(in.WorkspaceRoots) > 0 {
		cwd = in.WorkspaceRoots[0]
	}
	recordPrompt(stateDir(), cursorSession(in.ConversationID), agentCursor, cwd, in.Prompt)
}

// --- `yore init cursor`: install the capture hooks --------------------------

// cursorHooksPath resolves Cursor's hooks config: the project .cursor/hooks.json
// with --project, else the user ~/.cursor/hooks.json.
func cursorHooksPath(project bool) (string, error) {
	if project {
		return filepath.Join(".cursor", "hooks.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "hooks.json"), nil
}

// cursor hook commands, this binary ingesting the payload on stdin.
func cmdCursorShell(bin string) string  { return bin + " hook cursor" }
func cmdCursorPrompt(bin string) string { return bin + " hook cursor-prompt" }

// mergeCursorHook adds a Cursor hook block ({"command": …}) under
// hooks[event] unless one already runs command. Returns whether it changed
// anything, and seeds the required "version": 1 key.
func mergeCursorHook(cfg map[string]any, event, command string) bool {
	if _, ok := cfg["version"]; !ok {
		cfg["version"] = 1
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	blocks, _ := hooks[event].([]any)
	for _, blk := range blocks {
		if bm, ok := blk.(map[string]any); ok && bm["command"] == command {
			return false
		}
	}
	blocks = append(blocks, map[string]any{"command": command})
	hooks[event] = blocks
	cfg["hooks"] = hooks
	return true
}

// mergeCursorHooks installs both capture hooks: afterShellExecution records the
// command, beforeSubmitPrompt records the prompt it served. Idempotent.
func mergeCursorHooks(cfg map[string]any, bin string) (added bool) {
	a := mergeCursorHook(cfg, "afterShellExecution", cmdCursorShell(bin))
	b := mergeCursorHook(cfg, "beforeSubmitPrompt", cmdCursorPrompt(bin))
	return a || b
}

// cursorCaptureInstalled reports whether ~/.cursor/hooks.json has yore's
// afterShellExecution capture hook (for the given bin).
func cursorCaptureInstalled(bin string) bool {
	path, err := cursorHooksPath(false)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	hooks, _ := cfg["hooks"].(map[string]any)
	blocks, _ := hooks["afterShellExecution"].([]any)
	for _, blk := range blocks {
		if bm, ok := blk.(map[string]any); ok && bm["command"] == cmdCursorShell(bin) {
			return true
		}
	}
	return false
}

// installCursorHooks merges yore's capture hooks into Cursor's hooks.json,
// preserving everything else. A malformed existing file is left alone.
func installCursorHooks(bin string, project bool, u *ui) error {
	path, err := cursorHooksPath(project)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		if json.Unmarshal(data, &cfg) != nil {
			u.step("skipped hook install (existing hooks.json is not valid JSON)", path)
			return nil //nolint:nilerr // intentional: leave the unparseable file untouched
		}
	} else if !os.IsNotExist(rerr) {
		return rerr
	}
	if !mergeCursorHooks(cfg, bin) {
		u.step("Cursor capture hooks already installed", path)
		return nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return err
	}
	u.step("installed afterShellExecution + beforeSubmitPrompt hooks", path)
	return nil
}
