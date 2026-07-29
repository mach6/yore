package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/redact"
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
	require.Len(t, rows, 2, "the prompt record plus the command it caused")
	assert.Equal(t, rec.TypePrompt, rows[0].Type)
	assert.Equal(t, "add caching", rows[0].Prompt)
	assert.Equal(t, agentOpenCode, rows[0].Tag)
	assert.Equal(t, rows[0].ID, rows[1].PromptID, "the command references the prompt record")
	assert.Empty(t, rows[1].Prompt, "text lives on the prompt record only")
}

func TestOpenCodePromptRedacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"session_id":"s3","prompt":"use export DB_PASSWORD=hunter2"}`, runHookOpenCodePrompt)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1, "the prompt is recorded with the credential masked")
	assert.NotContains(t, rows[0].Prompt, "hunter2", "the secret is not persisted")
	assert.Contains(t, rows[0].Prompt, redact.Mark("generic-token-assign"))
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
