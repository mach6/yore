package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/risk"
)

// sampleCap bounds how many rows an aggregating tool pulls from the daemon in
// one query. The daemon serves from RAM, so a few thousand rows is cheap; this
// keeps reports bounded on a huge history.
const sampleCap = 5000

// toolDef is one MCP tool: its advertised schema plus the handler that runs it.
type toolDef struct {
	name    string
	desc    string
	schema  map[string]any
	handler func(json.RawMessage) (any, *rpcError)
}

// buildTools registers every tool. Handlers are closures over s so they reach
// the Querier and options.
func (s *Server) buildTools() []toolDef {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	intg := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	scope := map[string]any{
		"type": "string", "enum": []string{"local", "all"},
		"description": "local = this machine only; all = every enrolled machine (cross-machine, end-to-end encrypted). Default all.",
	}
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}

	return []toolDef{
		{
			name: "search_commands",
			desc: "Search shell command history by text, directory, executor, and date. Returns matching commands with directory, exit code, executor and time. Set scope=all to search every machine you own.",
			schema: obj(map[string]any{
				"query":     str("substring to match in the command (empty matches all)"),
				"directory": str("only commands run in this directory (exact path)"),
				"executor":  str("only commands run by this executor, e.g. claude-code (empty = any)"),
				"scope":     scope,
				"limit":     intg("max results (default 20)"),
			}),
			handler: s.toolSearchCommands,
		},
		{
			name: "recent_commands",
			desc: "The most recent commands, optionally in a directory or by an executor. Answers 'what just happened here'.",
			schema: obj(map[string]any{
				"directory": str("only commands run in this directory"),
				"executor":  str("only this executor"),
				"scope":     scope,
				"limit":     intg("max results (default 20)"),
			}),
			handler: s.toolRecentCommands,
		},
		{
			name: "command_status",
			desc: "Has this exact command been run before, and what happened? Returns prior runs with their exit codes and times — use before re-running something.",
			schema: obj(map[string]any{
				"command":   str("the command to look up"),
				"directory": str("restrict to this directory"),
				"scope":     scope,
				"limit":     intg("max prior runs to show (default 5)"),
			}, "command"),
			handler: s.toolCommandStatus,
		},
		{
			name: "session_history",
			desc: "Full chronological command history of one session (a shell session or an agent session id).",
			schema: obj(map[string]any{
				"session_id": str("the session id"),
				"limit":      intg("max commands (default 100)"),
			}, "session_id"),
			handler: s.toolSessionHistory,
		},
		{
			name: "list_sessions",
			desc: "Recent sessions with command counts, time ranges, host, and executor.",
			schema: obj(map[string]any{
				"executor": str("only sessions run by this executor"),
				"scope":    scope,
				"limit":    intg("max sessions (default 10)"),
			}),
			handler: s.toolListSessions,
		},
		{
			name: "get_stats",
			desc: "Aggregate statistics: total commands, success rate, top commands and directories, per-executor and per-host breakdown, over the last N days.",
			schema: obj(map[string]any{
				"days":      intg("lookback window in days (default 7; 0 = all history)"),
				"directory": str("restrict to this directory"),
				"scope":     scope,
			}),
			handler: s.toolGetStats,
		},
		{
			name: "get_prompts",
			desc: "Browse AI-agent prompts and the commands each one triggered. Traces every command back to the prompt behind it.",
			schema: obj(map[string]any{
				"executor":   str("only prompts from this executor"),
				"session_id": str("only prompts from this session"),
				"scope":      scope,
				"limit":      intg("max prompts (default 10)"),
			}),
			handler: s.toolGetPrompts,
		},
		{
			name: "suggest_next",
			desc: "Predict the next command from history using frecency (frequency + recency), biased to the current directory.",
			schema: obj(map[string]any{
				"directory": str("current directory, for the recency-in-dir boost"),
				"scope":     scope,
				"limit":     intg("max suggestions (default 10)"),
			}),
			handler: s.toolSuggestNext,
		},
		{
			name: "what_failed",
			desc: "Recent command failures (nonzero exit), grouped by the agent prompt or session that triggered them, so you can see what went wrong.",
			schema: obj(map[string]any{
				"directory": str("restrict to this directory"),
				"days":      intg("lookback in days (default 7)"),
				"scope":     scope,
				"limit":     intg("max failures (default 20)"),
			}),
			handler: s.toolWhatFailed,
		},
		{
			name: "find_agent_session",
			desc: "Search past AI-agent sessions by prompt text, directory, or executor. Returns session summaries with command counts, success rates, and the first prompt.",
			schema: obj(map[string]any{
				"prompt_text": str("substring to match in the triggering prompt"),
				"directory":   str("restrict to sessions active in this directory"),
				"executor":    str("only this executor"),
				"scope":       scope,
				"limit":       intg("max sessions (default 10)"),
			}),
			handler: s.toolFindAgentSession,
		},
		{
			name: "replay_agent_session",
			desc: "Full chronological timeline of one agent session: every prompt and command with exit codes, directories, and times.",
			schema: obj(map[string]any{
				"session_id": str("the agent session id"),
				"limit":      intg("max commands (default 100)"),
			}, "session_id"),
			handler: s.toolReplayAgentSession,
		},
		{
			name: "assess_risk",
			desc: "Assess how dangerous a command is BEFORE running it: safe/low/medium/high/critical with a category and reason. Also reports how often it has run across your machines and whether it succeeded. Use it to check a destructive command first.",
			schema: obj(map[string]any{
				"command":  str("a single command to assess"),
				"commands": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "assess several commands at once"},
			}),
			handler: s.toolAssessRisk,
		},
	}
}

