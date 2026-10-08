package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

const (
	// DefaultTunnelImage is the pinned cloudflared image (latest release on 2026-10-08).
	DefaultTunnelImage = "cloudflare/cloudflared:2026.10.0"
	// TunnelContainer is the connector container name (replicas: yoho-tunnel-1..N).
	TunnelContainer = "yoho-tunnel"
	// TunnelOrigin is where cloudflared forwards: the Proxy on the yoho network.
	TunnelOrigin = "http://" + ContainerName + ":80"

	tunnelConfigLabel = "yoho.tunnel.config"
	tunnelModeLabel   = "yoho.tunnel.mode"
	tunnelOwnerLabel  = "yoho.tunnel.owner"
	tunnelModeQuick   = "quick"
	tunnelModeToken   = "token"
)

// Tunnel timing; variables so tests don't sleep.
var (
	tunnelPollInterval = time.Second
	tunnelQuickWait    = 30 * time.Second
	tunnelRegisterWait = 60 * time.Second
)

var (
	quickURLRe  = regexp.MustCompile(`https://([a-z0-9-]+)\.trycloudflare\.com`)
	connIndexRe = regexp.MustCompile(`connIndex=(\d+)`)
)

const registeredMarker = "Registered tunnel connection"

// ParseQuickURL returns the Quick Tunnel URL from cloudflared logs, or "".
// api.trycloudflare.com appears in error messages and is not the tunnel.
func ParseQuickURL(logs string) string {
	for _, m := range quickURLRe.FindAllStringSubmatch(logs, -1) {
		if m[1] != "api" {
			return m[0]
		}
	}
	return ""
}

// parseConnections counts distinct registered connection indexes in logs.
func parseConnections(logs string) int {
	seen := map[string]bool{}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, registeredMarker) {
			continue
		}
		if m := connIndexRe.FindStringSubmatch(line); m != nil {
			seen[m[1]] = true
		} else {
			seen[line] = true
		}
	}
	return len(seen)
}

// TunnelContainers lists container names for the replica count.
func TunnelContainers(replicas int) []string {
	if replicas <= 1 {
		return []string{TunnelContainer}
	}
	names := make([]string, replicas)
	for i := range names {
		names[i] = TunnelContainer + "-" + strconv.Itoa(i+1)
	}
	return names
}

// TokenEnvPath is the 0600 env file holding TUNNEL_TOKEN on the Server.
func TokenEnvPath() string { return path.Join(release.Root, "tunnel", "token.env") }

func tunnelImage(cfg config.TunnelConfig) string {
	if cfg.Image != "" {
		return cfg.Image
	}
	return DefaultTunnelImage
}

// tunnelRunArgs is the `docker run -d` argument list for one connector. The
// token never appears here: it is read by docker from the env file.
func tunnelRunArgs(name, image, hash, owner string, quick bool) []string {
	mode := tunnelModeToken
	if quick {
		mode = tunnelModeQuick
	}
	args := []string{
		"--name", name,
		"--network", Network,
		"--restart", "unless-stopped",
		"--log-opt", "max-size=10m",
		"--label", tunnelConfigLabel + "=" + hash,
		"--label", tunnelModeLabel + "=" + mode,
	}
	if owner != "" {
		args = append(args, "--label", tunnelOwnerLabel+"="+owner)
	}
	if quick {
		return append(args, image, "tunnel", "--no-autoupdate", "--url", TunnelOrigin)
	}
	args = append(args, "--env-file", TokenEnvPath())
	return append(args, image, "tunnel", "--no-autoupdate", "run")
}

// TunnelOwner identifies the App Destination that manages the connector, so
// another App deployed to the same Server never removes or replaces it.
func TunnelOwner(app, dest string) string { return app + "/" + dest }

// TunnelConfigHash is the label value that identifies image and token. It is
// a truncated one-way hash, safe to show in a plan and to store on the Server.
func TunnelConfigHash(cfg config.TunnelConfig, token string) string {
	return tunnelHash(tunnelImage(cfg), token)
}

// tunnelHash identifies image, mode and token so changes trigger a replace.
func tunnelHash(image, token string) string {
	h := sha256.Sum256([]byte(image + "\x00" + token))
	return hex.EncodeToString(h[:8])
}

func runScript(image string, args []string) string {
	return "docker image inspect " + remote.Quote(image) + " >/dev/null 2>&1 || docker pull -q " + remote.Quote(image) + " >/dev/null\n" +
		"docker run -d " + remote.QuoteArgs(args...) + " >/dev/null\n"
}

