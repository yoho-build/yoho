package dash

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/remote"
)

// ProxyRoute is a Proxy route of the App: the kamal-proxy service that
// serves one proxied Service.
type ProxyRoute struct {
	Service string   // compose Service
	Route   string   // kamal-proxy service name (deploy.RouteName)
	Hosts   []string // configured hostnames; empty is the catch-all
}

// Traffic is the Proxy traffic of the App's routes on one Server, read from
// kamal-proxy's Prometheus metrics (proxy.metrics). Totals are counters
// since the Proxy started; the rates and latencies cover the interval since
// the previous Snapshot and are filled in by Live.
type Traffic struct {
	// Enabled is proxy.metrics. When false, Routes is empty and the
	// dashboards show how to enable it.
	Enabled bool           `json:"enabled"`
	Error   string         `json:"error,omitempty"`
	Routes  []RouteTraffic `json:"routes"`
}

// RouteTraffic is one route's counters and, from the second Snapshot on,
// its rates over the last interval.
type RouteTraffic struct {
	Service  string   `json:"service"`
	Route    string   `json:"route"`
	Hosts    []string `json:"hosts"`
	Requests float64  `json:"requests_total"`
	Errors   float64  `json:"errors_5xx_total"`
	InFlight float64  `json:"in_flight"`
	// Buckets is the latency histogram summed over status and method:
	// cumulative counts of requests that took at most LE seconds.
	Buckets []Bucket `json:"buckets"`

	// Rates over IntervalSeconds, nil until two Snapshots exist.
	IntervalSeconds float64  `json:"interval_seconds,omitempty"`
	PerMinute       *float64 `json:"requests_per_min,omitempty"`
	ErrorPercent    *float64 `json:"error_percent,omitempty"`
	// Latency quantiles in seconds over the interval; nil without requests.
	P50 *float64 `json:"p50_seconds,omitempty"`
	P95 *float64 `json:"p95_seconds,omitempty"`
	// History is requests/min of the last HistoryLen intervals, oldest
	// first; kept in memory of this process only.
	History []float64 `json:"history"`
}

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	LE    float64 `json:"le"` // +Inf as math.MaxFloat64 in JSON
	Count float64 `json:"count"`
}

// HistoryLen is how many intervals Live keeps per route.
const HistoryLen = 30

const (
	markMetrics = "@@yoho-metrics"

	metricRequests = "kamal_proxy_http_requests_total"
	metricDuration = "kamal_proxy_http_request_duration_seconds_bucket"
	metricInFlight = "kamal_proxy_http_in_flight_requests"
)

// MetricsScript is the read-only shell fragment that prints the Proxy's
// request metrics of routes only (the App's routes), fetched from
// 127.0.0.1 with curl or wget. Other Apps' routes never leave the Server.
func MetricsScript(routes []string) string {
	if len(routes) == 0 {
		return ""
	}
	var grep strings.Builder
	for _, r := range routes {
		grep.WriteString(" -e " + remote.Quote(`service="`+r+`"`))
	}
	url := remote.Quote("http://127.0.0.1:" + strconv.Itoa(proxy.MetricsPort) + "/metrics")
	return `echo ` + markMetrics + `
yoho_metrics() {
  if command -v curl >/dev/null 2>&1; then curl -fsS --max-time 3 ` + url + `
  elif command -v wget >/dev/null 2>&1; then wget -q -T 3 -O - ` + url + `
  else echo 'neither curl nor wget is installed on the Server' >&2; return 1; fi
}
if body=$(yoho_metrics 2>&1); then
  printf '%s\n' "$body" | grep '^kamal_proxy_http_' | grep -F` + grep.String() + ` || :
else
  printf 'error=%s\n' "$(printf '%s' "$body" | tr '\n' ' ' | cut -c1-200)"
fi
`
}

