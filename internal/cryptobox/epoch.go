package cryptobox

import "time"

// EpochStart returns the start of the epoch of the given width that contains t,
// as unix milliseconds. Epochs tile the timeline in fixed width steps measured
// from the unix epoch, so every device computes the same boundary for the same
// t and width without coordination. For example, with width 24h the boundaries
// fall on 00:00 UTC each day.
//
// A non-positive width has no epochs to speak of; t's own unix-ms is returned.
func EpochStart(t time.Time, width time.Duration) int64 {
	ms := t.UnixMilli()
	w := int64(width / time.Millisecond)
	if w <= 0 {
		return ms
	}
	r := ms % w
	if r < 0 { // floor toward -inf for pre-1970 times
		r += w
	}
	return ms - r
}
