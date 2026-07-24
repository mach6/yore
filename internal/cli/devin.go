package cli

import (
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

// agentDevin is the executor tag stamped on commands captured from the Devin
// CLI's hooks.
const agentDevin = "devin"

// devinExecTool is Devin's shell tool name (its `exec` tool), the matcher yore's
// capture hook binds to — Devin's analogue of Claude Code's "Bash".
const devinExecTool = "exec"

// Devin CLI hook payloads share Claude Code's shape (session_id, tool_name,
// tool_input.command, tool_response, and UserPromptSubmit's prompt), so the same
// claudeHookInput struct decodes them. Two differences are honored below: the
// shell tool is `exec` (not Bash), and tool_response reports `success` (a bool)
// rather than a numeric exit_code — there is no separate failure event, so
// PostToolUse fires on success AND failure and the outcome comes from `success`.

// devinSession namespaces a Devin session id so it can't collide with a shell or
// another agent's session.
func devinSession(id string) string {
	if id == "" {
		return ""
	}
	return "devin-" + id
}

// devinCwd resolves the command's directory: the payload's cwd when present,
// else Devin's DEVIN_PROJECT_DIR (the repo root it sets for hook processes).
func devinCwd(in claudeHookInput) string {
	if in.Cwd != "" {
		return in.Cwd
	}
	return os.Getenv("DEVIN_PROJECT_DIR")
}

// responseSuccess extracts tool_response.success (Devin's boolean outcome),
// tolerating a non-object response (returns nil then).
func responseSuccess(raw json.RawMessage) *bool {
	if len(raw) == 0 {
		return nil
	}
	var tr struct {
		Success *bool `json:"success"`
	}
	if json.Unmarshal(raw, &tr) == nil {
		return tr.Success
	}
	return nil
}

// deriveDevinExit resolves a command's exit status from a Devin payload: an
// explicit numeric exit_code wins (forward-compat), else tool_response.success
// maps to 0/1; unknown when neither is present.
func deriveDevinExit(in claudeHookInput) *int {
	if c := responseExitCode(in.ToolResponse); c != nil {
		return c
	}
	if ok := responseSuccess(in.ToolResponse); ok != nil {
		code := 0
		if !*ok {
			code = 1
		}
		return &code
	}
	return nil
}

// runHookDevin records an exec command from a Devin CLI PostToolUse hook payload,
// tagged devin, with its outcome (and duration when the PreToolUse hook stamped a
// start). Silent no-op on a non-exec tool, empty command, or malformed JSON;
// always exits 0 so it never disrupts the agent it observes.
func runHookDevin() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	if in.ToolName != "" && in.ToolName != devinExecTool {
		return
	}
	if strings.TrimSpace(in.ToolInput.Command) == "" {
		return
	}
	dir := stateDir()
	session := devinSession(in.SessionID)
	r := rec.Record{
		Session: session,
		Cmd:     in.ToolInput.Command,
		Cwd:     devinCwd(in),
		Tag:     agentDevin,
		Exit:    deriveDevinExit(in),
	}
	// Devin's payload carries no tool timing, so the PreToolUse hook stamps a start
	// and we take the delta here (anchoring the record to the real start).
	if start, ok := loadCmdStart(dir, session, in.ToolInput.Command); ok {
		if now := time.Now().UnixMilli(); now >= start {
			d := now - start
			r.StartMs = start
			r.DurMs = &d
		}
	}
	if ps, ok := loadPromptState(dir, session); ok {
		r.PromptID = ps.ID
		r.Prompt = ps.Text
	}
	spoolRecord(dir, r)
}

// runHookDevinPre stamps an exec command's start time from a Devin PreToolUse
// hook, so the matching PostToolUse can report a real duration. Never blocks,
// never prints, always exits 0.
func runHookDevinPre() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	if in.ToolName != "" && in.ToolName != devinExecTool {
		return
	}
	if strings.TrimSpace(in.ToolInput.Command) == "" {
		return
	}
	saveCmdStart(stateDir(), devinSession(in.SessionID), in.ToolInput.Command)
}

// runHookDevinPrompt records the session's current prompt from a Devin
// UserPromptSubmit hook, for the command hook to attach. Redaction-gated; never
// blocks or prints.
func runHookDevinPrompt() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	text := strings.TrimSpace(in.Prompt)
	session := devinSession(in.SessionID)
	if text == "" || session == "" {
		return
	}
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
	path := promptStatePath(dir, session)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// --- `yore init devin`: install the capture hooks + MCP server -------------

