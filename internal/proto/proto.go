// Package proto is the newline-delimited-JSON protocol spoken over the
// daemon's unix socket. One JSON object per line in each direction; every
// Request receives exactly one Response.
package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mach6/yore/internal/rec"
)

// Ops.
const (
	OpPing     = "ping"     // liveness + resets the idle timer; also nudges spool ingest
	OpRecord   = "record"   // deliver one record for immediate ingest
	OpQuery    = "query"    // search
	OpHosts    = "hosts"    // per-host record counts (browse TUI sidebar)
	OpDelete   = "delete"   // tombstone one record by id
	OpDevices  = "devices"  // list enrolled devices
	OpApprove  = "approve"  // approve a pending device (DeviceID)
	OpRevoke   = "revoke"   // revoke a device and rotate keys (DeviceID)
	OpToken    = "token"    // mint a single-use enrollment token
	OpTokens   = "tokens"   // list enrollment tokens and what became of them
	OpRevokeTk = "revoketk" // cancel an unclaimed enrollment token (TokenID)
	OpStatus   = "status"   // daemon status
	OpSync     = "sync"     // force a push/pull cycle now
	OpTags     = "tags"     // list known user tags with counts
	OpShutdown = "shutdown" // graceful exit
)

// Query scopes.
const (
	ScopeLocal     = "local"     // this host only (shallow)
	ScopeAll       = "all"       // every host (deep)
	ScopeHost      = "host"      // one specific host (deep unless it is this host)
	ScopeSession   = "session"   // this shell session
	ScopeCwd       = "cwd"       // commands run in a given directory, any host
	ScopeWorkspace = "workspace" // commands run anywhere in the current git repo (local)
)

// Remote states for RemoteInfo.State.
const (
	RemoteOff         = "off"         // no server configured
	RemoteUnavailable = "unavailable" // configured but unreachable
	RemoteSyncing     = "syncing"     // fetch in progress
	RemoteOK          = "ok"          // cache warm
	// RemoteRevoked: the server refused this device because its membership was
	// revoked. Terminal until the device is enrolled again; unlike
	// RemoteUnavailable, retrying cannot fix it, so nothing should keep polling.
	RemoteRevoked = "revoked"
)

type Request struct {
	Op       string      `json:"op"`
	Record   *rec.Record `json:"record,omitempty"`
	Query    *QueryReq   `json:"query,omitempty"`
	Tags     *TagsReq    `json:"tags,omitempty"`      // OpTags parameters
	DeleteID string      `json:"delete_id,omitempty"` // OpDelete target
	DeviceID string      `json:"device_id,omitempty"` // OpApprove / OpRevoke target
	TokenID  string      `json:"token_id,omitempty"`  // OpRevokeTk target
}

// TagsReq parameterizes OpTags. Scope decides which commands the counts are
// taken over: ScopeLocal (the default) this host, ScopeAll every host in the
// RAM remote cache. Tags themselves are group-wide either way: the scope is
// about how much history is being counted, not which labels exist.
type TagsReq struct {
	Scope string `json:"scope,omitempty"`
}

type QueryReq struct {
	Q        string `json:"q"`
	Scope    string `json:"scope"`              // one of the Scope* constants; "" = ScopeLocal
	Host     string `json:"host,omitempty"`     // hostname filter for ScopeHost
	Session  string `json:"session,omitempty"`  // session id for ScopeSession
	Cwd      string `json:"cwd,omitempty"`      // directory for ScopeCwd
	Executor string `json:"executor,omitempty"` // executor filter, e.g. "claude-code"; "" = any
	// HumanOnly drops every command yore attributed to an agent (any executor
	// tag), leaving the ones the user typed. It is applied server-side on
	// purpose: filtering after Limit would spend the row budget on rows the
	// caller is about to throw away, and a machine where an agent ran all
	// morning would answer a 1000-row request with a handful.
	HumanOnly bool   `json:"human_only,omitempty"`
	Tag       string `json:"tag,omitempty"`   // freeform user-tag filter (matches any effective tag); "" = any
	Sort      string `json:"sort,omitempty"`  // "" = recency (newest first); "frecency" = frequency×recency
	Fuzzy     bool   `json:"fuzzy,omitempty"` // subsequence (fzf-style) matching instead of substring
	// Limit windows the matched rows: 0 = the server default (200), LimitAll =
	// every match. The daemon already holds the whole corpus in RAM and sorts
	// all matches before windowing, so LimitAll costs serialization, not work.
	Limit  int  `json:"limit,omitempty"`
	Offset int  `json:"offset,omitempty"`
	Dedupe bool `json:"dedupe,omitempty"` // collapse identical commands, newest wins

	// WantPrompts asks for the prompt records behind the window as well, in
	// QueryResp.Prompts. The agent explorer needs them because a prompt that
	// triggered no command has no row to be discovered from. PromptDays bounds
	// how far back they reach (0 = all).
	WantPrompts bool `json:"want_prompts,omitempty"`
	PromptDays  int  `json:"prompt_days,omitempty"`
}

