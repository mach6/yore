package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/spool"
)

func TestMergeClaudeHookAddsAndIsIdempotent(t *testing.T) {
	settings := map[string]any{}

	require.True(t, mergeClaudeHook(settings, "yore"), "first merge must add the hook")
	require.False(t, mergeClaudeHook(settings, "yore"), "second merge must be a no-op")

	// The hook landed under hooks.PostToolUse with a Bash matcher and our command.
	hooks := settings["hooks"].(map[string]any)
	post := hooks["PostToolUse"].([]any)
	require.Len(t, post, 1, "exactly one matcher block")
	blk := post[0].(map[string]any)
	assert.Equal(t, "Bash", blk["matcher"])
	inner := blk["hooks"].([]any)
	cmd := inner[0].(map[string]any)
	assert.Equal(t, "command", cmd["type"])
	assert.Equal(t, "yore hook claude-code", cmd["command"])
}

// TestMergeClaudeHookPreservesExisting is the safety property: merging must not
// disturb unrelated settings or other people's hooks.
func TestMergeClaudeHookPreservesExisting(t *testing.T) {
	raw := `{
	  "model": "opus",
	  "hooks": {
	    "PostToolUse": [
	      {"matcher": "Edit", "hooks": [{"type": "command", "command": "prettier"}]}
	    ],
	    "UserPromptSubmit": [
	      {"hooks": [{"type": "command", "command": "logger"}]}
	    ]
	  }
	}`
	var settings map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &settings))

	require.True(t, mergeClaudeHook(settings, "yore"))

	assert.Equal(t, "opus", settings["model"], "unrelated top-level keys must survive")
	hooks := settings["hooks"].(map[string]any)
	assert.Len(t, hooks["UserPromptSubmit"].([]any), 1, "unrelated hook events must survive")

	post := hooks["PostToolUse"].([]any)
	require.Len(t, post, 2, "the existing Edit hook must be kept and ours appended")
	// The pre-existing Edit/prettier block must still be there.
	first := post[0].(map[string]any)
	assert.Equal(t, "Edit", first["matcher"], "existing PostToolUse block must be preserved")
}

// feedStdin swaps os.Stdin for a pipe carrying payload, for the duration of fn.
func feedStdin(t *testing.T, payload string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err, "pipe")
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	go func() {
		_, _ = w.WriteString(payload)
		_ = w.Close()
	}()
	fn()
	_ = r.Close()
}

// spooledRecords ingests the spool into a fresh store and returns its records.
func spooledRecords(t *testing.T, dir string) []rec.Record {
	t.Helper()
	rows := []rec.Record{}
	_, err := spool.Drain(config.SpoolDir(dir), func(r rec.Record) error {
		rows = append(rows, r)
		return nil
	})
	require.NoError(t, err, "drain spool")
	return rows
}

func TestRunHookClaudeCodeCaptures(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	payload := `{
	  "hook_event_name": "PostToolUse",
	  "session_id": "sess-1",
	  "cwd": "/work/proj",
	  "tool_name": "Bash",
	  "tool_input": {"command": "cargo test"}
	}`
	feedStdin(t, payload, func() {
		runHookClaudeCode()
	})

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1, "one command captured")
	got := rows[0]
	assert.Equal(t, "cargo test", got.Cmd)
	assert.Equal(t, "/work/proj", got.Cwd)
	assert.Equal(t, "sess-1", got.Session)
	assert.Equal(t, agentClaudeCode, got.Tag, "must be tagged claude-code")
	assert.Nil(t, got.Exit, "exit is unknown (not carried by PostToolUse)")
}

func TestRunHookClaudeCodeIgnoresNonBashAndJunk(t *testing.T) {
	tests := []struct {
		name, payload string
	}{
		{name: "non-Bash tool", payload: `{"tool_name":"Edit","tool_input":{"command":"x"}}`},
		{name: "empty command", payload: `{"tool_name":"Bash","tool_input":{"command":"   "}}`},
		{name: "malformed json", payload: `{not json`},
		{name: "empty", payload: ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("YORE_DIR", dir)
			require.NoError(t, config.EnsureDir(dir))
			feedStdin(t, tc.payload, func() {
				runHookClaudeCode()
			})
			assert.Empty(t, spooledRecords(t, dir), "nothing should be captured")
		})
	}
}

// TestRunHookClaudeCodeRedacts confirms the agent path shares the same secret
// gate as the shell path: a credential-bearing command is never captured.
func TestRunHookClaudeCodeRedacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	// A shape the built-in rules catch (see internal/redact): an inline secret
	// assignment. The agent path must gate it exactly as the shell path does.
	payload := `{"tool_name":"Bash","cwd":"/w","tool_input":{"command":"export DB_PASSWORD=hunter2"}}`
	feedStdin(t, payload, func() { runHookClaudeCode() })
	assert.Empty(t, spooledRecords(t, dir), "a secret-bearing agent command must be redacted, not captured")
}
