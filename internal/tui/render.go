package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yoho-build/yoho/internal/dash"
)

const (
	bold    = "1"
	dim     = "2"
	red     = "31"
	green   = "32"
	yellow  = "33"
	cyan    = "36"
	reverse = "7"
)

type seg struct{ text, style string }

// line is a row of styled segments, cut to the screen width.
type line []seg

func (m *Model) paint(l line, width int) string {
	var b strings.Builder
	left := width
	for _, s := range l {
		if left <= 0 {
			break
		}
		t := cut(s.text, left)
		left -= utf8.RuneCountInString(t)
		if m.Color && s.style != "" {
			b.WriteString("\x1b[" + s.style + "m" + t + "\x1b[0m")
		} else {
			b.WriteString(t)
		}
	}
	return b.String()
}

// cut truncates s to n runes and strips control characters, so container
// output cannot move the cursor or change terminal state.
func cut(s string, n int) string {
	var b strings.Builder
	i := 0
	for _, r := range s {
		if r == '\t' {
			r = ' '
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		if i >= n {
			break
		}
		b.WriteRune(r)
		i++
	}
	return b.String()
}

func pad(s string, n int) string {
	s = cut(s, n)
	if c := utf8.RuneCountInString(s); c < n {
		s += strings.Repeat(" ", n-c)
	}
	return s
}

func bar(pct float64, width int) (string, string) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	full := int(pct / 100 * float64(width))
	style := green
	switch {
	case pct >= 90:
		style = red
	case pct >= 75:
		style = yellow
	}
	return strings.Repeat("█", full) + strings.Repeat("░", width-full), style
}

// Render draws the whole screen: exactly height lines, each cleared to the end.
func Render(m *Model, width, height int) string {
	if width < 20 || height < 4 {
		return "\x1b[2J\x1b[H" + cut("terminal too small", max(0, width))
	}
	var ls []line
	if m.Logs != nil {
		ls = m.logLines(height)
	} else {
		ls = m.mainLines(width, height)
	}
	var b strings.Builder
	for i := 0; i < height; i++ {
		if i < len(ls) {
			b.WriteString(m.paint(ls[i], width))
		}
		b.WriteString("\x1b[K")
		if i < height-1 {
			b.WriteString("\r\n")
		}
	}
	return b.String()
}

func (m *Model) header() line {
	l := line{{" Yoho ", reverse + ";" + bold}}
	if m.Snap != nil {
		l = append(l, seg{" " + m.Snap.App, bold}, seg{" · " + m.Snap.Destination, ""}, seg{"  " + m.Snap.Runtime, dim})
		l = append(l, seg{"  updated " + ago(m.Snap.CollectedAt, m.Now), dim})
	}
	if m.Loading {
		l = append(l, seg{"  refreshing…", yellow})
	}
	return l
}

