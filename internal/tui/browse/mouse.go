package browse

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Mouse support: click a pane to focus it, drag the seam between panes to
// resize them, and use the wheel to scroll whichever pane the pointer is over.
// Everything hit-tests against the geometry applyLayout resolved (m.geo), so the
// mouse and the renderer can never disagree about where a pane is.

// wheelStep is how many rows one notch of the wheel moves a list.
const wheelStep = 3

// handleMouse routes a mouse event. A drag in flight owns every motion until the
// button is released, so the pointer can leave the seam without losing it.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// The stats screen is one fixed full-frame block: nothing to focus, nothing
	// to scroll, no seam to grab. It still carries the browse view's geometry
	// (applyGeometry has no case for it), so without this the wheel moved the
	// browse sidebar and a drag resized panes nobody could see.
	if m.view == viewStats {
		return m, nil
	}

	switch msg.Action {
	case tea.MouseActionRelease:
		// Persist once the drag settles rather than on every motion event: one
		// write per resize, and the file always holds a layout the user stopped on.
		wasDragging := m.drag != dragNone
		m.drag = dragNone
		if wasDragging {
			return m, m.savePrefsCmd()
		}
		return m, nil
	case tea.MouseActionMotion:
		if m.drag != dragNone {
			m.moveDivider(msg.X, msg.Y)
		}
		return m, nil
	}

	// A press: grab a seam if the pointer is on one, else focus the pane clicked.
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.wheel(msg.X, msg.Y, -wheelStep)
	case tea.MouseButtonWheelDown:
		return m.wheel(msg.X, msg.Y, wheelStep)
	case tea.MouseButtonLeft:
		m.hscroll = 0
		if k := m.seamAt(msg.X, msg.Y); k != dragNone {
			m.drag = k
			m.moveDivider(msg.X, msg.Y)
			return m, nil
		}
		return m.focusAt(msg.X, msg.Y)
	}
	return m, nil
}

// seamAt reports which divider (if any) the pointer is on. The vertical seam is
// tested first: where the two seams cross, dragging the column feels more
// natural than dragging the row.
func (m Model) seamAt(x, y int) dragKind {
	g := m.geo
	if y < 1 || y > m.midHeight {
		return dragNone
	}
	if nearSeam(x, g.vDiv) {
		return dragVert
	}
	if nearSeam(y, g.hDiv) && x >= g.hDivFrom {
		return dragHoriz
	}
	if nearSeam(y, g.hDiv2) && x < g.vDiv {
		return dragHosts
	}
	return dragNone
}

// moveDivider re-splits the layout so the dragged seam follows the pointer. The
// position is stored as a percentage of the axis, so the ratio the user chose
// survives a later terminal resize.
func (m *Model) moveDivider(x, y int) {
	switch m.drag {
	case dragVert:
		if m.width < 1 {
			return
		}
		if m.zoomDetailActive() {
			// This seam's ratio is the detail companion's own width, unlike the
			// sidebar seam it otherwise shares vDiv with — the companion sits on
			// the right, so its share is the far side of where the pointer is.
			m.splits.ZoomDetail = clampRatio(ratioFull-ratioOf(x, m.width), minColRatio, maxColRatio)
			break
		}
		// Only the two split-column views have a vertical seam; the devices view's
		// panes are full width, so seamAt can never report one there (its vDiv is
		// -1, which nearSeam rejects).
		r := clampRatio(ratioOf(x, m.width), minColRatio, maxColRatio)
		if m.view == viewAgents {
			m.splits.AgentLeft = r
		} else {
			m.splits.BrowseLeft = r
		}
	case dragHoriz:
		if m.midHeight < 1 {
			return
		}
		r := clampRatio(ratioOf(y-1, m.midHeight), minRowRatio, maxRowRatio)
		switch m.view {
		case viewAgents:
			m.splits.AgentTop = r
		case viewDevices:
			// The devices view has this seam too, and it is the one it has: MACHINES
			// over TOKENS. Without its own case the drag moved the browse view's
			// seam, so the pane under the pointer stayed exactly where it was.
			m.splits.DevicesTop = r
		default:
			m.splits.BrowseTop = r
		}
	case dragHosts:
		// This seam lives inside the explorer's top-left region, so the ratio is
		// of that region's height (the main horizontal seam's position), not the
		// whole frame — dragging the main seam later keeps the proportion.
		topH := m.geo.hDiv - 1
		if topH < 1 {
			return
		}
		m.splits.AgentHosts = clampRatio(ratioOf(y-1, topH), minRowRatio, maxRowRatio)
	default:
		return
	}
	m.applyLayout()
	m.syncDetail()
}

