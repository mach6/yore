package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/redact"
)

// agentClaudeCode is the executor tag stamped on commands captured from Claude
// Code's hook, matching the auto-detected value the shell path uses.
const agentClaudeCode = "claude-code"

// claudeHookInput is the subset of a Claude Code hook payload we consume. The
// full JSON is delivered on the hook process's stdin; unknown fields are
// ignored. Field names follow the documented hook schema.
type claudeHookInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	// ToolResponse is the tool's structured result. For Bash it carries an
	// exit_code; kept raw so a non-object response (other tools) never fails the
	// top-level decode.
	ToolResponse json.RawMessage `json:"tool_response"`
	// ISO-8601 tool timing, when the payload provides it; duration is derived.
	ToolStartTime string `json:"tool_start_time"`
	ToolEndTime   string `json:"tool_end_time"`
	// Prompt is only present on a UserPromptSubmit payload (unused here; reserved
	// for prompt tracing).
	Prompt string `json:"prompt"`
}

// responseExitCode extracts tool_response.exit_code when present, tolerating a
// non-object response (returns nil then).
func responseExitCode(raw json.RawMessage) *int {
	if len(raw) == 0 {
		return nil
	}
	var tr struct {
		ExitCode *int `json:"exit_code"`
	}
	if json.Unmarshal(raw, &tr) == nil {
		return tr.ExitCode
	}
	return nil
}

// deriveExit resolves a command's exit status: the payload's explicit exit_code
// wins; otherwise the event decides — PostToolUse fires on success (0),
// PostToolUseFailure on failure (nonzero, unknown code -> 1).
func deriveExit(in claudeHookInput, failed bool) *int {
	if c := responseExitCode(in.ToolResponse); c != nil {
		return c
	}
	code := 0
	if failed {
		code = 1
	}
	return &code
}

// deriveDurMs computes the command's wall time from tool_start_time/end_time
// when both are present and well-formed (ok=false otherwise, leaving it nil).
func deriveDurMs(in claudeHookInput) (int64, bool) {
	if in.ToolStartTime == "" || in.ToolEndTime == "" {
		return 0, false
	}
	start, err1 := time.Parse(time.RFC3339, in.ToolStartTime)
	end, err2 := time.Parse(time.RFC3339, in.ToolEndTime)
	if err1 != nil || err2 != nil || end.Before(start) {
		return 0, false
	}
	return end.Sub(start).Milliseconds(), true
}

// runHookClaudeCode ingests a Claude Code PostToolUse (success) payload.
// runHookClaudeCodeFailure ingests a PostToolUseFailure payload; the only
// difference is the exit status defaulted when the payload omits an explicit
// exit_code.
func runHookClaudeCode()        { ingestClaudeTool(false) }
func runHookClaudeCodeFailure() { ingestClaudeTool(true) }

// ingestClaudeTool records the Bash command described by a Claude Code tool hook
// payload on stdin, tagged claude-code, with its exit status (and duration when
// the payload timestamps allow it).
//
// Contract mirrors the shell fast path: it NEVER blocks, NEVER prints, and
// ALWAYS exits 0 — a hook that errored or stalled would disrupt the agent it is
// observing. A non-Bash tool call, an empty command, or malformed JSON is a
// silent no-op. failed selects the exit default (see deriveExit).
func ingestClaudeTool(failed bool) {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	// Only Bash tool calls carry a shell command. An empty tool_name is treated
	// permissively (older payloads), but a named non-Bash tool is skipped.
	if in.ToolName != "" && in.ToolName != "Bash" {
		return
	}
	if strings.TrimSpace(in.ToolInput.Command) == "" {
		return
	}
	dir := stateDir()
	r := rec.Record{
		Session: in.SessionID,
		Cmd:     in.ToolInput.Command,
		Cwd:     in.Cwd,
		Tag:     agentClaudeCode,
		Exit:    deriveExit(in, failed),
	}
	if d, ok := deriveDurMs(in); ok {
		r.DurMs = &d
	}
	// Stamp the prompt this command served, if the UserPromptSubmit hook recorded
	// one for this session. Best-effort: absent state just leaves it untraced.
	if ps, ok := loadPromptState(dir, in.SessionID); ok {
		r.PromptID = ps.ID
		r.Prompt = ps.Text
	}
	spoolRecord(dir, r)
}

// promptState is the latest user prompt seen for a session, persisted by the
// UserPromptSubmit hook so the separate PostToolUse hook processes can stamp it
// onto the commands it triggered.
type promptState struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Ms   int64  `json:"ms"`
}

// promptStatePath is where a session's current prompt is held. Session ids are
// opaque; hash to a safe filename.
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

