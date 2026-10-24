package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
)

func detailModel() *Model {
	s := snap()
	c := &s.Servers[0].Containers[0]
	c.ID = "container-id-1"
	c.CPUPercent = 12.5
	c.MemPercent = 3.25
	c.MemUsage = "100MiB / 7.6GiB"
	c.NetIO = "1kB / 2kB"
	c.BlockIO = "3kB / 4kB"
	c.PIDs = "9"
	c.Status = "Up 2 hours (healthy)"
	return &Model{
		Snap: s, Details: &DetailView{Server: s.Servers[0].Name, Container: c.Name, ID: c.ID},
		Now: s.CollectedAt.Add(3 * time.Second), Interval: 5 * time.Second,
	}
}

func TestDetailsRenderShowsIdentityAndRealZeroStats(t *testing.T) {
	m := detailModel()
	c := &m.Snap.Servers[0].Containers[0]
	c.CPUPercent, c.MemPercent = 0, 0
	c.MemUsage, c.NetIO, c.BlockIO, c.PIDs = "0B / 7.6GiB", "0B / 0B", "0B / 0B", "0"
	out := Render(m, 110, 32)
	for _, want := range []string{
		"Details", "updated", "Service: web", "Server: web-1", "Container: " + c.Name,
		"Container ID: container-id-1", "Image: shop-web:abc", "State: running", "Health: healthy",
		"Status: Up 2 hours (healthy)", "Uptime: 2 hours", "Ports: —", "CPU: 0.0%",
		"Memory usage / limit: 0B / 7.6GiB", "Memory percent: 0.0%",
		"Network I/O RX / TX: 0B / 0B (cumulative)", "Block I/O read / write: 0B / 0B (cumulative)",
		"PIDs: 0", "esc/q back",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in details screen:\n%s", want, out)
		}
	}
	if strings.Count(out, "0.0%") != 2 {
		t.Errorf("expected real zero CPU and memory percentages, got:\n%s", out)
	}
}

func TestDetailsStatsUnavailableForStoppedOrUncollectedContainer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*dash.Container)
	}{
		{name: "stopped", setup: func(c *dash.Container) {
			c.State = "exited"
			c.Status = "Exited (0) 5 minutes ago"
		}},
		{name: "missing memory usage", setup: func(c *dash.Container) {
			c.MemUsage = ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := detailModel()
			c := &m.Snap.Servers[0].Containers[0]
			c.CPUPercent, c.MemPercent = 81.2, 70
			c.MemUsage, c.NetIO, c.BlockIO, c.PIDs = "100MiB / 7.6GiB", "9kB / 8kB", "7kB / 6kB", "9"
			tt.setup(c)
			out := Render(m, 110, 32)
			for _, want := range []string{"CPU: —", "Memory usage / limit: —", "Memory percent: —", "Network I/O RX / TX: —", "Block I/O read / write: —", "PIDs: —"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing unavailable value %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "81.2%") || strings.Contains(out, "100MiB / 7.6GiB") {
				t.Errorf("stale or uncollected stats shown:\n%s", out)
			}
		})
	}
}

