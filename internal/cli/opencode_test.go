package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
)

func TestRunHookOpenCodeCaptures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	// OpenCode gives an exit code (output.metadata.exit), unlike Cursor.
	payload := `{"session_id":"s1","command":"go test ./...","cwd":"/work","exit":1}`
	feedStdin(t, payload, runHookOpenCode)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, "go test ./...", got.Cmd)
	assert.Equal(t, agentOpenCode, got.Tag)
	assert.Equal(t, "opencode-s1", got.Session)
	assert.Equal(t, "/work", got.Cwd)
	require.NotNil(t, got.Exit, "OpenCode carries an exit code")
	assert.Equal(t, 1, *got.Exit)
}

func TestOpenCodePromptTracing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	feedStdin(t, `{"session_id":"s2","prompt":"add caching"}`, runHookOpenCodePrompt)
	feedStdin(t, `{"session_id":"s2","command":"go build","cwd":"/w","exit":0}`, runHookOpenCode)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	assert.NotEmpty(t, rows[0].PromptID)
	assert.Equal(t, "add caching", rows[0].Prompt)
}

func TestOpenCodePromptRedacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"session_id":"s3","prompt":"use export DB_PASSWORD=hunter2"}`, runHookOpenCodePrompt)
	_, ok := loadPromptState(dir, "opencode-s3")
	assert.False(t, ok, "a secret-bearing prompt is not persisted")
}

func TestRunHookOpenCodeIgnoresEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"session_id":"s","command":"  "}`, runHookOpenCode)
	assert.Empty(t, spooledRecords(t, dir))
}

func TestOpenCodePluginScript(t *testing.T) {
	s := opencodePluginScript("yore")
	// The binary is substituted and the placeholder is gone.
	assert.Contains(t, s, `const YORE = "yore";`)
	assert.NotContains(t, s, "__YORE_BIN__")
	// The verified OpenCode hook names are present.
	assert.Contains(t, s, `"tool.execute.after"`)
	assert.Contains(t, s, "message.part.updated")
	assert.Contains(t, s, `"hook", sub`) // invokes `yore hook <sub>`
	assert.Contains(t, s, `send("opencode-prompt"`)
	assert.Contains(t, s, `send("opencode"`)
}

func TestOpenCodePluginPath(t *testing.T) {
	p, err := opencodePluginPath(true)
	require.NoError(t, err)
	assert.Equal(t, ".opencode/plugins/yore.js", p, "project path is verbatim per OpenCode docs")
}

func TestMergeOpenCodeMcp(t *testing.T) {
	cfg := map[string]any{"model": "x"}
	require.True(t, mergeOpenCodeMcp(cfg, "yore"))
	require.False(t, mergeOpenCodeMcp(cfg, "yore"), "idempotent")

	assert.Equal(t, "x", cfg["model"], "unrelated keys preserved")
	yore := cfg["mcp"].(map[string]any)["yore"].(map[string]any)
	assert.Equal(t, "local", yore["type"])
	assert.Equal(t, []any{"yore", "mcp-serve"}, yore["command"], "command is an array per OpenCode's schema")
	assert.Equal(t, true, yore["enabled"])
}
