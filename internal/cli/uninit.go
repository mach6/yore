package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"yore/internal/shell"
)

// newUninitCmd reverses `yore init <agent>`: it removes only yore's capture hooks
// and MCP-server registration from each agent's config, preserving everything
// else (including the agent's own auth).
func newUninitCmd() *cobra.Command {
	var bin string
	var project bool
	cmd := &cobra.Command{
		Use:   "uninit claude-code|cursor|opencode|codex|devin",
		Short: "Remove the hooks and MCP server that `yore init` added for an agent",
		Long: "uninit reverses `yore init <agent>`: it removes only yore's capture hooks\n" +
			"and MCP-server registration, preserving everything else in each config\n" +
			"file (including the agent's own auth). Pass the same --bin you used for\n" +
			"init, and --project to target the project-scoped config.\n\n" +
			"Shells (zsh/bash) aren't agents — remove the `eval \"$(yore init …)\"` line\n" +
			"from your rc file by hand.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []cobra.Completion{"claude-code", "cursor", "opencode", "codex", "devin"},
		RunE: func(_ *cobra.Command, args []string) error {
			switch args[0] {
			case "claude-code":
				return code(runUninitClaudeCode(bin, project))
			case "cursor":
				return code(runUninitCursor(bin, project))
			case "opencode":
				return code(runUninitOpenCode(project))
			case "codex":
				return code(runUninitCodex(bin, project))
			case "devin":
				return code(runUninitDevin(bin, project))
			}
			return fmt.Errorf("unknown agent %q (want: claude-code, cursor, opencode, codex, devin)", args[0])
		},
	}
	cmd.Flags().StringVar(&bin, "bin", shell.DefaultBin, "binary name the hooks invoke (match what init used)")
	cmd.Flags().BoolVar(&project, "project", false, "target the project-scoped config instead of the user file")
	return cmd
}

// --- generic removal helpers -----------------------------------------------

// editJSONFile reads path as a JSON object, applies fn, and rewrites it only when
// fn reports a change. A missing file is a no-op (nothing to remove); a malformed
// file is left untouched. Existing files keep their mode (WriteFile does not
// change it), so an auth-bearing config's permissions are preserved.
func editJSONFile(path string, fn func(map[string]any) bool) (bool, error) {
	data, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		return false, nil
	}
	if rerr != nil {
		return false, rerr
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return false, nil //nolint:nilerr // intentional: leave an unparseable file untouched
	}
	if !fn(cfg) {
		return false, nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o600)
}

// removeHookCmd deletes the hook that runs `command` from settings.hooks[event]
// (the Claude/Devin schema: event -> [{matcher, hooks:[{command}]}]). A block
// left with no hooks is dropped, an emptied event is dropped, and an emptied
// "hooks" map is removed. Returns whether anything was removed.
func removeHookCmd(settings map[string]any, event, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	blocks, _ := hooks[event].([]any)
	if blocks == nil {
		return false
	}
	kept := make([]any, 0, len(blocks))
	removed := false
	for _, blk := range blocks {
		bm, ok := blk.(map[string]any)
		if !ok {
			kept = append(kept, blk)
			continue
		}
		inner, _ := bm["hooks"].([]any)
		innerKept := make([]any, 0, len(inner))
		for _, h := range inner {
			if hm, ok := h.(map[string]any); ok && hm["command"] == command {
				removed = true
				continue
			}
			innerKept = append(innerKept, h)
		}
		// A block whose hooks we entirely removed is ours — drop it wholesale.
		if len(inner) > 0 && len(innerKept) == 0 {
			continue
		}
		bm["hooks"] = innerKept
		kept = append(kept, bm)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return true
}

// removeMcpServer deletes cfg.mcpServers[name], dropping an emptied map. Returns
// whether it removed anything.
func removeMcpServer(cfg map[string]any, name string) bool {
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		return false
	}
	if _, ok := servers[name]; !ok {
		return false
	}
	delete(servers, name)
	if len(servers) == 0 {
		delete(cfg, "mcpServers")
	}
	return true
}

// removeCursorHook deletes the Cursor hook block {"command": command} from
// cfg.hooks[event] (Cursor's flat block shape), pruning empties.
func removeCursorHook(cfg map[string]any, event, command string) bool {
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	blocks, _ := hooks[event].([]any)
	kept := make([]any, 0, len(blocks))
	removed := false
	for _, blk := range blocks {
		if bm, ok := blk.(map[string]any); ok && bm["command"] == command {
			removed = true
			continue
		}
		kept = append(kept, blk)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	if len(hooks) == 0 {
		delete(cfg, "hooks")
	}
	return true
}

// removeOpenCodeMcp deletes yore from cfg.mcp (OpenCode's local-server map).
func removeOpenCodeMcp(cfg map[string]any) bool {
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		return false
	}
	if _, ok := mcp["yore"]; !ok {
		return false
	}
	delete(mcp, "yore")
	if len(mcp) == 0 {
		delete(cfg, "mcp")
	}
	return true
}