// Sort modes for QueryReq.Sort.
const (
	SortRecency  = ""         // newest first
	SortFrecency = "frecency" // frequency × recency, same-dir boost (implies dedupe)
)

// LimitAll asks for every matching row rather than a window of them. It is for
// callers that browse the archive itself (a history explorer that stops short
// of your history is broken) as opposed to callers that want the top N.
const LimitAll = -1

type QueryResp struct {
	Rows  []rec.Record `json:"rows"`
	Total int          `json:"total"` // matches before Limit/Offset
	Scope string       `json:"scope"`
	// Prompts are the TypePrompt records covering the same period, present only
	// when QueryReq.WantPrompts asked for them. Rows already carry their prompt
	// text (the daemon fills it in from its index), so these matter for exactly
	// one thing: prompts that triggered no command and so appear in no row.
	Prompts []rec.Record `json:"prompts,omitempty"`
	// HiddenAgents is how many rows matched everything else and were dropped by
	// HumanOnly. A UI that hides a whole category of history has to be able to
	// say so; above all when the answer is otherwise "no matches" and the
	// command the user is looking for is sitting behind the filter.
	HiddenAgents int        `json:"hidden_agents,omitempty"`
	Remote       RemoteInfo `json:"remote"`
}

type RemoteInfo struct {
	State      string `json:"state"`
	LastSyncMs int64  `json:"last_sync_ms,omitempty"`
	Hosts      int    `json:"hosts,omitempty"` // remote hosts in the RAM cache
}

type StatusResp struct {
	PID       int        `json:"pid"`
	UptimeSec int64      `json:"uptime_sec"`
	LocalRows int        `json:"local_rows"`
	Remote    RemoteInfo `json:"remote"`
	Version   string     `json:"version"`
}

// HostCount is one host's live-record count.
type HostCount struct {
	Hostname string `json:"hostname"`
	HostID   string `json:"host_id"`
	Count    int    `json:"count"`
}

// HostsInfo answers OpHosts: every host with searchable records, local first,
// then remote hosts (when the sync cache is warm) by descending count.
type HostsInfo struct {
	Hosts  []HostCount `json:"hosts"`
	Remote RemoteInfo  `json:"remote"`
}

// DeviceInfo is one enrolled device, as shown in the browse devices pane.
type DeviceInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`         // pending | active | revoked
	Code   string `json:"code,omitempty"` // verification code (pending devices only)
	Self   bool   `json:"self,omitempty"` // this machine
}

// DevicesInfo answers OpDevices.
type DevicesInfo struct {
	Devices []DeviceInfo `json:"devices"`
}

// TokenInfo is a freshly minted single-use enrollment token, for adding
// another machine. The plaintext exists only in this response.
type TokenInfo struct {
	Token     string `json:"token"`
	ExpiresMs int64  `json:"expires_ms"`
}

// Token states, mirroring the server's (wire.Token*). The browser reads proto
// and never imports wire, so the names live in both places.
const (
	TokenOpen    = "open"
	TokenClaimed = "claimed"
	TokenExpired = "expired"
	TokenRevoked = "revoked"
)

// EnrollToken is one enrollment token as listed in the devices view. ID is the
// hex of its hash, not the token: the server keeps no plaintext, so a token
// can never be shown again after the moment it was minted.
type EnrollToken struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	CreatedMs int64  `json:"created_ms"`
	ExpiresMs int64  `json:"expires_ms"`
	ClaimedMs int64  `json:"claimed_ms,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"` // device id that enrolled on it
	RevokedMs int64  `json:"revoked_ms,omitempty"`
}

// TokensInfo answers OpTokens.
type TokensInfo struct {
	Tokens []EnrollToken `json:"tokens"`
}

// TagCount is one known user tag and how many commands carry it within the
// requested scope. Zero is a real answer: the tag is defined (a bare
// `tag create`, or an auto_tags rule) and nothing currently matches it.
type TagCount struct {
	Name  string `json:"name"`
	Desc  string `json:"desc,omitempty"`
	Count int    `json:"count"`
}

// TagsInfo answers OpTags: every known user tag, name-sorted, with the scope the
// counts were taken over echoed back so the caller can say what it counted.
type TagsInfo struct {
	Tags  []TagCount `json:"tags"`
	Scope string     `json:"scope,omitempty"`
}

type Response struct {
	OK      bool         `json:"ok"`
	Err     string       `json:"err,omitempty"`
	Query   *QueryResp   `json:"query,omitempty"`
	Hosts   *HostsInfo   `json:"hosts,omitempty"`
	Devices *DevicesInfo `json:"devices,omitempty"`
	Token   *TokenInfo   `json:"token,omitempty"`
	Tokens  *TokensInfo  `json:"tokens_list,omitempty"`
	Status  *StatusResp  `json:"status,omitempty"`
	Tags    *TagsInfo    `json:"tags,omitempty"`
}

// WriteMsg writes v as one JSON line.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadMsg reads one JSON line into v.
func ReadMsg(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("proto: bad message: %w", err)
	}
	return nil
}
