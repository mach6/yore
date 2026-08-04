package browse

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"yore/internal/proto"
	"yore/internal/tui/theme"
)

// devPane identifies one of the devices view's two panes. The values are also
// indices into the layout's rect array, so a pane is its own focus value.
type devPane int

const (
	dpDevices devPane = iota // the enrolled machines
	dpTokens                 // the enrollment tokens and what became of them
	devPaneCount
)

// devicesResultMsg carries a device list (a fetch, or the result of an
// approve/revoke) back to the model.
type devicesResultMsg struct {
	info proto.DevicesInfo
	err  error
}

// tokensResultMsg carries the enrollment-token list back to the model.
type tokensResultMsg struct {
	info proto.TokensInfo
	err  error
}

// mintedMsg carries a freshly minted token. Its plaintext exists nowhere else —
// the server kept only a hash — so it is put on screen and never written down.
type mintedMsg struct {
	info proto.TokenInfo
	err  error
}

// enterDevices opens the devices view and fetches both lists.
func (m Model) enterDevices() (Model, tea.Cmd) {
	m.view = viewDevices
	m.clearChecked() // leaving the table: see toggleAgents for why
	m.devConfirm, m.dpane, m.zoom, m.zoomDetail = "", dpDevices, false, false
	m.applyLayout()
	return m, m.refreshDevicesCmd()
}

// devicesCmd fetches the enrolled devices.
func (m Model) devicesCmd() tea.Cmd {
	b := m.b
	return func() tea.Msg {
		info, err := b.Devices()
		return devicesResultMsg{info: info, err: err}
	}
}

// tokensCmd fetches the enrollment tokens.
func (m Model) tokensCmd() tea.Cmd {
	b := m.b
	return func() tea.Msg {
		info, err := b.Tokens()
		return tokensResultMsg{info: info, err: err}
	}
}

// refreshDevicesCmd refetches both lists. They are one screen and an action on
// either can change the other — enrolling claims a token, so a device appearing
// and a token turning "claimed" are the same event.
func (m Model) refreshDevicesCmd() tea.Cmd {
	return tea.Batch(m.devicesCmd(), m.tokensCmd())
}

// deviceActionCmd approves or revokes a device, then always re-fetches the list
// so the pane reflects the new state even if the action itself errored.
func (m Model) deviceActionCmd(id string, approve bool) tea.Cmd {
	b := m.b
	return func() tea.Msg {
		var actErr error
		if approve {
			actErr = b.Approve(id)
		} else {
			actErr = b.Revoke(id)
		}
		info, fetchErr := b.Devices()
		err := actErr
		if err == nil {
			err = fetchErr
		}
		return devicesResultMsg{info: info, err: err}
	}
}

// revokeTokenCmd cancels an unclaimed token and refetches, on the same
// "report the action's error, but show the new truth either way" rule as
// deviceActionCmd.
func (m Model) revokeTokenCmd(id string) tea.Cmd {
	b := m.b
	return func() tea.Msg {
		actErr := b.RevokeToken(id)
		info, fetchErr := b.Tokens()
		err := actErr
		if err == nil {
			err = fetchErr
		}
		return tokensResultMsg{info: info, err: err}
	}
}

// mintCmd issues a new enrollment token.
func (m Model) mintCmd() tea.Cmd {
	b := m.b
	return func() tea.Msg {
		info, err := b.Token()
		return mintedMsg{info: info, err: err}
	}
}

func (m Model) selectedDevice() (proto.DeviceInfo, bool) {
	if m.devSel >= 0 && m.devSel < len(m.devices) {
		return m.devices[m.devSel], true
	}
	return proto.DeviceInfo{}, false
}

func (m Model) selectedToken() (proto.EnrollToken, bool) {
	if m.tokSel >= 0 && m.tokSel < len(m.tokens) {
		return m.tokens[m.tokSel], true
	}
	return proto.EnrollToken{}, false
}

