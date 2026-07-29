package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/rec"
)

func TestRunHookCodexCaptures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	// Codex PostToolUse payload (Claude-shaped); exit_code in tool_response is
	// honored when present.
	payload := `{
	  "hook_event_name": "PostToolUse",
	  "session_id": "cx1",
	  "cwd": "/work",
	  "tool_name": "Bash",
	  "tool_input": {"command": "go vet ./..."},
	  "tool_response": {"exit_code": 2}
	}`
	feedStdin(t, payload, runHookCodex)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, "go vet ./...", got.Cmd)
	assert.Equal(t, agentCodex, got.Executor)
	assert.Equal(t, "codex-cx1", got.Session)
	assert.Equal(t, "/work", got.Cwd)
	require.NotNil(t, got.Exit, "exit_code from tool_response is honored")
	assert.Equal(t, 2, *got.Exit)
}

func TestRunHookCodexNoExitWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	// No exit in tool_response, and Codex has no failure event, so exit is unknown
	// (never defaulted to 0).
	feedStdin(t, `{"tool_name":"Bash","session_id":"c","cwd":"/w","tool_input":{"command":"ls"},"tool_response":"ok"}`,
		runHookCodex)
	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	assert.Nil(t, rows[0].Exit, "no exit code is left unknown, not fabricated")
}

func TestCodexPromptTracing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"c2","prompt":"refactor the parser"}`,
		runHookCodexPrompt)
	feedStdin(t, `{"tool_name":"Bash","session_id":"c2","cwd":"/w","tool_input":{"command":"go test"}}`,
		runHookCodex)
	rows := spooledRecords(t, dir)
	require.Len(t, rows, 2, "the prompt record plus the command it caused")
	assert.Equal(t, rec.TypePrompt, rows[0].Type)
	assert.Equal(t, "refactor the parser", rows[0].Prompt)
	assert.Equal(t, agentCodex, rows[0].Executor)
	assert.Equal(t, rows[0].ID, rows[1].PromptID, "the command references the prompt record")
	assert.Empty(t, rows[1].Prompt, "text lives on the prompt record only")
}

func TestCodexHooksBlockIsValidToml(t *testing.T) {
	// The appended block must parse as TOML and carry both hook commands.
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal([]byte(codexHooksBlock("yore")), &parsed),
		"the appended hook block must be valid TOML")

	hooks := parsed["hooks"].(map[string]any)
	require.Contains(t, hooks, "PostToolUse")
	require.Contains(t, hooks, "UserPromptSubmit")
	post := hooks["PostToolUse"].([]any)
	first := post[0].(map[string]any)
	assert.Equal(t, "^Bash$", first["matcher"])
	inner := first["hooks"].([]any)
	assert.Equal(t, "yore hook codex", inner[0].(map[string]any)["command"])
}

func TestCodexMcpBlockIsValidToml(t *testing.T) {
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal([]byte(codexMcpBlock("yore")), &parsed))
	servers := parsed["mcp_servers"].(map[string]any)
	yore := servers["yore"].(map[string]any)
	assert.Equal(t, "yore", yore["command"], "command is a string per Codex's schema")
	assert.Equal(t, []any{"mcp-serve"}, yore["args"])
}

func TestCodexHooksAndMcpComposeAsValidToml(t *testing.T) {
	// The two appended blocks together must parse as one valid config.
	combined := "model = \"o1\"\n" + codexHooksBlock("yore") + codexMcpBlock("yore")
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal([]byte(combined), &parsed))
	assert.Equal(t, "o1", parsed["model"])
	assert.Contains(t, parsed, "hooks")
	assert.Contains(t, parsed, "mcp_servers")
}

func TestInstallCodexHooksPreservesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("model = \"o1\"\n[tui]\ntheme = \"dark\"\n"), 0o644))

	// Appending the block must keep config.toml valid and preserve prior content.
	orig, _ := os.ReadFile(path)
	require.NoError(t, os.WriteFile(path, append(orig, []byte(codexHooksBlock("yore"))...), 0o644))

	data, _ := os.ReadFile(path)
	var parsed map[string]any
	require.NoError(t, toml.Unmarshal(data, &parsed), "config.toml stays valid TOML after appending hooks")
	assert.Equal(t, "o1", parsed["model"], "existing top-level config preserved")
	assert.Contains(t, parsed, "hooks", "hooks added")
	assert.Contains(t, parsed, "tui", "existing table preserved")
}
