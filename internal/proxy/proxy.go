// Package proxy manages the shared kamal-proxy on a Server: one container
// `yoho-proxy` on the external docker network `yoho` that every proxied
// Service joins. Routes are switched with `kamal-proxy deploy`, which
// health-gates the new target and drains the old one (ADR 0004).
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

const (
	// DefaultImage is the pinned kamal-proxy image (latest tag on 2026-10-08).
	DefaultImage = "basecamp/kamal-proxy:v0.10.2"
	// Network is the external docker network shared by the Proxy and Apps.
	Network = "yoho"
	// ContainerName is the Proxy container on each Server.
	ContainerName = "yoho-proxy"
	// Volume holds kamal-proxy state (routes, certificates) across recreation.
	Volume = "yoho-proxy-config"

	stateDir    = "/home/kamal-proxy/.config/kamal-proxy"
	configLabel = "yoho.proxy.config"
	// LabelAttach marks a Docker network the Proxy joins besides Network: a
	// scoped App's own network (ADR 0011). A recreated Proxy rejoins them.
	LabelAttach = "yoho.proxy.attach"
)

// runArgs preserves the original json-file configuration fingerprint.
func runArgs(cfg config.ProxyConfig) []string {
	return runArgsForDriver(cfg, "json-file")
}

// runArgsForDriver respects the daemon default: no --log-driver override.
// Rotation options are supported only by json-file and local logging.
func runArgsForDriver(cfg config.ProxyConfig, driver string) []string {
	image := cfg.Image
	if image == "" {
		image = DefaultImage
	}
	args := []string{
		"--name", ContainerName,
		"--network", Network,
		"--restart", "unless-stopped",
		"--volume", Volume + ":" + stateDir,
	}
	if driver == "json-file" || driver == "local" {
		args = append(args, "--log-opt", "max-size=10m")
	}
	bind := cfg.Bind
	if strings.Contains(bind, ":") && !strings.HasPrefix(bind, "[") {
		bind = "[" + bind + "]" // IPv6
	}
	publish := func(p *int, def, inner int) {
		port := def
		if p != nil {
			port = *p
		}
		if port == 0 {
			return
		}
		spec := strconv.Itoa(port) + ":" + strconv.Itoa(inner)
		if bind != "" {
			spec = bind + ":" + spec
		}
		args = append(args, "--publish", spec)
	}
	publish(cfg.HTTPPort, 80, 80)
	publish(cfg.HTTPSPort, 443, 443)
	return append(args, image, "kamal-proxy", "run")
}

// configHash identifies the Proxy configuration; stored as a label so Boot
// can tell whether the running container matches.
func configHash(args []string) string {
	h := sha256.Sum256([]byte(strings.Join(args, "\x00")))
	return hex.EncodeToString(h[:8])
}

// desiredRunArgs keeps an already matching Proxy unchanged. A daemon logging
// change does not alter existing containers; only new/recreated ones inherit it.
func desiredRunArgs(ctx context.Context, host remote.Host, cfg config.ProxyConfig, label string) ([]string, error) {
	for _, args := range [][]string{runArgs(cfg), runArgsForDriver(cfg, "")} {
		if label == configHash(args) {
			return args, nil
		}
	}
	driver, err := host.Output(ctx, remote.Cmd{Script: "set -eu\ndocker info --format '{{.LoggingDriver}}'"})
	if err != nil {
		return nil, fmt.Errorf("inspect Docker logging driver: %w", err)
	}
	driver = strings.TrimSpace(driver)
	if driver == "" {
		return nil, errors.New("Docker returned an empty default logging driver")
	}
	return runArgsForDriver(cfg, driver), nil
}

// EnsureNetwork creates the shared `yoho` network if missing.
func EnsureNetwork(ctx context.Context, host remote.Host) error {
	script := "set -eu\ndocker network inspect " + Network + " >/dev/null 2>&1 || docker network create " + Network + " >/dev/null"
	if err := host.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("ensure docker network %s: %w", Network, err)
	}
	return nil
}

// reattachScript connects a new Proxy container to the scoped Apps' own
// networks (LabelAttach) again.
const reattachScript = "for n in $(docker network ls -q --filter label=" + LabelAttach + "=1); do docker network connect \"$n\" " + ContainerName + " >/dev/null 2>&1 || true; done\n"

// Boot ensures the network and a running Proxy matching cfg. Idempotent. When
// the configuration changed the container is recreated, which interrupts
// traffic for a moment; routes survive in the state volume.
func Boot(ctx context.Context, host remote.Host, cfg config.ProxyConfig, out io.Writer) error {
	return BootWith(ctx, host, cfg, out, BootOptions{})
}

