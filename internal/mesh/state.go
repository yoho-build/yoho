// Package mesh manages the opt-in WireGuard network over SSH.
package mesh

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"

	"github.com/yoho-build/yoho/internal/config"
)

// State contains public information only; removed allocations are tombstones.
type State struct {
	Network   config.NetworkConfig `json:"network"`
	Addresses map[string]string    `json:"addresses"`
	Peers     map[string]Peer      `json:"peers,omitempty"`
}
type Peer struct {
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// Allocate merges sorted Server copies in authority order and assigns unused IPs.
func Allocate(subnet string, names []string, copies []State) (State, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil || !p.Addr().Is4() || p != p.Masked() || p.Bits() > 30 {
		return State{}, fmt.Errorf("invalid mesh subnet %q", subnet)
	}
	s := State{Addresses: map[string]string{}, Peers: map[string]Peer{}}
	used := map[string]string{}
	for _, c := range copies {
		for _, n := range keys(c.Addresses) {
			a := c.Addresses[n]
			if _, ok := s.Addresses[n]; ok {
				continue
			}
			ip, e := netip.ParseAddr(a)
			if e != nil || !p.Contains(ip) || ip == p.Addr() || !p.Contains(ip.Next()) {
				return s, fmt.Errorf("invalid allocation for Server %s", n)
			}
			if other := used[a]; other != "" {
				return s, fmt.Errorf("mesh address conflict: %s and %s", n, other)
			}
			s.Addresses[n], used[a] = a, n
		}
	}
	names = append([]string(nil), names...)
	sort.Strings(names)
	for _, n := range names {
		if s.Addresses[n] != "" {
			continue
		}
		ip := p.Addr().Next()
		for p.Contains(ip) && p.Contains(ip.Next()) && used[ip.String()] != "" {
			ip = ip.Next()
		}
		if !p.Contains(ip) || !p.Contains(ip.Next()) {
			return s, fmt.Errorf("mesh subnet exhausted")
		}
		s.Addresses[n], used[ip.String()] = ip.String(), n
	}
	return s, nil
}
func keys[V any](m map[string]V) []string {
	ns := make([]string, 0, len(m))
	for n := range m {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}
func Defaults(n config.NetworkConfig) config.NetworkConfig {
	if n.Mesh == "" {
		n.Mesh = "none"
	}
	if n.Subnet == "" {
		n.Subnet = "10.210.0.0/24"
	}
	if n.Port == 0 {
		n.Port = 51820
	}
	return n
}
func Decode(b []byte) (State, error) { var s State; err := json.Unmarshal(b, &s); return s, err }