// savePrefsCmd persists the whole remembered UI state off the render path — the
// seams and every table's columns together, because they share one file and
// writing half of it would erase the other half. A failure to write is
// deliberately swallowed: remembered layout is a convenience, and losing it must
// never interrupt browsing or steal the status bar.
func (m Model) savePrefsCmd() tea.Cmd {
	save := m.opts.SavePrefs
	if save == nil {
		return nil
	}
	prefs := Prefs{Splits: m.splits, Columns: colPrefsOf(m.cols)}
	return func() tea.Msg {
		_ = save(prefs)
		return nil
	}
}

// focusAt moves focus to the pane under the pointer.
func (m Model) focusAt(x, y int) (tea.Model, tea.Cmd) {
	i, ok := m.paneAt(x, y)
	if !ok {
		return m, nil
	}
	switch m.view {
	case viewAgents:
		m.setAgentPane(agentPane(i))
		return m, nil
	case viewDevices:
		// Same as tab, and the pane keys (a/x/n) follow focus, so a click is how
		// you aim them with the mouse. A pending confirmation is unaffected: it
		// belongs to the pane that armed it and is drawn there (see devPaneTail),
		// so clicking away cannot leave the question stranded on the wrong list.
		m.dpane = devPane(i)
		m.applyLayout()
		return m, nil
	}
	m.focus = focus(i)
	m.applyLayout()
	return m, nil
}

// paneAt returns the index (into the active view's focus order) of the pane
// containing the pointer. It sweeps every slot: a view that lays out fewer panes
// leaves the rest zero, and a zoomed layout parks its one full-frame rect at the
// zoomed pane's own index — which is not necessarily the first.
func (m Model) paneAt(x, y int) (int, bool) {
	for i := range m.geo.p {
		if m.geo.p[i].contains(x, y) {
			return i, true
		}
	}
	return 0, false
}

// wheel scrolls the list under the pointer without moving focus, so glancing at
// a neighbouring pane costs nothing.
func (m Model) wheel(x, y, d int) (tea.Model, tea.Cmd) {
	i, ok := m.paneAt(x, y)
	if !ok {
		return m, nil
	}
	m.hscroll = 0
	if m.view == viewDevices {
		// Move the cursor in the list under the pointer, leaving m.dpane alone —
		// the same "glancing at a neighbouring pane costs nothing" rule the other
		// views follow. Falling through to the browse arm scrolled the HOST sidebar
		// of a view that was not even on screen, and re-ran its query.
		if devPane(i) == dpTokens {
			m.tokSel = clampIndex(m.tokSel+d, len(m.tokens))
		} else {
			m.devSel = clampIndex(m.devSel+d, len(m.devices))
		}
		return m, nil
	}
	if m.view == viewAgents {
		// Scroll the pane the pointer is over, leaving m.apane alone.
		switch agentPane(i) {
		case apAgents:
			m.selectAgent(m.agentSel + d)
		case apHosts:
			m.selectAgentHost(m.agentHostSel + d)
		case apPrompts:
			m.selectPrompt(m.promptSel + d)
		case apInfo:
			m.scrollInfo(d)
		default:
			m.setDrill(m.drillSel + d)
		}
		return m, nil
	}
	switch focus(i) {
	case focusHosts:
		return m.moveHost(d)
	case focusTable:
		m.moveSel(d)
		m.syncDetail()
	case focusDetail:
		m.detail.SetYOffset(m.detail.YOffset + d)
	}
	return m, nil
}
