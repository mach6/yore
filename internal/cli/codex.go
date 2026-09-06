package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"yore/internal/rec"
)

// agentCodex is the executor tag stamped on commands captured from OpenAI's
// Codex CLI hooks.
const agentCodex = "codex"

// Codex's hook payloads share Claude Code's shape (session_id, cwd, tool_name,
// tool_input.command, tool_response, and UserPromptSubmit's prompt), so the
// same claudeHookInput struct decodes them. Differences honored below: Codex
// has no separate failure event (PostToolUse fires on success AND failure), so
// exit status is taken only from tool_response.exit_code when present (never
// defaulted) and there is no start/end timestamp, so no duration.

func codexSession(id string) string {
	if id == "" {
		return ""
	}
	return "codex-" + id
}

// runHookCodex records a Bash command from a Codex PostToolUse hook payload,
// tagged codex. Silent no-op on a non-Bash tool, empty command, or malformed
// JSON; always exits 0.
func runHookCodex() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	if in.ToolName != "" && in.ToolName != "Bash" {
		return
	}
	cmd := strings.TrimSpace(in.ToolInput.Command)
	if cmd == "" {
		return
	}
	dir := stateDir()
	session := codexSession(in.SessionID)
	r := rec.Record{
		Session:  session,
		Cmd:      cmd,
		Cwd:      in.Cwd,
		Executor: agentCodex,
		// Codex has no failure event to distinguish outcomes, and its
		// tool_response exit field is not precisely documented, so exit is
		// best-effort: use tool_response.exit_code when present, else unknown.
		Exit: responseExitCode(in.ToolResponse),
	}
	stampPrompt(dir, session, &r)
	spoolRecord(dir, r)
}

// runHookCodexPrompt records the session's current prompt from a Codex
// UserPromptSubmit hook, for the command hook to attach. It never blocks
// (exit 0, no output; Codex only blocks on exit 2) and never prints.
func runHookCodexPrompt() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	recordPrompt(stateDir(), codexSession(in.SessionID), agentCodex, in.Cwd, in.Prompt)
}

// --- `yore init codex`: install the capture hooks --------------------------

func cmdCodexPost(bin string) string   { return bin + " hook codex" }
func cmdCodexPrompt(bin string) string { return bin + " hook codex-prompt" }

// codexConfigPath resolves Codex's config file: the project .codex/config.toml
// with --project, else the user ~/.codex/config.toml.
func codexConfigPath(project bool) (string, error) {
	if project {
		return filepath.Join(".codex", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

// codexHooksBlock renders the TOML capture-hook block for the given binary. The
// array-of-tables form is appended to config.toml; additive and order-
// independent, so it never disturbs existing config.
func codexHooksBlock(bin string) string {
	return "\n# yore: Codex capture hooks (added by `yore init codex`)\n" +
		"[[hooks.PostToolUse]]\n" +
		"matcher = \"^Bash$\"\n\n" +
		"[[hooks.PostToolUse.hooks]]\n" +
		"type = \"command\"\n" +
		"command = \"" + cmdCodexPost(bin) + "\"\n\n" +
		"[[hooks.UserPromptSubmit]]\n\n" +
		"[[hooks.UserPromptSubmit.hooks]]\n" +
		"type = \"command\"\n" +
		"command = \"" + cmdCodexPrompt(bin) + "\"\n"
}

// installCodexHooks appends yore's capture hooks to Codex's config.toml unless
// they are already present (idempotent).
func installCodexHooks(bin string, project bool, u *ui) error {
	path, err := codexConfigPath(project)
	if err != nil {
		return err
	}
	existing := ""
	if data, rerr := os.ReadFile(path); rerr == nil {
		existing = string(data)
	} else if !os.IsNotExist(rerr) {
		return rerr
	}
	if strings.Contains(existing, cmdCodexPost(bin)) {
		u.step("Codex capture hooks already installed", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Ensure a clean separation from any prior content.
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	if err := os.WriteFile(path, []byte(existing+codexHooksBlock(bin)), 0o644); err != nil {
		return err
	}
	u.step("installed PostToolUse + UserPromptSubmit hooks", path)
	return nil
}

// codexMcpMarker is the table header we search for to know MCP is registered.
const codexMcpMarker = "[mcp_servers.yore]"

// codexMcpBlock renders the TOML to register yore's MCP server with Codex.
func codexMcpBlock(bin string) string {
	return "\n# yore: MCP server (added by `yore init codex`)\n" +
		codexMcpMarker + "\n" +
		"command = \"" + bin + "\"\n" +
		"args = [\"mcp-serve\"]\n"
}

// registerCodexMcp appends the MCP-server registration to config.toml unless it
// is already present (idempotent).
func registerCodexMcp(bin string, project bool, u *ui) error {
	path, err := codexConfigPath(project)
	if err != nil {
		return err
	}
	existing := ""
	if data, rerr := os.ReadFile(path); rerr == nil {
		existing = string(data)
	} else if !os.IsNotExist(rerr) {
		return rerr
	}
	if strings.Contains(existing, codexMcpMarker) {
		u.step("MCP server already registered", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	if err := os.WriteFile(path, []byte(existing+codexMcpBlock(bin)), 0o644); err != nil {
		return err
	}
	u.step("registered MCP server", path)
	return nil
}

// codexMcpRegistered reports whether yore's MCP server is in ~/.codex/config.toml.
func codexMcpRegistered() bool {
	path, err := codexConfigPath(false)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), codexMcpMarker)
}

// codexHooksInstalled reports whether ~/.codex/config.toml has yore's capture
// hooks (for the given bin).
func codexHooksInstalled(bin string) bool {
	path, err := codexConfigPath(false)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), cmdCodexPost(bin))
}

// runInitCodex installs yore's Codex capture hooks so the commands its agent
// runs are recorded (tagged codex, traced to their prompt).
func runInitCodex(bin string, project bool) int {
	u := newUI()
	u.title("yore init codex")
	if err := installCodexHooks(bin, project, u); err != nil {
		u.fail(err.Error())
		return 1
	}
	if err := registerCodexMcp(bin, project, u); err != nil {
		u.step("could not register MCP server (capture still works)", err.Error())
	}
	u.step("captures every Bash command Codex runs", "tagged "+agentCodex+", traced to its prompt")
	u.step("Codex can also query your history via MCP", "cross-machine, end-to-end encrypted")
	u.blank()
	u.next("start a new Codex session, then see agent commands with:",
		"yore search --executor "+agentCodex,
		"hb   (browse)")
	return 0
}
