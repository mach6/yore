package theme

import (
	"fmt"
	"time"
)

// Unknown is the placeholder for a value the record never carried. A record
// whose timestamp is zero has no known time — bare bash history files hold no
// "#<epoch>" lines, so everything imported from one arrives untimed. Rendering
// that as 1970-01-01 reads like a bug rather than like missing data.
const Unknown = "—"

// RelTime renders a compact, width-stable (<= 8 columns) relative time for a
// row, given the current and event times in Unix milliseconds. It returns
// "now", "42s", "5m", "3h", "2d", "3w" for recent times, "Jan 5" for older
// times within the current year, and "Jan 2023" for anything earlier. A zero
// thenMs means "no timestamp" and renders as Unknown.
func RelTime(nowMs, thenMs int64) string {
	if thenMs <= 0 {
		return Unknown
	}
	return span(max(nowMs-thenMs, 0)/1000, nowMs, thenMs)
}

// TimeLeft is RelTime's forward-looking twin: how much of a deadline is still
// to run, in the same compact units, so "expires 25m" reads the way "minted 5m"
// does. A deadline already passed renders as "now".
//
// RelTime cannot answer this. It clamps a future time to zero and calls it
// "now", which turned a token with 30 minutes left into one that "expires now"
// — the single most alarming thing it could have said about a token that was
// perfectly good.
func TimeLeft(nowMs, thenMs int64) string {
	if thenMs <= 0 {
		return Unknown
	}
	return span(max(thenMs-nowMs, 0)/1000, nowMs, thenMs)
}

// span renders a distance of sec seconds in the compact units both directions
// share, falling back to thenMs as a date once a relative unit stops saying
// anything useful.
func span(sec, nowMs, thenMs int64) string {
	switch {
	case sec < 1:
		return "now"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	case sec < 7*86400:
		return fmt.Sprintf("%dd", sec/86400)
	case sec < 35*86400:
		return fmt.Sprintf("%dw", sec/(7*86400))
	}
	then := time.UnixMilli(thenMs).UTC()
	if then.Year() == time.UnixMilli(nowMs).UTC().Year() {
		return then.Format("Jan 2")
	}
	return then.Format("Jan 2006")
}

// AbsTime renders an absolute local wall-clock time for a details pane, or
// Unknown when ms is zero (see Unknown).
func AbsTime(ms int64) string {
	if ms <= 0 {
		return Unknown
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

// Duration renders a command's run time compactly: "412ms", "2.1s",
// "5m12s", "1h03m".
func Duration(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	case ms < 3_600_000:
		return fmt.Sprintf("%dm%02ds", ms/60_000, (ms%60_000)/1000)
	default:
		return fmt.Sprintf("%dh%02dm", ms/3_600_000, (ms%3_600_000)/60_000)
	}
}