// BootOptions tunes BootWith.
type BootOptions struct {
	// SkipNetwork leaves the `yoho` network alone. Swarm sets it: the network
	// is a cluster-wide attachable overlay created on a manager, and a worker
	// cannot see it until something attaches, so creating it there would make
	// a node-local bridge that shadows the overlay.
	SkipNetwork bool
	// Owner (TunnelOwner) labels Cloudflare Tunnel connectors and stops one
	// App from changing or removing connectors another App manages.
	Owner string
}

// BootWith is Boot with options.
func BootWith(ctx context.Context, host remote.Host, cfg config.ProxyConfig, out io.Writer, opts BootOptions) error {
	if out == nil {
		out = io.Discard
	}
	if !opts.SkipNetwork {
		if err := EnsureNetwork(ctx, host); err != nil {
			return err
		}
	}
	label, running, exists, err := inspectProxy(ctx, host)
	if err != nil {
		return err
	}
	args, err := desiredRunArgs(ctx, host, cfg, label)
	if err != nil {
		return err
	}
	want := configHash(args)
	image := args[len(args)-3]
	if exists && label == want {
		if !running {
			fmt.Fprintf(out, "[%s] starting proxy %s\n", host.Name(), ContainerName)
			if err := host.Run(ctx, remote.Cmd{Script: "docker start " + ContainerName + " >/dev/null"}); err != nil {
				return fmt.Errorf("start proxy: %w", err)
			}
		}
		return nil
	}

	if exists {
		fmt.Fprintf(out, "[%s] warning: proxy configuration changed, recreating %s (brief proxy downtime)\n", host.Name(), ContainerName)
	} else {
		fmt.Fprintf(out, "[%s] booting proxy %s (%s)\n", host.Name(), ContainerName, image)
	}
	run := "docker run -d --label " + remote.Quote(configLabel+"="+want) + " " + remote.QuoteArgs(args...) + " >/dev/null"
	var b strings.Builder
	b.WriteString("set -eu\n")
	// Pull first so an existing Proxy is only down for the swap itself.
	b.WriteString("docker image inspect " + remote.Quote(image) + " >/dev/null 2>&1 || docker pull -q " + remote.Quote(image) + " >/dev/null\n")
	b.WriteString(proxyLockScript(release.Root + "/proxy"))
	// Everything below runs under the Server-wide lock and re-checks the
	// current state: another App may have recreated the Proxy since we looked.
	old := ContainerName + "-old"
	// A crashed swap can leave only the fallback: bring it back first.
	b.WriteString("if ! docker container inspect " + ContainerName + " >/dev/null 2>&1 && docker container inspect " + old + " >/dev/null 2>&1; then\n")
	b.WriteString("  docker rename " + old + " " + ContainerName + "\n")
	b.WriteString("  docker start " + ContainerName + " >/dev/null 2>&1 || true\n")
	b.WriteString("fi\n")
	b.WriteString("if docker container inspect " + ContainerName + " >/dev/null 2>&1; then\n")
	b.WriteString("  cur=$(docker container inspect -f '{{index .Config.Labels \"" + configLabel + "\"}}' " + ContainerName + " 2>/dev/null || true)\n")
	b.WriteString("  if [ \"$cur\" = " + remote.Quote(want) + " ]; then\n")
	b.WriteString("    docker start " + ContainerName + " >/dev/null 2>&1 || true\n")
	b.WriteString("    exit 0\n")
	b.WriteString("  fi\n")
	// Keep the old container (stopped, renamed) until the new one runs,
	// so a failed start (port taken, bad option) restores the old Proxy.
	// Routes live in the state volume, which both containers share. The
	// previous fallback is only removed here, where yoho-proxy exists and is
	// the one being replaced.
	b.WriteString("  docker rm -f " + old + " >/dev/null 2>&1 || true\n")
	b.WriteString("  docker stop " + ContainerName + " >/dev/null 2>&1 || true\n")
	b.WriteString("  docker rename " + ContainerName + " " + old + "\n")
	b.WriteString("  if " + run + "; then\n")
	b.WriteString("    docker rm -f " + old + " >/dev/null\n")
	b.WriteString("  else\n")
	b.WriteString("    docker rm -f " + ContainerName + " >/dev/null 2>&1 || true\n")
	b.WriteString("    docker rename " + old + " " + ContainerName + "\n")
	b.WriteString("    docker start " + ContainerName + " >/dev/null\n")
	b.WriteString("    echo 'new proxy failed to start; the previous proxy was restored' >&2\n")
	b.WriteString("    exit 1\n")
	b.WriteString("  fi\n")
	b.WriteString("else\n")
	b.WriteString("  " + run + "\n")
	b.WriteString("fi\n")
	if !opts.SkipNetwork {
		b.WriteString(reattachScript)
	}
	if err := host.Run(ctx, remote.Cmd{Script: b.String()}); err != nil {
		return fmt.Errorf("boot proxy: %w", err)
	}
	return nil
}

