package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
)

func fp(v float64) *float64 { return &v }

func TestRenderTraffic(t *testing.T) {
	s := snap()
	s.Servers[0].Traffic = &dash.Traffic{Enabled: true, Routes: []dash.RouteTraffic{
		{Service: "web", Route: "shop-production-web", Hosts: []string{"shop.test"}, Requests: 900,
			PerMinute: fp(120), ErrorPercent: fp(2.5), P50: fp(0.004), P95: fp(0.25), History: []float64{0, 60, 120}},
		{Service: "api", Route: "shop-production-api", Hosts: []string{}, Requests: 3},
	}}
	m := &Model{Snap: s, Now: s.CollectedAt, Interval: 5 * time.Second}
	out := Render(m, 140, 40)
	for _, want := range []string{"TRAFFIC", "shop.test", "120 req/min", "5xx 2.5%", "p50 4ms", "p95 250ms", "▁▄█", "3 requests since proxy start"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	s.Servers[0].Traffic = &dash.Traffic{Routes: []dash.RouteTraffic{}}
	if out := Render(m, 200, 40); !strings.Contains(out, "proxy.metrics: true") {
		t.Errorf("hint missing:\n%s", out)
	}
	s.Servers[0].Traffic = nil
	if out := Render(m, 200, 40); strings.Contains(out, "TRAFFIC") || strings.Contains(out, "proxy.metrics") {
		t.Errorf("no proxied Service, no traffic:\n%s", out)
	}
}

func TestSparkline(t *testing.T) {
	if got := Sparkline([]float64{0, 1, 2, 4, 8}); got != "▁▁▂▄█" {
		t.Errorf("sparkline %q", got)
	}
	if got := Sparkline([]float64{0, 0}); got != "▁▁" {
		t.Errorf("flat %q", got)
	}
}
