package browse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/rec"
)

func TestRecentByExecutor(t *testing.T) {
	m := Model{statsRows: []rec.Record{
		{Cmd: "old", Tag: "claude-code", StartMs: 1},
		{Cmd: "new", Tag: "claude-code", StartMs: 3},
		{Cmd: "human", Tag: "", StartMs: 2},
		{Cmd: "cursor", Tag: "cursor", StartMs: 4},
	}}

	got := m.recentByExecutor("claude-code", 5)
	require.Len(t, got, 2, "only claude-code rows")
	assert.Equal(t, "new", got[0].Cmd, "newest first")
	assert.Equal(t, "old", got[1].Cmd)

	// The limit is honored.
	assert.Len(t, m.recentByExecutor("claude-code", 1), 1)
	// An executor with no rows yields nothing.
	assert.Empty(t, m.recentByExecutor("aider", 5))
}
