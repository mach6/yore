package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/config"
	"yore/internal/proto"
	"yore/internal/rec"
)

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
			cfg:  config.Config{RecordSpacePrefixed: true},
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
			// An empty state dir has no redact.yml, so Load falls back to the
			// built-ins: the behavior this gate is asserting.
			require.Equal(t, tc.keep, filterDecision(t.TempDir(), tc.cfg, tc.cmd, tc.cwd))
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

// TestFormatHeadless covers headless search line formatting: with showHost the
// host column is left-padded to the widest hostname (so commands align across
// differing hostname lengths, including an empty hostname); without it the
// lines are the bare commands.
func TestFormatHeadless(t *testing.T) {
	rows := []rec.Record{
		{Hostname: "web1", Cmd: "ls -la"},
		{Hostname: "database-01", Cmd: "psql"},
		{Hostname: "", Cmd: "whoami"},
	}
	tests := []struct {
		name     string
		rows     []rec.Record
		showHost bool
		want     []string
	}{
		{
			name:     "host column aligns across widths",
			rows:     rows,
			showHost: true,
			// hostnames padded to len("database-01")==11, then two spaces.
			want: []string{
				"web1         ls -la",
				"database-01  psql",
				"             whoami",
			},
		},
		{
			name:     "no host column is bare commands",
			rows:     rows,
			showHost: false,
			want:     []string{"ls -la", "psql", "whoami"},
		},
		{
			name:     "no rows yields no lines",
			rows:     nil,
			showHost: true,
			want:     []string{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, formatHeadless(tc.rows, tc.showHost))
		})
	}
}

// TestHeadlessShowHost pins the rule that only `--scope all` (hsa) leads results
// with the host column: every single-host scope stays a bare command list.
func TestHeadlessShowHost(t *testing.T) {
	assert.True(t, headlessShowHost(proto.ScopeAll, false), "all-scope shows the host column")
	assert.False(t, headlessShowHost(proto.ScopeAll, true), "--no-host suppresses it even for all-scope")

	for _, scope := range []string{
		proto.ScopeLocal, proto.ScopeHost, proto.ScopeSession, proto.ScopeCwd, proto.ScopeWorkspace,
	} {
		assert.False(t, headlessShowHost(scope, false), "single-host scope %q hides the host column", scope)
	}
}

// TestUnknownSubcommandIsAnError covers every command group at once: cobra only
// checks the root for unknown subcommands, which left `yore tag refactor`
// printing help and exiting 0 while `yore daemon bogus` ignored the word and
// started the daemon. All of them now fail the same way, with the same status.
// Nothing here reaches a RunE (argument validation rejects the line first) so
// no daemon, server, or socket is touched.
func TestUnknownSubcommandIsAnError(t *testing.T) {
	for _, args := range [][]string{
		{"bogus"},
		{"tag", "refactor"},
		{"hook", "bogus"},
		{"daemon", "bogus"},
		{"server", "bogus"},
		{"devices", "bogus"},
		// Devices are managed in one place: the browser's pane. These were a
		// second implementation of the same actions and are gone.
		{"devices", "approve", "01AAAAAAAAAAAAAAAAAAAAAAAA"},
		{"devices", "revoke", "01AAAAAAAAAAAAAAAAAAAAAAAA"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assert.Equal(t, 2, Run(args), "an unknown subcommand is a usage error")
		})
	}
}

// TestGroupWithoutArgsPrintsHelp is the other half: a bare group still explains
// itself and exits 0, which is what makes the check above safe to apply to the
// whole tree.
func TestGroupWithoutArgsPrintsHelp(t *testing.T) {
	for _, group := range []string{"tag", "hook"} {
		t.Run(group, func(t *testing.T) {
			assert.Equal(t, 0, Run([]string{group}))
		})
	}
}
