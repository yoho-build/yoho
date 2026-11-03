package dash

import (
	"encoding/json"
	"math"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Captured from basecamp/kamal-proxy:v0.10.2 run --metrics-port, trimmed.
const metricsFixture = `# HELP kamal_proxy_http_in_flight_requests Number of in-flight HTTP requests, labeled by service.
# TYPE kamal_proxy_http_in_flight_requests gauge
kamal_proxy_http_in_flight_requests{service="w4a-production-web"} 1
kamal_proxy_http_in_flight_requests{service="other-app-web"} 7
# TYPE kamal_proxy_http_request_duration_seconds histogram
kamal_proxy_http_request_duration_seconds_bucket{method="GET",rollout="false",service="w4a-production-web",status="200",le="0.005"} 2
kamal_proxy_http_request_duration_seconds_bucket{method="GET",rollout="false",service="w4a-production-web",status="200",le="0.01"} 3
kamal_proxy_http_request_duration_seconds_bucket{method="GET",rollout="false",service="w4a-production-web",status="200",le="+Inf"} 3
kamal_proxy_http_request_duration_seconds_bucket{method="GET",rollout="false",service="w4a-production-web",status="500",le="0.005"} 1
kamal_proxy_http_request_duration_seconds_bucket{method="GET",rollout="false",service="w4a-production-web",status="500",le="0.01"} 1
kamal_proxy_http_request_duration_seconds_bucket{method="GET",rollout="false",service="w4a-production-web",status="500",le="+Inf"} 1
kamal_proxy_http_request_duration_seconds_sum{method="GET",rollout="false",service="w4a-production-web",status="200"} 0.0065
kamal_proxy_http_request_duration_seconds_count{method="GET",rollout="false",service="w4a-production-web",status="200"} 3
kamal_proxy_http_requests_total{method="GET",rollout="false",service="w4a-production-web",status="200"} 3
kamal_proxy_http_requests_total{method="GET",rollout="false",service="w4a-production-web",status="500"} 1 1700000000000
kamal_proxy_http_requests_total{method="GET",rollout="false",service="other-app-web",status="503"} 99
`

func TestParsePrometheus(t *testing.T) {
	got := ParsePrometheus(`# comment
up 1
a_total{x="q\"uo\\te\n",y="b,c}"} 2.5 123
bad{x="unterminated} 1
inf{le="+Inf"} +Inf
nan NaN
novalue{}
`)
	if len(got) != 4 {
		t.Fatalf("got %d samples: %+v", len(got), got)
	}
	if got[0].Name != "up" || got[0].Value != 1 {
		t.Errorf("up = %+v", got[0])
	}
	if got[1].Labels["x"] != "q\"uo\\te\n" || got[1].Labels["y"] != "b,c}" || got[1].Value != 2.5 {
		t.Errorf("labels = %+v", got[1])
	}
	if !math.IsInf(got[2].Value, 1) || got[2].Labels["le"] != "+Inf" {
		t.Errorf("inf = %+v", got[2])
	}
	if !math.IsNaN(got[3].Value) {
		t.Errorf("nan = %+v", got[3])
	}
}

var fixtureRoutes = []ProxyRoute{{Service: "web", Route: "w4a-production-web", Hosts: []string{"w4a.test"}}, {Service: "api", Route: "w4a-production-api"}}

func TestParseTrafficOnlyOwnRoutes(t *testing.T) {
	tr := parseTraffic(strings.Split(metricsFixture, "\n"), fixtureRoutes)
	if !tr.Enabled || tr.Error != "" || len(tr.Routes) != 2 {
		t.Fatalf("traffic = %+v", tr)
	}
	web := tr.Routes[0]
	if web.Service != "web" || web.Requests != 4 || web.Errors != 1 || web.InFlight != 1 {
		t.Errorf("web = %+v", web)
	}
	want := []Bucket{{0.005, 3}, {0.01, 4}, {math.MaxFloat64, 4}}
	if len(web.Buckets) != 3 {
		t.Fatalf("buckets = %+v", web.Buckets)
	}
	for i, b := range want {
		if web.Buckets[i] != b {
			t.Errorf("bucket %d = %+v, want %+v", i, web.Buckets[i], b)
		}
	}
	if api := tr.Routes[1]; api.Requests != 0 || len(api.Buckets) != 0 || api.Hosts == nil {
		t.Errorf("api = %+v", api)
	}
	// Another App's route never counts, and the JSON has no +Inf.
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "other-app") {
		t.Errorf("foreign route leaked: %s", b)
	}
}

func TestParseTrafficError(t *testing.T) {
	tr := parseTraffic([]string{"error=curl: (7) Failed to connect"}, fixtureRoutes)
	if !strings.Contains(tr.Error, "Failed to connect") || len(tr.Routes) != 2 {
		t.Errorf("traffic = %+v", tr)
	}
}

