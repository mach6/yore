package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"yore/internal/proto"
)

// TestPushArm covers the experimental push-on-record debounce decision: a push
// is armed only when the feature is enabled (debounce > 0) and none is already
// pending, so a burst of records coalesces into a single push and a disabled
// (0 or negative) debounce never arms.
func TestPushArm(t *testing.T) {
	tests := []struct {
		name        string
		debounce    time.Duration
		pending     bool
		wantArm     bool
		wantPending bool
	}{
		{"enabled + idle arms once", 2 * time.Second, false, true, true},
		{"enabled + pending coalesces (no re-arm)", 2 * time.Second, true, false, true},
		{"disabled (0) never arms", 0, false, false, false},
		{"disabled (negative) never arms", -1 * time.Second, false, false, false},
		{"disabled (0) leaves pending untouched", 0, true, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			arm, pending := pushArm(tc.debounce, tc.pending)
			assert.Equal(t, tc.wantArm, arm, "arm")
			assert.Equal(t, tc.wantPending, pending, "newPending")
		})
	}
}

// TestRemoteOnline covers the reachability gate for the eager push-on-record
// nudge: an eager push fires only while the server is believed reachable (a
// sync succeeded or is in flight). When it is unavailable — or unconfigured, or
// the cache is nil — new records stay spooled locally instead of firing a push
// that would only fail.
func TestRemoteOnline(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{"ok is online", proto.RemoteOK, true},
		{"syncing is online", proto.RemoteSyncing, true},
		{"unavailable is offline", proto.RemoteUnavailable, false},
		{"off is offline", proto.RemoteOff, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := &remoteCache{state: tc.state}
			assert.Equal(t, tc.want, rc.online())
		})
	}

	var nilCache *remoteCache
	assert.False(t, nilCache.online(), "nil cache is never online")
}
