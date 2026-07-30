package browse

// Pane geometry: where each pane sits in the rendered frame, where the draggable
// seams between them are, and how a dragged seam is remembered. The renderers in
// view.go / agents.go size themselves from this, and mouse.go hit-tests against
// it, so there is exactly one place that decides how the screen is carved up.

// rect is a pane's position in the rendered frame, in absolute screen cells with
// (0,0) at the top-left of the whole view (the title line).
type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool {
	return r.w > 0 && r.h > 0 && x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// maxPanes is the largest pane count any view lays out (the agent explorer's
// five-pane grid).
const maxPanes = 5

// layout is the active view's pane geometry, recomputed by applyLayout. Panes are
// stored in the view's own focus order so a focus value indexes straight into p;
// an entry a view does not lay out is left zero, which contains() rejects, so a
// hit test sweeps all of p rather than tracking how many entries are live. (It
// used to carry a count, and zooming — which parks one full-frame rect at the
// focused pane's own index — set it to 1, so the wheel stopped hit-testing every
// pane but the first.) vDiv/hDiv/hDiv2 are the draggable seams; -1 means this
// view has no such seam (or the layout is zoomed to a single pane). hDivFrom is
// where the horizontal seam starts — the browse view splits only its right-hand
// column. hDiv2 is the agent explorer's second horizontal seam, between the
// executor sidebar and the host pane; it spans only the left column.
type layout struct {
	p        [maxPanes]rect
	vDiv     int
	hDiv     int
	hDivFrom int
	hDiv2    int
}

// noDividers is the geometry of a single full-screen pane.
func noDividers() layout { return layout{vDiv: -1, hDiv: -1, hDiv2: -1} }

// Prefs is everything the browser remembers between runs — the seams the user
// dragged and the columns they reshaped. It is one struct because ui.toml is one
// file: saving half of it would erase the other half.
//
// It is exported because it is what gets persisted: Options.Prefs restores it and
// Options.SavePrefs writes it back.
type Prefs struct {
	Splits Splits

	// Columns is one entry per reshapeable table, keyed by the table's own name.
	// Nil means every table opens at its defaults.
	Columns map[string]ColumnPrefs
}

// ColumnPrefs is one table's column choices, with columns named rather than
// numbered — an index in a file that outlives a release would come to mean a
// different column the moment one is added.
type ColumnPrefs struct {
	Hidden   []string
	Sort     string
	SortDesc bool
}

// Splits holds the divider positions the user has dragged to, as a fraction of
// the axis each one cuts, so the chosen proportions survive a terminal resize.
// Zero means "auto": the view's own default sizing applies. The browse view
// therefore keeps the layout it has always had until a divider is actually moved.
//
// The unit is per-mille, not percent: on a 140-column terminal one percent is
// 1.4 cells, coarse enough that a dragged seam would visibly snap away from the
// pointer instead of tracking it.
//
// It is exported because it is what gets persisted: Options.Splits restores a
// remembered layout and Options.SaveSplits is called when a drag finishes.
type Splits struct {
	BrowseLeft int // host sidebar width, ‰ of the terminal width
	BrowseTop  int // results-table height, ‰ of the middle region
	AgentLeft  int // agent sidebar width, ‰ of the terminal width
	AgentTop   int // prompt-list height, ‰ of the middle region
	AgentHosts int // executor-list height, ‰ of the explorer's top-left region
	DevicesTop int // device-list height, ‰ of the middle region
}

// Divider bounds. The ratios keep both sides of a seam meaningful; the absolute
// minimums are what a pane needs for a title plus a row of content.
const (
	ratioFull = 1000 // a whole axis, in the per-mille unit paneSplits uses

	minColRatio = 120
	maxColRatio = 600
	minRowRatio = 200
	maxRowRatio = 800
	minPaneCols = 12
	minPaneRows = 4

	// Default agent-explorer proportions: a sidebar narrow enough to leave the
	// prompt table room, and a prompt list that keeps more rows than the command
	// pane below it.
	defaultAgentLeftRatio = 260
	defaultAgentTopRatio  = 550

	// Devices default: the machine list is short and bounded (you have as many
	// machines as you have), while tokens accumulate — so the tokens pane gets
	// the larger share.
	defaultDevicesTopRatio = 400
)

// withDefaults fills in the agent explorer's starting proportions. That view is
// new, so it has no legacy layout to preserve and simply opens at a sensible
// ratio; the browse view's zeros are left alone, keeping the exact layout it has
// always had until the user drags a seam.
func (s Splits) withDefaults() Splits {
	if s.AgentLeft == 0 {
		s.AgentLeft = defaultAgentLeftRatio
	}
	if s.AgentTop == 0 {
		s.AgentTop = defaultAgentTopRatio
	}
	if s.DevicesTop == 0 {
		s.DevicesTop = defaultDevicesTopRatio
	}
	return s
}

// dragKind identifies which seam an in-flight mouse drag is moving.
type dragKind int

const (
	dragNone dragKind = iota
	dragVert
	dragHoriz
	dragHosts // the explorer's left-column seam, between the executor and host panes
)

// clampRatio bounds a divider ratio to a usable range.
func clampRatio(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ratioOf converts a cell offset along an axis into the stored ratio. It rounds
// to nearest so ratioOf and splitAt round-trip: a seam dropped on a column comes
// back on that same column.
func ratioOf(cells, total int) int {
	if total < 1 {
		return 0
	}
	return (ratioFull*cells + total/2) / total
}

// splitAt turns a divider ratio into a cell count along an axis of `total`
// cells, leaving at least `floor` cells on both sides. ratio == 0 means the
// caller's own default (fallback) applies.
func splitAt(ratio, total, floor, fallback int) int {
	v := fallback
	if ratio > 0 {
		v = (total*ratio + ratioFull/2) / ratioFull
	}
	if v < floor {
		v = floor
	}
	if v > total-floor {
		v = total - floor
	}
	if v < 1 {
		v = 1
	}
	return v
}

// nearSeam reports whether a mouse coordinate lands on a divider. A seam between
// two bordered boxes is two cells wide (one border from each), and `seam` is the
// second of them, so both count as a hit.
func nearSeam(v, seam int) bool { return seam >= 0 && (v == seam || v == seam-1) }

// --- per-view geometry ---------------------------------------------------

// browseGeom lays out the three browse panes: the host sidebar down the left,
// the results table over the detail pane on the right. leftW/tableH are the
// already-resolved outer sizes so the renderers and the geometry cannot drift.
func browseGeom(w, mid, leftW, tableH int) layout {
	right := w - leftW
	return layout{
		p: [maxPanes]rect{
			{x: 0, y: 1, w: leftW, h: mid},
			{x: leftW, y: 1, w: right, h: tableH},
			{x: leftW, y: 1 + tableH, w: right, h: mid - tableH},
		},
		vDiv:     leftW,
		hDiv:     1 + tableH,
		hDivFrom: leftW,
		hDiv2:    -1,
	}
}

// devicesGeom lays out the devices view: the enrolled machines over the
// enrollment tokens, both full width. One seam, spanning the frame.
func devicesGeom(w, mid, topH int) layout {
	return layout{
		p: [maxPanes]rect{
			{x: 0, y: 1, w: w, h: topH},
			{x: 0, y: 1 + topH, w: w, h: mid - topH},
		},
		vDiv:     -1,
		hDiv:     1 + topH,
		hDivFrom: 0,
		hDiv2:    -1,
	}
}

// agentGeom lays out the agent explorer's five panes: the executor sidebar over
// the host list over the details pane down the left, the prompt list over its
// command pane on the right. hostsH is carved from the sidebar's share of the
// top row — the host list is content-sized, so the executor list flexes above
// it. Both seams span the full frame, so either can be grabbed anywhere along it.
func agentGeom(w, mid, leftW, topH, hostsH int) layout {
	right := w - leftW
	return layout{
		p: [maxPanes]rect{
			{x: 0, y: 1, w: leftW, h: topH - hostsH},
			{x: 0, y: 1 + topH - hostsH, w: leftW, h: hostsH},
			{x: leftW, y: 1, w: right, h: topH},
			{x: leftW, y: 1 + topH, w: right, h: mid - topH},
			{x: 0, y: 1 + topH, w: leftW, h: mid - topH},
		},
		vDiv:     leftW,
		hDiv:     1 + topH,
		hDivFrom: 0,
		hDiv2:    1 + topH - hostsH,
	}
}
