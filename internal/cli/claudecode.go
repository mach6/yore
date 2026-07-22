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
	// Prompt is only present on a UserPromptSubmit payload (unused here; reserved
	// for prompt tracing).
	Prompt string `json:"prompt"`
}

// runHookClaudeCode ingests a Claude Code PostToolUse payload from stdin and
// records the Bash command it describes, tagged claude-code.
//
// Contract mirrors the shell fast path: it NEVER blocks, NEVER prints, and
// ALWAYS exits 0 — a hook that errored or stalled would disrupt the agent it is
// observing. A non-Bash tool call, an empty command, or malformed JSON is a
// silent no-op.
//
// The command's exit status is NOT recorded: the documented PostToolUse payload
// does not carry it (success and failure are separate events in Claude Code),
// so the record's exit stays unknown rather than fabricated. Capturing exit
// status is a follow-up once that event contract is pinned down.
func runHookClaudeCode() {
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
	spoolRecord(stateDir(), rec.Record{
		Session: in.SessionID,
		Cmd:     in.ToolInput.Command,
		Cwd:     in.Cwd,
		Tag:     agentClaudeCode,
	})
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

// claudeHookCommand is the shell command Claude Code runs for the hook: this
// binary, ingesting the payload on stdin.
func claudeHookCommand(bin string) string { return bin + " hook claude-code" }

// buildClaudeHookBlock returns the PostToolUse matcher block that runs our hook
// on every Bash tool call.
func buildClaudeHookBlock(bin string) map[string]any {
	return map[string]any{
		"matcher": "Bash",
		"hooks": []any{
			map[string]any{"type": "command", "command": claudeHookCommand(bin)},
		},
	}
}

// mergeClaudeHook adds our PostToolUse hook to an existing settings map without
// disturbing anything else. It is idempotent: if a block already runs our hook
// command, the map is returned unchanged with added=false. Everything is kept
// as generic JSON so unknown settings keys survive the round trip untouched.
func mergeClaudeHook(settings map[string]any, bin string) (added bool) {
	want := claudeHookCommand(bin)

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	post, _ := hooks["PostToolUse"].([]any)

	// Already present? Scan every matcher block's hook commands for ours.
	for _, blk := range post {
		bm, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := bm["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if ok && hm["command"] == want {
				return false
			}
		}
	}

	post = append(post, buildClaudeHookBlock(bin))
	hooks["PostToolUse"] = post
	settings["hooks"] = hooks
	return true
}

// runInitClaudeCode installs (or, with print, just shows) the Claude Code
// PostToolUse hook that records each Bash command the agent runs.
func runInitClaudeCode(bin string, project, printOnly bool) int {
	u := newUI()

	if printOnly {
		// Emit just the hook fragment on stdout, for a user who prefers to merge
		// it into settings.json by hand.
		frag := map[string]any{
			"hooks": map[string]any{
				"PostToolUse": []any{buildClaudeHookBlock(bin)},
			},
		}
		b, _ := json.MarshalIndent(frag, "", "  ")
		fmt.Println(string(b))
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

	u.step("installed PostToolUse hook", path)
	u.step("captures every Bash command Claude Code runs", "tagged "+agentClaudeCode)
	u.blank()
	u.next("see agent commands after Claude Code runs some:",
		"yore search --tag "+agentClaudeCode,
		"hb   (browse)")
	return 0
}
