package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/rec"
)

// TestRunHookDevinCaptures covers Devin's exec capture: the command is tagged
// devin, exit is derived from tool_response.success, and the session is
// namespaced.
func TestRunHookDevinCaptures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	payload := `{
	  "hook_event_name": "PostToolUse",
	  "session_id": "d1",
	  "cwd": "/repo",
	  "tool_name": "exec",
	  "tool_input": {"command": "cargo build"},
	  "tool_response": {"success": false}
	}`
	feedStdin(t, payload, runHookDevin)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1, "one command captured")
	got := rows[0]
	assert.Equal(t, "cargo build", got.Cmd)
	assert.Equal(t, "/repo", got.Cwd)
	assert.Equal(t, "devin-d1", got.Session, "session must be namespaced")
	assert.Equal(t, agentDevin, got.Executor)
	require.NotNil(t, got.Exit, "success:false → a recorded failure")
	assert.Equal(t, 1, *got.Exit, "success:false maps to exit 1")
}

// TestRunHookDevinNonExecSkipped ensures a non-exec tool call is ignored.
func TestRunHookDevinNonExecSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"tool_name":"edit","tool_input":{"command":"x"}}`, runHookDevin)
	require.Empty(t, spooledRecords(t, dir), "a non-exec tool must not be recorded")
}

// TestRunHookDevinPreDurationAndPrompt covers the full pre→prompt→post flow: the
// PreToolUse stamp yields a real duration, and the cached prompt is attached.
func TestRunHookDevinPreDurationAndPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	const cmd = "pytest -q"
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"d2","prompt":"run the tests"}`, runHookDevinPrompt)
	feedStdin(t, `{"session_id":"d2","tool_name":"exec","tool_input":{"command":"`+cmd+`"}}`, runHookDevinPre)
	feedStdin(t, `{"session_id":"d2","tool_name":"exec","tool_input":{"command":"`+cmd+`"},"tool_response":{"success":true}}`, runHookDevin)

	// The prompt hook records the prompt itself — that is the point of it being a
	// record of its own — while the pre hook records nothing at all.
	rows := spooledRecords(t, dir)
	require.Len(t, rows, 2, "the prompt record plus one command; the pre hook adds neither")

	prompt := rows[0]
	require.Equal(t, rec.TypePrompt, prompt.Type)
	assert.Equal(t, "run the tests", prompt.Prompt)
	assert.Empty(t, prompt.Cmd, "a prompt record is not a command")

	cmdRow := rows[1]
	assert.Equal(t, 0, *cmdRow.Exit, "success:true → exit 0")
	require.NotNil(t, cmdRow.DurMs, "duration derived from the PreToolUse stamp")
	assert.Equal(t, prompt.ID, cmdRow.PromptID, "command traced to the cached prompt")
}

// TestMergeDevinConfigSchema verifies the emitted config.json shape: hooks nested
// under "hooks" (exec-matched), the MCP server under "mcpServers", and idempotency.
func TestMergeDevinConfigSchema(t *testing.T) {
	cfg := map[string]any{}
	require.True(t, mergeDevinConfig(cfg, "yore"), "first merge installs hooks + MCP")
	require.False(t, mergeDevinConfig(cfg, "yore"), "second merge is a no-op")

	hooks, ok := cfg["hooks"].(map[string]any)
	require.True(t, ok, "hooks must nest under the \"hooks\" key")
	for _, ev := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit"} {
		blocks, ok := hooks[ev].([]any)
		require.Truef(t, ok, "event %s must be present as an array", ev)
		require.Len(t, blocks, 1)
	}
	// PreToolUse/PostToolUse bind the exec matcher; the command is our hook.
	post := hooks["PostToolUse"].([]any)[0].(map[string]any)
	assert.Equal(t, devinExecTool, post["matcher"])
	inner := post["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, "command", inner["type"])
	assert.Equal(t, "yore hook devin", inner["command"])

	// The MCP server registers as a stdio server (command + args, no type).
	servers := cfg["mcpServers"].(map[string]any)
	yore := servers["yore"].(map[string]any)
	assert.Equal(t, "yore", yore["command"])
	assert.Equal(t, []any{"mcp-serve"}, yore["args"])
}

// TestMergeDevinConfigPreservesExisting proves the merge never clobbers config
// already in the file — including Devin's auth and unrelated hooks/servers.
func TestMergeDevinConfigPreservesExisting(t *testing.T) {
	cfg := map[string]any{
		"auth":        map[string]any{"token": "KEEP-ME"},
		"theme_mode":  "dark",
		"mcpServers":  map[string]any{"github": map[string]any{"command": "npx"}},
		"permissions": map[string]any{"deny": []any{"Exec(sudo)"}},
	}
	require.True(t, mergeDevinConfig(cfg, "yore"))

	// Pre-existing keys survive untouched.
	assert.Equal(t, "KEEP-ME", cfg["auth"].(map[string]any)["token"], "auth must be preserved")
	assert.Equal(t, "dark", cfg["theme_mode"])
	assert.NotNil(t, cfg["permissions"])
	servers := cfg["mcpServers"].(map[string]any)
	assert.NotNil(t, servers["github"], "existing MCP server kept")
	assert.NotNil(t, servers["yore"], "ours appended")
}

// TestDevinConfigPath resolves the global (XDG) and project config files.
func TestDevinConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	global, err := devinConfigPath(false)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/xdg/devin/config.json", global)

	proj, err := devinConfigPath(true)
	require.NoError(t, err)
	assert.Equal(t, ".devin/config.json", proj)
}
