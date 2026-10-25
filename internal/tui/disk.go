package tui

import (
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/dash"
)

// DiskView is the disk usage screen: the App directory and volume sizes of
// the selected Server. Sizing is slow, so it loads on open and on r only.
type DiskView struct {
	Usage   *dash.DiskUsage
	Err     error
	Loading bool
	Sel     int             // selected volume
	Open    map[string]bool // volumes showing their top subdirectories
}

// diskServer is the DiskUsage entry of the selected Server tab.
func (m *Model) diskServer() *dash.ServerDisk {
	s := m.server()
	if m.Disk == nil || m.Disk.Usage == nil || s == nil {
		return nil
	}
	for i := range m.Disk.Usage.Servers {
		if m.Disk.Usage.Servers[i].Name == s.Name {
			return &m.Disk.Usage.Servers[i]
		}
	}
	return nil
}

func (m *Model) updateDisk(k Key) Action {
	dv := m.Disk
	n := 0
	if m.Snap != nil {
		n = len(m.Snap.Servers)
	}
	vols := 0
	if d := m.diskServer(); d != nil {
		vols = len(d.Volumes)
	}
	switch k {
	case KeyEsc, "q", "v":
		m.Disk = nil
		return ActNone
	case "r":
		return ActDisk
	case KeyTab, KeyRight, "l":
		if n > 0 {
			m.Server = (m.Server + 1) % n
			dv.Sel = 0
		}
	case KeyBackTab, KeyLeft, "h":
		if n > 0 {
			m.Server = (m.Server + n - 1) % n
			dv.Sel = 0
		}
	case KeyUp, "k":
		dv.Sel--
	case KeyDown, "j":
		dv.Sel++
	case KeyEnter, " ":
		if d := m.diskServer(); d != nil && dv.Sel >= 0 && dv.Sel < vols && d.Volumes[dv.Sel].Sized {
			name := d.Volumes[dv.Sel].Name
			dv.Open[name] = !dv.Open[name]
		}
	}
	dv.Sel = max(0, min(dv.Sel, vols-1))
	return ActNone
}

// dockerDisk is the filesystem holding Docker's root, which holds local
// volumes; the Yoho root's when Docker shares it.
func dockerDisk(s *dash.Server) *dash.Disk {
	if s == nil {
		return nil
	}
	var first *dash.Disk
	for i := range s.Host.Disks {
		d := &s.Host.Disks[i]
		if d.Label == "docker" {
			return d
		}
		if first == nil {
			first = d
		}
	}
	return first
}

