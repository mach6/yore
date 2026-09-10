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
	"github.com/mach6/yore/internal/spool"
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

	post := hooks["PostToolUse"].([]any)
	require.Len(t, post, 2, "the existing Edit hook must be kept and ours appended")
	// The pre-existing Edit/prettier block must still be there.
	first := post[0].(map[string]any)
	assert.Equal(t, "Edit", first["matcher"], "existing PostToolUse block must be preserved")

	// The pre-existing UserPromptSubmit (logger) survives, and ours is appended.
	ups := hooks["UserPromptSubmit"].([]any)
	require.Len(t, ups, 2, "existing UserPromptSubmit hook kept and ours appended")
	logger := ups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, "logger", logger["command"], "the pre-existing prompt hook must be preserved")
}

func TestMergeMcpServerAddsAndIsIdempotent(t *testing.T) {
	// Adds under mcpServers.yore as a stdio entry (command + args, no type/url).
	cfg := map[string]any{"existing": "keep me"}
	require.True(t, mergeMcpServer(cfg, "yore"))
	require.False(t, mergeMcpServer(cfg, "yore"), "second merge is a no-op")

	assert.Equal(t, "keep me", cfg["existing"], "unrelated keys survive")
	servers := cfg["mcpServers"].(map[string]any)
	yore := servers["yore"].(map[string]any)
	assert.Equal(t, "yore", yore["command"])
	assert.Equal(t, []any{"mcp-serve"}, yore["args"])
	_, hasType := yore["type"]
	assert.False(t, hasType, "stdio entry needs no type")
	_, hasURL := yore["url"]
	assert.False(t, hasURL, "stdio entry has no url")
}

func TestRegisterMcpAtAndDetect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	u := newUI()
	u.w = io.Discard // keep the test quiet; theme is still wired

	require.False(t, mcpRegistered(path), "absent before registration")
	require.NoError(t, registerMcpAt(path, "yore", u))
	require.True(t, mcpRegistered(path), "present after registration")

	// Idempotent: a second call leaves exactly one yore entry and preserves other keys.
	require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644))
	require.NoError(t, registerMcpAt(path, "yore", u))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))
	servers := cfg["mcpServers"].(map[string]any)
	require.Contains(t, servers, "yore", "yore added")
	require.Contains(t, servers, "other", "unrelated server preserved")

	// A malformed file is left untouched (non-fatal).
	require.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o644))
	require.NoError(t, registerMcpAt(path, "yore", u))
	require.False(t, mcpRegistered(path), "malformed file left as-is")
}

func TestHookCmdPresent(t *testing.T) {
	cfg := map[string]any{}
	require.True(t, mergeClaudeHook(cfg, "yore"))
	assert.True(t, hookCmdPresent(cfg, "PostToolUse", "yore hook claude-code"))
	assert.True(t, hookCmdPresent(cfg, "PostToolUseFailure", "yore hook claude-code-failure"))
	assert.False(t, hookCmdPresent(cfg, "PostToolUse", "yore hook nope"))
	assert.False(t, hookCmdPresent(map[string]any{}, "PostToolUse", "yore hook claude-code"))
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
	assert.Equal(t, agentClaudeCode, got.Executor, "must be tagged claude-code")
	require.NotNil(t, got.Exit, "PostToolUse fires on success -> exit recorded")
	assert.Equal(t, 0, *got.Exit, "success defaults to exit 0")
}