func (m *Model) mainLines(width, height int) []line {
	ls := []line{m.header()}
	if m.Err != nil {
		ls = append(ls, line{{"error: " + m.Err.Error(), red}})
	}
	s := m.server()
	if s == nil {
		ls = append(ls, line{}, line{{"  Connecting to Servers…", dim}})
		return m.withFooter(ls, height)
	}
	// Server tabs.
	tabs := line{{" ", ""}}
	for i, sv := range m.Snap.Servers {
		label := " " + sv.Name + " "
		if sv.Error != "" {
			label = " " + sv.Name + " ! "
		}
		st := dim
		if i == m.Server {
			st = reverse
		}
		tabs = append(tabs, seg{label, st}, seg{" ", ""})
	}
	ls = append(ls, tabs, line{})
	if s.Error != "" {
		ls = append(ls, line{{"  " + s.Error, red}})
	}
	h := s.Host
	hostDesc := strings.Join(nonEmpty(h.OS, h.Kernel, h.Arch), " · ")
	ls = append(ls, line{{"  " + s.Name, bold}, {"  " + s.SSH, dim}, {"  " + hostDesc, ""}})
	info := []string{"up " + uptime(h.UptimeSeconds)}
	if h.DockerVersion != "" {
		info = append(info, "docker "+h.DockerVersion)
	}
	ls = append(ls, line{{"  " + strings.Join(info, " · "), dim}})
	if h.DockerError != "" {
		ls = append(ls, line{{"  docker: " + h.DockerError, red}})
	}
	bw := min(30, max(10, width-50))
	meter := func(label string, pct float64, detail string) line {
		b, st := bar(pct, bw)
		return line{{"  " + pad(label, 18), ""}, {b, st}, {fmt.Sprintf(" %3.0f%%  ", pct), ""}, {detail, dim}}
	}
	if h.CPUs > 0 {
		ls = append(ls, meter(fmt.Sprintf("Load %dcpu", h.CPUs), h.Load[0]/float64(h.CPUs)*100,
			fmt.Sprintf("%.2f %.2f %.2f", h.Load[0], h.Load[1], h.Load[2])))
	}
	if h.MemTotal > 0 {
		used := h.MemTotal - h.MemAvailable
		ls = append(ls, meter("Memory", float64(used)/float64(h.MemTotal)*100, bytes(used)+" / "+bytes(h.MemTotal)))
	}
	for _, d := range h.Disks {
		if d.Total == 0 {
			continue
		}
		ls = append(ls, meter("Disk "+d.Mount, float64(d.Used)/float64(d.Total)*100, bytes(d.Avail)+" free of "+bytes(d.Total)))
	}
	ls = append(ls, line{})

	// Containers.
	rows := m.rows()
	cols := []int{14, 34, 11, 7, 22}
	head := line{{"  " + pad("SERVICE", cols[0]) + " " + pad("CONTAINER", cols[1]) + " " + pad("STATE", cols[2]) + " " + pad("CPU", cols[3]) + " " + pad("MEMORY", cols[4]) + " IMAGE", bold}}
	ls = append(ls, head)
	if len(rows) == 0 {
		ls = append(ls, line{{"  no containers for this App on " + s.Name + " — deploy with `yoho deploy`", dim}})
	}
	for i, c := range rows {
		state, st := stateText(c)
		cpu := "—"
		if c.State == "running" {
			cpu = fmt.Sprintf("%.1f%%", c.CPUPercent)
		}
		mem := c.MemUsage
		if mem == "" {
			mem = "—"
		}
		svc := c.Service
		if svc == "" {
			svc = "—"
		}
		marker, rowStyle := "  ", ""
		if i == m.Sel {
			marker, rowStyle = "▸ ", bold
		}
		ls = append(ls, line{
			{marker + pad(svc, cols[0]) + " " + pad(c.Name, cols[1]) + " ", rowStyle},
			{pad(state, cols[2]), st},
			{" " + pad(cpu, cols[3]) + " " + pad(mem, cols[4]) + " ", rowStyle},
			{c.Image, dim},
		})
	}
	ls = append(ls, line{})

	// Releases, as many as fit above the footer.
	ls = append(ls, line{{"  RELEASES", bold}})
	if len(s.Releases) == 0 {
		ls = append(ls, line{{"  none recorded", dim}})
	}
	room := height - len(ls) - 2
	for i, r := range s.Releases {
		if i >= max(1, room) {
			ls = append(ls, line{{fmt.Sprintf("  … %d more (yoho releases)", len(s.Releases)-i), dim}})
			break
		}
		cur := "  "
		if r.Current {
			cur = "* "
		}
		st := dim
		switch r.Status {
		case "deployed":
			st = green
		case "failed":
			st = red
		}
		ls = append(ls, line{{"  " + cur + pad(shortVersion(r.Version), 14) + " ", bold}, {pad(r.Status, 10), st}, {" " + pad(ago(r.DeployedAt, m.Now), 10) + " by " + r.Performer, dim}})
	}
	return m.withFooter(ls, height)
}

func (m *Model) withFooter(ls []line, height int) []line {
	help := line{{" q", bold}, {" quit  ", dim}, {"r", bold}, {" refresh  ", dim}, {"tab", bold}, {" server  ", dim}, {"↑↓", bold}, {" select  ", dim}, {"enter", bold}, {" logs", dim}, {fmt.Sprintf("   read-only · every %s", m.Interval), dim}}
	if len(ls) > height-1 {
		ls = ls[:max(0, height-1)]
	}
	for len(ls) < height-1 {
		ls = append(ls, line{})
	}
	return append(ls, help)
}

func (m *Model) logLines(height int) []line {
	lv := m.Logs
	title := line{{" Yoho ", reverse + ";" + bold}, {" logs ", bold}, {lv.Service + "  " + lv.Container + " on " + lv.Server, dim}}
	if lv.Loading {
		title = append(title, seg{"  loading…", yellow})
	}
	ls := []line{title, line{}}
	if lv.Err != nil {
		ls = append(ls, line{{"error: " + lv.Err.Error(), red}})
	}
	text := strings.Split(strings.TrimRight(lv.Text, "\n"), "\n")
	room := max(1, height-len(ls)-1)
	if lv.Offset > max(0, len(text)-room) {
		lv.Offset = max(0, len(text)-room)
	}
	end := len(text) - lv.Offset
	start := max(0, end-room)
	for _, t := range text[start:end] {
		ls = append(ls, line{{t, ""}})
	}
	for len(ls) < height-1 {
		ls = append(ls, line{})
	}
	pos := "bottom"
	if lv.Offset > 0 {
		pos = fmt.Sprintf("%d lines up", lv.Offset)
	}
	return append(ls[:height-1], line{{" esc", bold}, {" back  ", dim}, {"↑↓ pgup pgdn", bold}, {" scroll  ", dim}, {"g/G", bold}, {" top/bottom  ", dim}, {"r", bold}, {" reload", dim}, {"   " + pos, dim}})
}

func stateText(c dash.Container) (string, string) {
	switch {
	case c.State != "running":
		s := c.State
		if s == "" {
			s = "unknown"
		}
		return s, red
	case c.Health == "unhealthy":
		return "unhealthy", red
	case c.Health == "starting":
		return "starting", yellow
	case c.Health == "healthy":
		return "healthy", green
	}
	return "running", green
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func bytes(n uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 || f >= 100 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func uptime(sec float64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d <= 0:
		return "?"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours())/24)
}

func shortVersion(v string) string {
	if len(v) == 40 && strings.Trim(v, "0123456789abcdef") == "" {
		return v[:12]
	}
	return v
}