// handleDevicesKey services the devices view. A pending confirmation swallows
// input until answered, and a freshly minted token is dismissed before anything
// else — it is on screen precisely because it cannot be recovered.
func (m Model) handleDevicesKey(s string) (tea.Model, tea.Cmd) {
	if m.devConfirm != "" {
		id, approve := m.devConfirm, m.devApproving
		kind := m.devConfirmKind
		m.devConfirm, m.devApproving, m.devConfirmKind = "", false, dpDevices
		if s != "y" && s != "Y" {
			return m, nil
		}
		if kind == dpTokens {
			return m, m.revokeTokenCmd(id)
		}
		return m, m.deviceActionCmd(id, approve)
	}
	if m.minted != "" && (s == "esc" || s == "enter" || s == "q") {
		m.minted, m.mintedTill = "", 0
		return m, nil
	}

	switch s {
	case "esc", "D", "q":
		m.view = viewBrowse
		m.zoom, m.zoomDetail = false, false
		m.applyLayout()
		return m, nil
	case "?":
		// This pane runs before the global keys, so ? has to be handled here too
		// — otherwise the one screen with destructive keys is the one screen that
		// cannot show you what they are.
		return m.openHelp()
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "tab", "shift+tab":
		m.dpane = devPane((int(m.dpane) + 1) % int(devPaneCount))
		m.applyLayout()
		return m, nil
	case "z":
		m.zoom = !m.zoom
		m.applyLayout()
		return m, nil
	case "j", "down":
		m.moveDevCursor(1)
		return m, nil
	case "k", "up":
		m.moveDevCursor(-1)
		return m, nil
	case "g":
		m.setDevCursor(0)
		return m, nil
	case "G":
		m.setDevCursor(m.devPaneLen() - 1)
		return m, nil
	case "c":
		// The columns pane aims at whichever of the two lists has focus, the same
		// way it aims at the explorer's lists (see colTarget).
		return m.toggleColumns()
	case "S":
		// The same key that syncs from the server everywhere else: here what the
		// server has to say IS these two lists, so S refetches them — and says so
		// while it runs and when it lands, the way the browse view's sync does. A
		// refetch that finished silently was indistinguishable from a key that did
		// nothing, which is exactly what it looks like when nothing has changed.
		if m.devRefreshing {
			return m, nil
		}
		m.devRefreshing = true
		m.flash = "refreshing…" // no expiry tick: replaced when the lists land
		m.flashID++
		return m, m.refreshDevicesCmd()
	case "n": // mint an enrollment token
		// One at a time, and one on screen at a time. Every press mints a REAL
		// token — a standing invitation into everything the group can read — so a
		// held key would leave a fistful of them open on the server; and each new
		// banner would bury the plaintext of the one before it, which exists
		// nowhere else and can never be shown again. Dismissing is the second act
		// that makes minting a second token deliberate.
		if m.minting {
			return m, nil
		}
		if m.minted != "" {
			return m.flashOnly("copy or dismiss this token first (y / esc)")
		}
		m.minting = true
		return m, m.mintCmd()
	case "y": // copy the token that was just minted
		// The only secret this view ever holds. Every other token is a hash on
		// the server and a short ID here, so there is nothing else worth copying
		// — and the banner is the one place the plaintext exists, where a narrow
		// pane may well have clipped it out of reach of the mouse.
		if m.minted == "" {
			return m, nil
		}
		return m.copyText(m.minted, "✓ token copied")
	case "a": // approve a pending device (with confirm)
		if m.dpane != dpDevices {
			return m, nil
		}
		if d, ok := m.selectedDevice(); ok && d.Status == "pending" {
			m.devConfirm, m.devApproving, m.devConfirmKind = d.ID, true, dpDevices
		}
		return m, nil
	case "x": // revoke the selected device or token (with confirm)
		if m.dpane == dpTokens {
			if t, ok := m.selectedToken(); ok && t.State == proto.TokenOpen {
				m.devConfirm, m.devApproving, m.devConfirmKind = t.ID, false, dpTokens
			}
			return m, nil
		}
		if d, ok := m.selectedDevice(); ok && d.Status != "revoked" && !d.Self {
			m.devConfirm, m.devApproving, m.devConfirmKind = d.ID, false, dpDevices
		}
		return m, nil
	}
	return m, nil
}

// devPaneLen is how many rows the focused pane holds.
func (m Model) devPaneLen() int {
	if m.dpane == dpTokens {
		return len(m.tokens)
	}
	return len(m.devices)
}

func (m *Model) moveDevCursor(d int) {
	if m.dpane == dpTokens {
		m.setDevCursor(m.tokSel + d)
		return
	}
	m.setDevCursor(m.devSel + d)
}

