package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/rec"
)

// TestCommandListShowsExecutorAndTags pins the two apart in the one place an
// agent reads history from. The executor used to be printed only when the row
// carried no tags, so labelling a command erased the record of which agent had
// run it: the same conflation the tag index had.
func TestCommandListShowsExecutorAndTags(t *testing.T) {
	tests := []struct {
		name   string
		row    rec.Record
		want   []string
		absent []string
	}{
		{
			name: "executor and tags both appear",
			row:  rec.Record{Cmd: "go test ./...", Executor: "claude-code", Tags: []string{"refactor", "wip"}},
			want: []string{"claude-code", "#refactor #wip"},
		},
		{
			name:   "a command the user typed shows only its tags",
			row:    rec.Record{Cmd: "git rebase -i", Tags: []string{"refactor"}},
			want:   []string{"#refactor"},
			absent: []string{"claude-code"},
		},
		{
			name:   "an untagged agent command shows only its executor",
			row:    rec.Record{Cmd: "make build", Executor: "cursor"},
			want:   []string{"cursor"},
			absent: []string{"#"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := formatCommandList("Recent", []rec.Record{tc.row}, "laptop")
			for _, w := range tc.want {
				require.Containsf(t, out, w, "output was:\n%s", out)
			}
			for _, a := range tc.absent {
				require.NotContainsf(t, out, a, "output was:\n%s", out)
			}
		})
	}
}