// proxyLockScript returns POSIX sh that takes a Server-wide lock named by base
// and holds it until the script exits. With flock(1) it locks the file
// base.flock on fd 9; the kernel drops the lock on exit, so there is no stale
// state. Without flock it falls back to an atomic mkdir of base.lock.d that is
// never reclaimed automatically (reclaiming races); the operator removes it.
// Either way it waits up to two minutes.
func proxyLockScript(base string) string {
	file := remote.Quote(base + ".flock")
	dir := remote.Quote(base + ".lock.d")
	return "lockf=" + file + "\n" +
		"lockd=" + dir + "\n" +
		"mkdir -p \"$(dirname \"$lockf\")\"\n" +
		"if type flock >/dev/null 2>&1; then\n" +
		"  exec 9>\"$lockf\"\n" +
		"  flock -w 120 9 || { echo \"proxy lock $lockf is held by another yoho run\" >&2; exit 1; }\n" +
		"else\n" +
		"  waited=0\n" +
		"  until mkdir \"$lockd\" 2>/dev/null; do\n" +
		"    waited=$((waited + 1))\n" +
		"    if [ \"$waited\" -ge 120 ]; then echo \"proxy lock $lockd is held; if no yoho run is active, remove it with: rmdir $lockd\" >&2; exit 1; fi\n" +
		"    sleep 1\n" +
		"  done\n" +
		"  trap 'rmdir \"$lockd\"' EXIT\n" +
		"fi\n"
}

// inspectProxy reads the Proxy container's config label and running state.
func inspectProxy(ctx context.Context, host remote.Host) (label string, running, exists bool, err error) {
	// Prints "<label>|<running>" or nothing when the container is missing.
	state, err := host.Output(ctx, remote.Cmd{Script: "docker container inspect -f '{{index .Config.Labels \"" + configLabel + "\"}}|{{.State.Running}}' " + ContainerName + " 2>/dev/null || true"})
	if err != nil {
		return "", false, false, fmt.Errorf("inspect proxy: %w", err)
	}
	label, r, exists := strings.Cut(strings.TrimSpace(state), "|")
	return label, r == "true", exists, nil
}

// NeedsBoot reports, read-only, whether Boot would create, recreate or start
// the Proxy for cfg, and why.
func NeedsBoot(ctx context.Context, host remote.Host, cfg config.ProxyConfig) (bool, string, error) {
	label, running, exists, err := inspectProxy(ctx, host)
	if err != nil {
		return false, "", err
	}
	args, err := desiredRunArgs(ctx, host, cfg, label)
	if err != nil {
		return false, "", err
	}
	switch {
	case !exists:
		return true, "proxy container " + ContainerName + " is missing", nil
	case label != configHash(args):
		return true, "proxy configuration changed (recreated, brief proxy downtime)", nil
	case !running:
		return true, "proxy container is stopped", nil
	}
	return false, "", nil
}

// Route is one kamal-proxy route.
type Route struct {
	Service      string   `json:"service"`
	Hosts        []string `json:"hosts,omitempty"`
	PathPrefixes []string `json:"path_prefixes,omitempty"`
	Target       string   `json:"target"` // comma-separated host:port list
	State        string   `json:"state"`
	TLS          bool     `json:"tls,omitempty"`
}

