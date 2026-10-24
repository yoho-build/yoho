package tui

import (
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/dash"
)

func TestOverviewDetailsShortcutVisible(t *testing.T) {
	m := &Model{Snap: snap()}
	out := Render(m, 80, 24)
	if !strings.Contains(out, "d details") || !strings.Contains(out, "enter logs") {
		t.Fatal("a standard-width terminal must show the Details and logs shortcuts")
	}
}

func TestDetailsNavigation(t *testing.T) {
	m := &Model{Snap: snap(), Sel: 1}
	if got := m.Update("d", 20); got != ActNone || m.Details == nil || m.Logs != nil {
		t.Fatalf("opening details: action=%v details=%+v logs=%+v", got, m.Details, m.Logs)
	}
	if m.Details.Server != "web-1" || m.Details.Container != "yoho-shop-production-web-1" {
		t.Fatalf("wrong details target: %+v", m.Details)
	}
	if m.Update("r", 20) != ActRefresh {
		t.Fatal("details refresh must request a new snapshot")
	}
	m.Update(KeyTab, 20)
	if m.Server != 0 || m.Sel != 1 {
		t.Fatal("details must keep the selected Server and Service")
	}
	m.Update(KeyPageDown, 20)
	if m.Details.Offset <= 0 {
		t.Fatal("page down must scroll details")
	}
	m.Update("g", 20)
	m.Update(KeyUp, 20)
	if m.Details.Offset != 0 {
		t.Fatal("scroll must stop at the top")
	}
	m.Update("G", 20)
	m.Update(KeyDown, 20)
	if m.Details.Offset <= 0 {
		t.Fatal("bottom followed by down must not overflow the offset")
	}
	if got := m.Update(KeyEnter, 20); got != ActLogs || m.Logs == nil || m.Logs.Container != m.Details.Container {
		t.Fatalf("details logs: action=%v logs=%+v", got, m.Logs)
	}
	if m.Update(KeyEsc, 20) != ActNone || m.Logs != nil || m.Details == nil {
		t.Fatal("leaving logs must return to details")
	}
	if m.Update("q", 20) != ActNone || m.Details != nil {
		t.Fatal("q in details must return to the overview")
	}
	if m.Update("q", 20) != ActQuit {
		t.Fatal("q on the overview must quit")
	}
}

func TestDetailsIdentityAcrossSnapshots(t *testing.T) {
	first := snap()
	first.Servers[0].Containers[0].ID = "original"
	m := &Model{Snap: first, Sel: 1}
	m.Update("d", 30)
	updated := snap()
	updated.Servers[0].Containers[0].ID = "original"
	updated.Servers[0].Containers[0].CPUPercent = 42
	updated.Servers[0].Containers[0], updated.Servers[0].Containers[1] = updated.Servers[0].Containers[1], updated.Servers[0].Containers[0]
	updated.Servers[0], updated.Servers[1] = updated.Servers[1], updated.Servers[0]
	m.Snap = updated
	s, c := m.detailContainer()
	if s == nil || s.Name != "web-1" || c == nil || c.ID != "original" || c.CPUPercent != 42 {
		t.Fatalf("details must follow identity in the latest snapshot: server=%+v container=%+v", s, c)
	}
	c.ID = "replacement"
	if _, c := m.detailContainer(); c != nil {
		t.Fatal("a new container with the same name must not replace the pinned target")
	}
	if m.Update(KeyEnter, 30) != ActNone || m.Logs != nil {
		t.Fatal("a replaced container must not open another container's logs")
	}
	m.Snap.Servers = nil
	if s, c := m.detailContainer(); s != nil || c != nil {
		t.Fatal("removed Server must not retain stale details")
	}
}

func TestDetailsProxyAndEmptySelection(t *testing.T) {
	m := &Model{Snap: snap()}
	m.Update("d", 20)
	if _, c := m.detailContainer(); c == nil || c.Name != "yoho-proxy" {
		t.Fatal("the Proxy must support details too")
	}
	if m.Update(KeyCtrlC, 20) != ActQuit {
		t.Fatal("Ctrl-C must quit from details")
	}
	m.Update(KeyEsc, 20)
	m.Update(KeyTab, 20)
	if m.Update("d", 20) != ActNone || m.Details != nil {
		t.Fatal("details without a container must do nothing")
	}
	m = &Model{Snap: &dash.Snapshot{}}
	if m.Update("d", 20) != ActNone || m.Details != nil {
		t.Fatal("details before collection must do nothing")
	}
}

func TestDetailsCannotOpenLogsFromFailedServer(t *testing.T) {
	m := &Model{Snap: snap(), Sel: 1}
	m.Update("d", 20)
	m.Snap.Servers[0].Error = "connection lost"
	if m.Update(KeyEnter, 20) != ActNone || m.Logs != nil {
		t.Fatal("a failed Server must not open logs from its old container list")
	}
}
