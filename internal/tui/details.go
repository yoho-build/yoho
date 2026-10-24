package tui

import (
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/dash"
)

func (m *Model) detailNamedContainer(s *dash.Server) *dash.Container {
	if m == nil || m.Details == nil || s == nil {
		return nil
	}
	if c := s.Proxy; c != nil && c.Name == m.Details.Container {
		return c
	}
	for i := range s.Containers {
		if s.Containers[i].Name == m.Details.Container {
			return &s.Containers[i]
		}
	}
	return nil
}

// detailLines renders the pinned container view. Details.Offset counts lines
// from the top of the body; the title and footer stay fixed while scrolling.
func (m *Model) detailLines(width, height int) []line {
	width = max(1, width)
	height = max(2, height)
	title := append(m.header(), seg{"  Details", bold})
	body := []line{line{}}
	if m.Details == nil {
		body = append(body, detailField("Target", "No container selected", width, dim)...)
	} else {
		s, c := m.detailContainer()
		service, image, state, health, status, uptimeText, ports := "—", "—", "—", "—", "—", "—", "—"
		stateStyle := ""
		if c != nil {
			service, image = detailOrDash(c.Service), detailOrDash(c.Image)
			state = c.State
			if state == "" {
				state = "unknown"
			}
			_, stateStyle = stateText(*c)
			health = detailOrDash(c.Health)
			status = detailOrDash(c.Status)
			uptimeText = detailUptime(*c)
			ports = detailOrDash(c.Ports)
		}
		body = append(body, detailField("Service", service, width, "")...)
		body = append(body, detailField("Server", m.Details.Server, width, "")...)
		body = append(body, detailField("Container", m.Details.Container, width, "")...)
		body = append(body, detailField("Container ID", m.Details.ID, width, "")...)
		body = append(body, detailField("Image", image, width, "")...)
		body = append(body, detailField("State", state, width, stateStyle)...)
		body = append(body, detailField("Health", health, width, "")...)
		body = append(body, detailField("Status", status, width, "")...)
		body = append(body, detailField("Uptime", uptimeText, width, "")...)
		body = append(body, detailField("Ports", ports, width, "")...)

		if m.Err != nil {
			body = append(body, detailField("Refresh error", m.Err.Error(), width, red)...)
		}
		if s == nil {
			body = append(body, detailField("Target", "Server is absent from the latest snapshot", width, red)...)
		} else {
			if s.Error != "" {
				body = append(body, detailField("Server error", s.Error, width, red)...)
			}
			if s.Host.DockerError != "" {
				body = append(body, detailField("Docker error", s.Host.DockerError, width, red)...)
			}
			if c == nil {
				if current := m.detailNamedContainer(s); current != nil && m.Details.ID != "" && current.ID != m.Details.ID {
					body = append(body, detailField("Target", "Container was replaced in the latest snapshot", width, yellow)...)
					body = append(body, detailField("Current ID", current.ID, width, yellow)...)
				} else if m.Details.ID == "" {
					body = append(body, detailField("Target", "Container ID is missing; target cannot be verified", width, red)...)
				} else {
					body = append(body, detailField("Target", "Container is absent from the latest snapshot", width, red)...)
				}
			}
		}

		statsReady := c != nil && c.State == "running" && c.MemUsage != "" && m.Err == nil && s.Error == "" && s.Host.DockerError == ""
		cpu, memory, memoryPercent := "—", "—", "—"
		network, blockIO, pids := "—", "—", "—"
		if statsReady {
			cpu = fmt.Sprintf("%.1f%%", c.CPUPercent)
			memory = c.MemUsage
			memoryPercent = fmt.Sprintf("%.1f%%", c.MemPercent)
			network = detailCumulative(c.NetIO)
			blockIO = detailCumulative(c.BlockIO)
			pids = detailOrDash(c.PIDs)
		}
		body = append(body, line{})
		body = append(body, detailField("CPU", cpu, width, "")...)
		body = append(body, detailField("Memory usage / limit", memory, width, "")...)
		body = append(body, detailField("Memory percent", memoryPercent, width, "")...)
		body = append(body, detailField("Network I/O RX / TX", network, width, "")...)
		body = append(body, detailField("Block I/O read / write", blockIO, width, "")...)
		body = append(body, detailField("PIDs", pids, width, "")...)
	}

	footer := detailFooter(width, height)
	room := max(0, height-1-len(footer))
	maxOffset := max(0, len(body)-room)
	if m.Details != nil {
		m.Details.Offset = min(max(0, m.Details.Offset), maxOffset)
	}
	offset := 0
	if m.Details != nil {
		offset = m.Details.Offset
	}
	end := min(len(body), offset+room)
	ls := []line{title}
	ls = append(ls, body[offset:end]...)
	for len(ls) < height-len(footer) {
		ls = append(ls, line{})
	}
	return append(ls, footer...)
}