func (m *Model) diskLines(width, height int) []line {
	dv := m.Disk
	title := line{{" Yoho ", reverse + ";" + bold}, {" disk ", bold}}
	if m.Snap != nil {
		title = append(title, seg{m.Snap.App + " · " + m.Snap.Destination, ""})
	}
	if dv.Usage != nil {
		title = append(title, seg{"  sized " + ago(dv.Usage.CollectedAt, m.Now), dim})
	}
	if dv.Loading {
		title = append(title, seg{"  sizing… (du walks every file)", yellow})
	}
	ls := []line{title}
	s := m.server()
	if s != nil && len(m.Snap.Servers) > 1 {
		tabs := line{{" ", ""}}
		for i, sv := range m.Snap.Servers {
			st := dim
			if i == m.Server {
				st = reverse
			}
			tabs = append(tabs, seg{" " + sv.Name + " ", st}, seg{" ", ""})
		}
		ls = append(ls, tabs)
	}
	ls = append(ls, line{})
	if dv.Err != nil {
		ls = append(ls, line{{"  error: " + dv.Err.Error(), red}})
	}
	d := m.diskServer()
	if d == nil {
		if dv.Loading {
			ls = append(ls, line{{"  Sizing the App directory and volumes…", dim}})
		}
		return m.diskFooter(ls, height)
	}
	if d.Error != "" {
		ls = append(ls, line{{"  " + d.Error, red}})
	}
	access := d.Access
	switch {
	case access == "sudo" && d.User != "":
		access = d.User + " via sudo"
	case access != "root" && access != "scoped":
		access = "volumes not sized"
		if d.User != "" {
			access += " as " + d.User
		}
	}
	ls = append(ls, line{{"  " + d.Name, bold}, {"  access: " + access, dim}, {fmt.Sprintf("  %.1fs", d.Elapsed), dim}})
	appSize := "—"
	if d.AppDirSized {
		appSize = bytes(d.AppDirBytes)
	}
	ls = append(ls, line{{"  " + pad("App dir", 10), ""}, {pad(appSize, 10), bold}, {" " + d.AppDir, dim}})
	disk := dockerDisk(s)
	bw := min(20, max(8, width-70))
	if disk != nil && disk.Total > 0 {
		b, st := bar(float64(disk.Used)/float64(disk.Total)*100, bw)
		ls = append(ls, line{{"  " + pad("Disk "+disk.Mount, 20), ""}, {b, st}, {fmt.Sprintf(" %3.0f%%  ", float64(disk.Used)/float64(disk.Total)*100), ""}, {bytes(disk.Avail) + " free of " + bytes(disk.Total), dim}})
	}
	if d.Note != "" {
		ls = append(ls, line{})
		for _, l := range wrap(d.Note, max(20, width-4)) {
			ls = append(ls, line{{"  " + l, yellow}})
		}
	}
	ls = append(ls, line{})
	nameW := max(16, min(44, width-52))
	ls = append(ls, line{{"  " + pad("VOLUME", nameW) + " " + pad("SERVICES", 14) + " " + pad("SIZE", 10) + " OF DISK", bold}})
	if len(d.Volumes) == 0 {
		ls = append(ls, line{{"  no volumes for this App on " + d.Name, dim}})
	}
	for i, v := range d.Volumes {
		marker, style := "  ", ""
		if i == dv.Sel {
			marker, style = "▸ ", bold
		}
		size, sizeStyle := "—", dim
		if v.Sized {
			size, sizeStyle = bytes(v.Bytes), style
			if v.Partial {
				size = "≥" + size
				sizeStyle = yellow
			}
		}
		svcs := strings.Join(v.Services, ",")
		if svcs == "" {
			svcs = "—"
		}
		row := line{{marker + pad(v.Name, nameW) + " " + pad(svcs, 14) + " ", style}, {pad(size, 10), sizeStyle}}
		if v.Sized && disk != nil && disk.Total > 0 {
			pct := float64(v.Bytes) / float64(disk.Total) * 100
			b, st := bar(pct, bw)
			f := " %3.0f%%"
			if pct < 1 {
				f = " %3.1f%%"
			}
			row = append(row, seg{" " + b, st}, seg{fmt.Sprintf(f, pct), ""})
		}
		ls = append(ls, row)
		if v.Error != "" {
			ls = append(ls, line{{"      " + v.Error, yellow}})
		}
		if v.Sized && dv.Open[v.Name] {
			if len(v.Top) == 0 {
				ls = append(ls, line{{"      empty", dim}})
			}
			for _, e := range v.Top {
				ls = append(ls, line{{"      " + pad(e.Name, nameW-4) + " " + pad("", 14) + " ", dim}, {bytes(e.Bytes), ""}})
			}
		}
	}
	return m.diskFooter(ls, height)
}

func (m *Model) diskFooter(ls []line, height int) []line {
	help := line{{" esc", bold}, {" back  ", dim}, {"enter", bold}, {" largest entries  ", dim}, {"r", bold}, {" rescan  ", dim}, {"tab", bold}, {" server  ", dim}, {"↑↓", bold}, {" select", dim}, {"   read-only · sized on demand", dim}}
	if len(ls) > height-1 {
		ls = ls[:max(0, height-1)]
	}
	for len(ls) < height-1 {
		ls = append(ls, line{})
	}
	return append(ls, help)
}

// wrap breaks s into lines of at most n columns at spaces.
func wrap(s string, n int) []string {
	var out []string
	cur := ""
	for _, w := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = w
		case colWidth(cur)+1+colWidth(w) <= n:
			cur += " " + w
		default:
			out = append(out, cur)
			cur = w
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
