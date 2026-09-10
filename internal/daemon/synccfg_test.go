package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/cryptobox"
	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/store"
	"github.com/mach6/yore/internal/syncer"
)

// newTestSyncer builds a syncer pointed at an unroutable host. Nothing in these
// tests performs I/O with it; it only needs to be a non-nil attachable value.
func newTestSyncer(t *testing.T) *syncer.Syncer {
	t.Helper()
	st, err := store.Open(t.TempDir())
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = st.Close() })
	key, err := cryptobox.GenerateDeviceKey()
	require.NoError(t, err, "GenerateDeviceKey")
	return syncer.New(st, syncer.NewHTTPClient("https://example.invalid", ""), key, time.Hour)
}

// TestLoadSyncConfChange covers the comparison that decides whether a config
// edit warrants re-attaching the syncer: identical configuration must compare
// equal (so a warm remote cache survives an unrelated edit), while a changed
// server must not.
func TestLoadSyncConfChange(t *testing.T) {
	dir := t.TempDir()

	unset := loadSyncConf(dir)
	assert.False(t, unset.configured(), "no config.toml should not be configured")

	require.NoError(t, config.Save(dir, config.Config{ServerURL: "https://a.example"}), "save")
	first := loadSyncConf(dir)
	assert.True(t, first.configured(), "a server URL is all sync needs; the device key is the credential")
	assert.NotEqual(t, unset, first, "configuring sync must register as a change")
	assert.Equal(t, first, loadSyncConf(dir), "re-reading the same config must compare equal")

	require.NoError(t, config.Save(dir, config.Config{ServerURL: "https://b.example"}), "save")
	assert.NotEqual(t, first, loadSyncConf(dir), "a new server URL must register as a change")

	require.NoError(t, config.Save(dir, config.Config{ServerURL: "https://b.example", ServerPin: "pin"}), "save")
	assert.NotEqual(t, loadSyncConf(dir), first, "a new certificate pin must register as a change")
}

// TestSameServerAs pins which field identifies the server. Everything else in
// syncConf is about how to reach it, and must not cost a re-pull of every
// machine's archive when it is edited.
func TestSameServerAs(t *testing.T) {
	base := syncConf{url: "https://a.example", pin: "p1", epoch: time.Hour, keep: 100}
	tests := []struct {
		name string
		cur  syncConf
		want bool
	}{
		{name: "identical", cur: base, want: true},
		{name: "pin added", cur: syncConf{url: base.url, pin: "p2", epoch: base.epoch, keep: base.keep}, want: true},
		{name: "pin cleared", cur: syncConf{url: base.url, epoch: base.epoch, keep: base.keep}, want: true},
		{name: "epoch and retention", cur: syncConf{url: base.url, pin: base.pin, epoch: time.Minute, keep: 5}, want: true},
		{name: "prompts toggled", cur: syncConf{url: base.url, pin: base.pin, epoch: base.epoch, keep: base.keep, syncPrompts: true}, want: true},
		{name: "different server", cur: syncConf{url: "https://b.example", pin: base.pin, epoch: base.epoch, keep: base.keep}, want: false},
		{name: "sync disabled", cur: syncConf{}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cur.sameServerAs(base))
		})
	}
}

func TestNewSyncerUnconfigured(t *testing.T) {
	tests := []struct {
		name string
		sc   syncConf
	}{
		{name: "no server", sc: syncConf{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(dir)
			require.NoError(t, err, "store.Open")
			t.Cleanup(func() { _ = st.Close() })
			assert.Nil(t, newSyncer(dir, st, tc.sc))
		})
	}
}

// TestNewSyncerNoDeviceKey pins that a configured server without a device key
// degrades to local-only rather than failing daemon startup.
func TestNewSyncerNoDeviceKey(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	require.NoError(t, err, "store.Open")
	t.Cleanup(func() { _ = st.Close() })

	sc := syncConf{url: "https://a.example", epoch: time.Hour}
	assert.Nil(t, newSyncer(dir, st, sc), "missing device.key should yield no syncer")
}

// TestRemoteAttach is the item-1 guarantee: sync configured after the daemon
// started comes alive without a restart, and switching servers never lets the
// previous server's cached history survive.
func TestRemoteAttach(t *testing.T) {
	rc := &remoteCache{state: proto.RemoteOff, cursors: map[string]uint64{}}
	require.False(t, rc.enabled(), "fresh cache should be disabled")
	require.Equal(t, proto.RemoteOff, rc.info().State)

	rc.attach(newTestSyncer(t), 0, false)
	assert.True(t, rc.enabled(), "attaching a syncer must enable the cache")
	assert.Equal(t, proto.RemoteUnavailable, rc.info().State, "newly attached sync is unavailable until it succeeds")

	// Warm the cache, then re-attach: the previous server's records and cursors
	// must not leak into the new one's view.
	warm := func() {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.records = []rec.Record{{ID: "1", Cmd: "echo hi", HostID: "h1", Hostname: "other"}}
		rc.cmds = []string{"echo hi"}
		rc.cursors["h1"] = 42
		rc.hydrated = true
	}
	warm()
	require.Len(t, rc.search("echo", ""), 1, "cache should be warm before re-attach")

	// Same server, different transport (a pin added or cleared): re-pulling every
	// machine's archive over a certificate edit is what the cache exists to avoid.
	rc.attach(newTestSyncer(t), 0, true)
	assert.Len(t, rc.search("echo", ""), 1, "the same server's records must survive")
	assert.Equal(t, uint64(42), rc.cursors["h1"], "the same server's pull cursors must survive")
	assert.True(t, rc.hydrated, "a surviving cache must not be re-hydrated from scratch")

	rc.attach(newTestSyncer(t), 0, false)
	assert.Empty(t, rc.search("echo", ""), "records from the previous server must be dropped")
	assert.Empty(t, rc.cursors, "pull cursors from the previous server must be dropped")
	assert.False(t, rc.hydrated, "a dropped cache must hydrate again")

	rc.attach(nil, 0, false)
	assert.False(t, rc.enabled(), "detaching must disable the cache")
	assert.Equal(t, proto.RemoteOff, rc.info().State)
}

func TestSyncTick(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		interval time.Duration
		want     time.Duration
	}{
		{name: "enabled uses the configured interval", enabled: true, interval: 5 * time.Minute, want: 5 * time.Minute},
		{name: "unconfigured polls for a config change", enabled: false, interval: 5 * time.Minute, want: unconfiguredPoll},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, syncTick(tc.enabled, tc.interval))
		})
	}
}

// TestSoloOpsDoNotDisturbSharedConn covers item 2: the network-backed ops run on
// their own connection, so the shared one stays usable for queries.
func TestSoloOpsDoNotDisturbSharedConn(t *testing.T) {
	dir := t.TempDir()
	c, _ := startDaemon(t, dir, 30*time.Second)

	// Sync with no server configured is a no-op, but still exercises solo().
	require.NoError(t, c.Sync(), "Sync")

	// Devices needs sync configured, so it reports that over its own connection
	// without poisoning the shared one.
	_, err := c.Devices()
	require.Error(t, err, "Devices should fail when sync is not configured")

	require.NoError(t, c.Ping(), "shared connection unusable after solo ops")
	_, err = c.Query(proto.QueryReq{})
	assert.NoError(t, err, "query on the shared connection after solo ops")
}