func detailFooter(width, height int) []line {
	var labels []string
	switch {
	case width >= 78:
		labels = []string{"esc/q back · enter logs · r refresh · ↑↓/PgUp/PgDn scroll · g/G top/bottom"}
	case width >= 38:
		labels = []string{"esc/q back · enter logs · r refresh", "↑↓/PgUp/PgDn scroll · g/G top/bottom"}
	default:
		labels = []string{"esc/q back", "enter logs", "r refresh", "↑↓ scroll", "PgUp/PgDn scroll", "g/G top/bottom"}
	}
	maxLines := max(1, height-2)
	if len(labels) > maxLines {
		labels = []string{"esc/q back · enter logs · r refresh · ↑↓/PgUp/PgDn · g/G"}
	}
	footer := make([]line, 0, len(labels))
	for _, label := range labels {
		footer = append(footer, line{{label, dim}})
	}
	return footer
}

func detailField(label, value string, width int, valueStyle string) []line {
	if value == "" {
		value = "—"
	}
	prefix := "  " + label + ": "
	valueWidth := width - colWidth(prefix)
	if valueWidth >= max(8, width/3) {
		chunks := detailWrap(value, valueWidth)
		out := []line{{{prefix, dim}, {chunks[0], valueStyle}}}
		continuation := strings.Repeat(" ", colWidth(prefix))
		for _, chunk := range chunks[1:] {
			out = append(out, line{{continuation, ""}, {chunk, valueStyle}})
		}
		return out
	}
	labelWidth := max(1, width-2)
	valueWidth = max(1, width-4)
	var out []line
	for _, chunk := range detailWrap(label+":", labelWidth) {
		out = append(out, line{{"  " + chunk, dim}})
	}
	for _, chunk := range detailWrap(value, valueWidth) {
		out = append(out, line{{"    " + chunk, valueStyle}})
	}
	return out
}

// detailWrap filters terminal controls and wraps text using the same width
// approximation as paint, so container values cannot inject escape sequences.
func detailWrap(value string, width int) []string {
	width = max(1, width)
	var out []string
	var b strings.Builder
	used := 0
	flush := func() {
		out = append(out, b.String())
		b.Reset()
		used = 0
	}
	for _, r := range value {
		if r == '\t' {
			r = ' '
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		w := runeWidth(r)
		if w == 0 {
			b.WriteRune(r)
			continue
		}
		if w > width {
			r, w = '?', 1
		}
		if used+w > width && used > 0 {
			flush()
		}
		b.WriteRune(r)
		used += w
	}
	if b.Len() > 0 || len(out) == 0 {
		flush()
	}
	return out
}

func detailOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func detailCumulative(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s + " (cumulative)"
}

func detailUptime(c dash.Container) string {
	if c.State != "running" {
		return "—"
	}
	status := strings.TrimSpace(c.Status)
	if !strings.HasPrefix(status, "Up ") {
		return "—"
	}
	value := strings.TrimPrefix(status, "Up ")
	if i := strings.Index(value, " ("); i >= 0 {
		value = value[:i]
	}
	return detailOrDash(value)
}