func (m *Model) setDevCursor(i int) {
	i = clampIndex(i, m.devPaneLen())
	if m.dpane == dpTokens {
		m.tokSel = i
		return
	}
	m.devSel = i
}

// --- rendering -----------------------------------------------------------

func (m Model) devicesTitle(w int) string {
	// The key hints live in the contextual footer (see helpKeys); the title is
	// just the view name.
	return clipW(m.th.Title.Render("DEVICES"), w)
}

// renderDevices draws the two panes stacked, or the zoomed one full-frame.
func (m Model) renderDevices(w, h int) string {
	if m.zoom {
		return m.devPaneBox(m.dpane, true, w-2, h-2)
	}
	g := m.geo
	return lipgloss.JoinVertical(lipgloss.Left,
		m.devPaneBox(dpDevices, m.dpane == dpDevices, g.p[dpDevices].w-2, g.p[dpDevices].h-2),
		m.devPaneBox(dpTokens, m.dpane == dpTokens, g.p[dpTokens].w-2, g.p[dpTokens].h-2),
	)
}

// devPaneBox renders one pane's title line plus its body inside a border.
func (m Model) devPaneBox(p devPane, focused bool, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	// The pane title carries the list's count and how it has been reshaped: with
	// no status bar of its own, this pane is the only place an unexpected order or
	// a switched-off column can say so.
	if p == dpTokens {
		return m.titledBox(focused, w, h, "TOKENS",
			m.listSuffix(ctTokens, m.devCountSuffix(len(m.tokens))), m.tokenListInner(w, h))
	}
	return m.titledBox(focused, w, h, "MACHINES",
		m.listSuffix(ctDevices, m.devCountSuffix(len(m.devices))), m.deviceListInner(w, h))
}

func (m Model) devCountSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// deviceListInner is the enrolled-machine table, padded to h lines: a column
// header over one row per machine, windowed on the cursor so a long list scrolls
// rather than running off the bottom of the pane.
func (m Model) deviceListInner(w, h int) string {
	tail := m.devPaneTail(dpDevices)
	body := maxInt(1, h-len(tail))

	var lines []string
	switch {
	case !m.gotDevices:
		lines = []string{m.th.Dim.Render("  loading…")}
	case m.devErr != nil:
		lines = []string{m.th.ExitErr.Render("  " + m.devErr.Error())}
	case len(m.devices) == 0:
		lines = []string{m.th.Dim.Render("  no devices enrolled — run `yore setup`")}
	default:
		l := m.tableLayout(ctDevices, w)
		lines = []string{composeSegs(m.tableHeaderSegs(l), false, w, m.th)}
		rows := maxInt(1, body-1)
		sel := clampIndex(m.devSel, len(m.devices))
		now := m.now()
		top := windowStart(sel, rows, len(m.devices))
		for i := top; i < len(m.devices) && i < top+rows; i++ {
			lines = append(lines, m.deviceRow(m.devices[i], l, i == sel && m.dpane == dpDevices, w, now))
		}
	}
	return stackPane(nil, lines, tail, body, w, h)
}

// tokenListInner is the enrollment-token table, on the same shape. A token that
// was just minted is shown above it, because that is the only moment its
// plaintext exists anywhere.
func (m Model) tokenListInner(w, h int) string {
	var head []string
	if m.minted != "" {
		head = []string{
			m.th.Match.Render("  new token: ") + m.th.Accent.Render(m.minted),
			m.th.Dim.Render("  valid until " + theme.AbsTime(m.mintedTill) +
				" — y copies it, it is never shown again (esc to dismiss)"),
			"",
		}
	}
	tail := m.devPaneTail(dpTokens)
	body := maxInt(1, h-len(head)-len(tail))

	var lines []string
	switch {
	case !m.gotTokens:
		lines = []string{m.th.Dim.Render("  loading…")}
	case len(m.tokens) == 0:
		lines = []string{m.th.Dim.Render("  no tokens — n mints one for another machine")}
	default:
		l := m.tableLayout(ctTokens, w)
		lines = []string{composeSegs(m.tableHeaderSegs(l), false, w, m.th)}
		rows := maxInt(1, body-1)
		sel := clampIndex(m.tokSel, len(m.tokens))
		now := m.now()
		top := windowStart(sel, rows, len(m.tokens))
		for i := top; i < len(m.tokens) && i < top+rows; i++ {
			lines = append(lines, m.tokenRow(m.tokens[i], l, i == sel && m.dpane == dpTokens, w, now))
		}
	}
	return stackPane(head, lines, tail, body, w, h)
}

