package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"yore/internal/proto"
	"yore/internal/rec"
)

// sessionURIPrefix is the read-template for a single session's history.
const sessionURIPrefix = "yore://history/session/"

// resourceDef is one browsable MCP resource: fixed metadata plus a reader.
type resourceDef struct {
	uri  string
	name string
	desc string
	read func() (string, *rpcError)
}

// buildResources registers the auto-injected context resources. (risk/summary
// arrives with the risk engine in a later phase.)
func (s *Server) buildResources() []resourceDef {
	return []resourceDef{
		{
			uri: "yore://history/recent", name: "Recent Commands",
			desc: "The last commands across all your machines, with exit codes.",
			read: func() (string, *rpcError) {
				rows, err := s.fetch(proto.QueryReq{Scope: proto.ScopeAll, Limit: 20})
				if err != nil {
					return "", &rpcError{Code: codeInternal, Message: err.Error()}
				}
				return formatCommandList("Recent commands", rows, s.opts.LocalHost), nil
			},
		},
		{
			uri: "yore://failures/recent", name: "Recent Failures",
			desc: "Recent failed commands grouped by the prompt that caused them.",
			read: func() (string, *rpcError) {
				res, _ := s.toolWhatFailed(json.RawMessage(`{"days":1,"limit":20}`))
				return resultText(res), nil
			},
		},
		{
			uri: "yore://stats/today", name: "Today's Stats",
			desc: "Command count, success rate, and top commands for today.",
			read: func() (string, *rpcError) {
				rows, err := s.sample(proto.ScopeAll)
				if err != nil {
					return "", &rpcError{Code: codeInternal, Message: err.Error()}
				}
				return formatStats(rows, "", 1, s.excluded), nil
			},
		},
		{
			uri: "yore://agents/activity", name: "Agent Activity",
			desc: "Per-agent command counts and success rates.",
			read: func() (string, *rpcError) {
				rows, err := s.sample(proto.ScopeAll)
				if err != nil {
					return "", &rpcError{Code: codeInternal, Message: err.Error()}
				}
				return formatAgentActivity(rows), nil
			},
		},
		{
			uri: "yore://agents/sessions", name: "Recent Agent Sessions",
			desc: "The most recent agent sessions with their first prompt.",
			read: func() (string, *rpcError) {
				res, _ := s.toolFindAgentSession(json.RawMessage(`{"limit":5}`))
				return resultText(res), nil
			},
		},
		{
			uri: "yore://context/project", name: "Project Context",
			desc: "Briefing for the current project: common commands, failures, agents.",
			read: func() (string, *rpcError) {
				rows, err := s.sample(proto.ScopeLocal)
				if err != nil {
					return "", &rpcError{Code: codeInternal, Message: err.Error()}
				}
				return formatStats(rows, "", 7, s.excluded), nil
			},
		},
	}
}

func (s *Server) resourceDisabled(uri string) bool {
	for _, d := range s.opts.DisabledResources {
		if d != "" && strings.HasSuffix(uri, d) {
			return true
		}
	}
	return false
}

func (s *Server) listResources() []map[string]any {
	out := make([]map[string]any, 0, len(s.resources))
	for _, r := range s.resources {
		if s.resourceDisabled(r.uri) {
			continue
		}
		out = append(out, map[string]any{
			"uri": r.uri, "name": r.name, "description": r.desc, "mimeType": "text/plain",
		})
	}
	return out
}

func (s *Server) listResourceTemplates() []map[string]any {
	return []map[string]any{{
		"uriTemplate": sessionURIPrefix + "{session_id}",
		"name":        "Session History",
		"description": "Full command history for a specific session.",
		"mimeType":    "text/plain",
	}}
}

// handleResourceRead serves one resource by URI (static or the session template).
func (s *Server) handleResourceRead(params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "resources/read needs a uri"}
	}
	// Dynamic session template.
	if strings.HasPrefix(p.URI, sessionURIPrefix) {
		session := strings.TrimPrefix(p.URI, sessionURIPrefix)
		res, rerr := s.toolSessionHistory(json.RawMessage(fmt.Sprintf(`{"session_id":%q}`, session)))
		if rerr != nil {
			return nil, rerr
		}
		return resourceContents(p.URI, resultText(res)), nil
	}
	for _, r := range s.resources {
		if r.uri == p.URI {
			if s.resourceDisabled(p.URI) {
				return nil, &rpcError{Code: codeInvalidParams, Message: "resource disabled: " + p.URI}
			}
			text, rerr := r.read()
			if rerr != nil {
				return nil, rerr
			}
			return resourceContents(p.URI, text), nil
		}
	}
	return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource: " + p.URI}
}

func resourceContents(uri, text string) map[string]any {
	return map[string]any{
		"contents": []any{map[string]any{"uri": uri, "mimeType": "text/plain", "text": truncate(text, maxOutputChars)}},
	}
}

// resultText extracts the text of a tool result (best effort) for reuse inside
// a resource read.
func resultText(res any) string {
	m, ok := res.(map[string]any)
	if !ok {
		return ""
	}
	content, ok := m["content"].([]any)
	if !ok || len(content) == 0 {
		return ""
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := first["text"].(string)
	return t
}

// formatAgentActivity summarizes per-executor activity (agent commands only)
// from a sample: command count and success rate per executor, busiest first.
func formatAgentActivity(rows []rec.Record) string {
	type agg struct {
		count, ok, known int
	}
	by := map[string]*agg{}
	order := []string{}
	for i := range rows {
		r := rows[i]
		if r.Deleted() || r.Tag == "" {
			continue
		}
		a := by[r.Tag]
		if a == nil {
			a = &agg{}
			by[r.Tag] = a
			order = append(order, r.Tag)
		}
		a.count++
		if r.Exit != nil {
			a.known++
			if *r.Exit == 0 {
				a.ok++
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]].count > by[order[j]].count })
	var b strings.Builder
	b.WriteString("Agent activity\n\n")
	if len(order) == 0 {
		b.WriteString("(no agent commands)\n")
		return b.String()
	}
	for _, name := range order {
		a := by[name]
		success := "n/a"
		if a.known > 0 {
			success = fmt.Sprintf("%.0f%%", 100*float64(a.ok)/float64(a.known))
		}
		fmt.Fprintf(&b, "%-16s %5d cmds  success %s\n", name, a.count, success)
	}
	return b.String()
}
