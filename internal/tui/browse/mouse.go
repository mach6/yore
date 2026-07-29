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
	switch msg.Action {
	case tea.MouseActionRelease:
		// Persist once the drag settles rather than on every motion event: one
		// write per resize, and the file always holds a layout the user stopped on.
		wasDragging := m.drag != dragNone
		m.drag = dragNone
		if wasDragging {
			return m, m.saveSplitsCmd()
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
		if m.view == viewAgents {
			m.splits.AgentTop = r
		} else {
			m.splits.BrowseTop = r
		}
	default:
		return
	}
	m.applyLayout()
	m.syncDetail()
}

// saveSplitsCmd persists the current pane layout off the render path. A failure
// to write is deliberately swallowed: a remembered layout is a convenience, and
// losing it must never interrupt browsing or steal the status bar.
func (m Model) saveSplitsCmd() tea.Cmd {
	save, splits := m.opts.SaveSplits, m.splits
	if save == nil {
		return nil
	}
	return func() tea.Msg {
		_ = save(splits)
		return nil
	}
}

// focusAt moves focus to the pane under the pointer.
func (m Model) focusAt(x, y int) (tea.Model, tea.Cmd) {
	i, ok := m.paneAt(x, y)
	if !ok {
		return m, nil
	}
	if m.view == viewAgents {
		m.setAgentPane(agentPane(i))
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
	if m.view == viewAgents {
		// Scroll the pane the pointer is over, leaving m.apane alone.
		switch agentPane(i) {
		case apAgents:
			m.selectAgent(m.agentSel + d)
		case apPrompts:
			m.selectPrompt(m.promptSel + d)
		default:
			m.drillSel = clampIndex(m.drillSel+d, m.drillLen())
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
