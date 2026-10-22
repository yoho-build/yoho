package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
)

// ReservedSubnetRange is where Yoho looks for a free /24 to suggest when
// Docker's default address pools are exhausted. It is a suggestion range
// only: Yoho never creates a network in it by itself.
const ReservedSubnetRange = "10.200.0.0/16"

// poolProbeTimeout bounds the read-only probe run after a failure.
const poolProbeTimeout = 10 * time.Second

var poolExhaustedMessages = []string{
	"all predefined address pools have been fully subnetted",
	"could not find an available, non-overlapping ipv4 address pool",
}

// IsPoolExhausted reports whether err is Docker running out of address pools
// while creating a network (docker compose up, docker network create, docker
// stack deploy).
func IsPoolExhausted(err error) bool {
	var ee *remote.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	s := strings.ToLower(ee.Stderr)
	for _, m := range poolExhaustedMessages {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// NetworkHint is the actionable part of a pool exhaustion failure.
type NetworkHint struct {
	// Network is the default network that could not be created.
	Network         string `json:"name"`
	SuggestedSubnet string `json:"suggested_subnet"`
	// Snippet is the compose override that pins the subnet.
	Snippet string `json:"-"`
}

// SnippetFor is the compose YAML that pins the default network's subnet.
func SnippetFor(subnet string) string {
	return "networks:\n  default:\n    ipam:\n      config:\n        - subnet: " + subnet + "\n"
}

const usedSubnetsScript = `set -eu
ids=$(docker network ls -q 2>/dev/null || true)
if [ -n "$ids" ]; then
  # shellcheck disable=SC2086
  docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' $ids 2>/dev/null || true
fi
if command -v ip >/dev/null 2>&1; then
  ip -4 -o route 2>/dev/null | while read -r dst rest; do echo "$dst"; done || true
fi
`

// SuggestSubnet returns the first /24 of ReservedSubnetRange that overlaps no
// Docker network or IPv4 route on the Server, plus the prefixes seen in use.
// It only reads (docker network inspect, ip route) and creates nothing.
func SuggestSubnet(ctx context.Context, h remote.Host) (netip.Prefix, []netip.Prefix, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: usedSubnetsScript})
	if err != nil {
		return netip.Prefix{}, nil, fmt.Errorf("probe used subnets: %w", err)
	}
	used := parsePrefixes(out)
	p, err := firstFree(netip.MustParsePrefix(ReservedSubnetRange), used)
	return p, used, err
}

func parsePrefixes(out string) []netip.Prefix {
	var used []netip.Prefix
	for _, f := range strings.Fields(out) {
		if p, err := netip.ParsePrefix(f); err == nil && p.Addr().Is4() {
			used = append(used, p.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil && a.Is4() {
			used = append(used, netip.PrefixFrom(a, 32))
		}
	}
	return used
}

func firstFree(rng netip.Prefix, used []netip.Prefix) (netip.Prefix, error) {
	for a := rng.Addr(); rng.Contains(a); {
		c := netip.PrefixFrom(a, 24)
		free := true
		for _, u := range used {
			if u.Overlaps(c) {
				free = false
				break
			}
		}
		if free {
			return c, nil
		}
		b := a.As4()
		b[2]++
		if b[2] == 0 {
			break
		}
		a = netip.AddrFrom4(b)
	}
	return netip.Prefix{}, fmt.Errorf("no free /24 left in %s", rng)
}

// AnnotatePoolExhausted turns a pool exhaustion error into a *Failure that
// carries a suggested subnet for network (the default network that failed).
// Anything else, and a failed probe, returns err unchanged.
func AnnotatePoolExhausted(ctx context.Context, h remote.Host, network string, err error) error {
	if err == nil || !IsPoolExhausted(err) {
		return err
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poolProbeTimeout)
	defer cancel()
	p, _, perr := SuggestSubnet(pctx, h)
	if perr != nil {
		return err
	}
	nh := &NetworkHint{Network: network, SuggestedSubnet: p.String(), Snippet: SnippetFor(p.String())}
	var f *Failure
	if errors.As(err, &f) {
		f.Network = nh
		return err
	}
	return &Failure{Cause: err, Message: "Docker has no free address pool for network " + network, Phase: "create-network", Network: nh}
}
