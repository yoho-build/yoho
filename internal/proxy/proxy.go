// Package proxy manages the shared kamal-proxy on a Server: one container
// `yoho-proxy` on the external docker network `yoho` that every proxied
// Service joins. Routes are switched with `kamal-proxy deploy`, which
// health-gates the new target and drains the old one (ADR 0004).
package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/remote"
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
	if out == nil {
		out = io.Discard
	}
	if err := EnsureNetwork(ctx, host); err != nil {
		return err
	}
	args := runArgs(cfg)
	want := configHash(args)
	image := args[len(args)-3]

	// Prints "<label>|<running>" or nothing when the container is missing.
	state, err := host.Output(ctx, remote.Cmd{Script: "docker container inspect -f '{{index .Config.Labels \"" + configLabel + "\"}}|{{.State.Running}}' " + ContainerName + " 2>/dev/null || true"})
	if err != nil {
		return fmt.Errorf("inspect proxy: %w", err)
	}
	label, running, exists := strings.Cut(strings.TrimSpace(state), "|")
	if exists && label == want {
		if running != "true" {
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
