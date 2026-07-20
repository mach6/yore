package theme

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

func TestHostStable(t *testing.T) {
	th := New()
	// Same name -> same color, repeatedly.
	for _, name := range []string{"web-01", "db.prod", "laptop", ""} {
		a := th.Host(name).GetForeground()
		b := th.Host(name).GetForeground()
		if a != b {
			t.Errorf("Host(%q) not stable within an instance: %v vs %v", name, a, b)
		}
	}
	// Same name -> same color across independently constructed themes
	// (a proxy for cross-process stability of the FNV hash + fixed palette).
	th2 := New()
	for _, name := range []string{"web-01", "db.prod", "gateway-7"} {
		if th.Host(name).GetForeground() != th2.Host(name).GetForeground() {
			t.Errorf("Host(%q) differs across Theme instances", name)
		}
	}
}

func TestHostDistinguishes(t *testing.T) {
	th := New()
	names := []string{"web", "db", "cache", "gw", "auth", "queue", "edge", "cron", "api", "worker"}
	seen := map[lipgloss.TerminalColor]bool{}
	for _, n := range names {
		seen[th.Host(n).GetForeground()] = true
	}
	// With 8 buckets and 10 distinct names, we must observe >1 distinct color.
	if len(seen) < 2 {
		t.Fatalf("Host produced only %d distinct colors across %d names; want >= 2", len(seen), len(names))
	}
}

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	nowMs := now.UnixMilli()
	ms := func(d time.Duration) int64 { return nowMs - int64(d/time.Millisecond) }
	at := func(y int, m time.Month, d int) int64 {
		return time.Date(y, m, d, 9, 0, 0, 0, time.UTC).UnixMilli()
	}

	tests := []struct {
		name string
		then int64
		want string
	}{
		{"future clamps to now", nowMs + 5000, "now"},
		{"sub-second", ms(500 * time.Millisecond), "now"},
		{"seconds", ms(42 * time.Second), "42s"},
		{"just under a minute", ms(59 * time.Second), "59s"},
		{"minutes", ms(5 * time.Minute), "5m"},
		{"hours", ms(3 * time.Hour), "3h"},
		{"days", ms(2 * 24 * time.Hour), "2d"},
		{"weeks", ms(3 * 7 * 24 * time.Hour), "3w"},
		{"older this year -> month day", at(2026, 3, 5), "Mar 5"},
		{"prior year -> month year", at(2023, 1, 9), "Jan 2023"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RelTime(nowMs, tc.then)
			if got != tc.want {
				t.Fatalf("RelTime = %q, want %q", got, tc.want)
			}
			if utf8.RuneCountInString(got) > 8 {
				t.Fatalf("RelTime = %q exceeds 8 columns", got)
			}
		})
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{-5, "0ms"},
		{412, "412ms"},
		{999, "999ms"},
		{1000, "1.0s"},
		{2100, "2.1s"},
		{59500, "59.5s"},
		{60000, "1m00s"},
		{312000, "5m12s"},
		{3599000, "59m59s"},
		{3600000, "1h00m"},
		{3780000, "1h03m"},
	}
	for _, tc := range tests {
		if got := Duration(tc.ms); got != tc.want {
			t.Errorf("Duration(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}
