package theme

import (
	"fmt"
	"time"
)

// RelTime renders a compact, width-stable (<= 8 columns) relative time for a
// row, given the current and event times in Unix milliseconds. It returns
// "now", "42s", "5m", "3h", "2d", "3w" for recent times, "Jan 5" for older
// times within the current year, and "Jan 2023" for anything earlier.
func RelTime(nowMs, thenMs int64) string {
	diff := nowMs - thenMs
	if diff < 0 {
		diff = 0
	}
	sec := diff / 1000
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