func TestQuantile(t *testing.T) {
	b := []Bucket{{0.1, 50}, {0.2, 90}, {0.5, 100}, {math.MaxFloat64, 100}}
	for _, c := range []struct{ q, want float64 }{{0.5, 0.1}, {0.25, 0.05}, {0.7, 0.15}, {0.95, 0.35}} {
		got, ok := Quantile(c.q, b)
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("q%.2f = %v %v, want %v", c.q, got, ok, c.want)
		}
	}
	if got, _ := Quantile(0.99, []Bucket{{0.1, 1}, {math.MaxFloat64, 10}}); got != 0.1 {
		t.Errorf("+Inf bucket quantile = %v, want the highest finite bound", got)
	}
	if _, ok := Quantile(0.5, []Bucket{{0.1, 0}, {math.MaxFloat64, 0}}); ok {
		t.Error("no observations must be !ok")
	}
}

func trafficSnap(at time.Time, req, errs float64, buckets ...Bucket) *Snapshot {
	return &Snapshot{CollectedAt: at, Servers: []Server{{Name: "s1", Traffic: &Traffic{Enabled: true, Routes: []RouteTraffic{{
		Service: "web", Route: "r", Requests: req, Errors: errs, Buckets: buckets, History: []float64{},
	}}}}}}
}

func TestApplyRates(t *testing.T) {
	t0 := time.Unix(1000, 0)
	a := trafficSnap(t0, 100, 10, Bucket{0.1, 90}, Bucket{1, 100}, Bucket{math.MaxFloat64, 100})
	applyRates(a, nil)
	if r := a.Servers[0].Traffic.Routes[0]; r.PerMinute != nil || len(r.History) != 0 {
		t.Fatalf("first snapshot has no rate: %+v", r)
	}
	b := trafficSnap(t0.Add(30*time.Second), 160, 13, Bucket{0.1, 90}, Bucket{1, 160}, Bucket{math.MaxFloat64, 160})
	applyRates(b, a)
	r := b.Servers[0].Traffic.Routes[0]
	if r.PerMinute == nil || *r.PerMinute != 120 {
		t.Fatalf("per minute = %v", r.PerMinute)
	}
	if *r.ErrorPercent != 5 {
		t.Errorf("error %% = %v", *r.ErrorPercent)
	}
	// All 60 new requests took 0.1..1s.
	if r.P50 == nil || math.Abs(*r.P50-0.55) > 1e-9 || math.Abs(*r.P95-0.955) > 1e-9 {
		t.Errorf("p50/p95 = %v/%v", *r.P50, *r.P95)
	}
	if len(r.History) != 1 || r.History[0] != 120 {
		t.Errorf("history = %v", r.History)
	}
	// A Proxy restart resets the counters: count from zero.
	c := trafficSnap(t0.Add(60*time.Second), 6, 0, Bucket{0.1, 6}, Bucket{1, 6}, Bucket{math.MaxFloat64, 6})
	applyRates(c, b)
	r = c.Servers[0].Traffic.Routes[0]
	if *r.PerMinute != 12 || *r.ErrorPercent != 0 || len(r.History) != 2 {
		t.Errorf("after reset: %+v %v", r, *r.PerMinute)
	}
	// A failed read keeps the history and has no rate.
	d := trafficSnap(t0.Add(90*time.Second), 0, 0)
	d.Servers[0].Traffic.Error = "proxy metrics: down"
	applyRates(d, c)
	if r := d.Servers[0].Traffic.Routes[0]; r.PerMinute != nil || len(r.History) != 2 {
		t.Errorf("failed read: %+v", r)
	}
	// History is bounded.
	prev := c
	for i := range HistoryLen + 5 {
		n := trafficSnap(t0.Add(time.Duration(120+i*5)*time.Second), 6, 0)
		applyRates(n, prev)
		prev = n
	}
	if got := len(prev.Servers[0].Traffic.Routes[0].History); got != HistoryLen {
		t.Errorf("history len = %d", got)
	}
}

func TestMetricsScript(t *testing.T) {
	if MetricsScript(nil) != "" {
		t.Error("no routes, no fetch")
	}
	s := MetricsScript([]string{"w4a-production-web", "it's"})
	for _, want := range []string{
		markMetrics,
		"'http://127.0.0.1:9188/metrics'",
		`-e 'service="w4a-production-web"'`,
		`-e 'service="it'\''s"'`,
		"curl -fsS --max-time 3",
		"wget -q -T 3 -O -",
		"grep '^kamal_proxy_http_'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	full := script("yoho-w4a-production", "/var/lib/yoho", s)
	if !strings.HasPrefix(full, "set -eu\nLC_ALL=C; export LC_ALL\necho "+markMetrics) {
		t.Errorf("metrics must run first:\n%s", full[:120])
	}
	if out, err := exec.Command("sh", "-n", "-c", full).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v %s", err, out)
	}
	if strings.Contains(Script("p", "/r"), markMetrics) {
		t.Error("Script without metrics must not fetch them")
	}
	p := Parse(full[:0] + markHost + "\nhostname=h\n" + markMetrics + "\n" + metricsFixture)
	if p.Host.Hostname != "h" || len(p.Metrics) < 5 {
		t.Errorf("parse = %+v", p)
	}
}