// stackPane assembles one devices pane: the minted-token banner over the list
// over the pending question, with only the LIST clipped. padLines would truncate
// too, but from the bottom — which is the end holding the question the user is
// being asked.
func stackPane(head, list, tail []string, body, w, h int) string {
	out := make([]string, 0, len(head)+len(list)+len(tail))
	out = append(out, head...)
	if len(list) > body {
		list = list[:body]
	}
	out = append(out, list...)
	out = append(out, tail...)
	return padLines(out, w, h)
}

// devPaneTail is what stands under a pane's list — the pending confirmation, and
// only in the pane whose row it is asking about, which is not necessarily the
// focused one: a click can move focus while a question is armed. It is measured
// before the list is windowed so the question cannot be pushed off the bottom by
// the rows it is asking about.
func (m Model) devPaneTail(p devPane) []string {
	if m.devConfirmKind != p {
		return nil
	}
	return m.devConfirmLines()
}

// devConfirmLines is the question standing over the list while an action waits
// on y/n. Approving quotes the pending machine's verification code, because the
// only thing that makes an approval safe is the user comparing it to the code
// that machine is showing — a prompt that does not put the code in front of
// them is a prompt that trains them to press y.
func (m Model) devConfirmLines() []string {
	if m.devConfirm == "" {
		return nil
	}
	if m.devConfirmKind == dpTokens {
		return []string{"", m.th.ExitErr.Render("  revoke this token so it can admit no one? [y/N]")}
	}
	var d proto.DeviceInfo
	for _, dev := range m.devices {
		if dev.ID == m.devConfirm {
			d = dev
			break
		}
	}
	if !m.devApproving {
		return []string{"", m.th.ExitErr.Render("  revoke this device and rotate keys? [y/N]")}
	}
	return []string{
		"",
		m.th.Match.Render("  verification code: " + d.Code),
		m.th.ExitErr.Render("  does " + d.Name + " show this code? [y/N]"),
	}
}

// deviceRow renders one machine. The fixed cells come from the shared column
// machinery; the flexible one is the machine's name, in the identity color it
// carries everywhere else in the UI, followed by the marker for the machine you
// are sitting at.
func (m Model) deviceRow(d proto.DeviceInfo, l colLayout, selected bool, w int, now int64) string {
	th := m.th
	segs := rowSegs(th, ctDevices, deviceSpecs, l, d, now)
	if nw := l.w[mcName]; nw > 0 {
		marker := ""
		if d.Self {
			marker = "  (this machine)"
		}
		name := truncCols(d.Name, maxInt(1, nw-runewidth.StringWidth(marker)))
		segs = append(segs, styledSeg{text: name, style: th.Host(d.Name)})
		if marker != "" {
			segs = append(segs, styledSeg{text: marker, style: th.Dim})
		}
		// Pad the cell out by hand: the marker sits against the name, not against
		// the far edge of the pane.
		if pad := nw - runewidth.StringWidth(name) - runewidth.StringWidth(marker); pad > 0 {
			segs = append(segs, styledSeg{text: strings.Repeat(" ", pad), raw: true})
		}
	}
	return composeSegs(segs, selected, w, th)
}

// tokenRow renders one token: its state, when it was minted, what became of it,
// and its id. The token itself is not here and cannot be — only its hash was
// kept, which is what the id column shows.
func (m Model) tokenRow(t proto.EnrollToken, l colLayout, selected bool, w int, now int64) string {
	th := m.th
	segs := rowSegs(th, ctTokens, tokenSpecs, l, t, now)
	if iw := l.w[tcID]; iw > 0 {
		segs = append(segs, styledSeg{
			text:  padRight(truncCols(t.ID, iw), iw),
			style: th.Norm,
		})
	}
	return composeSegs(segs, selected, w, th)
}

// shortID trims a ULID or a token hash to a readable prefix for display.
func shortID(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}
