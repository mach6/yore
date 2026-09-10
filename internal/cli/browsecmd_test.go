package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/tui/browse"
)

// TestPrefsRoundTripThroughUIState: ui.toml is rewritten whole on every save, so
// anything one of these two functions drops the other silently deletes from the
// file. They are only correct as a pair, which is what this asserts.
func TestPrefsRoundTripThroughUIState(t *testing.T) {
	want := browse.Prefs{
		Splits: browse.Splits{
			BrowseLeft: 333, BrowseTop: 700,
			AgentLeft: 260, AgentTop: 550, AgentHosts: 400,
			DevicesTop: 450,
		},
		Columns: map[string]browse.ColumnPrefs{
			"browse":   {Hidden: []string{"host", "tags"}, Sort: "dur", SortDesc: true},
			"prompts":  {Hidden: []string{"session"}, Sort: "cmds"},
			"commands": {Sort: "dur", SortDesc: true},
		},
	}

	got := prefsFromUI(uiFromPrefs(want))
	require.Equal(t, want, got, "every field must survive the trip in both directions")
}

// TestPrefsRoundTripThroughTheFile: and through ui.toml itself, since that is the
// journey the user's choices actually make.
func TestPrefsRoundTripThroughTheFile(t *testing.T) {
	dir := t.TempDir()
	want := browse.Prefs{
		Splits:  browse.Splits{BrowseLeft: 250},
		Columns: map[string]browse.ColumnPrefs{"prompts": {Hidden: []string{"session"}, Sort: "cmds"}},
	}
	require.NoError(t, config.SaveUI(dir, uiFromPrefs(want)))

	ui, err := config.LoadUI(dir)
	require.NoError(t, err)
	require.Equal(t, want, prefsFromUI(ui))
}

// TestEmptyPrefsWriteNothingExtra: an untouched browser leaves no column state in
// the file, so a hand-edited ui.toml is not littered with defaults.
func TestEmptyPrefsWriteNothingExtra(t *testing.T) {
	ui := uiFromPrefs(browse.Prefs{})
	require.Empty(t, ui.Columns)
	require.Equal(t, config.UIState{}, ui, "nothing chosen, nothing written")

	back := prefsFromUI(config.UIState{})
	require.Empty(t, back.Columns)
	require.Equal(t, browse.Splits{}, back.Splits)
}
