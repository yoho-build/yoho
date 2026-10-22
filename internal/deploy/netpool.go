package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
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
	// Network is the network that could not be created; empty when Docker's
	// message and the failing command do not say.
	Network string `json:"name"`
	// Default is true for the App's compose default network, the only one
	// destinations.<name>.subnet configures.
	Default         bool   `json:"default"`
	SuggestedSubnet string `json:"suggested_subnet"`
	// Key is the compose network key (Network without the project prefix).
	Key string `json:"-"`
}

// SnippetFor is the compose YAML that pins the subnet of the network with
// the given compose key ("default" or a named network).
func SnippetFor(subnet string, key ...string) string {
	k := "default"
	if len(key) > 0 {
		k = key[0]
	}
	return "networks:\n  " + k + ":\n    ipam:\n      config:\n        - subnet: " + subnet + "\n"
}

var (
	composeNetErr = regexp.MustCompile(`(?i)network\s+([^\s:]+)\s+error`)
	stackNetErr   = regexp.MustCompile(`(?i)failed to create (?:network )?"?([A-Za-z0-9][A-Za-z0-9_.-]*)"?:`)
)

// failedNetwork names the network behind a pool exhaustion error: from
// Docker's or Compose's message, else from a `docker network create` of a
// known network in the failing script, else the compose default network
// (a plain `up`). Empty when a create failed and nothing says which.
func failedNetwork(err error, project string, known []string) string {
	var ee *remote.ExitError
	if !errors.As(err, &ee) {
		return ""
	}
	for _, re := range []*regexp.Regexp{composeNetErr, stackNetErr} {
		if m := re.FindStringSubmatch(ee.Stderr); m != nil {
			return m[1]
		}
	}
	if strings.Contains(ee.Script, "network create") {
		for _, k := range known {
			if k != "" && strings.Contains(ee.Script, k) {
				return k
			}
		}
		return ""
	}
	return project + "_default"
}

// usedSubnetsScript fails when any inventory command fails, so an incomplete
// list never produces a suggestion. A missing `ip` fails the script too
// (command substitution under set -e); routes of type blackhole, unreachable,
// prohibit and throw count as taken, so their prefix is printed, not the type.
const usedSubnetsScript = `set -eu
ids=$(docker network ls -q)
if [ -n "$ids" ]; then
  # shellcheck disable=SC2086
  docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' $ids
fi
routes=$(ip -4 -o route)
printf '%s\n' "$routes" | while read -r dst rest; do
  case "$dst" in
  blackhole|unreachable|prohibit|throw|unicast|local|broadcast|multicast|anycast|nat)
    # shellcheck disable=SC2086
    set -- $rest
    dst=${1:-}
    ;;
  esac
  echo "$dst"
done
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
// carries a suggested subnet and the network it is for. project is the
// compose project (or stack) name; known lists other networks Yoho creates
// itself (the shared Proxy network, a scoped App's own). Anything else, and a
// failed probe, returns err unchanged.
func AnnotatePoolExhausted(ctx context.Context, h remote.Host, project string, known []string, err error) error {
	if err == nil || !IsPoolExhausted(err) {
		return err
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poolProbeTimeout)
	defer cancel()
	p, _, perr := SuggestSubnet(pctx, h)
	if perr != nil {
		return err
	}
	name := failedNetwork(err, project, known)
	nh := &NetworkHint{Network: name, Default: name == project+"_default", SuggestedSubnet: p.String(), Key: strings.TrimPrefix(name, project+"_")}
	var f *Failure
	if errors.As(err, &f) {
		f.Network = nh
		return err
	}
	msg := "Docker has no free address pool"
	if name != "" {
		msg += " for network " + name
	}
	return &Failure{Cause: err, Message: msg, Phase: "create-network", Network: nh}
}

// poolKnownNetworks lists the non-default networks Yoho creates for d.
func poolKnownNetworks(d *plan.Deploy) []string {
	known := []string{proxy.Network}
	if d.Scoped != nil && d.Scoped.ProxyNetwork != "" {
		known = append([]string{d.Scoped.ProxyNetwork}, known...)
	}
	return known
}

// SubnetChangeError refuses to change the subnet of the default network of a
// deployed App: Docker cannot alter a network's subnet in place, Compose
// would have to recreate it (stopping every attached container), and the
// Service-only plan diff would not show that.
type SubnetChangeError struct {
	Project  string
	Old, New string
}

func (e *SubnetChangeError) Error() string {
	return fmt.Sprintf("the default network subnet of %s would change from %q to %q, which Docker only does by recreating the network", e.Project, e.Old, e.New)
}

// defaultSubnet is the first ipam subnet of the compiled file's default
// network, "" when none. A file it cannot parse is an error, never "".
func defaultSubnet(composeYAML []byte) (string, error) {
	var doc struct {
		Networks map[string]struct {
			Ipam struct {
				Config []struct {
					Subnet string `yaml:"subnet"`
				} `yaml:"config"`
			} `yaml:"ipam"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		return "", err
	}
	if n, ok := doc.Networks["default"]; ok && len(n.Ipam.Config) > 0 {
		return n.Ipam.Config[0].Subnet, nil
	}
	return "", nil
}