// TestRunHookClaudeCodePreStampsDuration covers the PreToolUse→PostToolUse
// timing path: the pre hook records a start, the post hook turns it into a real
// duration, and the stamp is consumed exactly once.
func TestRunHookClaudeCodePreStampsDuration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	const cmd = "make build"
	pre := `{"hook_event_name":"PreToolUse","session_id":"s9","tool_name":"Bash","tool_input":{"command":"` + cmd + `"}}`
	post := `{"hook_event_name":"PostToolUse","session_id":"s9","cwd":"/w","tool_name":"Bash","tool_input":{"command":"` + cmd + `"}}`

	feedStdin(t, pre, runHookClaudeCodePre)
	require.Empty(t, spooledRecords(t, dir), "PreToolUse must not record a command")

	feedStdin(t, post, runHookClaudeCode)
	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].DurMs, "duration derived from the PreToolUse start stamp")
	assert.GreaterOrEqual(t, *rows[0].DurMs, int64(0))
	assert.Positive(t, rows[0].StartMs, "record anchored to the stamped start time")

	// The start file is consumed on read, so a repeat PostToolUse for the same
	// command has no timing left to attach.
	feedStdin(t, post, runHookClaudeCode)
	rows2 := spooledRecords(t, dir)
	require.Len(t, rows2, 1)
	assert.Nil(t, rows2[0].DurMs, "start stamp consumed once; no duration the second time")
}

// TestRunHookClaudeCodeExitAndDuration covers exit-status + duration capture:
// an explicit tool_response.exit_code wins, the PostToolUseFailure hook records
// a failure, and tool timestamps yield a duration.
func TestRunHookClaudeCodeExitAndDuration(t *testing.T) {
	t.Run("explicit exit_code wins", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("YORE_DIR", dir)
		require.NoError(t, config.EnsureDir(dir))
		payload := `{"tool_name":"Bash","cwd":"/w","tool_input":{"command":"false"},
		  "tool_response":{"exit_code":3},
		  "tool_start_time":"2024-01-15T10:30:00Z","tool_end_time":"2024-01-15T10:30:02Z"}`
		feedStdin(t, payload, runHookClaudeCode)
		rows := spooledRecords(t, dir)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].Exit)
		assert.Equal(t, 3, *rows[0].Exit, "exit_code from tool_response is used")
		require.NotNil(t, rows[0].DurMs)
		assert.Equal(t, int64(2000), *rows[0].DurMs, "duration derived from timestamps")
	})

	t.Run("failure hook defaults to nonzero", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("YORE_DIR", dir)
		require.NoError(t, config.EnsureDir(dir))
		payload := `{"tool_name":"Bash","cwd":"/w","tool_input":{"command":"make"},"error_message":"boom"}`
		feedStdin(t, payload, runHookClaudeCodeFailure)
		rows := spooledRecords(t, dir)
		require.Len(t, rows, 1)
		require.NotNil(t, rows[0].Exit)
		assert.NotEqual(t, 0, *rows[0].Exit, "a failure without an explicit code is a nonzero exit")
	})

	t.Run("non-object tool_response is tolerated", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("YORE_DIR", dir)
		require.NoError(t, config.EnsureDir(dir))
		payload := `{"tool_name":"Bash","cwd":"/w","tool_input":{"command":"ok"},"tool_response":"plain string"}`
		feedStdin(t, payload, runHookClaudeCode)
		rows := spooledRecords(t, dir)
		require.Len(t, rows, 1, "a string tool_response must not break the decode")
		require.NotNil(t, rows[0].Exit)
		assert.Equal(t, 0, *rows[0].Exit)
	})
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
// gate as the shell path: the credential is masked and the command is kept.
func TestRunHookClaudeCodeRedacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	// A shape the built-in rules catch (see internal/redact): an inline secret
	// assignment. The agent path must gate it exactly as the shell path does.
	payload := `{"tool_name":"Bash","cwd":"/w","tool_input":{"command":"export DB_PASSWORD=hunter2"}}`
	feedStdin(t, payload, func() { runHookClaudeCode() })

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1, "the command is kept: only the credential goes")
	assert.NotContains(t, rows[0].Cmd, "hunter2", "the secret must never be spooled")
	assert.Contains(t, rows[0].Cmd, "export DB_PASSWORD=", "the rest of the command survives")
	assert.Contains(t, rows[0].Cmd, redact.Mark("generic-token-assign"), "the marker names the rule that fired")
}

