package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/redact"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

func TestRunHookCursorCaptures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	payload := `{
	  "hook_event_name": "afterShellExecution",
	  "command": "cargo test",
	  "duration": 4200,
	  "conversation_id": "conv-1",
	  "workspace_roots": ["/work/proj"]
	}`
	feedStdin(t, payload, runHookCursor)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, "cargo test", got.Cmd)
	assert.Equal(t, agentCursor, got.Executor)
	assert.Equal(t, "cursor-conv-1", got.Session, "session is prefixed with the conversation id")
	assert.Equal(t, "/work/proj", got.Cwd, "cwd falls back to the first workspace root")
	require.NotNil(t, got.DurMs)
	assert.Equal(t, int64(4200), *got.DurMs)
	assert.Nil(t, got.Exit, "Cursor's afterShellExecution carries no exit code")
}

func TestCursorPromptTracing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	// beforeSubmitPrompt saves the conversation's prompt AND must allow it through.
	out := captureStdout(t, func() {
		feedStdin(t, `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"c9","prompt":"add rate limiting","workspace_roots":["/w"]}`,
			runHookCursorPrompt)
	})
	assert.JSONEq(t, `{"continue": true}`, out, "the prompt hook must let the prompt through")

	// A following command in that conversation carries the prompt.
	feedStdin(t, `{"hook_event_name":"afterShellExecution","command":"cargo add tower","conversation_id":"c9","workspace_roots":["/w"]}`,
		runHookCursor)
	rows := spooledRecords(t, dir)
	require.Len(t, rows, 2, "the prompt record plus the command it caused")
	assert.Equal(t, rec.TypePrompt, rows[0].Type)
	assert.Equal(t, "add rate limiting", rows[0].Prompt)
	assert.Equal(t, "/w", rows[0].Cwd, "the workspace root is the prompt's directory")
	assert.Equal(t, rows[0].ID, rows[1].PromptID, "command traced to the prompt record")
	assert.Empty(t, rows[1].Prompt, "text lives on the prompt record only")
}

func TestCursorPromptRedactsButStillContinues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	out := captureStdout(t, func() {
		feedStdin(t, `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"c1","prompt":"use export DB_PASSWORD=hunter2"}`,
			runHookCursorPrompt)
	})
	assert.JSONEq(t, `{"continue": true}`, out, "a secret-bearing prompt is still allowed through")

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1, "the prompt is recorded with the credential masked")
	assert.NotContains(t, rows[0].Prompt, "hunter2", "the secret is not persisted")
	assert.Contains(t, rows[0].Prompt, redact.Mark("generic-token-assign"))
}

func TestRunHookCursorIgnoresEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"hook_event_name":"afterShellExecution","command":"   "}`, runHookCursor)
	assert.Empty(t, spooledRecords(t, dir))
}

func TestMergeCursorHooks(t *testing.T) {
	cfg := map[string]any{}
	require.True(t, mergeCursorHooks(cfg, "yore"))
	require.False(t, mergeCursorHooks(cfg, "yore"), "idempotent")

	assert.EqualValues(t, 1, cfg["version"], "required version key seeded")
	hooks := cfg["hooks"].(map[string]any)
	shell := hooks["afterShellExecution"].([]any)
	require.Len(t, shell, 1)
	assert.Equal(t, "yore hook cursor", shell[0].(map[string]any)["command"])
	prompt := hooks["beforeSubmitPrompt"].([]any)
	require.Len(t, prompt, 1)
	assert.Equal(t, "yore hook cursor-prompt", prompt[0].(map[string]any)["command"])
}

func TestInstallCursorHooksPreservesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"hooks":{"afterFileEdit":[{"command":"prettier"}]}}`), 0o644))

	cfg := map[string]any{}
	data, _ := os.ReadFile(path)
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.True(t, mergeCursorHooks(cfg, "yore"))

	hooks := cfg["hooks"].(map[string]any)
	assert.Len(t, hooks["afterFileEdit"].([]any), 1, "existing hook preserved")
	assert.Len(t, hooks["afterShellExecution"].([]any), 1, "ours added")
}

// ensure the executor tag matches the interactive auto-detect value.
func TestCursorTagMatchesMarker(t *testing.T) {
	assert.Equal(t, "cursor", agentCursor)
	_ = rec.TypeCmd
}
