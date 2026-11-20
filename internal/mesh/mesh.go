package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

func statePath(h plan.NamedHost) string {
	root := h.Server.Root
	if root == "" {
		root = "/var/lib/yoho"
	}
	return path.Join(root, "mesh/state.json")
}
func Read(ctx context.Context, hosts []plan.NamedHost) ([]State, error) {
	copies := make([]State, 0, len(hosts))
	for _, h := range hosts {
		b, err := h.Host.ReadFile(ctx, statePath(h), true)
		if errors.Is(err, os.ErrNotExist) {
			copies = append(copies, State{})
			continue
		}
		if err != nil {
			return nil, err
		}
		s, err := Decode(b)
		if err != nil {
			return nil, fmt.Errorf("Server %s mesh state: %w", h.Name, err)
		}
		copies = append(copies, s)
	}
	return copies, nil
}
func names(hosts []plan.NamedHost) []string {
	ns := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ns = append(ns, h.Name)
	}
	return ns
}

// Diff reads only persisted public state. Hosts must be in sorted name order.
func Diff(ctx context.Context, n config.NetworkConfig, hosts []plan.NamedHost) ([]plan.Change, error) {
	n = Defaults(n)
	copies, err := Read(ctx, hosts)
	if err != nil {
		return nil, err
	}
	s, err := Allocate(n.Subnet, names(hosts), copies)
	if err != nil {
		return nil, err
	}
	var changes []plan.Change
	for i, h := range hosts {
		ep, e := endpoint(h.Server, n.Port)
		if e != nil {
			return nil, e
		}
		if copies[i].Network != n || copies[i].Peers[h.Name].Endpoint != ep {
			changes = append(changes, plan.Change{Kind: "mesh", Name: "yoho0", Server: h.Name, Action: plan.ActionUpdate, Reasons: []string{"mesh interface configuration changed"}})
		}
	}
	for i, h := range hosts {
		if copies[i].Addresses[h.Name] == "" {
			changes = append(changes, plan.Change{Kind: "mesh", Name: h.Name, Server: h.Name, Action: plan.ActionCreate, Reasons: []string{"assign address " + s.Addresses[h.Name]}})
		}
		for _, peer := range hosts {
			if peer.Name == h.Name {
				continue
			}
			old, ok := copies[i].Peers[peer.Name]
			action := plan.ActionCreate
			reason := "add peer"
			if ok {
				action = plan.ActionUpdate
				reason = "peer endpoint or address changed"
			}
			ep, e := endpoint(peer.Server, n.Port)
			if e != nil {
				return nil, e
			}
			if !ok || old.Endpoint != ep || old.Address != s.Addresses[peer.Name] {
				changes = append(changes, plan.Change{Kind: "mesh", Name: peer.Name, Server: h.Name, Action: action, Reasons: []string{reason}})
			}
		}
		for _, old := range keys(copies[i].Peers) {
			if _, ok := s.Addresses[old]; ok {
				found := false
				for _, p := range hosts {
					if p.Name == old {
						found = true
					}
				}
				if !found {
					changes = append(changes, plan.Change{Kind: "mesh", Name: old, Server: h.Name, Action: plan.ActionDelete, Reasons: []string{"remove peer"}})
				}
			}
		}
	}
	return changes, nil
}
func endpoint(s config.Server, port int) (string, error) {
	if s.Endpoint == "none" {
		return "none", nil
	}
	host := s.Endpoint
	if host == "" {
		_, h, _, e := remote.ParseTarget(s.SSH)
		if e != nil {
			return "", e
		}
		host = h
	}
	if _, _, e := net.SplitHostPort(host); e == nil {
		return host, nil
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

const prepare = `set -eu
if ! command -v wg >/dev/null 2>&1 || ! command -v ufw >/dev/null 2>&1 || ! command -v ping >/dev/null 2>&1; then apt-get update >&2; DEBIAN_FRONTEND=noninteractive apt-get install -y wireguard-tools iproute2 iputils-ping ufw >&2; fi
if [ ! -d /etc/wireguard ]; then install -d -m 700 /etc/wireguard; fi
for file in /etc/wireguard/yoho0.conf /etc/wireguard/yoho0.key /etc/wireguard/yoho0.key.next /etc/wireguard/yoho0.owned /etc/sysctl.d/99-yoho-mesh.conf; do
 test ! -L "$file" || { echo 'refusing mesh file symlink' >&2; exit 1; }
done
if ip link show yoho0 >/dev/null 2>&1 && [ ! -f /etc/wireguard/yoho0.owned ]; then
 echo 'yoho0 is not owned by Yoho' >&2; exit 1
fi
if [ -e /etc/wireguard/yoho0.conf ]; then
 grep -qx '# Managed by Yoho' /etc/wireguard/yoho0.conf || { echo 'yoho0 is not owned by Yoho' >&2; exit 1; }
fi
if [ -e /etc/wireguard/yoho0.key ] && [ ! -e /etc/wireguard/yoho0.conf ]; then
 test -f /etc/wireguard/yoho0.owned || { echo 'unowned yoho0 key' >&2; exit 1; }
fi
touch /etc/wireguard/yoho0.owned
umask 077
if [ ! -s /etc/wireguard/yoho0.key ] || [ "$ROTATE" = 1 ]; then
 wg genkey > /etc/wireguard/yoho0.key.next
 chmod 600 /etc/wireguard/yoho0.key.next
 mv /etc/wireguard/yoho0.key.next /etc/wireguard/yoho0.key
fi
chown root:root /etc/wireguard/yoho0.key
wg pubkey < /etc/wireguard/yoho0.key
`
const clearFirewall = `if command -v ufw >/dev/null 2>&1; then
 # show added includes configured rules even while UFW is inactive.
 rules=$(ufw show added)
 printf '%s\n' "$rules" | while IFS= read -r line; do
  case "$line" in
   *" comment 'yoho-mesh'"|*" comment yoho-mesh") ;;
   *) continue ;;
  esac
  rule=${line% comment *}
  case "$rule" in 'ufw allow '*|'ufw route allow '*|'ufw deny '*|'ufw route deny '*) ;; *) continue ;; esac
  rule=${rule#ufw }
  # UFW prints these address/interface/port rules as space-delimited tokens.
  # Disable glob expansion and pass tokens as argv; never evaluate shell text.
  set -f
  set -- $rule
  ufw --force delete "$@" >/dev/null
 done
fi
`

// Up reconciles every Server after reading all allocation tables and checking collisions.
func Up(ctx context.Context, n config.NetworkConfig, hosts []plan.NamedHost, rotate bool) error {
	n = Defaults(n)
	copies, err := Read(ctx, hosts)
	if err != nil {
		return err
	}
	s, err := Allocate(n.Subnet, names(hosts), copies)
	if err != nil {
		return err
	}
	s.Network = n
	sources := map[string][]string{}
	for _, h := range hosts {
		ep, e := endpoint(h.Server, n.Port)
		if e != nil {
			return e
		}
		s.Peers[h.Name] = Peer{Address: s.Addresses[h.Name], Endpoint: ep}
		// An endpoint:none Server still needs a resolvable public SSH address for
		// its egress firewall allowlist. No internet-wide fallback is permitted.
		public := ep
		if ep == "none" {
			_, public, _, e = remote.ParseTarget(h.Server.SSH)
			if e != nil {
				return e
			}
		} else {
			public, _, e = net.SplitHostPort(ep)
			if e != nil {
				return e
			}
		}
		ips, e := net.DefaultResolver.LookupIPAddr(ctx, public)
		if e != nil {
			return fmt.Errorf("resolve public address for Server %s: %w", h.Name, e)
		}
		for _, ip := range ips {
			sources[h.Name] = append(sources[h.Name], ip.IP.String())
		}
		routes, e := h.Host.Output(ctx, remote.Cmd{Script: "set -eu\nip -4 route show table all", Sudo: true})
		if e != nil {
			return e
		}
		pools, e := h.Host.Output(ctx, remote.Cmd{Script: `set -eu
ids=$(docker network ls -q)
if [ -n "$ids" ]; then docker network inspect --format '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' $ids; fi`, Sudo: true})
		if e != nil {
			return e
		}
		if e = Collisions(n.Subnet, routes, strings.Fields(pools)); e != nil {
			return fmt.Errorf("Server %s: %w", h.Name, e)
		}
	}
	for _, h := range hosts {
		r := "0"
		if rotate {
			r = "1"
		}
		pub, e := h.Host.Output(ctx, remote.Cmd{Script: prepare, Sudo: true, Env: map[string]string{"ROTATE": r}})
		if e != nil {
			return e
		}
		p := s.Peers[h.Name]
		p.PublicKey = strings.TrimSpace(pub)
		s.Peers[h.Name] = p
	}
	// Validate every public key and configuration before publishing any config.
	configs := map[string]string{}
	for _, h := range hosts {
		c, e := Render(h.Name, n, s)
		if e != nil {
			return e
		}
		configs[h.Name] = c
	}
	for _, h := range hosts {
		old, e := h.Host.ReadFile(ctx, "/etc/wireguard/yoho0.conf", true)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		changed := string(old) != configs[h.Name]
		if changed {
			if e = h.Host.Run(ctx, remote.Cmd{Script: "set -eu\nif ip link show yoho0 >/dev/null 2>&1; then wg-quick down yoho0; fi", Sudo: true}); e != nil {
				return e
			}
		}
		if e = h.Host.WriteFile(ctx, "/etc/wireguard/yoho0.conf", []byte(configs[h.Name]), 0600, true); e != nil {
			return e
		}
		script := "set -eu\nchown root:root /etc/wireguard/yoho0.conf\n" + clearFirewall
		for _, peer := range hosts {
			if peer.Name == h.Name {
				continue
			}
			for _, ip := range sources[peer.Name] {
				script += "ufw allow proto udp from " + remote.Quote(ip) + " to any port " + strconv.Itoa(n.Port) + " comment yoho-mesh >/dev/null\n"
			}
		}
		if h.Name == hub(s) {
			script += `if [ -e /etc/sysctl.d/99-yoho-mesh.conf ]; then grep -qx '# Managed by Yoho' /etc/sysctl.d/99-yoho-mesh.conf; fi
printf '# Managed by Yoho\nnet.ipv4.ip_forward=1\n' > /etc/sysctl.d/99-yoho-mesh.conf
sysctl -p /etc/sysctl.d/99-yoho-mesh.conf >/dev/null
`
			// UFW's default forward policy otherwise blocks NAT peer traffic.
			script += "ufw route allow in on yoho0 out on yoho0 from " + remote.Quote(n.Subnet) + " to " + remote.Quote(n.Subnet) + " comment yoho-mesh >/dev/null\n"
		} else {
			script += "if [ -f /etc/sysctl.d/99-yoho-mesh.conf ]; then grep -qx '# Managed by Yoho' /etc/sysctl.d/99-yoho-mesh.conf; rm /etc/sysctl.d/99-yoho-mesh.conf; fi\n"
		}
		script += "systemctl enable wg-quick@yoho0 >/dev/null\nif ! ip link show yoho0 >/dev/null 2>&1; then wg-quick up yoho0; fi\n"
		script += "wg set yoho0 private-key /etc/wireguard/yoho0.key\n"
		if e = h.Host.Run(ctx, remote.Cmd{Script: script, Sudo: true}); e != nil {
			return e
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		if e = h.Host.WriteFile(ctx, statePath(h), b, 0600, true); e != nil {
			return e
		}
	}
	return nil
}

func Down(ctx context.Context, h plan.NamedHost) error {
	return h.Host.Run(ctx, remote.Cmd{Sudo: true, Script: `set -eu
if [ ! -f /etc/wireguard/yoho0.owned ]; then exit 0; fi
if [ -e /etc/wireguard/yoho0.conf ]; then
 grep -qx '# Managed by Yoho' /etc/wireguard/yoho0.conf
 if ip link show yoho0 >/dev/null 2>&1; then wg-quick down yoho0; fi
fi
systemctl disable wg-quick@yoho0 >/dev/null
rm -f /etc/wireguard/yoho0.conf /etc/wireguard/yoho0.key /etc/wireguard/yoho0.key.next
if [ -f /etc/sysctl.d/99-yoho-mesh.conf ]; then
 grep -qx '# Managed by Yoho' /etc/sysctl.d/99-yoho-mesh.conf
 rm /etc/sysctl.d/99-yoho-mesh.conf
fi
` + clearFirewall + "rm -f " + remote.Quote(statePath(h)) + "\nrm -f /etc/wireguard/yoho0.owned\n"})
}

// PeerStatus never includes the interface row (which contains the private key).
type PeerStatus struct {
	PublicKey           string `json:"public_key"`
	HandshakeAgeSeconds *int64 `json:"handshake_age_seconds"`
	RX                  uint64 `json:"rx_bytes"`
	TX                  uint64 `json:"tx_bytes"`
}

const statusScript = "set -eu\nwg show yoho0 public-key >/dev/null\nwg show yoho0 dump | awk 'NR > 1 { print $1, $5, $6, $7 }'"

func ParseStatus(out string, now time.Time) ([]PeerStatus, error) {
	var result []PeerStatus
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 {
			return nil, fmt.Errorf("invalid WireGuard peer status")
		}
		ts, e := strconv.ParseInt(f[1], 10, 64)
		if e != nil {
			return nil, e
		}
		rx, e := strconv.ParseUint(f[2], 10, 64)
		if e != nil {
			return nil, e
		}
		tx, e := strconv.ParseUint(f[3], 10, 64)
		if e != nil {
			return nil, e
		}
		p := PeerStatus{PublicKey: f[0], RX: rx, TX: tx}
		if ts > 0 {
			age := max(int64(0), now.Unix()-ts)
			p.HandshakeAgeSeconds = &age
		}
		result = append(result, p)
	}
	return result, nil
}
func Status(ctx context.Context, h plan.NamedHost) ([]PeerStatus, error) {
	// Suppress stderr: no raw dump or private key may enter remote error output.
	out, err := h.Host.Output(ctx, remote.Cmd{Script: statusScript, Sudo: true, Stderr: io.Discard})
	if err != nil {
		return nil, err
	}
	return ParseStatus(out, time.Now())
}
func Ping(ctx context.Context, h plan.NamedHost, address string) (string, error) {
	if _, err := netipAddress(address); err != nil {
		return "", err
	}
	return h.Host.Output(ctx, remote.Cmd{Script: "set -eu\nping -c 3 -W 1 " + remote.Quote(address)})
}
func netipAddress(s string) (net.IP, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("invalid mesh address")
	}
	return ip, nil
}