func TestPromptTracing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))

	// UserPromptSubmit records the session's current prompt.
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s1","prompt":"add rate limiting to the API"}`,
		runHookClaudePrompt)

	// Two PostToolUse commands in that session then carry the prompt + a shared id.
	feedStdin(t, `{"session_id":"s1","tool_name":"Bash","cwd":"/w","tool_input":{"command":"cargo add tower"}}`,
		runHookClaudeCode)
	feedStdin(t, `{"session_id":"s1","tool_name":"Bash","cwd":"/w","tool_input":{"command":"cargo build"}}`,
		runHookClaudeCode)

	rows := spooledRecords(t, dir)
	require.Len(t, rows, 3, "one prompt record plus two commands")

	prompt := rows[0]
	require.Equal(t, rec.TypePrompt, prompt.Type, "the prompt is a record in its own right")
	assert.Equal(t, "add rate limiting to the API", prompt.Prompt)
	assert.Equal(t, agentClaudeCode, prompt.Executor)
	assert.Empty(t, prompt.Cmd, "a prompt record is not a command")

	cmds := rows[1:]
	require.Equal(t, prompt.ID, cmds[0].PromptID, "command must be traced to the prompt record")
	assert.Equal(t, prompt.ID, cmds[1].PromptID, "both commands share the prompt id")
	// The text is stored once, on the prompt record. Copying it onto every
	// command is what this change removed; the daemon rejoins them on read.
	assert.Empty(t, cmds[0].Prompt, "prompt text must not be duplicated onto commands")
	assert.Empty(t, cmds[1].Prompt, "prompt text must not be duplicated onto commands")

	// A command in a session with no prompt is untraced (not an error).
	feedStdin(t, `{"session_id":"other","tool_name":"Bash","tool_input":{"command":"ls"}}`, runHookClaudeCode)
	rows = spooledRecords(t, dir)
	var untraced *rec.Record
	for i := range rows {
		if rows[i].Cmd == "ls" {
			untraced = &rows[i]
		}
	}
	require.NotNil(t, untraced)
	assert.Empty(t, untraced.PromptID, "a command with no session prompt stays untraced")
}

func TestPromptHookRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YORE_DIR", dir)
	require.NoError(t, config.EnsureDir(dir))
	feedStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s1","prompt":"use export DB_PASSWORD=hunter2 please"}`,
		runHookClaudePrompt)

	// The prompt is recorded, with the credential masked, not the whole prompt
	// thrown away, so the tracing still works and the question is still readable.
	rows := spooledRecords(t, dir)
	require.Len(t, rows, 1)
	assert.NotContains(t, rows[0].Prompt, "hunter2", "the secret must never be spooled")
	assert.Contains(t, rows[0].Prompt, "please", "the rest of the prompt survives")
	assert.Contains(t, rows[0].Prompt, redact.Mark("generic-token-assign"))

	// The session state exists so the commands that follow can be traced to it,
	// and it holds the id alone; never a second copy of the text.
	ps, ok := loadPromptState(dir, "s1")
	require.True(t, ok, "a redacted prompt is still a prompt")
	assert.Equal(t, rows[0].ID, ps.ID)
}

func TestMergeClaudeHookInstallsBothEvents(t *testing.T) {
	settings := map[string]any{}
	require.True(t, mergeClaudeHook(settings, "yore"))
	require.False(t, mergeClaudeHook(settings, "yore"), "idempotent across both events")

	hooks := settings["hooks"].(map[string]any)
	require.Len(t, hooks["PostToolUse"].([]any), 1, "PostToolUse hook installed")
	require.Len(t, hooks["PostToolUseFailure"].([]any), 1, "PostToolUseFailure hook installed")
	require.Len(t, hooks["UserPromptSubmit"].([]any), 1, "UserPromptSubmit hook installed")

	failBlk := hooks["PostToolUseFailure"].([]any)[0].(map[string]any)
	assert.Equal(t, "Bash", failBlk["matcher"])
	failCmd := failBlk["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, "yore hook claude-code-failure", failCmd["command"])
}
