package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// Image is the kamal-proxy image cfg selects: proxy.image or the pinned
// DefaultImage.
func Image(cfg config.ProxyConfig) string {
	if cfg.Image != "" {
		return cfg.Image
	}
	return DefaultImage
}

// imageTag returns the tag of an image reference ("" without one; digests
// are not tags).
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	// The tag is after the last ":" that follows the last "/" (a registry
	// port such as host:5000/x has its ":" before the last "/").
	slash := strings.LastIndex(image, "/")
	if i := strings.LastIndex(image, ":"); i > slash {
		return image[i+1:]
	}
	return ""
}

// VersionLabel is the short form for output: the tag (v0.10.2), or the whole
// reference when it has none.
func VersionLabel(image string) string {
	if t := imageTag(image); t != "" {
		return t
	}
	if image == "" {
		return "unknown"
	}
	return image
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?(?:-([0-9A-Za-z.-]+))?$`)

type semver struct {
	n   [3]int
	pre string
}

func parseVersion(tag string) (semver, bool) {
	m := versionRE.FindStringSubmatch(tag)
	if m == nil {
		return semver{}, false
	}
	var v semver
	for i := 0; i < 3; i++ {
		if m[i+1] != "" {
			n, err := strconv.Atoi(m[i+1])
			if err != nil {
				return semver{}, false
			}
			v.n[i] = n
		}
	}
	v.pre = m[4]
	return v, true
}

// CompareVersions compares two kamal-proxy version tags such as v0.10.2 or
// v0.10.0-rc1: negative when a is older. A pre-release is older than its
// release. ok is false when either is not a version (latest, a digest).
func CompareVersions(a, b string) (cmp int, ok bool) {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range va.n {
		if va.n[i] != vb.n[i] {
			if va.n[i] < vb.n[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case va.pre == vb.pre:
		return 0, true
	case va.pre == "":
		return 1, true
	case vb.pre == "":
		return -1, true
	}
	return strings.Compare(va.pre, vb.pre), true
}

// CompareImages compares the tags of two image references. Unordered images
// (a moving tag such as latest, or another repository's scheme) compare as 0.
func CompareImages(a, b string) int {
	c, _ := CompareVersions(imageTag(a), imageTag(b))
	return c
}

// StatusInfo is what `proxy status` reports about the running Proxy.
type StatusInfo struct {
	Exists, Running bool
	// Image is the running container's image; Want the configured/pinned one.
	Image, Want string
}

// UpgradeAvailable reports that Want differs from the running image.
func (s StatusInfo) UpgradeAvailable() bool {
	return s.Exists && s.Image != "" && s.Image != s.Want
}

// Summary is the one-line version report: no network call, only the
// running container's image against the configured/pinned one.
func (s StatusInfo) Summary() string {
	switch {
	case !s.Exists:
		return "proxy: not running (wanted " + VersionLabel(s.Want) + ")"
	case s.Image == "":
		return "proxy version: unknown"
	case s.Image == s.Want:
		return "proxy version: " + VersionLabel(s.Image) + " (up to date)"
	case CompareImages(s.Image, s.Want) > 0:
		return "proxy version: " + VersionLabel(s.Image) + "; configured " + VersionLabel(s.Want) + " is older (`yoho proxy upgrade --allow-downgrade`)"
	}
	return "proxy version: " + VersionLabel(s.Image) + "; upgrade available: " + VersionLabel(s.Want) + " (`yoho proxy upgrade`)"
}

// Status inspects the Proxy container against cfg. Read-only.
func Status(ctx context.Context, host remote.Host, cfg config.ProxyConfig) (StatusInfo, error) {
	_, running, image, exists, err := inspectProxyImage(ctx, host)
	if err != nil {
		return StatusInfo{}, err
	}
	return StatusInfo{Exists: exists, Running: running, Image: image, Want: Image(cfg)}, nil
}

// UpgradeOptions tunes Upgrade.
type UpgradeOptions struct {
	// AllowDowngrade lets the upgrade move to an older kamal-proxy version.
	AllowDowngrade bool
	// SkipNetwork as in BootOptions (Swarm).
	SkipNetwork bool
	// OnReboot runs the pre/post-proxy-reboot Hooks. The post Hook runs only
	// after every route was verified.
	OnReboot func(ctx context.Context, hook string) error
	// HealthPaths maps a route's Service name to the path to probe through
	// the Proxy. Routes without an entry are probed at /up (the Yoho default).
	HealthPaths map[string]string
	// Timeout bounds the wait for the routes to answer again (default 45s),
	// Interval is the poll period (default 1s).
	Timeout, Interval time.Duration
}

// UpgradeResult reports one Server's upgrade.
type UpgradeResult struct {
	From, To string
	// Changed is false when the Proxy already ran the wanted image.
	Changed bool
	Routes  int
	// Blind: the old Proxy could not list routes as JSON (before v0.10.0), so
	// they were not compared; only a working `list` afterwards was required.
	Blind bool
	// Swap is how long recreating the container took (pull excluded); the
	// interruption seen by clients is within it.
	Swap time.Duration
	// Unverified lists routes that answered nothing before the upgrade either,
	// so their survival could not be established.
	Unverified []string
}

// Upgrade recreates the shared Proxy with cfg's image, keeping ports, flags,
// the `yoho` network and the state volume (routes of every App), then checks
// that every route is still listed and still answers. The image is pulled
// first: when the pull fails nothing was stopped. Unlike Boot it is explicit
// about the version change and refuses a downgrade unless allowed.
func Upgrade(ctx context.Context, host remote.Host, cfg config.ProxyConfig, out io.Writer, opts UpgradeOptions) (*UpgradeResult, error) {
	if out == nil {
		out = io.Discard
	}
	name := host.Name()
	_, running, current, exists, err := inspectProxyImage(ctx, host)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("there is no " + ContainerName + " on " + name + " to upgrade; run `yoho proxy boot`")
	}
	want := Image(cfg)
	res := &UpgradeResult{From: current, To: want}
	if CompareImages(current, want) > 0 && !opts.AllowDowngrade {
		return nil, fmt.Errorf("%s runs %s, newer than the configured %s; pass --allow-downgrade to go back (older versions may not read the newer state file)", name, VersionLabel(current), VersionLabel(want))
	}
	need, why, err := NeedsBoot(ctx, host, cfg)
	if err != nil {
		return nil, err
	}
	if !need {
		fmt.Fprintf(out, "[%s] %s already runs %s\n", name, ContainerName, VersionLabel(current))
		return res, nil
	}
	if current == want {
		fmt.Fprintf(out, "[%s] %s: %s\n", name, ContainerName, why)
	}
	res.Changed = true

	before, err := List(ctx, host)
	blind := false
	if err != nil && running {
		// kamal-proxy before v0.10.0 has no `list --json`: the routes cannot
		// be compared, only seen to load again.
		if c, ok := CompareVersions(imageTag(current), "v0.10.0"); !ok || c >= 0 {
			return nil, fmt.Errorf("read routes before the upgrade (nothing was changed): %w", err)
		}
		blind, before = true, nil
		res.Blind = true
		fmt.Fprintf(out, "[%s] warning: %s has no `list --json`; routes cannot be compared before and after the upgrade\n", name, VersionLabel(current))
	}
	baseline, err := probeRoutes(ctx, host, cfg, before, opts.HealthPaths)
	if err != nil {
		return nil, err
	}
	res.Routes = len(before)

	// Pull before touching the running Proxy.
	fmt.Fprintf(out, "[%s] pulling %s\n", name, want)
	pull := "set -eu\ndocker image inspect " + remote.Quote(want) + " >/dev/null 2>&1 || docker pull -q " + remote.Quote(want) + " >/dev/null\n"
	if err := host.Run(ctx, remote.Cmd{Script: pull}); err != nil {
		return nil, fmt.Errorf("pull %s (the running Proxy was not touched): %w", want, err)
	}

	if opts.OnReboot != nil {
		if err := opts.OnReboot(ctx, "pre-proxy-reboot"); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(out, "[%s] upgrading %s %s -> %s (brief proxy downtime; %d route(s) are kept in %s)\n", name, ContainerName, VersionLabel(current), VersionLabel(want), len(before), Volume)
	start := time.Now()
	if err := BootWith(ctx, host, cfg, out, BootOptions{SkipNetwork: opts.SkipNetwork}); err != nil {
		return nil, err
	}
	res.Swap = time.Since(start)

	unverified, err := verifyRoutes(ctx, host, cfg, before, baseline, blind, opts)
	if err != nil {
		return res, fmt.Errorf("%w; the new Proxy %s keeps running, the post-proxy-reboot Hook was not run (previous image: %s; to go back set proxy.image and run `yoho proxy upgrade --allow-downgrade`)", err, VersionLabel(want), current)
	}
	res.Unverified = unverified
	if opts.OnReboot != nil {
		if err := opts.OnReboot(ctx, "post-proxy-reboot"); err != nil {
			return res, err
		}
	}
	return res, nil
}

// verifyRoutes waits until the routes listed before are listed again,
// unchanged, and every route that answered before answers now.
func verifyRoutes(ctx context.Context, host remote.Host, cfg config.ProxyConfig, before []Route, baseline map[string]int, blind bool, opts UpgradeOptions) (unverified []string, err error) {
	timeout, interval := opts.Timeout, opts.Interval
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(timeout)
	var problems []string
	for {
		problems = nil
		after, lerr := List(ctx, host)
		if lerr != nil {
			problems = []string{lerr.Error()}
		} else {
			if !blind {
				problems = DiffRoutes(before, after)
			}
			if len(problems) == 0 && !blind {
				var codes map[string]int
				codes, lerr = probeRoutes(ctx, host, cfg, after, opts.HealthPaths)
				if lerr != nil {
					problems = []string{lerr.Error()}
				} else {
					problems, unverified = CompareProbes(baseline, codes)
				}
			}
		}
		if len(problems) == 0 {
			return unverified, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if time.Now().Add(interval).After(deadline) {
			return nil, fmt.Errorf("proxy routes not verified after %s: %s", timeout, strings.Join(problems, "; "))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// DiffRoutes lists how after differs from before: a route missing, added,
// retargeted or changed in hosts/paths/TLS, or no longer running if it was.
// Targets are compared as sets. Nil means identical routing.
func DiffRoutes(before, after []Route) []string {
	byName := map[string]Route{}
	for _, r := range after {
		byName[r.Service] = r
	}
	var out []string
	seen := map[string]bool{}
	for _, b := range before {
		seen[b.Service] = true
		a, ok := byName[b.Service]
		switch {
		case !ok:
			out = append(out, "route "+b.Service+" is missing")
		case !sameSet(b.Hosts, a.Hosts), !sameSet(b.PathPrefixes, a.PathPrefixes), b.TLS != a.TLS:
			out = append(out, "route "+b.Service+" changed hosts, paths or TLS")
		case !sameSet(splitTargets(b.Target), splitTargets(a.Target)):
			out = append(out, "route "+b.Service+" changed targets ("+b.Target+" -> "+a.Target+")")
		case b.State == "running" && a.State != "running":
			out = append(out, "route "+b.Service+" is "+a.State+", was running")
		}
	}
	for _, a := range after {
		if !seen[a.Service] {
			out = append(out, "unexpected new route "+a.Service)
		}
	}
	return out
}

func splitTargets(t string) []string {
	if t == "" {
		return nil
	}
	return strings.Split(t, ",")
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// answered reports whether an HTTP status shows that the Proxy reached a
// target (any status below 500; 000 is a failed connection).
func answered(code int) bool { return code > 0 && code < 500 }

// CompareProbes compares probe status codes taken before and after. A route
// that answered before must answer after. A route that did not answer before
// is returned in unverified (its health could not be established), not as a
// problem.
func CompareProbes(before, after map[string]int) (problems, unverified []string) {
	names := make([]string, 0, len(before))
	for n := range before {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		switch {
		case !answered(before[n]):
			unverified = append(unverified, n)
		case !answered(after[n]):
			problems = append(problems, fmt.Sprintf("route %s answered HTTP %d before, %s now", n, before[n], codeText(after[n])))
		}
	}
	return problems, unverified
}

func codeText(c int) string {
	if c == 0 {
		return "no answer"
	}
	return "HTTP " + strconv.Itoa(c)
}

// probeRoutes requests each route's health path through the Proxy's
// published HTTP port from the Server itself, with the route's Host header.
// Without a published port (Tunnel-only) nothing is probed and the route
// list comparison alone verifies. Result: Service -> HTTP status, 0 when
// the request failed.
func probeRoutes(ctx context.Context, host remote.Host, cfg config.ProxyConfig, routes []Route, paths map[string]string) (map[string]int, error) {
	port := 80
	if cfg.HTTPPort != nil {
		port = *cfg.HTTPPort
	}
	if port == 0 || len(routes) == 0 {
		return map[string]int{}, nil
	}
	addr := "127.0.0.1"
	if b := cfg.Bind; b != "" && b != "0.0.0.0" && b != "::" {
		addr = b
		if strings.Contains(b, ":") && !strings.HasPrefix(b, "[") {
			addr = "[" + b + "]"
		}
	}
	base := "http://" + net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(port))
	out, err := host.Output(ctx, remote.Cmd{Script: ProbeScript(base, routes, paths)})
	if err != nil {
		return nil, fmt.Errorf("probe proxy routes: %w", err)
	}
	return ParseProbe(out)
}

// ProbeScript is the POSIX sh that prints "<service> <http status>" per
// route (000 when curl failed) against base, or "NOCURL".
func ProbeScript(base string, routes []Route, paths map[string]string) string {
	var b strings.Builder
	b.WriteString("set -eu\ncommand -v curl >/dev/null 2>&1 || { echo NOCURL; exit 0; }\n")
	for _, r := range routes {
		path := paths[r.Service]
		if path == "" {
			path = "/up"
		}
		prefix := "/"
		if len(r.PathPrefixes) > 0 {
			prefix = r.PathPrefixes[0]
		}
		full := strings.TrimRight(prefix, "/") + path
		args := []string{"curl", "-s", "-o", "/dev/null", "-m", "5", "-w", "%{http_code}"}
		if h := probeHost(r); h != "" {
			args = append(args, "-H", "Host: "+h)
		}
		args = append(args, base+full)
		b.WriteString("code=$(" + remote.QuoteArgs(args...) + " 2>/dev/null) || code=000\n")
		b.WriteString("printf '%s %s\\n' " + remote.Quote(r.Service) + " \"$code\"\n")
	}
	return b.String()
}

// probeHost is the Host header for a route: its first literal host
// (kamal-proxy lists a catch-all as "*"). A wildcard host is probed as a
// concrete name under it.
func probeHost(r Route) string {
	for _, h := range r.Hosts {
		switch {
		case h == "" || h == "*":
			continue
		case strings.HasPrefix(h, "*."):
			return "yoho-probe." + h[2:]
		}
		return h
	}
	return ""
}

// ParseProbe parses ProbeScript output. NOCURL yields no codes: such routes
// are verified by the route list only.
func ParseProbe(out string) (map[string]int, error) {
	codes := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "NOCURL" {
			return map[string]int{}, nil
		}
		i := strings.LastIndex(line, " ")
		if i < 0 {
			return nil, fmt.Errorf("parse proxy probe: unexpected line %q", line)
		}
		c, err := strconv.Atoi(line[i+1:])
		if err != nil {
			return nil, fmt.Errorf("parse proxy probe: unexpected line %q", line)
		}
		codes[line[:i]] = c
	}
	return codes, nil
}