// ParseRoutes parses `kamal-proxy list --json`.
func ParseRoutes(out string) ([]Route, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var m map[string]struct {
		Hosts        []string `json:"hosts"`
		PathPrefixes []string `json:"path_prefixes"`
		Targets      []string `json:"targets"`
		State        string   `json:"state"`
		TLS          bool     `json:"tls"`
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, fmt.Errorf("parse kamal-proxy list: %w", err)
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	routes := make([]Route, 0, len(names))
	for _, n := range names {
		r := m[n]
		routes = append(routes, Route{Service: n, Hosts: r.Hosts, PathPrefixes: r.PathPrefixes, Target: strings.Join(r.Targets, ","), State: r.State, TLS: r.TLS})
	}
	return routes, nil
}

// List returns the Proxy's routes, sorted by Service. Read-only. A Server
// that never ran the Proxy (no container) has no routes: nil, nil. An error
// means the Proxy exists but could not be queried (for example not running).
func List(ctx context.Context, host remote.Host) ([]Route, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Listing containers distinguishes an absent Proxy from an unavailable
	// Docker daemon/API. Never turn an arbitrary inspect failure into no routes.
	script := "set -eu\nnames=$(docker container ls -a --filter 'name=^/" + ContainerName + "$' --format '{{.Names}}')\n" +
		"if [ -z \"$names\" ]; then printf 'null\\n'; else docker exec " + ContainerName + " kamal-proxy list --json; fi"
	out, err := host.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil, fmt.Errorf("list proxy routes: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, errors.New("list proxy routes: empty response; route reservations could not be verified")
	}
	return ParseRoutes(out)
}

// DeployOptions configure one route.
type DeployOptions struct {
	// Hostnames routed to the target. Empty: catch-all.
	Hosts      []string
	HealthPath string
	// Zero: probe the traffic target port / use the pinned Proxy probe timeout.
	HealthPort    int
	HealthTimeout time.Duration
	TLS           bool
	// TLSOnDemandURL is kamal-proxy --tls-on-demand-url: a path on the
	// Service or an absolute http(s) URL. Requires TLS and forbids Hosts.
	TLSOnDemandURL string
	// Zero: kamal-proxy defaults.
	DeployTimeout time.Duration
	DrainTimeout  time.Duration
}

// ServiceDeployOptions maps retained per-Service settings for both runtimes.
// The caller applies existing path/deploy/drain defaults; omitted HTTP probe
// settings remain zero so kamal-proxy supplies its own defaults.
func ServiceDeployOptions(p config.ServiceProxy) (DeployOptions, error) {
	if err := p.Validate(); err != nil {
		return DeployOptions{}, err
	}
	o := DeployOptions{
		Hosts: p.Hosts, HealthPath: p.HealthPath, TLS: p.TLS,
		TLSOnDemandURL: p.CustomDomainsAllow(),
		DeployTimeout:  time.Duration(p.DeployTimeout) * time.Second,
		DrainTimeout:   time.Duration(p.DrainTimeout) * time.Second,
	}
	if p.HealthPort != nil {
		o.HealthPort = *p.HealthPort
	}
	if p.HealthTimeout != nil {
		// ValidateHealthCheck has already parsed and checked the duration.
		o.HealthTimeout, _ = time.ParseDuration(*p.HealthTimeout)
	}
	return o, nil
}

// DeployArgs is the kamal-proxy argv for Deploy (exported for tests and
// dry-run output). target is host:port; several may be comma-separated
// (kamal-proxy --target is a list flag).
func DeployArgs(service, target string, opts DeployOptions) []string {
	argv := []string{"docker", "exec", ContainerName, "kamal-proxy", "deploy", service, "--target", target}
	for _, h := range opts.Hosts {
		argv = append(argv, "--host", h)
	}
	if opts.HealthPath != "" {
		argv = append(argv, "--health-check-path", opts.HealthPath)
	}
	if opts.HealthPort > 0 {
		argv = append(argv, "--health-check-port", strconv.Itoa(opts.HealthPort))
	}
	if opts.HealthTimeout > 0 {
		argv = append(argv, "--health-check-timeout", opts.HealthTimeout.String())
	}
	if opts.TLS {
		argv = append(argv, "--tls")
	}
	if opts.TLSOnDemandURL != "" {
		argv = append(argv, "--tls-on-demand-url", opts.TLSOnDemandURL)
	}
	if opts.DeployTimeout > 0 {
		argv = append(argv, "--deploy-timeout", opts.DeployTimeout.String())
	}
	if opts.DrainTimeout > 0 {
		argv = append(argv, "--drain-timeout", opts.DrainTimeout.String())
	}
	return argv
}

// Deploy routes service to target. kamal-proxy waits until the target is
// healthy, switches traffic, and drains the previous target before
// returning; on failure the previous target keeps serving.
func Deploy(ctx context.Context, host remote.Host, service, target string, opts DeployOptions) error {
	if service == "" || target == "" {
		return errors.New("proxy deploy: service and target are required")
	}
	if opts.HealthPort < 0 || opts.HealthPort > 65535 || opts.HealthTimeout < 0 {
		return fmt.Errorf("proxy deploy %s: invalid HTTP probe port or timeout", service)
	}
	if opts.TLSOnDemandURL != "" {
		if !opts.TLS {
			return fmt.Errorf("proxy deploy %s: tls-on-demand-url requires tls", service)
		}
		if len(opts.Hosts) > 0 {
			return fmt.Errorf("proxy deploy %s: tls-on-demand-url cannot be set with hosts", service)
		}
	} else if opts.TLS && len(opts.Hosts) == 0 {
		return fmt.Errorf("proxy deploy %s: tls requires at least one host", service)
	}
	if err := host.Run(ctx, remote.Cmd{Script: remote.QuoteArgs(DeployArgs(service, target, opts)...)}); err != nil {
		return fmt.Errorf("proxy deploy %s -> %s: %w", service, target, err)
	}
	return nil
}

// Remove deletes the route for service.
func Remove(ctx context.Context, host remote.Host, service string) error {
	if err := host.Run(ctx, remote.Cmd{Script: remote.QuoteArgs("docker", "exec", ContainerName, "kamal-proxy", "remove", service)}); err != nil {
		return fmt.Errorf("proxy remove %s: %w", service, err)
	}
	return nil
}
