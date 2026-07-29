package browse

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
		m.zoom = false
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
	case "r":
		return m, m.refreshDevicesCmd()
	case "n": // mint an enrollment token
		return m, m.mintCmd()
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
	if p == dpTokens {
		return m.titledBox(focused, w, h, "TOKENS", m.devCountSuffix(len(m.tokens)), m.tokenListInner(w, h))
	}
	return m.titledBox(focused, w, h, "MACHINES", m.devCountSuffix(len(m.devices)), m.deviceListInner(w, h))
}

func (m Model) devCountSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// deviceListInner is the enrolled-machine list, padded to h lines.
func (m Model) deviceListInner(w, h int) string {
	var lines []string
	switch {
	case !m.gotDevices:
		lines = append(lines, m.th.Dim.Render("  loading…"))
	case m.devErr != nil:
		lines = append(lines, m.th.ExitErr.Render("  "+m.devErr.Error()))
	case len(m.devices) == 0:
		lines = append(lines, m.th.Dim.Render("  no devices enrolled — run `yore setup`"))
	default:
		for i, d := range m.devices {
			lines = append(lines, m.deviceRow(i, d, w))
		}
	}
	if m.dpane == dpDevices {
		lines = append(lines, m.devConfirmLines()...)
	}
	return padLines(lines, w, h)
}

// tokenListInner is the enrollment-token list, padded to h lines. A token that
// was just minted is shown above it, because that is the only moment its
// plaintext exists anywhere.
func (m Model) tokenListInner(w, h int) string {
	var lines []string
	if m.minted != "" {
		lines = append(lines,
			m.th.Match.Render("  new token: ")+m.th.Accent.Render(m.minted),
			m.th.Dim.Render("  valid until "+theme.AbsTime(m.mintedTill)+
				" — copy it now, it is never shown again (esc to dismiss)"),
			"",
		)
	}
	switch {
	case !m.gotTokens:
		lines = append(lines, m.th.Dim.Render("  loading…"))
	case len(m.tokens) == 0:
		lines = append(lines, m.th.Dim.Render("  no tokens — n mints one for another machine"))
	default:
		for i, t := range m.tokens {
			lines = append(lines, m.tokenRow(i, t, w))
		}
	}
	if m.dpane == dpTokens {
		lines = append(lines, m.devConfirmLines()...)
	}
	return padLines(lines, w, h)
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

func (m Model) deviceRow(i int, d proto.DeviceInfo, w int) string {
	cursor := "  "
	if i == m.devSel && m.dpane == dpDevices {
		cursor = m.th.Accent.Render("▸ ")
	}

	statusStyle := m.th.Dim
	switch d.Status {
	case "active":
		statusStyle = m.th.ExitOK
	case "pending":
		statusStyle = m.th.Match
	}
	status := statusStyle.Render(padRight(d.Status, 8))

	name := padRight(truncCols(d.Name, 18), 18)
	tail := ""
	if d.Self {
		tail = m.th.Dim.Render("  (this machine)")
	} else if d.Code != "" {
		tail = m.th.Dim.Render("  code " + d.Code)
	}

	line := cursor + status + " " + m.th.Norm.Render(name) + " " + m.th.Dim.Render(shortID(d.ID)) + tail
	return clipW(line, w)
}

// tokenRow renders one token: its state, when it was minted, and what became of
// it. The token itself is not here and cannot be — only its hash was kept.
func (m Model) tokenRow(i int, t proto.EnrollToken, w int) string {
	cursor := "  "
	if i == m.tokSel && m.dpane == dpTokens {
		cursor = m.th.Accent.Render("▸ ")
	}

	style := m.th.Dim
	switch t.State {
	case proto.TokenOpen:
		style = m.th.Match // still live: the one state that is a standing invitation
	case proto.TokenClaimed:
		style = m.th.ExitOK
	case proto.TokenRevoked:
		style = m.th.ExitErr
	}
	state := style.Render(padRight(t.State, 8))

	// What became of it, in one phrase. An open token says how long it has left,
	// because that is the only thing anyone wants to know about it.
	var tail string
	switch t.State {
	case proto.TokenOpen:
		tail = "expires " + theme.RelTime(m.now(), t.ExpiresMs)
	case proto.TokenClaimed:
		tail = "claimed by " + shortID(t.ClaimedBy) + " " + theme.RelTime(m.now(), t.ClaimedMs)
	case proto.TokenRevoked:
		tail = "revoked " + theme.RelTime(m.now(), t.RevokedMs)
	default:
		tail = "expired " + theme.RelTime(m.now(), t.ExpiresMs)
	}

	line := cursor + state + " " + m.th.Norm.Render(padRight(shortID(t.ID), 12)) +
		" " + m.th.Dim.Render("minted "+theme.RelTime(m.now(), t.CreatedMs)) +
		m.th.Dim.Render("  "+tail)
	return clipW(line, w)
}

// shortID trims a ULID or a token hash to a readable prefix for display.
func shortID(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}
