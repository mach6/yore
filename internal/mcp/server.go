// Package mcp is yore's Model Context Protocol server: a local, read-only, stdio
// JSON-RPC 2.0 endpoint that lets a coding agent (Claude Code, Cursor, …) query
// the shell history it is creating, across every machine you own. It never opens
// the bbolt store or a network port. It is a thin adapter over the daemon's
// query layer (internal/proto over the unix socket), so it inherits the daemon's
// single-writer guarantee and its cross-machine RAM corpus for free: an agent
// can ask "have I ever run this migration anywhere?" and get an answer spanning
// all enrolled devices, while the sync server still holds only ciphertext.
// Transport is stdio only (newline-delimited JSON, one message per line);
// logging goes to stderr. Security rests on the OS process boundary plus the
// read-only query surface: the server has no way to mutate history.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"yore/internal/proto"
	"yore/internal/risk"
)

// protocolVersion is the MCP revision we implement. We echo the client's
// requested version when it is one we recognize (better interop), else fall back
// to this.
const protocolVersion = "2025-06-18"

// maxOutputChars bounds a single tool/resource result so a huge history can't
// blow past an agent's context window. Truncation is UTF-8-boundary-safe.
const maxOutputChars = 50_000

// Querier is the read-only slice of the daemon client the server needs. The
// daemon.Client satisfies it; tests pass a fake.
type Querier interface {
	Query(proto.QueryReq) (proto.QueryResp, error)
}

// Options tunes server behavior; the zero value is usable (sane defaults apply
// in normalize).
type Options struct {
	DefaultLimit      int      // rows per tool call when the caller omits limit (default 20)
	DefaultDays       int      // lookback window when the caller omits days (default 7)
	DisabledTools     []string // tool names to hide and refuse
	DisabledResources []string // resource URI suffixes to hide and refuse
	ExcludeDirs       []string // cwd prefixes excluded from every query result
	Version           string   // reported in serverInfo.version
	LocalHost         string   // this machine's hostname, for labeling

	// Risk is the ruleset behind assess_risk and the risk-summary resource;
	// risk.Load's result, so the user's risk.toml applies here exactly as it
	// does to the browse detail panes. Nil falls back to the built-in rules.
	Risk *risk.Ruleset
}

func (o *Options) normalize() {
	if o.DefaultLimit <= 0 {
		o.DefaultLimit = 20
	}
	if o.DefaultDays <= 0 {
		o.DefaultDays = 7
	}
	if o.Risk == nil {
		o.Risk = risk.DefaultRuleset()
	}
}

// Server serves MCP over a single stdio pair. Construct with New, then Serve.
type Server struct {
	q         Querier
	opts      Options
	tools     []toolDef
	resources []resourceDef
}

// New builds a server over q with the given options.
func New(q Querier, opts Options) *Server {
	opts.normalize()
	s := &Server{q: q, opts: opts}
	s.tools = s.buildTools()
	s.resources = s.buildResources()
	return s
}

// --- JSON-RPC 2.0 wire types -------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC standard error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// Serve runs the read-dispatch-write loop until in reaches EOF. Each stdin line
// is one JSON-RPC message; responses are written to out, one per line. A
// notification (no id) yields no response. Malformed lines get a parse error.
func (s *Server) Serve(in io.Reader, out, logw io.Writer) error {
	_ = logw // reserved for future diagnostics; the transport is stdout-only
	br := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if resp, write := s.handleLine(line); write {
				if werr := writeMessage(out, resp); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// handleLine parses and dispatches one line, returning the response to write and
// whether to write it (notifications and blank lines produce none).
func (s *Server) handleLine(line []byte) (rpcResponse, bool) {
	if strings.TrimSpace(string(line)) == "" {
		return rpcResponse{}, false
	}
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return errorResponse(nil, codeParse, "parse error"), true
	}
	// A request with no id is a notification: dispatch for side effects, never
	// reply (per JSON-RPC 2.0).
	notification := len(req.ID) == 0
	result, rerr := s.dispatch(req.Method, req.Params)
	if notification {
		return rpcResponse{}, false
	}
	if rerr != nil {
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rerr}, true
	}
	return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, true
}

// dispatch routes one method to its handler.
func (s *Server) dispatch(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.handleInitialize(params), nil
	case "notifications/initialized":
		return nil, nil // notification; no result
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.listTools()}, nil
	case "tools/call":
		return s.handleToolCall(params)
	case "resources/list":
		return map[string]any{"resources": s.listResources()}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": s.listResourceTemplates()}, nil
	case "resources/read":
		return s.handleResourceRead(params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
}

// handleInitialize answers the handshake, echoing a recognized client protocol
// version for maximum interop.
func (s *Server) handleInitialize(params json.RawMessage) any {
	ver := protocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && knownProtocolVersion(p.ProtocolVersion) {
		ver = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities": map[string]any{
			"tools":     map[string]any{"listChanged": false},
			"resources": map[string]any{"subscribe": false, "listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    "yore-mcp",
			"version": s.opts.Version,
		},
		"instructions": "yore shell-history server. Query command history, agent " +
			"prompts, exit codes, sessions, and stats across ALL your machines " +
			"(pass scope=\"all\"): end-to-end encrypted, nothing leaves your devices.",
	}
}

func knownProtocolVersion(v string) bool {
	switch v {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		return true
	}
	return false
}

// --- helpers -----------------------------------------------------------------

func errorResponse(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// writeMessage writes v as one JSON line.
func writeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// textResult wraps plain text as an MCP tool result content block, truncating to
// maxOutputChars on a rune boundary.
func textResult(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": truncate(text, maxOutputChars)}},
	}
}

// toolError wraps text as an MCP tool result flagged isError (a tool-level
// failure, distinct from a JSON-RPC protocol error).
func toolError(format string, args ...any) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf(format, args...)}},
		"isError": true,
	}
}

// truncate clamps s to at most n bytes without splitting a rune, appending a
// notice when it cuts.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (output truncated)"
}

// utf8Start reports whether b is a UTF-8 leading byte (not a continuation).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