// reportRemoval prints a uniform "removed X" / "no X to remove" step.
func reportRemoval(u *ui, changed bool, what, path string) {
	if changed {
		u.step("removed "+what, path)
	} else {
		u.step("no "+what+" to remove", path)
	}
}

// --- per-agent uninstall ----------------------------------------------------

func runUninitClaudeCode(bin string, project bool) int {
	u := newUI()
	u.title("yore uninit claude-code")

	spath, err := claudeSettingsPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	changed, err := editJSONFile(spath, func(s map[string]any) bool {
		pre := removeHookCmd(s, "PreToolUse", cmdPreToolUse(bin))
		post := removeHookCmd(s, "PostToolUse", cmdPostToolUse(bin))
		fail := removeHookCmd(s, "PostToolUseFailure", cmdPostToolUseFailure(bin))
		prompt := removeHookCmd(s, "UserPromptSubmit", cmdUserPromptSubmit(bin))
		return pre || post || fail || prompt
	})
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	reportRemoval(u, changed, "capture hooks", spath)

	mpath, err := mcpConfigPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	mchanged, err := editJSONFile(mpath, func(c map[string]any) bool { return removeMcpServer(c, "yore") })
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	reportRemoval(u, mchanged, "MCP server", mpath)
	u.blank()
	return 0
}

func runUninitCursor(bin string, project bool) int {
	u := newUI()
	u.title("yore uninit cursor")

	hpath, err := cursorHooksPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	changed, err := editJSONFile(hpath, func(c map[string]any) bool {
		a := removeCursorHook(c, "afterShellExecution", cmdCursorShell(bin))
		b := removeCursorHook(c, "beforeSubmitPrompt", cmdCursorPrompt(bin))
		return a || b
	})
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	reportRemoval(u, changed, "capture hooks", hpath)

	mpath, err := cursorMcpPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	mchanged, err := editJSONFile(mpath, func(c map[string]any) bool { return removeMcpServer(c, "yore") })
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	reportRemoval(u, mchanged, "MCP server", mpath)
	u.blank()
	return 0
}

func runUninitOpenCode(project bool) int {
	u := newUI()
	u.title("yore uninit opencode")

	ppath, err := opencodePluginPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	switch rerr := os.Remove(ppath); {
	case rerr == nil:
		u.step("removed capture plugin", ppath)
	case os.IsNotExist(rerr):
		u.step("no capture plugin to remove", ppath)
	default:
		u.fail(rerr.Error())
		return 1
	}

	cpath, err := opencodeConfigPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	mchanged, err := editJSONFile(cpath, removeOpenCodeMcp)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	reportRemoval(u, mchanged, "MCP server", cpath)
	u.blank()
	return 0
}

func runUninitCodex(bin string, project bool) int {
	u := newUI()
	u.title("yore uninit codex")

	path, err := codexConfigPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	data, rerr := os.ReadFile(path)
	if os.IsNotExist(rerr) {
		u.step("nothing to remove (no config.toml)", path)
		u.blank()
		return 0
	}
	if rerr != nil {
		u.fail(rerr.Error())
		return 1
	}
	// The installer appends deterministic marked blocks; remove those exact
	// blocks. If the file was hand-reformatted since, the block won't match and
	// we report nothing removed rather than guess.
	content := string(data)
	stripped := strings.Replace(content, codexHooksBlock(bin), "", 1)
	stripped = strings.Replace(stripped, codexMcpBlock(bin), "", 1)
	if stripped == content {
		u.step("no yore hooks/MCP found to remove", path)
		u.blank()
		return 0
	}
	if err := os.WriteFile(path, []byte(stripped), 0o644); err != nil {
		u.fail(err.Error())
		return 1
	}
	u.step("removed capture hooks + MCP server", path)
	u.blank()
	return 0
}

func runUninitDevin(bin string, project bool) int {
	u := newUI()
	u.title("yore uninit devin")

	path, err := devinConfigPath(project)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	changed, err := editJSONFile(path, func(c map[string]any) bool {
		pre := removeHookCmd(c, "PreToolUse", cmdDevinPre(bin))
		post := removeHookCmd(c, "PostToolUse", cmdDevinPost(bin))
		prompt := removeHookCmd(c, "UserPromptSubmit", cmdDevinPrompt(bin))
		mcp := removeMcpServer(c, "yore")
		return pre || post || prompt || mcp
	})
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	reportRemoval(u, changed, "capture hooks + MCP server", path)
	u.blank()
	return 0
}