// checkSubnetChange compares the wanted compiled compose with the subnet of
// the current Release. exists is true only when a current Release was
// confirmed; a first subnet on an App without one is fine.
func checkSubnetChange(project string, want []byte, curSubnet string, exists bool) error {
	if !exists {
		return nil
	}
	n, err := defaultSubnet(want)
	if err != nil {
		return fmt.Errorf("parse compiled compose file of %s: %w", project, err)
	}
	if curSubnet != n {
		return &SubnetChangeError{Project: project, Old: curSubnet, New: n}
	}
	return nil
}

// currentSubnet reads the default network subnet of the Release `current`
// points at. It fails closed: exists is false only when there is confirmed no
// `current` link; a failed read of the link, the compose file, or its parse is
// an error, so a subnet change cannot slip through as a "first" subnet.
func currentSubnet(ctx context.Context, h remote.Host, app, dest string) (subnet string, exists bool, err error) {
	dir := release.AppDir(app, dest)
	cur := path.Join(dir, "current")
	link, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif [ -L " + remote.Quote(cur) + " ]; then readlink " + remote.Quote(cur) + "\nelif [ -e " + remote.Quote(cur) + " ]; then echo 'current is not a symlink' >&2; exit 1\nfi"})
	if err != nil {
		return "", false, fmt.Errorf("read current Release link: %w", err)
	}
	link = strings.TrimSpace(link)
	if link == "" {
		return "", false, nil
	}
	file := path.Join(dir, "releases", path.Base(link), "compose.yaml")
	raw, err := h.ReadFile(ctx, file, false)
	if err != nil {
		return "", false, fmt.Errorf("read current Release compose file (cannot tell whether the default network subnet changes): %w", err)
	}
	subnet, err = defaultSubnet(raw)
	if err != nil {
		return "", false, fmt.Errorf("parse %s (cannot tell whether the default network subnet changes): %w", file, err)
	}
	return subnet, true, nil
}

// OutputTail keeps the last bytes of a command's stdout so a pool exhaustion
// line survives when the script merges stderr into stdout (2>&1).
type OutputTail struct {
	// Redact masks secret values in the line Annotate keeps; the merged
	// output can carry anything the command printed.
	Redact func(string) string
	mu     sync.Mutex
	buf    []byte
}

const outputTailMax = 16 << 10

func (t *OutputTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > outputTailMax {
		t.buf = t.buf[len(t.buf)-outputTailMax:]
	}
	return len(p), nil
}

// Annotate lets IsPoolExhausted see a pool exhaustion message that went to the
// merged output: when err is a *remote.ExitError whose Stderr lacks it, the
// matching line (and only that line, never other output) becomes its Stderr.
func (t *OutputTail) Annotate(err error) error {
	var ee *remote.ExitError
	if err == nil || !errors.As(err, &ee) || IsPoolExhausted(err) {
		return err
	}
	t.mu.Lock()
	text := string(t.buf)
	t.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(line)
		for _, m := range poolExhaustedMessages {
			if strings.Contains(l, m) {
				line = strings.TrimSpace(line)
				if t.Redact != nil {
					line = t.Redact(line)
				}
				ee.Stderr = line
				return err
			}
		}
	}
	return err
}