func cmdDevinPre(bin string) string    { return bin + " hook devin-pre" }
func cmdDevinPost(bin string) string   { return bin + " hook devin" }
func cmdDevinPrompt(bin string) string { return bin + " hook devin-prompt" }

// devinConfigPath resolves Devin's unified config file — the one that holds both
// hooks and mcpServers. The project file (.devin/config.json) with --project,
// else the user/global file at $XDG_CONFIG_HOME/devin/config.json (defaulting to
// ~/.config/devin/config.json). This is the same file that carries Devin's own
// auth; yore merges into it (preserving every other key) but never reads its
// secrets out.
func devinConfigPath(project bool) (string, error) {
	if project {
		return filepath.Join(".devin", "config.json"), nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "devin", "config.json"), nil
}

// mergeDevinConfig installs yore's capture hooks and MCP server into Devin's
// config map, preserving everything else: PreToolUse(exec) stamps a start time,
// PostToolUse(exec) records the command + outcome, UserPromptSubmit records the
// prompt each served, and mcpServers.yore registers the query-back server. The
// hook events nest under the config's "hooks" key (mergeHookInto's contract) and
// the server under "mcpServers" — exactly Devin's documented schema. Idempotent;
// returns whether anything changed.
func mergeDevinConfig(cfg map[string]any, bin string) bool {
	pre := mergeHookInto(cfg, "PreToolUse", devinExecTool, cmdDevinPre(bin))
	post := mergeHookInto(cfg, "PostToolUse", devinExecTool, cmdDevinPost(bin))
	prompt := mergeHookInto(cfg, "UserPromptSubmit", "", cmdDevinPrompt(bin))
	mcp := mergeMcpServer(cfg, bin)
	return pre || post || prompt || mcp
}

// runInitDevin installs (or, with printOnly, just prints) yore's Devin CLI
// capture hooks and MCP server in Devin's config.json — the global user file by
// default, or ./.devin/config.json with project. Any existing config (including
// Devin's auth) is preserved.
func runInitDevin(bin string, project, printOnly bool) int {
	u := newUI()

	if printOnly {
		cfg := map[string]any{}
		mergeDevinConfig(cfg, bin)
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println("// ~/.config/devin/config.json  (or ./.devin/config.json with --project)")
		fmt.Println(string(b))
		return 0
	}

	path, err := devinConfigPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	u.title("yore init devin")

	cfg := map[string]any{}
	if data, rerr := os.ReadFile(path); rerr == nil {
		if json.Unmarshal(data, &cfg) != nil {
			u.fail("existing " + path + " is not valid JSON — fix or move it first")
			return 1
		}
	} else if !os.IsNotExist(rerr) {
		u.fail(rerr.Error())
		return 1
	}

	if !mergeDevinConfig(cfg, bin) {
		u.step("hooks + MCP server already configured", path)
		u.blank()
		return 0
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		u.fail(err.Error())
		return 1
	}
	// 0o600 on create; an existing file keeps its own mode — this file can hold
	// Devin's auth, so never loosen it.
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		u.fail(err.Error())
		return 1
	}

	u.step("installed PreToolUse + PostToolUse + UserPromptSubmit hooks", path)
	u.step("captures every exec command Devin runs", "tagged "+agentDevin+", with exit status + duration, traced to its prompt")
	u.step("registered MCP server", path)
	u.step("Devin can also query your history via MCP", "cross-machine, end-to-end encrypted")
	u.blank()
	u.next("start a Devin session, then see agent commands with:",
		"yore search --executor "+agentDevin,
		"hb   (browse)")
	u.next("or ask Devin (MCP), e.g.:",
		`"what commands failed in this project recently?"`,
		`"have I run this migration on any machine?"`)
	return 0
}

// devinCaptureInstalled reports whether the global Devin config has yore's
// PostToolUse capture hook (for the given bin).
func devinCaptureInstalled(bin string) bool {
	path, err := devinConfigPath(false)
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
	return hookCmdPresent(cfg, "PostToolUse", cmdDevinPost(bin))
}

// devinMcpRegistered reports whether yore's MCP server is in the global Devin
// config.
func devinMcpRegistered() bool {
	path, err := devinConfigPath(false)
	if err != nil {
		return false
	}
	return mcpRegistered(path)
}
