package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
)

func snap() *dash.Snapshot {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return &dash.Snapshot{
		App: "shop", Destination: "production", Runtime: "compose", CollectedAt: now,
		Servers: []dash.Server{
			{
				Name: "web-1", SSH: "root@10.0.0.1",
				Host: dash.Host{Hostname: "web-1", OS: "Debian 13", Kernel: "6.12", Arch: "x86_64", CPUs: 4, Load: [3]float64{1, 0.5, 0.2},
					MemTotal: 8 << 30, MemAvailable: 2 << 30, UptimeSeconds: 90000, DockerVersion: "29.1",
					Disks: []dash.Disk{{Label: "yoho", Mount: "/", Total: 100 << 30, Used: 95 << 30, Avail: 5 << 30}}},
				Proxy: &dash.Container{Name: "yoho-proxy", Service: "proxy", State: "running", Image: "basecamp/kamal-proxy"},
				Containers: []dash.Container{
					{Name: "yoho-shop-production-web-1", Service: "web", State: "running", Health: "healthy", CPUPercent: 12.5, MemUsage: "100MiB / 8GiB", Image: "shop-web:abc"},
					{Name: "yoho-shop-production-db-1", Service: "db", State: "exited", Image: "postgres:17\x1b[2J"},
				},
				Releases: []dash.Release{{Version: strings.Repeat("a", 40), Status: "deployed", Current: true, DeployedAt: now.Add(-2 * time.Hour), Performer: "alice"}},
			},
			{Name: "web-2", SSH: "root@10.0.0.2", Error: "ssh: connection refused", Containers: []dash.Container{}},
		},
	}
}

func TestRenderMain(t *testing.T) {
	m := &Model{Snap: snap(), Now: snap().CollectedAt.Add(3 * time.Second), Interval: 5 * time.Second}
	out := Render(m, 120, 30)
	for _, want := range []string{"shop", "production", "web-1", "web-2 !", "Debian 13", "docker 29.1", "Load 4cpu", "Memory", "Disk /", "▸ proxy", "healthy", "exited", "12.5%", "aaaaaaaaaaaa", "deployed", "by alice", "q quit"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[2J") {
		t.Error("container data must not inject escape sequences")
	}
	if got := strings.Count(out, "\r\n"); got != 29 {
		t.Errorf("want exactly 30 lines, got %d", got+1)
	}
	for i, l := range strings.Split(out, "\r\n") {
		if n := len([]rune(strings.TrimSuffix(l, "\x1b[K"))); n > 120 {
			t.Errorf("line %d is %d runes wide", i, n)
		}
	}
}

func TestRenderSmallAndEmpty(t *testing.T) {
	if out := Render(&Model{}, 40, 2); !strings.Contains(out, "too small") {
		t.Error(out)
	}
	if out := Render(&Model{}, 80, 10); !strings.Contains(out, "Connecting") {
		t.Error(out)
	}
	m := &Model{Snap: snap(), Server: 1}
	if out := Render(m, 80, 20); !strings.Contains(out, "connection refused") || !strings.Contains(out, "no containers") {
		t.Error(out)
	}
}

func TestKeysAndUpdate(t *testing.T) {
	got := ParseKeys([]byte("j\x1b[A\x1b[B\t\x1b[Z\r\x1b\x03q\x1b[5~\x1b[1;5C"))
	want := []Key{"j", KeyUp, KeyDown, KeyTab, KeyBackTab, KeyEnter, KeyEsc, KeyCtrlC, "q", KeyPageUp}
	if len(got) != len(want) {
		t.Fatalf("keys %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d: %q want %q", i, got[i], want[i])
		}
	}

	m := &Model{Snap: snap()}
	m.Update("j", 30)
	m.Update("j", 30)
	m.Update("j", 30) // clamps at the last row
	if m.Sel != 2 {
		t.Errorf("sel %d", m.Sel)
	}
	if a := m.Update(KeyEnter, 30); a != ActLogs || m.Logs == nil || m.Logs.Container != "yoho-shop-production-db-1" || m.Logs.Server != "web-1" {
		t.Fatalf("enter: %v %+v", a, m.Logs)
	}
	if a := m.Update("q", 30); a != ActNone || m.Logs != nil {
		t.Error("q in logs goes back, not quit")
	}
	m.Update(KeyTab, 30)
	if m.Server != 1 || m.Sel != 0 {
		t.Errorf("tab: server %d sel %d", m.Server, m.Sel)
	}
	if a := m.Update(KeyEnter, 30); a != ActNone || m.Logs != nil {
		t.Error("enter without containers does nothing")
	}
	if m.Update("r", 30) != ActRefresh || m.Update("q", 30) != ActQuit || m.Update(KeyCtrlC, 30) != ActQuit {
		t.Error("r / q / ctrl-c")
	}
}

