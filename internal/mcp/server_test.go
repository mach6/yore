package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/risk"
)

// fakeQ is a minimal in-memory Querier: it honors Q (substring), Tag, ScopeCwd
// (exact cwd), and Limit — enough to drive the tool handlers deterministically.
type fakeQ struct{ rows []rec.Record }

func (f fakeQ) Query(q proto.QueryReq) (proto.QueryResp, error) {
	var out []rec.Record
	for _, r := range f.rows {
		if q.Q != "" && !strings.Contains(r.Cmd, q.Q) {
			continue
		}
		if q.Executor != "" && r.Executor != q.Executor {
			continue
		}
		if q.Scope == proto.ScopeCwd && r.Cwd != q.Cwd {
			continue
		}
		out = append(out, r)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return proto.QueryResp{Rows: out, Total: len(out), Scope: q.Scope}, nil
}

func exit(i int) *int { return &i }

func sampleRows() []rec.Record {
	return []rec.Record{
		{ID: "1", Cmd: "go build ./...", Cwd: "/repo", Hostname: "laptop", Exit: exit(0), StartMs: 10_000, Session: "s1"},
		{ID: "2", Cmd: "go test ./...", Cwd: "/repo", Hostname: "laptop", Exit: exit(1), StartMs: 20_000, Session: "s1", Executor: "claude-code", PromptID: "p1", Prompt: "fix the failing test"},
		{ID: "3", Cmd: "npm install", Cwd: "/web", Hostname: "server", Exit: exit(0), StartMs: 30_000, Session: "s2", Executor: "claude-code", PromptID: "p1", Prompt: "fix the failing test"},
	}
}

// call runs one JSON-RPC request through Serve and returns the decoded response.
func call(t *testing.T, s *Server, method string, params any) rpcResponse {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, s.Serve(bytes.NewReader(append(body, '\n')), &out, io.Discard))
	var resp rpcResponse
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp))
	return resp
}

func newTestServer() *Server {
	return New(fakeQ{rows: sampleRows()}, Options{Version: "test", LocalHost: "laptop"})
}

func TestInitialize(t *testing.T) {
	resp := call(t, newTestServer(), "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	require.Nil(t, resp.Error)
	res := resp.Result.(map[string]any)
	require.Equal(t, "2025-06-18", res["protocolVersion"])
	info := res["serverInfo"].(map[string]any)
	require.Equal(t, "yore-mcp", info["name"])
}

func TestToolsListAndDisable(t *testing.T) {
	s := newTestServer()
	resp := call(t, s, "tools/list", nil)
	require.Nil(t, resp.Error)
	tools := resp.Result.(map[string]any)["tools"].([]any)
	require.GreaterOrEqual(t, len(tools), 11)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	require.True(t, names["search_commands"])
	require.True(t, names["what_failed"])
	require.True(t, names["replay_agent_session"])

	// A disabled tool disappears from the list and refuses to run.
	s2 := New(fakeQ{rows: sampleRows()}, Options{DisabledTools: []string{"search_commands"}})
	resp2 := call(t, s2, "tools/list", nil)
	for _, tl := range resp2.Result.(map[string]any)["tools"].([]any) {
		require.NotEqual(t, "search_commands", tl.(map[string]any)["name"])
	}
	callResp := call(t, s2, "tools/call", map[string]any{"name": "search_commands", "arguments": map[string]any{}})
	require.NotNil(t, callResp.Error)
}

func TestToolSearchCommands(t *testing.T) {
	resp := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "search_commands", "arguments": map[string]any{"query": "go", "scope": "all"},
	})
	require.Nil(t, resp.Error)
	text := resultText(resp.Result)
	require.Contains(t, text, "go build ./...")
	require.Contains(t, text, "go test ./...")
	require.NotContains(t, text, "npm install")
	// A local row (host == LocalHost) is not host-tagged.
	require.NotContains(t, text, "@laptop")

	// A cross-machine row is tagged with its host.
	resp2 := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "search_commands", "arguments": map[string]any{"query": "npm", "scope": "all"},
	})
	require.Nil(t, resp2.Error)
	require.Contains(t, resultText(resp2.Result), "@server")
}

