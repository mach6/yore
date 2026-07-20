package theme

import (
	"io"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"
)

// TestRendererColorProfile proves NewWithRenderer's styles carry ANSI color
// exactly when the bound renderer's profile supports it. This is the core of
// the inline-search fix: styles must follow the renderer (the tty), not the
// default renderer's (possibly piped) os.Stdout.
func TestRendererColorProfile(t *testing.T) {
	tests := []struct {
		name      string
		profile   termenv.Profile
		wantColor bool
	}{
		{"truecolor keeps ANSI", termenv.TrueColor, true},
		{"ascii strips ANSI", termenv.Ascii, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := lipgloss.NewRenderer(io.Discard)
			r.SetColorProfile(tc.profile)
			th := NewWithRenderer(r)
			out := th.SynFlag.Render("--flag")
			if tc.wantColor {
				require.Contains(t, out, "\x1b[", "expected an ANSI escape under %v", tc.profile)
			} else {
				require.NotContains(t, out, "\x1b[", "expected no ANSI escape under %v", tc.profile)
				require.Equal(t, "--flag", out, "ascii profile must render the raw string")
			}
		})
	}
}

func TestHostStable(t *testing.T) {
	th := New()
	// Same name -> same color, repeatedly.
	for _, name := range []string{"web-01", "db.prod", "laptop", ""} {
		a := th.Host(name).GetForeground()
		b := th.Host(name).GetForeground()
		require.Equalf(t, a, b, "Host(%q) not stable within an instance", name)
	}
	// Same name -> same color across independently constructed themes
	// (a proxy for cross-process stability of the FNV hash + fixed palette).
	th2 := New()
	for _, name := range []string{"web-01", "db.prod", "gateway-7"} {
		require.Equalf(t, th.Host(name).GetForeground(), th2.Host(name).GetForeground(), "Host(%q) differs across Theme instances", name)
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
	require.GreaterOrEqualf(t, len(seen), 2, "Host produced only %d distinct colors across %d names", len(seen), len(names))
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
			require.Equal(t, tc.want, got)
			require.LessOrEqualf(t, utf8.RuneCountInString(got), 8, "RelTime = %q exceeds 8 columns", got)
		})
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		name string
		ms   int64
		want string
	}{
		{"zero", 0, "0ms"},
		{"negative clamps to zero", -5, "0ms"},
		{"sub-second", 412, "412ms"},
		{"just under a second", 999, "999ms"},
		{"one second", 1000, "1.0s"},
		{"seconds with tenths", 2100, "2.1s"},
		{"just under a minute", 59500, "59.5s"},
		{"one minute", 60000, "1m00s"},
		{"minutes and seconds", 312000, "5m12s"},
		{"just under an hour", 3599000, "59m59s"},
		{"one hour", 3600000, "1h00m"},
		{"hours and minutes", 3780000, "1h03m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Duration(tc.ms))
		})
	}
}
