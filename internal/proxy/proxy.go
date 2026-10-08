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
)

// runArgs returns the `docker run` arguments (after `docker run -d`) for cfg.
func runArgs(cfg config.ProxyConfig) []string {
	image := cfg.Image
	if image == "" {
		image = DefaultImage
	}
	args := []string{
		"--name", ContainerName,
		"--network", Network,
		"--restart", "unless-stopped",
		"--volume", Volume + ":" + stateDir,
		"--log-opt", "max-size=10m",
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

// EnsureNetwork creates the shared `yoho` network if missing.
func EnsureNetwork(ctx context.Context, host remote.Host) error {
	script := "set -eu\ndocker network inspect " + Network + " >/dev/null 2>&1 || docker network create " + Network + " >/dev/null"
	if err := host.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("ensure docker network %s: %w", Network, err)
	}
	return nil
}

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
	args := runArgs(cfg)
	want := configHash(args)
	image := args[len(args)-3]

	label, running, exists, err := inspectProxy(ctx, host)
	if err != nil {
		return err
	}
	if exists && label == want {
		if !running {
			fmt.Fprintf(out, "[%s] starting proxy %s\n", host.Name(), ContainerName)
			if err := host.Run(ctx, remote.Cmd{Script: "docker start " + ContainerName + " >/dev/null"}); err != nil {
				return fmt.Errorf("start proxy: %w", err)
			}
		}
		return nil
	}

	var b strings.Builder
	b.WriteString("set -eu\n")
	// Pull first so an existing Proxy is only down for the swap itself.
	b.WriteString("docker image inspect " + remote.Quote(image) + " >/dev/null 2>&1 || docker pull -q " + remote.Quote(image) + " >/dev/null\n")
	if exists {
		fmt.Fprintf(out, "[%s] warning: proxy configuration changed, recreating %s (brief proxy downtime)\n", host.Name(), ContainerName)
		b.WriteString("docker rm -f " + ContainerName + " >/dev/null\n")
	} else {
		fmt.Fprintf(out, "[%s] booting proxy %s (%s)\n", host.Name(), ContainerName, image)
	}
	b.WriteString("docker run -d --label " + remote.Quote(configLabel+"="+want) + " " + remote.QuoteArgs(args...) + " >/dev/null\n")
	if err := host.Run(ctx, remote.Cmd{Script: b.String()}); err != nil {
		return fmt.Errorf("boot proxy: %w", err)
	}
	return nil
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
	switch {
	case !exists:
		return true, "proxy container " + ContainerName + " is missing", nil
	case label != configHash(runArgs(cfg)):
		return true, "proxy configuration changed (recreated, brief proxy downtime)", nil
	case !running:
		return true, "proxy container is stopped", nil
	}
	return false, "", nil
}

// Route is one kamal-proxy route.
type Route struct {
	Service string   `json:"service"`
	Hosts   []string `json:"hosts,omitempty"`
	Target  string   `json:"target"` // comma-separated host:port list
	State   string   `json:"state"`
	TLS     bool     `json:"tls,omitempty"`
}

// ParseRoutes parses `kamal-proxy list --json`.
func ParseRoutes(out string) ([]Route, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var m map[string]struct {
		Hosts   []string `json:"hosts"`
		Targets []string `json:"targets"`
		State   string   `json:"state"`
		TLS     bool     `json:"tls"`
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
		routes = append(routes, Route{Service: n, Hosts: r.Hosts, Target: strings.Join(r.Targets, ","), State: r.State, TLS: r.TLS})
	}
	return routes, nil
}

// List returns the Proxy's routes, sorted by Service. Read-only. An error
// means the Proxy container is missing or not running.
func List(ctx context.Context, host remote.Host) ([]Route, error) {
	out, err := host.Output(ctx, remote.Cmd{Script: "docker exec " + ContainerName + " kamal-proxy list --json"})
	if err != nil {
		return nil, fmt.Errorf("list proxy routes: %w", err)
	}
	return ParseRoutes(out)
}

// DeployOptions configure one route.
type DeployOptions struct {
	// Hostnames routed to the target. Empty: catch-all.
	Hosts      []string
	HealthPath string
	TLS        bool
	// Zero: kamal-proxy defaults.
	DeployTimeout time.Duration
	DrainTimeout  time.Duration
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
	if opts.TLS {
		argv = append(argv, "--tls")
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
	if opts.TLS && len(opts.Hosts) == 0 {
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
