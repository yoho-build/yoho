package tui

import (
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/dash"
)

// TrafficHint is shown when the App has proxied Services but proxy.metrics
// is off.
const TrafficHint = "traffic: set `proxy.metrics: true` in the Yoho file, then `yoho proxy boot` (or deploy) to see req/min, 5xx and latency"

// trafficLines is one compact line per proxied Service, or the hint.
func trafficLines(t *dash.Traffic) []line {
	if t == nil {
		return nil
	}
	if !t.Enabled {
		return []line{{{"  " + TrafficHint, dim}}, {}}
	}
	ls := []line{{{"  TRAFFIC", bold}, {"  via yoho-proxy metrics", dim}}}
	if t.Error != "" {
		ls = append(ls, line{{"  " + t.Error, red}})
	}
	for _, r := range t.Routes {
		host := "*"
		if len(r.Hosts) > 0 {
			host = strings.Join(r.Hosts, ",")
		}
		l := line{{"  " + pad(r.Service, 14) + " ", bold}, {pad(host, 22) + " ", dim}}
		if r.PerMinute == nil {
			l = append(l, seg{fmt.Sprintf("%.0f requests since proxy start · rates after the next refresh", r.Requests), dim})
			ls = append(ls, l)
			continue
		}
		errStyle := green
		switch {
		case *r.ErrorPercent >= 5:
			errStyle = red
		case *r.ErrorPercent > 0:
			errStyle = yellow
		}
		l = append(l,
			seg{pad(rate(*r.PerMinute)+" req/min", 15) + " ", ""},
			seg{pad(fmt.Sprintf("5xx %.1f%%", *r.ErrorPercent), 10) + " ", errStyle},
			seg{pad("p50 "+latency(r.P50), 10) + " " + pad("p95 "+latency(r.P95), 10) + " ", ""},
			seg{Sparkline(r.History), cyan},
		)
		ls = append(ls, l)
	}
	return append(ls, line{})
}

func rate(v float64) string {
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// latency formats seconds; nil is no requests in the interval.
func latency(s *float64) string {
	if s == nil {
		return "—"
	}
	switch v := *s; {
	case v < 0.001:
		return fmt.Sprintf("%.0fµs", v*1e6)
	case v < 1:
		return fmt.Sprintf("%.0fms", v*1000)
	default:
		return fmt.Sprintf("%.2fs", v)
	}
}

// Sparkline draws values with block characters scaled to their maximum.
func Sparkline(vs []float64) string {
	ticks := []rune("▁▂▃▄▅▆▇█")
	hi := 0.0
	for _, v := range vs {
		hi = max(hi, v)
	}
	var b strings.Builder
	for _, v := range vs {
		i := 0
		if hi > 0 {
			i = int(v / hi * float64(len(ticks)-1))
		}
		b.WriteRune(ticks[i])
	}
	return b.String()
}