func TestLogScroll(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%3))
		b.WriteString("\n")
	}
	m := &Model{Logs: &LogView{Server: "web-1", Container: "c", Text: "first\n" + b.String() + "last\x1b]0;pwn\x07"}}
	out := Render(m, 80, 20)
	if !strings.Contains(out, "last") || strings.Contains(out, "first") || strings.Contains(out, "\x1b]0") {
		t.Errorf("bottom view:\n%q", out)
	}
	m.Update("g", 20)
	out = Render(m, 80, 20)
	if !strings.Contains(out, "first") || strings.Contains(out, "last") {
		t.Errorf("top view:\n%s", out)
	}
	m.Update("G", 20)
	if m.Logs.Offset != 0 {
		t.Error("G returns to the bottom")
	}
}

func TestWideCharacters(t *testing.T) {
	if got := cut("日本語abc", 5); got != "日本" {
		t.Errorf("cut by columns: %q", got)
	}
	if got := pad("日本", 6); got != "日本  " {
		t.Errorf("pad: %q", got)
	}
	m := &Model{Logs: &LogView{Text: strings.Repeat("日志🚀", 80)}}
	for i, l := range strings.Split(Render(m, 50, 10), "\r\n") {
		if w := colWidth(strings.TrimSuffix(l, "\x1b[K")); w > 50 {
			t.Errorf("line %d is %d columns", i, w)
		}
	}
	if s, _ := stateText(dash.Container{State: "exited", Completed: true}); s != "completed" {
		t.Error(s)
	}
}

func diskUsage() *dash.DiskUsage {
	return &dash.DiskUsage{CollectedAt: snap().CollectedAt.Add(-2 * time.Minute), Servers: []dash.ServerDisk{
		{Name: "web-1", Access: "sudo", User: "deploy", AppDir: "/var/lib/yoho/apps/shop/production", AppDirSized: true, AppDirBytes: 12 << 20,
			Volumes: []dash.Volume{
				{Name: "yoho-shop-production_pgdata", Services: []string{"db"}, Sized: true, Bytes: 8 << 30,
					Top: []dash.DirEntry{{Name: "base", Bytes: 7 << 30}, {Name: "evil\x1b[2J", Bytes: 4096}}},
				{Name: "yoho-shop-production_cache", Services: []string{"web"}, Sized: true, Partial: true, Bytes: 1 << 30, Error: "du stopped after 120s"},
				{Name: "yoho-shop-production_nfs", Error: "volume options (device,o,type): not sized"},
			}},
		{Name: "web-2", Access: "none", User: "deploy", Note: "Volume sizes need root: connect as root, or allow passwordless sudo for du",
			Volumes: []dash.Volume{{Name: "yoho-shop-production_pgdata"}}},
	}}
}

func TestDiskScreen(t *testing.T) {
	m := &Model{Snap: snap(), Now: snap().CollectedAt, Interval: 5 * time.Second}
	if m.Update("v", 30) != ActDiskOpen || m.Disk == nil || !m.Disk.Loading {
		t.Fatal("v opens the disk screen and loads")
	}
	if out := Render(m, 120, 30); !strings.Contains(out, "sizing") {
		t.Errorf("loading state:\n%s", out)
	}
	m.Disk.Usage, m.Disk.Loading = diskUsage(), false
	m.Update(KeyEnter, 30)
	out := Render(m, 120, 30)
	for _, want := range []string{"disk", "sized 2m ago", "deploy via sudo", "12.0 MiB", "VOLUME", "yoho-shop-production_pgdata", "8.0 GiB", "base", "7.0 GiB", "≥1.0 GiB", "du stopped", "not sized", "Disk /", "rescan"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[2J") {
		t.Error("file names must not inject escape sequences")
	}
	if m.Update("r", 30) != ActDisk {
		t.Error("r rescans")
	}
	m.Update(KeyTab, 30)
	out = Render(m, 120, 30)
	if !strings.Contains(out, "need root") || !strings.Contains(out, "—") {
		t.Errorf("web-2 shows the note and no sizes:\n%s", out)
	}
	if m.Update("q", 30) != ActNone || m.Disk != nil {
		t.Error("q leaves the disk screen, not yoho")
	}
}
