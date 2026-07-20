package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/proto"
	"yore/internal/rec"
)

func TestMergeHostCounts(t *testing.T) {
	tests := []struct {
		name    string
		local   string
		localC  []proto.HostCount
		remoteC []proto.HostCount
		want    []proto.HostCount
	}{
		{
			name:   "local leads even when a remote host has more records",
			local:  "alpha",
			localC: []proto.HostCount{{Hostname: "alpha", HostID: "A", Count: 3}},
			remoteC: []proto.HostCount{
				{Hostname: "beta", HostID: "B", Count: 5},
				{Hostname: "gamma", HostID: "G", Count: 9},
			},
			want: []proto.HostCount{
				{Hostname: "alpha", HostID: "A", Count: 3}, // local first, regardless of count
				{Hostname: "gamma", HostID: "G", Count: 9}, // then non-local by count desc
				{Hostname: "beta", HostID: "B", Count: 5},
			},
		},
		{
			name:   "remote entry duplicating the local hostname is skipped",
			local:  "alpha",
			localC: []proto.HostCount{{Hostname: "alpha", HostID: "A", Count: 3}},
			remoteC: []proto.HostCount{
				{Hostname: "alpha", HostID: "A2", Count: 100}, // must not double-count
				{Hostname: "beta", HostID: "B", Count: 1},
			},
			want: []proto.HostCount{
				{Hostname: "alpha", HostID: "A", Count: 3},
				{Hostname: "beta", HostID: "B", Count: 1},
			},
		},
		{
			name:   "no local records: remote-only, still count-desc",
			local:  "alpha",
			localC: nil,
			remoteC: []proto.HostCount{
				{Hostname: "beta", HostID: "B", Count: 2},
				{Hostname: "gamma", HostID: "G", Count: 7},
			},
			want: []proto.HostCount{
				{Hostname: "gamma", HostID: "G", Count: 7},
				{Hostname: "beta", HostID: "B", Count: 2},
			},
		},
		{
			name:  "nothing at all",
			local: "alpha",
			want:  []proto.HostCount{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeHostCounts(tt.local, tt.localC, tt.remoteC)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestRemoteCacheHostCounts(t *testing.T) {
	// Buildable and callable without a syncer — enabled() is false here.
	rc := &remoteCache{records: []rec.Record{
		{ID: "1", Hostname: "beta", HostID: "B", Cmd: "ls"},
		{ID: "2", Hostname: "beta", HostID: "B", Cmd: "pwd"},
		{ID: "3", Hostname: "gamma", HostID: "G", Cmd: "top"},
	}}
	require.False(t, rc.enabled(), "test cache must not need a syncer")

	got := rc.hostCounts()
	require.Len(t, got, 2)
	byHost := map[string]proto.HostCount{}
	for _, hc := range got {
		byHost[hc.Hostname] = hc
	}
	require.Equal(t, proto.HostCount{Hostname: "beta", HostID: "B", Count: 2}, byHost["beta"])
	require.Equal(t, proto.HostCount{Hostname: "gamma", HostID: "G", Count: 1}, byHost["gamma"])

	// nil and empty caches contribute nothing.
	var nilRC *remoteCache
	require.Nil(t, nilRC.hostCounts())
	require.Nil(t, (&remoteCache{}).hostCounts())
}