// runHookClaudePrompt ingests a Claude Code UserPromptSubmit payload from stdin
// and records it as the session's current prompt, for the PostToolUse hook to
// attach to the commands that follow. Like the other hooks it never blocks,
// never prints, and always exits 0.
func runHookClaudePrompt() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	text := strings.TrimSpace(in.Prompt)
	if text == "" || in.SessionID == "" {
		return
	}
	// The redaction gate also guards prompts: a prompt containing a secret must
	// not be persisted or later attached to a synced record.
	dir := stateDir()
	cfg, _ := config.Load(dir)
	if filter, _ := redact.Load(dir, cfg.IgnorePatterns, cfg.IgnoreDirs); filter.Sensitive(text) {
		return
	}
	ps := promptState{ID: rec.NewID(), Text: text, Ms: time.Now().UnixMilli()}
	b, err := json.Marshal(ps)
	if err != nil {
		return
	}
	path := promptStatePath(dir, in.SessionID)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// --- `yore init claude-code`: install the capture hook -----------------------

// claudeSettingsPath resolves the Claude Code settings file to edit: the
// project file (.claude/settings.json under the cwd) when project is true, else
// the user file (~/.claude/settings.json).
func claudeSettingsPath(project bool) (string, error) {
	if project {
		return filepath.Join(".claude", "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// hook commands Claude Code runs, this binary ingesting the payload on stdin.
func cmdPostToolUse(bin string) string        { return bin + " hook claude-code" }
func cmdPostToolUseFailure(bin string) string { return bin + " hook claude-code-failure" }
func cmdUserPromptSubmit(bin string) string   { return bin + " hook claude-prompt" }

// buildMatcherBlock returns a hook block that runs command for the given tool
// matcher (an empty matcher fires on every event of that kind).
func buildMatcherBlock(matcher, command string) map[string]any {
	blk := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": command}},
	}
	if matcher != "" {
		blk["matcher"] = matcher
	}
	return blk
}

// mergeHookInto adds a hook block for command under settings.hooks[event] unless
// a block there already runs command. Returns whether it added anything. Keeps
// everything as generic JSON so unrelated settings survive untouched.
func mergeHookInto(settings map[string]any, event, matcher, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	blocks, _ := hooks[event].([]any)
	for _, blk := range blocks {
		bm, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := bm["hooks"].([]any)
		for _, h := range inner {
			if hm, ok := h.(map[string]any); ok && hm["command"] == command {
				return false
			}
		}
	}
	blocks = append(blocks, buildMatcherBlock(matcher, command))
	hooks[event] = blocks
	settings["hooks"] = hooks
	return true
}

// mergeClaudeHook installs the capture hooks: PostToolUse(Bash) records a
// successful command, PostToolUseFailure(Bash) records a failed one (so exit
// status is captured), and UserPromptSubmit records the prompt each served.
// Idempotent; returns whether anything changed.
func mergeClaudeHook(settings map[string]any, bin string) (added bool) {
	a := mergeHookInto(settings, "PostToolUse", "Bash", cmdPostToolUse(bin))
	f := mergeHookInto(settings, "PostToolUseFailure", "Bash", cmdPostToolUseFailure(bin))
	b := mergeHookInto(settings, "UserPromptSubmit", "", cmdUserPromptSubmit(bin))
	return a || f || b
}

// mcpServerEntry is the stdio MCP server registration Claude Code (and Cursor)
// launch to reach yore's history. No "type" is needed: an entry with a command
// and no url is a stdio server.
func mcpServerEntry(bin string) map[string]any {
	return map[string]any{"command": bin, "args": []any{"mcp-serve"}}
}

// mergeMcpServer registers yore under mcpServers unless it is already there.
// Returns whether it changed anything. Keeps everything as generic JSON so the
// rest of the (often large) config survives untouched.
func mergeMcpServer(cfg map[string]any, bin string) bool {
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["yore"]; ok {
		return false
	}
	servers["yore"] = mcpServerEntry(bin)
	cfg["mcpServers"] = servers
	return true
}

// mcpConfigPath resolves the file holding MCP server registrations: the
// project-scoped .mcp.json (version-controlled) with --project, else the
// user-scoped ~/.claude.json.
func mcpConfigPath(project bool) (string, error) {
	if project {
		return ".mcp.json", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// installMcpServer registers yore's MCP server in the Claude Code config file
// (~/.claude.json, or ./.mcp.json with --project).
func installMcpServer(bin string, project bool, u *ui) error {
	path, err := mcpConfigPath(project)
	if err != nil {
		return err
	}
	return registerMcpAt(path, bin, u)
}

// registerMcpAt merges yore's MCP server into the mcpServers map of the JSON
// config at path, preserving everything else. A malformed existing file is left
// alone. Shared by the Claude Code and Cursor init paths.
func registerMcpAt(path, bin string, u *ui) error {
	cfg := map[string]any{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		if json.Unmarshal(data, &cfg) != nil {
			// A malformed existing config is non-fatal: skip registration rather
			// than clobber a file we can't safely parse.
			u.step("skipped MCP registration (existing file is not valid JSON)", path)
			return nil //nolint:nilerr // intentional: leave the unparseable file untouched
		}
	} else if !os.IsNotExist(rerr) {
		return rerr
	}
	if !mergeMcpServer(cfg, bin) {
		u.step("MCP server already registered", path)
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
	u.step("registered MCP server", path)
	return nil
}

// mcpRegistered reports whether the JSON config at path has a "yore" entry in
// its mcpServers map.
func mcpRegistered(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return false
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	_, ok := servers["yore"]
	return ok
}

// hookCmdPresent reports whether the Claude settings config runs command under
// hooks[event].
func hookCmdPresent(cfg map[string]any, event, command string) bool {
	hooks, _ := cfg["hooks"].(map[string]any)
	blocks, _ := hooks[event].([]any)
	for _, blk := range blocks {
		bm, _ := blk.(map[string]any)
		inner, _ := bm["hooks"].([]any)
		for _, h := range inner {
			if hm, ok := h.(map[string]any); ok && hm["command"] == command {
				return true
			}
		}
	}
	return false
}

// claudeCaptureInstalled reports whether ~/.claude/settings.json has yore's
// PostToolUse capture hook (for the given bin).
func claudeCaptureInstalled(bin string) bool {
	path, err := claudeSettingsPath(false)
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
	return hookCmdPresent(cfg, "PostToolUse", cmdPostToolUse(bin))
}

// cursorMcpPath resolves Cursor's MCP config: the project .cursor/mcp.json with
// --project, else the user ~/.cursor/mcp.json.
func cursorMcpPath(project bool) (string, error) {
	if project {
		return filepath.Join(".cursor", "mcp.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

// runInitCursor registers yore's MCP server with Cursor so its agent can query
// your (cross-machine) history. Cursor auto-detects interactively via
// $CURSOR_TRACE_ID, so no capture hook is installed here.
func runInitCursor(bin string, project bool) int {
	u := newUI()
	u.title("yore init cursor")
	path, err := cursorMcpPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	if err := registerMcpAt(path, bin, u); err != nil {
		u.fail(err.Error())
		return 1
	}
	u.step("Cursor can now query your history via MCP", "cross-machine, end-to-end encrypted")
	u.blank()
	u.next("ask Cursor, e.g.:",
		`"what commands failed in this project recently?"`,
		`"have I run this migration on any machine?"`)
	return 0
}

// runInitClaudeCode installs (or, with print, just shows) the Claude Code
// capture hooks and registers yore's MCP server so the agent can query history.
func runInitClaudeCode(bin string, project, printOnly bool) int {
	u := newUI()

	if printOnly {
		// Emit the hook fragment (for settings.json) and the MCP fragment (for
		// ~/.claude.json), for a user who prefers to merge them by hand.
		frag := map[string]any{}
		mergeClaudeHook(frag, bin)
		b, _ := json.MarshalIndent(frag, "", "  ")
		fmt.Println("// ~/.claude/settings.json")
		fmt.Println(string(b))
		mcp := map[string]any{}
		mergeMcpServer(mcp, bin)
		mb, _ := json.MarshalIndent(mcp, "", "  ")
		fmt.Println("// ~/.claude.json")
		fmt.Println(string(mb))
		return 0
	}

	path, err := claudeSettingsPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	u.title("yore init claude-code")

	settings := map[string]any{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		if json.Unmarshal(data, &settings) != nil {
			u.fail("existing " + path + " is not valid JSON — fix or move it first")
			return 1
		}
	} else if !os.IsNotExist(rerr) {
		u.fail(rerr.Error())
		return 1
	}

	if !mergeClaudeHook(settings, bin) {
		u.step("hook already installed", path)
		u.blank()
		return 0
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		u.fail(err.Error())
		return 1
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		u.fail(err.Error())
		return 1
	}

	u.step("installed PostToolUse + PostToolUseFailure + UserPromptSubmit hooks", path)
	u.step("captures every Bash command Claude Code runs", "tagged "+agentClaudeCode+", with exit status, traced to its prompt")

	// Register the MCP server so the agent can query history back (across all
	// machines). Non-fatal: capture still works without it.
	if err := installMcpServer(bin, project, u); err != nil {
		u.step("could not register MCP server (capture still works)", err.Error())
	}

	u.blank()
	u.next("see agent commands after Claude Code runs some:",
		"yore search --tag "+agentClaudeCode,
		"hb   (browse)")
	u.next("or ask Claude Code (MCP), e.g.:",
		`"what commands failed in this project recently?"`,
		`"have I run this migration on any machine?"`)
	return 0
}