// TunnelContainerStatus is one connector.
type TunnelContainerStatus struct {
	Name        string
	Image       string
	ConfigHash  string // yoho.tunnel.config label: hash of image and token
	Owner       string // yoho.tunnel.owner label; empty on connectors from older yoho
	State       string // docker status text, e.g. "Up 3 minutes"
	Running     bool
	Connections int // distinct registered edge connections
}

// TunnelStatus describes the managed cloudflared connector on a Server.
type TunnelStatus struct {
	Host       string
	Owner      string // App Destination that manages the connector(s); "" when unknown or mixed
	Mode       string // "quick", "token" or "" when not deployed
	URL        string // Quick Tunnel URL
	Containers []TunnelContainerStatus
}

// Exists reports whether any connector container exists.
func (s *TunnelStatus) Exists() bool { return s != nil && len(s.Containers) > 0 }

// OwnedBy reports whether every connector carries the owner label.
func (s *TunnelStatus) OwnedBy(owner string) bool {
	return s.Exists() && owner != "" && s.Owner == owner
}

// Healthy reports whether every connector is running and registered.
func (s *TunnelStatus) Healthy() bool {
	if !s.Exists() {
		return false
	}
	for _, c := range s.Containers {
		if !c.Running || c.Connections == 0 {
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// containerLogs returns "<running>\n<logs>"-split results.
func containerLogs(ctx context.Context, host remote.Host, name string) (running bool, logs string, err error) {
	out, err := host.Output(ctx, remote.Cmd{Script: "docker inspect -f '{{.State.Running}}' " + name + " 2>/dev/null || echo false; docker logs " + name + " 2>&1 || true"})
	if err != nil {
		return false, "", err
	}
	first, rest, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(first) == "true", rest, nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// waitFor polls container logs until ok(logs) is true.
func waitFor(ctx context.Context, host remote.Host, name string, limit time.Duration, what string, ok func(string) bool) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		running, logs, err := containerLogs(ctx, host, name)
		if err != nil {
			return "", err
		}
		if ok(logs) {
			return logs, nil
		}
		if !running {
			return "", fmt.Errorf("%s exited before %s; last logs:\n%s", name, what, tail(logs, 10))
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s: timed out after %s waiting for %s; last logs:\n%s", name, limit, what, tail(logs, 10))
		}
		if err := sleepCtx(ctx, tunnelPollInterval); err != nil {
			return "", err
		}
	}
}

// EnsureTunnel makes the Cloudflare Tunnel connector(s) match cfg. With a
// token the tunnel is remotely managed (public hostname -> TunnelOrigin is
// configured in the Cloudflare dashboard) and connectors are replaced one at
// a time so replicas > 1 upgrade without dropping the tunnel. Without a token
// a single Quick Tunnel is started and its URL returned in the status; the
// container is kept while unchanged so the URL stays stable. Idempotent.
func EnsureTunnel(ctx context.Context, host remote.Host, cfg config.TunnelConfig, token string, out io.Writer) (*TunnelStatus, error) {
	return EnsureTunnelWith(ctx, host, cfg, token, out, BootOptions{})
}

// EnsureTunnelWith is EnsureTunnel with options. SkipNetwork is for Swarm:
// the attachable overlay already exists cluster-wide, and creating `yoho`
// on a worker would make a conflicting node-local bridge.
func EnsureTunnelWith(ctx context.Context, host remote.Host, cfg config.TunnelConfig, token string, out io.Writer, opts BootOptions) (*TunnelStatus, error) {
	if out == nil {
		out = io.Discard
	}
	quick := token == ""
	if quick && cfg.TokenSecret != "" {
		// A configured managed tunnel must never degrade to a public Quick Tunnel.
		return nil, fmt.Errorf("tunnel: token secret %s resolved to an empty token", cfg.TokenSecret)
	}
	replicas := cfg.Replicas
	if replicas < 1 {
		replicas = 1
	}
	if quick && replicas > 1 {
		return nil, errors.New("tunnel: Quick Tunnels support only 1 replica; set tunnel.token_secret for more")
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("tunnel: token must be a single line")
	}
	image := tunnelImage(cfg)
	want := tunnelHash(image, token)
	if !opts.SkipNetwork {
		if err := EnsureNetwork(ctx, host); err != nil {
			return nil, err
		}
	}
	if !quick {
		if err := host.WriteFile(ctx, TokenEnvPath(), []byte("TUNNEL_TOKEN="+token+"\n"), 0o600, false); err != nil {
			return nil, fmt.Errorf("write tunnel token file: %w", err)
		}
	}

	names := TunnelContainers(replicas)
	owners, err := tunnelOwners(ctx, host)
	if err != nil {
		return nil, err
	}
	// Connectors another App Destination manages are kept unless their config
	// is exactly what we want; changing them would take that App's traffic down.
	foreign := func(name string) error {
		if o := owners[name]; o != "" && opts.Owner != "" && o != opts.Owner {
			return fmt.Errorf("tunnel: %s is managed by %s on this Server; run `yoho tunnel down` there (or here) before this App can change it", name, o)
		}
		return nil
	}
	// Check every ownership conflict (connectors to replace and stale ones to
	// drop) before creating anything, so a refusal changes nothing.
	for _, name := range names {
		state, err := host.Output(ctx, remote.Cmd{Script: "docker container inspect -f '{{index .Config.Labels \"" + tunnelConfigLabel + "\"}}|{{.State.Running}}' " + name + " 2>/dev/null || true"})
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		if label, _, exists := strings.Cut(strings.TrimSpace(state), "|"); exists && label != want {
			if err := foreign(name); err != nil {
				return nil, err
			}
		}
	}
	stale, err := staleTunnelContainers(ctx, host, names)
	if err != nil {
		return nil, err
	}
	for _, n := range stale {
		if err := foreign(n); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		state, err := host.Output(ctx, remote.Cmd{Script: "docker container inspect -f '{{index .Config.Labels \"" + tunnelConfigLabel + "\"}}|{{.State.Running}}' " + name + " 2>/dev/null || true"})
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", name, err)
		}
		label, running, exists := strings.Cut(strings.TrimSpace(state), "|")
		if exists && label == want {
			if running != "true" {
				fmt.Fprintf(out, "[%s] starting %s\n", host.Name(), name)
				if err := host.Run(ctx, remote.Cmd{Script: "docker start " + name + " >/dev/null"}); err != nil {
					return nil, fmt.Errorf("start %s: %w", name, err)
				}
			} else {
				continue
			}
		} else {
			if exists {
				if err := foreign(name); err != nil {
					return nil, err
				}
			}
			var b strings.Builder
			b.WriteString("set -eu\n")
			if exists {
				fmt.Fprintf(out, "[%s] replacing %s\n", host.Name(), name)
				// Pull before removing so the gap is only the swap.
				b.WriteString("docker image inspect " + remote.Quote(image) + " >/dev/null 2>&1 || docker pull -q " + remote.Quote(image) + " >/dev/null\n")
				b.WriteString("docker rm -f " + name + " >/dev/null\n")
			} else {
				fmt.Fprintf(out, "[%s] starting %s (%s)\n", host.Name(), name, image)
			}
			b.WriteString(runScript(image, tunnelRunArgs(name, image, want, opts.Owner, quick)))
			if err := host.Run(ctx, remote.Cmd{Script: b.String()}); err != nil {
				return nil, fmt.Errorf("start %s: %w", name, err)
			}
		}
		// One at a time: the next connector is touched only after this one is registered.
		if !quick {
			if _, err := waitFor(ctx, host, name, tunnelRegisterWait, "tunnel registration", func(l string) bool { return strings.Contains(l, registeredMarker) }); err != nil {
				return nil, err
			}
		}
	}

	// Drop connectors from a previous replica layout.
	if len(stale) > 0 {
		fmt.Fprintf(out, "[%s] removing old connectors %s\n", host.Name(), strings.Join(stale, ", "))
		if err := host.Run(ctx, remote.Cmd{Script: "docker rm -f " + strings.Join(stale, " ") + " >/dev/null"}); err != nil {
			return nil, fmt.Errorf("remove old connectors: %w", err)
		}
	}

	if quick {
		logs, err := waitFor(ctx, host, names[0], tunnelQuickWait, "the trycloudflare.com URL", func(l string) bool { return ParseQuickURL(l) != "" })
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(out, "[%s] Quick Tunnel URL: %s (changes if the container is recreated)\n", host.Name(), ParseQuickURL(logs))
	} else {
		fmt.Fprintf(out, "[%s] in the Cloudflare dashboard, point the tunnel's public hostname to %s\n", host.Name(), TunnelOrigin)
	}
	return TunnelStatusOf(ctx, host)
}

// tunnelOwners maps connector container names to their owner label.
func tunnelOwners(ctx context.Context, host remote.Host) (map[string]string, error) {
	out, err := host.Output(ctx, remote.Cmd{Script: "docker ps -a --filter 'name=^/" + TunnelContainer + "(-[0-9]+)?$' --format '{{.Names}}|{{.Label \"" + tunnelOwnerLabel + "\"}}'"})
	if err != nil {
		return nil, fmt.Errorf("list tunnel containers: %w", err)
	}
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if name, owner, ok := strings.Cut(strings.TrimSpace(l), "|"); ok && name != "" {
			m[name] = owner
		}
	}
	return m, nil
}

