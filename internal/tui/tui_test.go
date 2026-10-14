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