func TestDetailsUsesMetricsFromRefreshedSnapshot(t *testing.T) {
	m := detailModel()
	m.Snap.Servers[0].Containers[0].CPUPercent = 1.2
	first := Render(m, 110, 32)
	fresh := snap()
	c := &fresh.Servers[0].Containers[0]
	c.ID, c.CPUPercent, c.MemUsage = m.Details.ID, 7.4, "250MiB / 7.6GiB"
	m.Snap = fresh
	second := Render(m, 110, 32)
	if !strings.Contains(first, "CPU: 1.2%") || !strings.Contains(second, "CPU: 7.4%") || strings.Contains(second, "CPU: 1.2%") {
		t.Fatalf("renderer did not use refreshed metrics:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(second, "250MiB / 7.6GiB") {
		t.Errorf("refreshed memory not shown:\n%s", second)
	}
}

func TestDetailsMissingReplacedAndServerErrors(t *testing.T) {
	t.Run("disappeared", func(t *testing.T) {
		m := detailModel()
		m.Snap.Servers[0].Containers = nil
		out := Render(m, 110, 32)
		if !strings.Contains(out, "Container is absent from the latest snapshot") || strings.Contains(out, "CPU: 12.5%") {
			t.Errorf("missing target was not reported safely:\n%s", out)
		}
	})
	t.Run("replaced", func(t *testing.T) {
		m := detailModel()
		c := &m.Snap.Servers[0].Containers[0]
		c.ID, c.CPUPercent = "replacement-id", 88.8
		out := Render(m, 110, 32)
		if !strings.Contains(out, "Container was replaced in the latest snapshot") || !strings.Contains(out, "Current ID: replacement-id") || strings.Contains(out, "88.8%") {
			t.Errorf("replacement was not distinguished from pinned target:\n%s", out)
		}
	})
	t.Run("refresh error", func(t *testing.T) {
		m := detailModel()
		m.Err = errors.New("snapshot refresh failed")
		assertDetailErrorAndUnavailableStats(t, m, "Refresh error", "snapshot refresh failed")
	})
	t.Run("server error", func(t *testing.T) {
		m := detailModel()
		m.Snap.Servers[0].Error = "SSH connection refused"
		assertDetailErrorAndUnavailableStats(t, m, "Server error", "SSH connection refused")
	})
	t.Run("docker error", func(t *testing.T) {
		m := detailModel()
		m.Snap.Servers[0].Host.DockerError = "docker unavailable"
		assertDetailErrorAndUnavailableStats(t, m, "Docker error", "docker unavailable")
	})
	t.Run("server disappeared", func(t *testing.T) {
		m := detailModel()
		m.Snap.Servers = nil
		out := Render(m, 110, 32)
		if !strings.Contains(out, "Server is absent from the latest snapshot") || strings.Contains(out, "CPU: 12.5%") {
			t.Errorf("missing server was not reported safely:\n%s", out)
		}
	})
}

func assertDetailErrorAndUnavailableStats(t *testing.T, m *Model, label, message string) {
	t.Helper()
	out := Render(m, 110, 32)
	if !strings.Contains(out, label+": "+message) || !strings.Contains(out, "CPU: —") || strings.Contains(out, "CPU: 12.5%") {
		t.Errorf("error or unavailable stats missing:\n%s", out)
	}
}

func TestDetailsIncludesProxyAndClampsScroll(t *testing.T) {
	m := detailModel()
	s := &m.Snap.Servers[0]
	s.Proxy = &dash.Container{ID: "proxy-id", Name: "yoho-proxy", Service: "proxy", State: "running", MemUsage: "1MiB / 7GiB"}
	m.Details = &DetailView{Server: s.Name, Container: s.Proxy.Name, ID: s.Proxy.ID, Offset: 1 << 30}
	gotServer, gotContainer := m.detailContainer()
	if gotServer != s || gotContainer != s.Proxy {
		t.Fatalf("proxy target not resolved: server=%p container=%p", gotServer, gotContainer)
	}
	out := Render(m, 80, 10)
	if m.Details.Offset <= 0 || m.Details.Offset >= 1<<30 || !strings.Contains(out, "esc/q back") {
		t.Errorf("scroll offset/footer not clamped and rendered: offset=%d\n%s", m.Details.Offset, out)
	}
}

func TestDetailsNarrowLayoutKeepsFooterVisible(t *testing.T) {
	m := detailModel()
	const width, height = 26, 12
	out := Render(m, width, height)
	rows := strings.Split(out, "\r\n")
	if len(rows) != height {
		t.Fatalf("got %d rows, want %d:\n%s", len(rows), height, out)
	}
	for i, row := range rows {
		row = strings.TrimSuffix(row, "\x1b[K")
		if got := colWidth(row); got > width {
			t.Errorf("row %d is %d columns, want <= %d: %q", i, got, width, row)
		}
	}
	for _, want := range []string{"esc/q back", "enter logs", "r refresh", "↑↓ scroll", "PgUp/PgDn scroll", "g/G top/bottom"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrow footer missing %q:\n%s", want, out)
		}
	}
}

func TestDetailsStripsTerminalControlsFromContainerValues(t *testing.T) {
	m := detailModel()
	c := &m.Snap.Servers[0].Containers[0]
	c.Name = "worker\x1b[2J"
	c.Image = "image\x1b]0;owned\x07"
	c.Status = "Up 2 minutes\x1b[2J"
	c.MemUsage = "1MiB\x1b[2J / 7GiB"
	c.NetIO = "rx\x1b[2J / tx"
	m.Details.Container = c.Name
	out := Render(m, 110, 32)
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]0;") || strings.Contains(out, "\x07") {
		t.Errorf("container value injected terminal control:\n%q", out)
	}
	if !strings.Contains(out, "worker[2J") || !strings.Contains(out, "image]0;owned") {
		t.Errorf("sanitized container values missing:\n%s", out)
	}
}