// listTools returns the advertised tool list, minus any disabled by config.
func (s *Server) listTools() []map[string]any {
	out := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		if s.toolDisabled(t.name) {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.desc,
			"inputSchema": t.schema,
		})
	}
	return out
}

func (s *Server) toolDisabled(name string) bool {
	for _, d := range s.opts.DisabledTools {
		if d == name {
			return true
		}
	}
	return false
}

// handleToolCall dispatches tools/call to the named tool.
func (s *Server) handleToolCall(params json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tool call params"}
	}
	if s.toolDisabled(call.Name) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tool disabled: " + call.Name}
	}
	for _, t := range s.tools {
		if t.name == call.Name {
			return t.handler(call.Arguments)
		}
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool: " + call.Name}
}

// --- shared param parsing + fetch --------------------------------------------

// commonArgs are the parameters most tools share.
type commonArgs struct {
	Query      string `json:"query"`
	Directory  string `json:"directory"`
	Executor   string `json:"executor"`
	Scope      string `json:"scope"`
	Command    string `json:"command"`
	SessionID  string `json:"session_id"`
	PromptText string `json:"prompt_text"`
	Days       *int   `json:"days"`
	Limit      *int   `json:"limit"`
}

func (a commonArgs) limitOr(def int) int {
	if a.Limit != nil && *a.Limit > 0 {
		return *a.Limit
	}
	return def
}

// scopeOr resolves the query scope: explicit local/all, else the given default.
func scopeOr(v, def string) string {
	switch v {
	case "local":
		return proto.ScopeLocal
	case "all":
		return proto.ScopeAll
	default:
		return def
	}
}

