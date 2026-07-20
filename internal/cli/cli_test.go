package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"yore/internal/config"
)

// boolPtr is a tiny helper for setting *bool config fields in table rows.
func boolPtr(b bool) *bool { return &b }

// TestFilterDecision covers the keep/drop gate mirrored from runRecord.
func TestFilterDecision(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		cmd  string
		cwd  string
		keep bool
	}{
		{
			name: "clean command is kept",
			cfg:  config.Config{},
			cmd:  "git status",
			cwd:  "/home/user/project",
			keep: true,
		},
		{
			name: "secret export is dropped",
			cfg:  config.Config{},
			cmd:  "export SECRET=x",
			cwd:  "/home/user/project",
			keep: false,
		},
		{
			name: "space-prefixed dropped by default",
			cfg:  config.Config{},
			cmd:  " ls -la",
			cwd:  "/home/user/project",
			keep: false,
		},
		{
			name: "tab-prefixed dropped by default",
			cfg:  config.Config{},
			cmd:  "\tls -la",
			cwd:  "/home/user/project",
			keep: false,
		},
		{
			name: "space-prefixed kept when RecordSpacePrefixed=true",
			cfg:  config.Config{RecordSpacePrefixed: boolPtr(true)},
			cmd:  " ls -la",
			cwd:  "/home/user/project",
			keep: true,
		},
		{
			name: "cwd under an ignore dir is dropped",
			cfg:  config.Config{IgnoreDirs: []string{"/home/user/secrets"}},
			cmd:  "ls -la",
			cwd:  "/home/user/secrets/sub",
			keep: false,
		},
		{
			name: "cwd outside ignore dir is kept",
			cfg:  config.Config{IgnoreDirs: []string{"/home/user/secrets"}},
			cmd:  "ls -la",
			cwd:  "/home/user/project",
			keep: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.keep, filterDecision(tc.cfg, tc.cmd, tc.cwd))
		})
	}
}

// TestShellHistoryLine covers the zsh extended-history line formatting.
func TestShellHistoryLine(t *testing.T) {
	tests := []struct {
		name    string
		startMs int64
		cmd     string
		want    string
	}{
		{
			name:    "single line with timestamp",
			startMs: 1_700_000_000_000,
			cmd:     "git status",
			want:    ": 1700000000:0;git status",
		},
		{
			name:    "zero timestamp",
			startMs: 0,
			cmd:     "ls",
			want:    ": 0:0;ls",
		},
		{
			name:    "sub-second timestamp truncates to seconds",
			startMs: 1500,
			cmd:     "echo hi",
			want:    ": 1:0;echo hi",
		},
		{
			name:    "multiline command escapes newlines as backslash-newline",
			startMs: 2000,
			cmd:     "for i in 1 2\ndo echo $i\ndone",
			want:    ": 2:0;for i in 1 2\\\ndo echo $i\\\ndone",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, shellHistoryLine(tc.startMs, tc.cmd))
		})
	}
}
