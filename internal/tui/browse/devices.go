package browse

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"yore/internal/proto"
)

// devicesResultMsg carries a device list (a fetch, or the result of an
// approve/revoke) back to the model.
type devicesResultMsg struct {
	info proto.DevicesInfo
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

func (m Model) selectedDevice() (proto.DeviceInfo, bool) {
	if m.devSel >= 0 && m.devSel < len(m.devices) {
		return m.devices[m.devSel], true
	}
	return proto.DeviceInfo{}, false
}

// handleDevicesKey services the devices pane. A pending revoke confirmation
// swallows input until answered.
func (m Model) handleDevicesKey(s string) (tea.Model, tea.Cmd) {
	if m.devConfirm != "" {
		if s == "y" || s == "Y" {
			id := m.devConfirm
			m.devConfirm = ""
			return m, m.deviceActionCmd(id, false)
		}
		m.devConfirm = ""
		return m, nil
	}

	switch s {
	case "esc", "D", "q":
		m.view = viewBrowse
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "j", "down":
		if m.devSel < len(m.devices)-1 {
			m.devSel++
		}
		return m, nil
	case "k", "up":
		if m.devSel > 0 {
			m.devSel--
		}
		return m, nil
	case "g":
		m.devSel = 0
		return m, nil
	case "G":
		if len(m.devices) > 0 {
			m.devSel = len(m.devices) - 1
		}
		return m, nil
	case "r":
		return m, m.devicesCmd()
	case "a": // approve a pending device
		if d, ok := m.selectedDevice(); ok && d.Status == "pending" {
			return m, m.deviceActionCmd(d.ID, true)
		}
		return m, nil
	case "x": // revoke (with confirm)
		if d, ok := m.selectedDevice(); ok && d.Status != "revoked" && !d.Self {
			m.devConfirm = d.ID
		}
		return m, nil
	}
	return m, nil
}

func (m Model) devicesTitle(w int) string {
	t := m.th.Title.Render("Devices")
	hint := m.th.Dim.Render("  a approve · x revoke · r refresh · j/k move · esc back")
	return clipW(t+hint, w)
}

// renderDevices draws the enrolled-device list, padded to h lines.
func (m Model) renderDevices(w, h int) string {
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

	if m.devConfirm != "" {
		lines = append(lines, "", m.th.ExitErr.Render("  revoke this device and rotate keys? [y/N]"))
	}

	// Pad / clip to h lines.
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

func (m Model) deviceRow(i int, d proto.DeviceInfo, w int) string {
	cursor := "  "
	if i == m.devSel {
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

// shortID trims a ULID to a readable prefix for display.
func shortID(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}