// fetch runs one daemon query and drops rows under any excluded directory.
func (s *Server) fetch(q proto.QueryReq) ([]rec.Record, error) {
	resp, err := s.q.Query(q)
	if err != nil {
		return nil, err
	}
	if len(s.opts.ExcludeDirs) == 0 {
		return resp.Rows, nil
	}
	out := resp.Rows[:0]
	for _, r := range resp.Rows {
		if s.excluded(r.Cwd) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// sample pulls a broad recent slice for aggregation (empty query, big limit).
func (s *Server) sample(scope string) ([]rec.Record, error) {
	return s.fetch(proto.QueryReq{Scope: scope, Limit: sampleCap})
}

func (s *Server) excluded(cwd string) bool {
	for _, d := range s.opts.ExcludeDirs {
		if d != "" && (cwd == d || strings.HasPrefix(cwd, strings.TrimRight(d, "/")+"/")) {
			return true
		}
	}
	return false
}

// cutoffMs converts a days lookback into a StartMs floor (0 = no floor).
func cutoffMs(days int) int64 {
	if days <= 0 {
		return 0
	}
	return time.Now().UnixMilli() - int64(days)*86_400_000
}

// --- tool handlers -----------------------------------------------------------

func (s *Server) toolSearchCommands(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	limit := a.limitOr(s.opts.DefaultLimit)
	q := proto.QueryReq{Q: a.Query, Scope: scopeOr(a.Scope, proto.ScopeAll), Executor: a.Executor, Limit: limit}
	if a.Directory != "" {
		q.Scope, q.Cwd = proto.ScopeCwd, a.Directory
	}
	rows, err := s.fetch(q)
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	return textResult(formatCommandList(fmt.Sprintf("%d command(s)", len(rows)), rows, s.opts.LocalHost)), nil
}

func (s *Server) toolRecentCommands(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	q := proto.QueryReq{Scope: scopeOr(a.Scope, proto.ScopeLocal), Executor: a.Executor, Limit: a.limitOr(s.opts.DefaultLimit)}
	if a.Directory != "" {
		q.Scope, q.Cwd = proto.ScopeCwd, a.Directory
	}
	rows, err := s.fetch(q)
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	return textResult(formatCommandList("recent commands", rows, s.opts.LocalHost)), nil
}

func (s *Server) toolCommandStatus(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	if strings.TrimSpace(a.Command) == "" {
		return toolError("command is required"), nil
	}
	q := proto.QueryReq{Q: a.Command, Scope: scopeOr(a.Scope, proto.ScopeAll), Limit: 500}
	if a.Directory != "" {
		q.Scope, q.Cwd = proto.ScopeCwd, a.Directory
	}
	rows, err := s.fetch(q)
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	// Keep only exact-command matches (the daemon does substring).
	var exact []rec.Record
	for _, r := range rows {
		if strings.TrimSpace(r.Cmd) == strings.TrimSpace(a.Command) {
			exact = append(exact, r)
		}
	}
	limit := a.limitOr(5)
	var b strings.Builder
	if len(exact) == 0 {
		fmt.Fprintf(&b, "Command not seen before: %s\n", a.Command)
		return textResult(b.String()), nil
	}
	ok, fail, unknown := tallyExits(exact)
	fmt.Fprintf(&b, "Command run %d time(s): %d ok, %d failed, %d unknown\n\n", len(exact), ok, fail, unknown)
	b.WriteString(formatCommandList("most recent runs", capRows(exact, limit), s.opts.LocalHost))
	return textResult(b.String()), nil
}

func (s *Server) toolSessionHistory(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	if a.SessionID == "" {
		return toolError("session_id is required"), nil
	}
	rows, err := s.sessionRows(a.SessionID)
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	if len(rows) == 0 {
		return textResult("No commands for session " + a.SessionID), nil
	}
	sortChronological(rows)
	return textResult(formatChronological("session "+a.SessionID, capRows(rows, a.limitOr(100)))), nil
}

func (s *Server) toolListSessions(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	rows, err := s.sample(scopeOr(a.Scope, proto.ScopeAll))
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	sess := groupSessions(rows, a.Executor)
	limit := a.limitOr(10)
	if len(sess) > limit {
		sess = sess[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d recent session(s)\n\n", len(sess))
	for _, ss := range sess {
		fmt.Fprintf(&b, "%s  %s  %d cmds (%d ok / %d fail)  %s..%s  %s\n",
			shortID(ss.id), execLabel(ss.executor), ss.count, ss.ok, ss.fail,
			relTime(ss.firstMs), relTime(ss.lastMs), ss.host)
	}
	return textResult(b.String()), nil
}

func (s *Server) toolGetStats(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	rows, err := s.sample(scopeOr(a.Scope, proto.ScopeAll))
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	days := 7
	if a.Days != nil {
		days = *a.Days
	}
	return textResult(formatStats(rows, a.Directory, days, s.excluded)), nil
}

func (s *Server) toolGetPrompts(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	rows, err := s.sample(scopeOr(a.Scope, proto.ScopeAll))
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	groups := groupPrompts(rows, a.Executor, a.SessionID, "")
	limit := a.limitOr(10)
	if len(groups) > limit {
		groups = groups[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d prompt(s)\n\n", len(groups))
	for _, g := range groups {
		fmt.Fprintf(&b, "[%s] %s (%s)\n  %d cmds, %d ok / %d fail, %s\n\n",
			relTime(g.lastMs), oneLine(g.text, 100), execLabel(g.executor), g.count, g.ok, g.fail, shortID(g.session))
	}
	return textResult(b.String()), nil
}

func (s *Server) toolSuggestNext(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	rows, err := s.fetch(proto.QueryReq{
		Scope: scopeOr(a.Scope, proto.ScopeLocal), Sort: proto.SortFrecency, Cwd: a.Directory, Limit: a.limitOr(10),
	})
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d suggestion(s) (frecency)\n\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "%s\n", strings.TrimSpace(r.Cmd))
	}
	return textResult(b.String()), nil
}

func (s *Server) toolWhatFailed(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	rows, err := s.sample(scopeOr(a.Scope, proto.ScopeAll))
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	days := 7
	if a.Days != nil {
		days = *a.Days
	}
	// Group failures by the prompt that triggered them, falling back to the
	// session for commands that were never prompt-traced. Most recent first.
	groups := groupFailures(rows, a.Directory, cutoffMs(days))
	var b strings.Builder
	shown := 0
	limit := a.limitOr(20)
	for _, g := range groups {
		if shown >= limit {
			break
		}
		shown++
		label := oneLine(g.text, 100)
		if label == "" {
			label = "session " + shortID(g.session)
		}
		fmt.Fprintf(&b, "[%s] %s — %d failed\n", relTime(g.lastMs), label, g.fail)
		for _, r := range g.cmds {
			fmt.Fprintf(&b, "    ✗%d  %s  (%s)\n", *r.Exit, oneLine(r.Cmd, 80), r.Cwd)
		}
		b.WriteString("\n")
	}
	if shown == 0 {
		window := "all history"
		if days > 0 {
			window = fmt.Sprintf("the last %d day(s)", days)
		}
		return textResult("No failures in " + window + "."), nil
	}
	return textResult(b.String()), nil
}

func (s *Server) toolFindAgentSession(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	rows, err := s.sample(scopeOr(a.Scope, proto.ScopeAll))
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	sess := groupSessions(rows, a.Executor)
	needle := strings.ToLower(a.PromptText)
	var b strings.Builder
	shown := 0
	limit := a.limitOr(10)
	for _, ss := range sess {
		if ss.executor == "" { // agent sessions only
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(ss.firstPrompt), needle) {
			continue
		}
		if a.Directory != "" && !ss.dirs[a.Directory] {
			continue
		}
		if shown >= limit {
			break
		}
		shown++
		fmt.Fprintf(&b, "%s  %s  %d cmds (%d ok / %d fail)  %s\n  first prompt: %s\n\n",
			shortID(ss.id), execLabel(ss.executor), ss.count, ss.ok, ss.fail, relTime(ss.lastMs), oneLine(ss.firstPrompt, 120))
	}
	if shown == 0 {
		return textResult("No matching agent sessions."), nil
	}
	return textResult(b.String()), nil
}

func (s *Server) toolReplayAgentSession(raw json.RawMessage) (any, *rpcError) {
	a, rerr := parseArgs(raw)
	if rerr != nil {
		return nil, rerr
	}
	if a.SessionID == "" {
		return toolError("session_id is required"), nil
	}
	rows, err := s.sessionRows(a.SessionID)
	if err != nil {
		return toolError("query failed: %v", err), nil
	}
	if len(rows) == 0 {
		return textResult("No commands for session " + a.SessionID), nil
	}
	sortChronological(rows)
	rows = capRows(rows, a.limitOr(100))
	var b strings.Builder
	fmt.Fprintf(&b, "Session %s — %d command(s)\n\n", a.SessionID, len(rows))
	var lastPrompt string
	for _, r := range rows {
		if r.Prompt != "" && r.Prompt != lastPrompt {
			lastPrompt = r.Prompt
			fmt.Fprintf(&b, "▸ prompt: %s\n", oneLine(r.Prompt, 200))
		}
		fmt.Fprintf(&b, "    %s  %s  %s  (%s)\n", relTime(r.StartMs), exitGlyph(r), oneLine(r.Cmd, 100), r.Cwd)
	}
	return textResult(b.String()), nil
}

func (s *Server) toolAssessRisk(raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Command  string   `json:"command"`
		Commands []string `json:"commands"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid arguments"}
		}
	}
	cmds := a.Commands
	if a.Command != "" {
		cmds = append([]string{a.Command}, cmds...)
	}
	if len(cmds) == 0 {
		return toolError("provide a command (or commands)"), nil
	}
	var b strings.Builder
	for _, c := range cmds {
		v := risk.Assess(c)
		fmt.Fprintf(&b, "%s  [%s / %s]  %s\n    %s\n", riskGlyph(v.Level), v.Level, v.Category, oneLine(c, 100), v.Reason)
		// History-aware context (the cross-machine edge): how has this exact
		// command fared before, anywhere? Best-effort; ignore query errors.
		if rows, err := s.fetch(proto.QueryReq{Q: c, Scope: proto.ScopeAll, Limit: 500}); err == nil {
			var exact []rec.Record
			for _, r := range rows {
				if strings.TrimSpace(r.Cmd) == strings.TrimSpace(c) {
					exact = append(exact, r)
				}
			}
			if len(exact) > 0 {
				ok, fail, unknown := tallyExits(exact)
				fmt.Fprintf(&b, "    history: run %d time(s) across your machines — %d ok, %d failed, %d unknown\n", len(exact), ok, fail, unknown)
			} else {
				b.WriteString("    history: never run before on any of your machines\n")
			}
		}
		b.WriteByte('\n')
	}
	return textResult(b.String()), nil
}

// sessionRows returns every record in a session, pulled from a broad sample.
func (s *Server) sessionRows(session string) ([]rec.Record, error) {
	rows, err := s.sample(proto.ScopeAll)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if r.Session == session {
			out = append(out, r)
		}
	}
	return out, nil
}

// parseArgs unmarshals tool arguments, tolerating null/empty.
func parseArgs(raw json.RawMessage) (commonArgs, *rpcError) {
	var a commonArgs
	if len(raw) == 0 {
		return a, nil
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, &rpcError{Code: codeInvalidParams, Message: "invalid arguments"}
	}
	return a, nil
}

// capRows returns at most n rows.
func capRows(rows []rec.Record, n int) []rec.Record {
	if n > 0 && len(rows) > n {
		return rows[:n]
	}
	return rows
}

func sortChronological(rows []rec.Record) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartMs < rows[j].StartMs })
}
