// Package proto is the newline-delimited-JSON protocol spoken over the
// daemon's unix socket. One JSON object per line in each direction; every
// Request receives exactly one Response.
package proto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"yore/internal/rec"
)

// Ops.
const (
	OpPing     = "ping"     // liveness + resets the idle timer; also nudges spool ingest
	OpRecord   = "record"   // deliver one record for immediate ingest
	OpQuery    = "query"    // search
	OpHosts    = "hosts"    // per-host record counts (browse TUI sidebar)
	OpDelete   = "delete"   // tombstone one record by id
	OpStatus   = "status"   // daemon status
	OpSync     = "sync"     // force a push/pull cycle now
	OpShutdown = "shutdown" // graceful exit
)

// Query scopes.
const (
	ScopeLocal   = "local"   // this host only (shallow)
	ScopeAll     = "all"     // every host (deep)
	ScopeHost    = "host"    // one specific host (deep unless it is this host)
	ScopeSession = "session" // this shell session
	ScopeCwd     = "cwd"     // commands run in a given directory, any host
)

// Remote states for RemoteInfo.State.
const (
	RemoteOff         = "off"         // no server configured
	RemoteUnavailable = "unavailable" // configured but unreachable
	RemoteSyncing     = "syncing"     // fetch in progress
	RemoteOK          = "ok"          // cache warm
)

type Request struct {
	Op       string      `json:"op"`
	Record   *rec.Record `json:"record,omitempty"`
	Query    *QueryReq   `json:"query,omitempty"`
	DeleteID string      `json:"delete_id,omitempty"` // OpDelete target
}

type QueryReq struct {
	Q       string `json:"q"`
	Scope   string `json:"scope"`             // one of the Scope* constants; "" = ScopeLocal
	Host    string `json:"host,omitempty"`    // hostname filter for ScopeHost
	Session string `json:"session,omitempty"` // session id for ScopeSession
	Cwd     string `json:"cwd,omitempty"`     // directory for ScopeCwd
	Limit   int    `json:"limit,omitempty"`   // 0 = server default (200)
	Offset  int    `json:"offset,omitempty"`
	Dedupe  bool   `json:"dedupe,omitempty"` // collapse identical commands, newest wins
}

type QueryResp struct {
	Rows   []rec.Record `json:"rows"`
	Total  int          `json:"total"` // matches before Limit/Offset
	Scope  string       `json:"scope"`
	Remote RemoteInfo   `json:"remote"`
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

type Response struct {
	OK     bool        `json:"ok"`
	Err    string      `json:"err,omitempty"`
	Query  *QueryResp  `json:"query,omitempty"`
	Hosts  *HostsInfo  `json:"hosts,omitempty"`
	Status *StatusResp `json:"status,omitempty"`
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