func staleTunnelContainers(ctx context.Context, host remote.Host, keep []string) ([]string, error) {
	all, err := tunnelContainerNames(ctx, host)
	if err != nil {
		return nil, err
	}
	k := map[string]bool{}
	for _, n := range keep {
		k[n] = true
	}
	var stale []string
	for _, n := range all {
		if !k[n] {
			stale = append(stale, n)
		}
	}
	return stale, nil
}

func tunnelContainerNames(ctx context.Context, host remote.Host) ([]string, error) {
	out, err := host.Output(ctx, remote.Cmd{Script: "docker ps -a --filter 'name=^/" + TunnelContainer + "(-[0-9]+)?$' --format '{{.Names}}'"})
	if err != nil {
		return nil, fmt.Errorf("list tunnel containers: %w", err)
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names, nil
}

// TunnelStatusOf reads connector state, registered connections and (Quick
// Tunnel) the URL from the Server. An absent tunnel yields an empty status.
func TunnelStatusOf(ctx context.Context, host remote.Host) (*TunnelStatus, error) {
	st := &TunnelStatus{Host: host.Name()}
	out, err := host.Output(ctx, remote.Cmd{Script: "docker ps -a --filter 'name=^/" + TunnelContainer + "(-[0-9]+)?$' --format '{{.Names}}|{{.Image}}|{{.Status}}|{{.Label \"" + tunnelModeLabel + "\"}}|{{.Label \"" + tunnelConfigLabel + "\"}}|{{.Label \"" + tunnelOwnerLabel + "\"}}'"})
	if err != nil {
		return nil, fmt.Errorf("tunnel status: %w", err)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(l), "|", 6)
		if len(f) < 4 {
			continue
		}
		c := TunnelContainerStatus{Name: f[0], Image: f[1], State: f[2], Running: strings.HasPrefix(f[2], "Up")}
		if len(f) == 6 {
			c.ConfigHash, c.Owner = f[4], f[5]
		}
		st.Mode = f[3]
		_, logs, err := containerLogs(ctx, host, c.Name)
		if err != nil {
			return nil, fmt.Errorf("tunnel status: %w", err)
		}
		c.Connections = parseConnections(logs)
		if st.URL == "" && st.Mode == tunnelModeQuick {
			st.URL = ParseQuickURL(logs)
		}
		st.Containers = append(st.Containers, c)
	}
	sort.Slice(st.Containers, func(i, j int) bool { return st.Containers[i].Name < st.Containers[j].Name })
	for i, c := range st.Containers {
		if i == 0 {
			st.Owner = c.Owner
		} else if c.Owner != st.Owner {
			st.Owner = ""
		}
	}
	return st, nil
}

// RemoveTunnel removes all connector containers and the token file.
func RemoveTunnel(ctx context.Context, host remote.Host, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	names, err := tunnelContainerNames(ctx, host)
	if err != nil {
		return err
	}
	script := "set -eu\n"
	if len(names) > 0 {
		fmt.Fprintf(out, "[%s] removing %s\n", host.Name(), strings.Join(names, ", "))
		script += "docker rm -f " + strings.Join(names, " ") + " >/dev/null\n"
	}
	script += "rm -f " + remote.Quote(TokenEnvPath()) + "\n"
	if err := host.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("remove tunnel: %w", err)
	}
	return nil
}

// RemoveTunnelOwned is RemoveTunnel for a deploy that no longer configures a
// tunnel: it only removes connectors labelled with owner. Connectors of
// another App Destination, or from a yoho that did not label them, stay (use
// `yoho tunnel down` to remove those). Reports whether it removed anything.
func RemoveTunnelOwned(ctx context.Context, host remote.Host, owner string, out io.Writer) (bool, error) {
	st, err := TunnelStatusOf(ctx, host)
	if err != nil {
		return false, err
	}
	if !st.OwnedBy(owner) {
		return false, nil
	}
	return true, RemoveTunnel(ctx, host, out)
}
