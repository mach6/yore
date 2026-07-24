package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRemoveHookCmdKeepsForeign removes only yore's hook, leaving a co-located
// third-party hook in the same event untouched.
func TestRemoveHookCmdKeepsForeign(t *testing.T) {
	settings := map[string]any{}
	mergeHookInto(settings, "PostToolUse", "Bash", "yore hook claude-code")
	mergeHookInto(settings, "PostToolUse", "Bash", "some other tool")

	require.True(t, removeHookCmd(settings, "PostToolUse", "yore hook claude-code"))
	blocks := settings["hooks"].(map[string]any)["PostToolUse"].([]any)
	require.Len(t, blocks, 1, "the foreign hook block survives")
	inner := blocks[0].(map[string]any)["hooks"].([]any)
	require.Equal(t, "some other tool", inner[0].(map[string]any)["command"])
}

// TestRemoveHookCmdPrunesEmpty drops the event (and the whole hooks map) once the
// last hook is removed, and is a no-op the second time.
func TestRemoveHookCmdPrunesEmpty(t *testing.T) {
	settings := map[string]any{}
	mergeHookInto(settings, "PostToolUse", "Bash", "yore hook claude-code")
	require.True(t, removeHookCmd(settings, "PostToolUse", "yore hook claude-code"))
	require.Nil(t, settings["hooks"], "emptied hooks map is removed")
	require.False(t, removeHookCmd(settings, "PostToolUse", "yore hook claude-code"), "second removal is a no-op")
}

// TestDevinInitUninitRoundTrip proves `init` then `uninit` restores Devin's
// config exactly — auth and a foreign MCP server survive, yore's keys are gone,
// and the file keeps its 0600 mode.
func TestDevinInitUninitRoundTrip(t *testing.T) {
	x := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", x)
	dir := filepath.Join(x, "devin")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"auth":{"token":"KEEP"},"mcpServers":{"github":{"command":"npx"}}}`), 0o600))

	require.Equal(t, 0, runInitDevin("yore", false, false))
	require.True(t, devinCaptureInstalled("yore"))
	require.True(t, devinMcpRegistered())

	require.Equal(t, 0, runUninitDevin("yore", false))
	require.False(t, devinCaptureInstalled("yore"), "hooks removed")
	require.False(t, devinMcpRegistered(), "yore MCP removed")

	var cfg map[string]any
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.Equal(t, "KEEP", cfg["auth"].(map[string]any)["token"], "auth preserved")
	require.NotNil(t, cfg["mcpServers"].(map[string]any)["github"], "foreign MCP kept")
	require.Nil(t, cfg["mcpServers"].(map[string]any)["yore"], "yore MCP gone")
	require.Nil(t, cfg["hooks"], "emptied hooks map removed")

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "auth-bearing file keeps 0600")
}

// TestClaudeInitUninitRoundTrip round-trips the Claude hooks + MCP registration.
func TestClaudeInitUninitRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	require.Equal(t, 0, runInitClaudeCode("yore", false, false))
	require.True(t, claudeCaptureInstalled("yore"))
	p, _ := mcpConfigPath(false)
	require.True(t, mcpRegistered(p))

	require.Equal(t, 0, runUninitClaudeCode("yore", false))
	require.False(t, claudeCaptureInstalled("yore"), "hooks removed")
	require.False(t, mcpRegistered(p), "MCP removed")
}

// TestCodexUninitStripsExactly proves the codex text-block removal restores the
// pre-yore config byte-for-byte.
func TestCodexUninitStripsExactly(t *testing.T) {
	existing := "[model]\nname = \"gpt\"\n"
	content := existing + codexHooksBlock("yore") + codexMcpBlock("yore")
	stripped := strings.Replace(content, codexHooksBlock("yore"), "", 1)
	stripped = strings.Replace(stripped, codexMcpBlock("yore"), "", 1)
	require.Equal(t, existing, stripped, "original config restored exactly")
}