func TestToolWhatFailed(t *testing.T) {
	resp := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "what_failed", "arguments": map[string]any{"days": 0},
	})
	require.Nil(t, resp.Error)
	text := resultText(resp.Result)
	require.Contains(t, text, "fix the failing test") // the triggering prompt
	require.Contains(t, text, "go test ./...")        // the failed command
	require.NotContains(t, text, "npm install")       // succeeded, not shown
}

func TestToolReplayAgentSession(t *testing.T) {
	resp := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "replay_agent_session", "arguments": map[string]any{"session_id": "s1"},
	})
	require.Nil(t, resp.Error)
	text := resultText(resp.Result)
	require.Contains(t, text, "▸ prompt: fix the failing test")
	require.Contains(t, text, "go build ./...")
}

func TestToolAssessRisk(t *testing.T) {
	resp := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "assess_risk", "arguments": map[string]any{"command": "rm -rf /tmp/x"},
	})
	require.Nil(t, resp.Error)
	text := resultText(resp.Result)
	require.Contains(t, text, "critical")
	require.Contains(t, text, "destructive")
	// History-aware: this command isn't in the sample, so it's never-run.
	require.Contains(t, text, "never run before")

	// A batch with a known command surfaces its cross-machine run history.
	resp2 := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "assess_risk", "arguments": map[string]any{"commands": []string{"npm install"}},
	})
	require.Nil(t, resp2.Error)
	require.Contains(t, resultText(resp2.Result), "run 1 time")
}

// TestAssessRiskUsesInjectedRuleset: a user risk.toml rule reaches assess_risk
// through Options.Risk, and nil Risk means the built-ins alone.
func TestAssessRiskUsesInjectedRuleset(t *testing.T) {
	rs, errs := risk.Compile([]risk.Spec{
		{Pattern: `\bmake\s+deploy\b`, Level: "high", Category: "infra", Reason: "ships to production"},
	}, nil)
	require.Empty(t, errs)
	s := New(fakeQ{rows: sampleRows()}, Options{Version: "test", LocalHost: "laptop", Risk: rs})

	resp := call(t, s, "tools/call", map[string]any{
		"name": "assess_risk", "arguments": map[string]any{"command": "make deploy"},
	})
	require.Nil(t, resp.Error)
	text := resultText(resp.Result)
	require.Contains(t, text, "high")
	require.Contains(t, text, "infra")

	resp2 := call(t, newTestServer(), "tools/call", map[string]any{
		"name": "assess_risk", "arguments": map[string]any{"command": "make deploy"},
	})
	require.Nil(t, resp2.Error)
	require.Contains(t, resultText(resp2.Result), "safe", "without injection the built-ins apply")
}

func TestResources(t *testing.T) {
	s := newTestServer()
	resp := call(t, s, "resources/list", nil)
	require.Nil(t, resp.Error)
	res := resp.Result.(map[string]any)["resources"].([]any)
	require.GreaterOrEqual(t, len(res), 6)

	read := call(t, s, "resources/read", map[string]any{"uri": "yore://history/recent"})
	require.Nil(t, read.Error)
	contents := read.Result.(map[string]any)["contents"].([]any)
	require.NotEmpty(t, contents)

	// Session template.
	tmpl := call(t, s, "resources/read", map[string]any{"uri": "yore://history/session/s1"})
	require.Nil(t, tmpl.Error)
	require.Contains(t, resultText2(tmpl.Result), "go build ./...")
}

func TestNotificationAndUnknownMethod(t *testing.T) {
	s := newTestServer()
	// A notification (no id) yields no response line at all.
	var out bytes.Buffer
	require.NoError(t, s.Serve(strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out, io.Discard))
	require.Empty(t, bytes.TrimSpace(out.Bytes()))

	// Unknown method → method-not-found error.
	resp := call(t, s, "no/such/method", nil)
	require.NotNil(t, resp.Error)
	require.Equal(t, codeMethodNotFound, resp.Error.Code)
}

// resultText2 extracts text from a resources/read result.
func resultText2(res any) string {
	m := res.(map[string]any)
	contents := m["contents"].([]any)
	return contents[0].(map[string]any)["text"].(string)
}