// Sample is one Prometheus text-format sample.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// ParsePrometheus parses the Prometheus text exposition format. Comments,
// blank and malformed lines are skipped; timestamps are ignored.
func ParsePrometheus(text string) []Sample {
	var out []Sample
	for _, line := range strings.Split(text, "\n") {
		if s, ok := parseSample(strings.TrimSpace(line)); ok {
			out = append(out, s)
		}
	}
	return out
}

func parseSample(line string) (Sample, bool) {
	if line == "" || line[0] == '#' {
		return Sample{}, false
	}
	s := Sample{Labels: map[string]string{}}
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return Sample{}, false
	}
	s.Name, line = line[:i], line[i:]
	if line[0] == '{' {
		rest, ok := parseLabels(line[1:], s.Labels)
		if !ok {
			return Sample{}, false
		}
		line = rest
	}
	f := strings.Fields(line)
	if len(f) == 0 || len(f) > 2 {
		return Sample{}, false
	}
	v, err := parseValue(f[0])
	if err != nil {
		return Sample{}, false
	}
	s.Value = v
	return s, true
}

// parseLabels reads `k="v",...}` into m and returns the text after `}`.
func parseLabels(s string, m map[string]string) (string, bool) {
	for {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			return "", false
		}
		if s[0] == '}' {
			return s[1:], true
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || len(s) < eq+2 || s[eq+1] != '"' {
			return "", false
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var v strings.Builder
		closed := false
		for j := 0; j < len(s); j++ {
			c := s[j]
			if c == '\\' && j+1 < len(s) {
				j++
				switch s[j] {
				case 'n':
					v.WriteByte('\n')
				default: // \\ and \"
					v.WriteByte(s[j])
				}
				continue
			}
			if c == '"' {
				s = s[j+1:]
				closed = true
				break
			}
			v.WriteByte(c)
		}
		if !closed {
			return "", false
		}
		m[key] = v.String()
	}
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// parseTraffic turns the metrics section into per-route counters, in the
// order of routes. Samples of other routes are ignored.
func parseTraffic(lines []string, routes []ProxyRoute) *Traffic {
	t := &Traffic{Enabled: true, Routes: []RouteTraffic{}}
	var body []string
	for _, l := range lines {
		if e, ok := strings.CutPrefix(l, "error="); ok {
			t.Error = "proxy metrics: " + strings.TrimSpace(e)
			continue
		}
		body = append(body, l)
	}
	byRoute := map[string]*RouteTraffic{}
	buckets := map[string]map[float64]float64{}
	for _, r := range routes {
		hosts := r.Hosts
		if hosts == nil {
			hosts = []string{}
		}
		t.Routes = append(t.Routes, RouteTraffic{Service: r.Service, Route: r.Route, Hosts: hosts, Buckets: []Bucket{}, History: []float64{}})
	}
	for i := range t.Routes {
		byRoute[t.Routes[i].Route] = &t.Routes[i]
	}
	for _, s := range ParsePrometheus(strings.Join(body, "\n")) {
		rt := byRoute[s.Labels["service"]]
		if rt == nil || math.IsNaN(s.Value) {
			continue
		}
		switch s.Name {
		case metricRequests:
			rt.Requests += s.Value
			if strings.HasPrefix(s.Labels["status"], "5") {
				rt.Errors += s.Value
			}
		case metricInFlight:
			rt.InFlight += s.Value
		case metricDuration:
			le, err := parseValue(s.Labels["le"])
			if err != nil {
				continue
			}
			if buckets[rt.Route] == nil {
				buckets[rt.Route] = map[float64]float64{}
			}
			buckets[rt.Route][le] += s.Value
		}
	}
	for i := range t.Routes {
		rt := &t.Routes[i]
		for le, n := range buckets[rt.Route] {
			rt.Buckets = append(rt.Buckets, Bucket{LE: le, Count: n})
		}
		sort.Slice(rt.Buckets, func(a, b int) bool { return rt.Buckets[a].LE < rt.Buckets[b].LE })
		for j := range rt.Buckets {
			if math.IsInf(rt.Buckets[j].LE, 1) {
				rt.Buckets[j].LE = math.MaxFloat64 // JSON has no +Inf
			}
		}
	}
	return t
}

// Quantile estimates the q quantile (0..1) from cumulative buckets the way
// Prometheus histogram_quantile does: linear interpolation inside the
// bucket, and the highest finite bound for the +Inf bucket. ok is false
// without observations.
func Quantile(q float64, buckets []Bucket) (float64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	total := buckets[len(buckets)-1].Count
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	prevLE, prevCount := 0.0, 0.0
	for i, b := range buckets {
		if b.Count >= rank {
			if b.LE == math.MaxFloat64 || math.IsInf(b.LE, 1) {
				if i == 0 {
					return 0, true
				}
				return buckets[i-1].LE, true
			}
			if b.Count == prevCount {
				return b.LE, true
			}
			return prevLE + (b.LE-prevLE)*(rank-prevCount)/(b.Count-prevCount), true
		}
		prevLE, prevCount = b.LE, b.Count
	}
	return prevLE, true
}

// delta is a counter increase; a decrease means the Proxy restarted and
// the counter began again at zero.
func delta(cur, prev float64) float64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// rates fills cur's interval fields from prev (the same route in the
// previous Snapshot, dt earlier) and appends to its history.
func rates(cur *RouteTraffic, prev *RouteTraffic, dt time.Duration, history []float64) {
	cur.History = append([]float64{}, history...)
	if prev == nil || dt <= 0 {
		return
	}
	reset := cur.Requests < prev.Requests
	req := delta(cur.Requests, prev.Requests)
	errs := delta(cur.Errors, prev.Errors)
	if reset {
		errs = cur.Errors
	}
	perMin := req / dt.Minutes()
	cur.IntervalSeconds = dt.Seconds()
	cur.PerMinute = &perMin
	if req > 0 {
		p := 100 * errs / req
		cur.ErrorPercent = &p
	} else {
		zero := 0.0
		cur.ErrorPercent = &zero
	}
	prevBy := map[float64]float64{}
	if !reset {
		for _, b := range prev.Buckets {
			prevBy[b.LE] = b.Count
		}
	}
	d := make([]Bucket, len(cur.Buckets))
	for i, b := range cur.Buckets {
		d[i] = Bucket{LE: b.LE, Count: delta(b.Count, prevBy[b.LE])}
	}
	if v, ok := Quantile(0.5, d); ok {
		cur.P50 = &v
	}
	if v, ok := Quantile(0.95, d); ok {
		cur.P95 = &v
	}
	cur.History = append(cur.History, perMin)
	if n := len(cur.History); n > HistoryLen {
		cur.History = cur.History[n-HistoryLen:]
	}
}

// applyRates computes every route's interval rates of snap against prev.
func applyRates(snap, prev *Snapshot) {
	for i := range snap.Servers {
		s := &snap.Servers[i]
		if s.Traffic == nil || !s.Traffic.Enabled {
			continue
		}
		var pt *Traffic
		if prev != nil {
			for j := range prev.Servers {
				if prev.Servers[j].Name == s.Name {
					pt = prev.Servers[j].Traffic
				}
			}
		}
		for k := range s.Traffic.Routes {
			cur := &s.Traffic.Routes[k]
			var pr *RouteTraffic
			if pt != nil {
				for m := range pt.Routes {
					if pt.Routes[m].Route == cur.Route {
						pr = &pt.Routes[m]
					}
				}
			}
			switch {
			case pr == nil:
				rates(cur, nil, 0, nil)
			case s.Traffic.Error != "":
				// Keep the history visible while the endpoint fails.
				cur.History = append([]float64{}, pr.History...)
			case pt.Error != "":
				// No counters to compare with; the next interval has them.
				rates(cur, nil, 0, pr.History)
			default:
				rates(cur, pr, snap.CollectedAt.Sub(prev.CollectedAt), pr.History)
			}
		}
	}
}
