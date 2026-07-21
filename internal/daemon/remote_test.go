package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
