package mesh

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
)

func hub(s State) string {
	for _, n := range keys(s.Peers) {
		if s.Peers[n].Endpoint != "none" {
			return n
		}
	}
	return ""
}

// Render produces a deterministic, secret-free wg-quick configuration.
func Render(name string, network config.NetworkConfig, s State) (string, error) {
	network = Defaults(network)
	self, ok := s.Peers[name]
	if !ok {
		return "", fmt.Errorf("missing mesh Server %s", name)
	}
	h := hub(s)
	if h == "" {
		return "", fmt.Errorf("mesh needs a public hub")
	}
	p, err := netip.ParsePrefix(network.Subnet)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by Yoho\n[Interface]\nAddress = %s/%d\n", self.Address, p.Bits())
	if self.Endpoint != "none" {
		fmt.Fprintf(&b, "ListenPort = %d\n", network.Port)
	}
	b.WriteString("PostUp = wg set %i private-key /etc/wireguard/yoho0.key\n")
	if name == h {
		fmt.Fprintf(&b, "PostUp = %s\nPostDown = %s\n", forwardUp, forwardDown)
	}
	for _, n := range keys(s.Peers) {
		peer := s.Peers[n]
		if n == name {
			continue
		}
		if self.Endpoint == "none" && n != h {
			continue
		}
		if self.Endpoint != "none" && name != h && peer.Endpoint == "none" {
			continue
		}
		key, e := base64.StdEncoding.DecodeString(peer.PublicKey)
		if e != nil || len(key) != 32 {
			return "", fmt.Errorf("invalid public key for Server %s", n)
		}
		allowed := peer.Address + "/32"
		if self.Endpoint == "none" {
			allowed = network.Subnet
		} else if n == h {
			for _, other := range keys(s.Peers) {
				if s.Peers[other].Endpoint == "none" {
					allowed += ", " + s.Peers[other].Address + "/32"
				}
			}
		}
		fmt.Fprintf(&b, "\n[Peer]\nPublicKey = %s\nAllowedIPs = %s\n", peer.PublicKey, allowed)
		if peer.Endpoint != "none" {
			if strings.ContainsAny(peer.Endpoint, "\r\n") {
				return "", fmt.Errorf("invalid endpoint")
			}
			fmt.Fprintf(&b, "Endpoint = %s\n", peer.Endpoint)
		}
		if self.Endpoint == "none" {
			b.WriteString("PersistentKeepalive = 25\n")
		}
	}
	return b.String(), nil
}

// Collisions checks route lines and Docker IPAM prefixes, ignoring owned yoho0 routes.
func Collisions(subnet, routes string, pools []string) error {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "default" {
			continue
		}
		owned := false
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "dev" && f[i+1] == "yoho0" {
				owned = true
			}
		}
		if owned {
			continue
		}
		candidate := f[0]
		if candidate == "local" || candidate == "broadcast" || candidate == "blackhole" || candidate == "unreachable" || candidate == "prohibit" || candidate == "throw" || candidate == "unicast" {
			if len(f) < 2 {
				continue
			}
			candidate = f[1]
		}
		if ip, e := netip.ParseAddr(candidate); e == nil {
			candidate = netip.PrefixFrom(ip, ip.BitLen()).String()
		}
		pools = append(pools, candidate)
	}
	for _, raw := range pools {
		q, e := netip.ParsePrefix(raw)
		if e == nil && p.Overlaps(q) {
			return fmt.Errorf("mesh subnet %s collides with %s", subnet, raw)
		}
	}
	return nil
}

// Only Yoho's interface forwarding rule is managed, even with Docker's DROP policy.
const forwardUp = `chain=FORWARD; if iptables -nL DOCKER-USER >/dev/null 2>&1; then chain=DOCKER-USER; fi; iptables -C "$chain" -i yoho0 -o yoho0 -m comment --comment yoho-mesh -j ACCEPT 2>/dev/null || iptables -I "$chain" 1 -i yoho0 -o yoho0 -m comment --comment yoho-mesh -j ACCEPT`
const forwardDown = `for chain in DOCKER-USER FORWARD; do while iptables -C "$chain" -i yoho0 -o yoho0 -m comment --comment yoho-mesh -j ACCEPT 2>/dev/null; do iptables -D "$chain" -i yoho0 -o yoho0 -m comment --comment yoho-mesh -j ACCEPT; done; done`
