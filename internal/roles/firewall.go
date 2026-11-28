package roles

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

const firewallChain = "YOHO-ROLES"

// Rule filters only new requests before DNAT; replies never match.
type Rule struct {
	Tool string
	Args []string
}

func firewallOwner(app, dest string) string { return "yoho-roles " + app + "/" + dest }
func chainPrefix(app, dest string) string {
	sum := sha256.Sum256([]byte(app + "/" + dest))
	return fmt.Sprintf("YOHO-R-%x-", sum[:4])
}

func FirewallRules(app, dest, server string, d config.Destination, addresses map[string]string) ([]Rule, error) {
	var out []Rule
	for _, name := range sortedKeys(d.Connections) {
		c := d.Connections[name]
		providers, e := Providers(d, c)
		if e != nil {
			return nil, e
		}
		if !slices.Contains(providers, server) {
			continue
		}
		addr, e := netip.ParseAddr(addresses[server])
		if e != nil {
			return nil, e
		}
		tool := "iptables"
		if addr.Is6() {
			tool = "ip6tables"
		}
		sources := []string{}
		for _, svc := range c.Consumers {
			for _, h := range d.ServiceServers(svc) {
				ip, e := netip.ParseAddr(addresses[h])
				if e != nil || ip.Is4() != addr.Is4() {
					return nil, fmt.Errorf("connection %s consumer %s needs an address of the provider's IP family", name, h)
				}
				sources = append(sources, ip.String())
			}
		}
		slices.Sort(sources)
		sources = slices.Compact(sources)
		base := []string{"-d", addr.String(), "-p", "tcp", "--dport", fmt.Sprint(c.Port), "-m", "conntrack", "--ctstate", "NEW", "--ctdir", "ORIGINAL", "-m", "comment", "--comment", firewallOwner(app, dest) + "/" + name}
		for _, src := range sources {
			args := append([]string{"-s", src}, base...)
			out = append(out, Rule{tool, append(args, "-j", "RETURN")})
		}
		out = append(out, Rule{tool, append(slices.Clone(base), "-j", "DROP")})
	}
	return out, nil
}

// FirewallScript fills a fresh owned sub-chain, then swaps only this App's jump
// in one xtables operation. The shared chain and other Apps are never flushed.
func FirewallScript(app, dest string, rules []Rule) string {
	owner := firewallOwner(app, dest)
	prefix := chainPrefix(app, dest)
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\ncommand -v flock >/dev/null\nmkdir -p /run/lock\nexec 9>/run/lock/yoho-roles-firewall.lock\nflock 9\n")
	for _, tool := range []string{"iptables", "ip6tables"} {
		xt := tool + " -w -t mangle"
		b.WriteString("command -v " + tool + " >/dev/null\n" + xt + " -N YOHO-ROLES 2>/dev/null || " + xt + " -S YOHO-ROLES >/dev/null\n")
		b.WriteString("new=" + remote.Quote(prefix) + "$$\n" + xt + " -N \"$new\"\nswapped=0\ntrap 'if [ \"$swapped\" = 0 ]; then " + xt + " -F \"$new\"; " + xt + " -X \"$new\"; fi' EXIT HUP INT TERM\n")
		for _, r := range rules {
			if r.Tool == tool {
				b.WriteString(xt + " -A \"$new\" " + remote.QuoteArgs(r.Args...) + "\n")
			}
		}
		// Read the owned pointer by exact comment; unrelated jumps retain position.
		b.WriteString("old=$(" + xt + " -S YOHO-ROLES | awk -v tag=" + remote.Quote("--comment \""+owner+"\"") + " '$1==\"-A\" { n++; if(index($0,tag)) print n, $NF }')\n")
		b.WriteString("if [ -n \"$old\" ]; then\n set -- $old\n [ \"$#\" = 2 ] || { echo 'duplicate Roles firewall owner jump' >&2; exit 1; }\n slot=$1; previous=$2\n case \"$previous\" in " + prefix + "*) ;; *) echo 'foreign firewall chain at owned jump' >&2; exit 1;; esac\n " + xt + " -R YOHO-ROLES \"$slot\" -m comment --comment " + remote.Quote(owner) + " -j \"$new\"\nelse\n previous=\n " + xt + " -A YOHO-ROLES -m comment --comment " + remote.Quote(owner) + " -j \"$new\"\nfi\nswapped=1\n")
		b.WriteString(xt + " -C PREROUTING -m comment --comment yoho-roles -j YOHO-ROLES 2>/dev/null || " + xt + " -I PREROUTING 1 -m comment --comment yoho-roles -j YOHO-ROLES\n")
		b.WriteString("if [ -n \"$previous\" ]; then " + xt + " -F \"$previous\"; " + xt + " -X \"$previous\"; fi\ntrap - EXIT HUP INT TERM\n")
		// Upgrade legacy protection only after the earlier mangle hook is installed.
		b.WriteString("if " + tool + " -w -t filter -S DOCKER-USER >/dev/null 2>&1; then\n " + tool + " -w -t filter -L DOCKER-USER -n --line-numbers | awk -v tag=" + remote.Quote("/* "+owner+"/") + " 'index($0,tag) {print $1}' | sort -rn | while IFS= read -r n; do " + tool + " -w -t filter -D DOCKER-USER \"$n\"; done\nfi\n")
	}
	return b.String()
}

const firewallReplay = `#!/bin/sh
set -u
failed=0
for f in /etc/yoho/firewall/*.sh; do
 [ -f "$f" ] || continue
 if ! /bin/sh "$f"; then echo "Yoho firewall replay failed: $f" >&2; failed=1; fi
done
exit "$failed"
`
const firewallUnit = `[Unit]
Description=Yoho connection firewall before networking and Docker
DefaultDependencies=no
After=local-fs.target
Wants=network-pre.target
Before=network-pre.target docker.service
[Service]
Type=oneshot
ExecStart=/bin/sh /etc/yoho/firewall-replay
RemainAfterExit=yes
[Install]
WantedBy=network-pre.target
RequiredBy=docker.service
`

func Protect(ctx context.Context, h remote.Host, app, dest string, rules []Rule) error {
	if len(rules) == 0 {
		_, e := h.ReadFile(ctx, FirewallPath(app, dest), true)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
	}
	if e := h.Run(ctx, remote.Cmd{Script: "set -eu\ncommand -v iptables >/dev/null\ncommand -v ip6tables >/dev/null\ncommand -v flock >/dev/null\ncommand -v systemctl >/dev/null\n", Sudo: true}); e != nil {
		return fmt.Errorf("Roles firewall unavailable: %w", e)
	}
	script := FirewallScript(app, dest, rules)
	// Persist before installing: a crash cannot publish unprotected providers on reboot.
	if e := h.WriteFile(ctx, FirewallPath(app, dest), []byte(script), 0700, true); e != nil {
		return e
	}
	data, e := json.Marshal(rules)
	if e != nil {
		return e
	}
	if e = h.WriteFile(ctx, FirewallPath(app, dest)+".json", data, 0600, true); e != nil {
		return e
	}
	if e = h.WriteFile(ctx, "/etc/yoho/firewall-replay", []byte(firewallReplay), 0700, true); e != nil {
		return e
	}
	if e = h.WriteFile(ctx, "/etc/systemd/system/yoho-firewall.service", []byte(firewallUnit), 0644, true); e != nil {
		return e
	}
	if e = h.Run(ctx, remote.Cmd{Script: script, Sudo: true}); e != nil {
		return fmt.Errorf("Roles firewall reconciliation failed: %w", e)
	}
	if e = h.Run(ctx, remote.Cmd{Script: "set -eu\nsystemctl daemon-reload\nsystemctl enable yoho-firewall.service\n", Sudo: true}); e != nil {
		return e
	}
	return CheckFirewall(ctx, h, rules, app+"/"+dest)
}

// CheckFirewall checks graph membership and exact relative rule content. It
// never assumes this App is first among other Apps in the shared chain.
func CheckFirewall(ctx context.Context, h remote.Host, rules []Rule, owners ...string) error {
	owner := ""
	if len(owners) > 0 {
		owner = "yoho-roles " + owners[0]
	}
	if owner == "" {
		for _, r := range rules {
			for i, a := range r.Args {
				if a == "--comment" && i+1 < len(r.Args) {
					tag := r.Args[i+1]
					owner = tag[:strings.LastIndex(tag, "/")]
					break
				}
			}
		}
	}
	if owner == "" {
		return nil
	}
	parts := strings.SplitN(strings.TrimPrefix(owner, "yoho-roles "), "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid firewall owner")
	}
	for _, tool := range []string{"iptables", "ip6tables"} {
		read := func(chain string) ([][]string, error) {
			out, e := h.Output(ctx, remote.Cmd{Script: "set -eu\n" + tool + " -w -t mangle -S " + remote.Quote(chain), Sudo: true})
			if e != nil {
				return nil, e
			}
			var rows [][]string
			for _, line := range strings.Split(out, "\n") {
				args, e := splitRule(line)
				if e != nil {
					return nil, e
				}
				if len(args) > 2 && args[0] == "-A" {
					if args[1] != chain {
						return nil, fmt.Errorf("unexpected chain")
					}
					rows = append(rows, args[2:])
				}
			}
			return rows, nil
		}
		root, e := read("PREROUTING")
		if e != nil {
			return e
		}
		found := 0
		for _, row := range root {
			if sameRule(row, []string{"-m", "comment", "--comment", "yoho-roles", "-j", firewallChain}) {
				found++
			}
		}
		if found != 1 {
			return fmt.Errorf("%s: missing or duplicate Yoho PREROUTING jump", tool)
		}
		entries, e := read(firewallChain)
		if e != nil {
			return e
		}
		chain := ""
		for _, row := range entries {
			for i, a := range row {
				if a == "--comment" && i+1 < len(row) && row[i+1] == owner {
					if chain != "" {
						return fmt.Errorf("duplicate App firewall jump")
					}
					if len(row) < 2 {
						return fmt.Errorf("invalid App jump")
					}
					chain = row[len(row)-1]
					if !sameRule(row, []string{"-m", "comment", "--comment", owner, "-j", chain}) {
						return fmt.Errorf("conditional App firewall jump")
					}
				}
			}
		}
		if !strings.HasPrefix(chain, chainPrefix(parts[0], parts[1])) {
			return fmt.Errorf("%s: missing owned App firewall chain", tool)
		}
		actual, e := read(chain)
		if e != nil {
			return e
		}
		var desired [][]string
		for _, r := range rules {
			if r.Tool == tool {
				desired = append(desired, r.Args)
			}
		}
		if len(actual) != len(desired) {
			return fmt.Errorf("%s: App firewall rule count differs", tool)
		}
		for i := range actual {
			if !sameRule(actual[i], desired[i]) {
				return fmt.Errorf("%s: App firewall rule %d differs", tool, i+1)
			}
		}
	}
	return nil
}

// splitRule reads xtables' shell-quoted output without executing it.
func splitRule(s string) ([]string, error) {
	var out []string
	var b strings.Builder
	quote := rune(0)
	escape, started := false, false
	for _, c := range s {
		if escape {
			b.WriteRune(c)
			escape = false
			started = true
			continue
		}
		if c == '\\' && quote != '\'' {
			escape = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				b.WriteRune(c)
			}
			started = true
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			started = true
			continue
		}
		if c == ' ' || c == '\t' {
			if started {
				out = append(out, b.String())
				b.Reset()
				started = false
			}
			continue
		}
		b.WriteRune(c)
		started = true
	}
	if quote != 0 || escape {
		return nil, fmt.Errorf("invalid xtables rule quoting")
	}
	if started {
		out = append(out, b.String())
	}
	return out, nil
}
func sameRule(a, b []string) bool {
	normalize := func(args []string) string {
		var tokens []string
		for i := 0; i < len(args); i++ {
			k := args[i]
			if i+1 >= len(args) {
				tokens = append(tokens, k)
				continue
			}
			v := args[i+1]
			i++
			if k == "-m" && v == "tcp" {
				continue
			}
			if k == "-s" || k == "-d" {
				if p, e := netip.ParsePrefix(v); e == nil && p.Bits() == p.Addr().BitLen() {
					v = p.Addr().String()
				} else if ip, e := netip.ParseAddr(v); e == nil {
					v = ip.String()
				}
			}
			tokens = append(tokens, k+"="+v)
		}
		sort.Strings(tokens)
		return strings.Join(tokens, "\x00")
	}
	return normalize(a) == normalize(b)
}

// CheckAddress verifies the published address is assigned to an interface.
func CheckAddress(ctx context.Context, h remote.Host, addr string) error {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return err
	}
	script := "set -eu\nip -o addr show | awk '{ split($4,a,\"/\"); print a[1] }' | grep -Fx -- " + remote.Quote(ip.String()) + " >/dev/null\n"
	if err = h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("private address %s is not assigned on %s: %w", addr, h.Name(), err)
	}
	return nil
}

// Ready probes the provider from the consumer Server with bounded retries.
func Ready(ctx context.Context, h remote.Host, addr string, port int) error {
	script := "set -eu\ncommand -v timeout >/dev/null\ni=0\nwhile [ \"$i\" -lt 30 ]; do\n if command -v nc >/dev/null; then\n  if timeout 2 nc -z -w 1 " + remote.Quote(addr) + " " + fmt.Sprint(port) + "; then exit 0; fi\n else\n  if timeout 2 bash -c " + remote.Quote("exec 3<>/dev/tcp/"+addr+"/"+fmt.Sprint(port)) + "; then exit 0; fi\n fi\n i=$((i+1)); sleep 1\ndone\nexit 1\n"
	return h.Run(ctx, remote.Cmd{Script: script})
}

// FirewallPath is the owned per-App policy replay file.
func FirewallPath(app, dest string) string {
	sum := sha256.Sum256([]byte(app + "/" + dest))
	return fmt.Sprintf("/etc/yoho/firewall/%x.sh", sum[:12])
}

// ExpandProtection retains previously protected ports during a rollout. A
// removed provider's publication stays protected until its container is gone.
func ExpandProtection(ctx context.Context, h remote.Host, app, dest string, desired []Rule) ([]Rule, error) {
	b, e := h.ReadFile(ctx, FirewallPath(app, dest)+".json", true)
	if errors.Is(e, os.ErrNotExist) {
		return desired, nil
	}
	if e != nil {
		return nil, e
	}
	var previous []Rule
	if e = json.Unmarshal(b, &previous); e != nil {
		return nil, fmt.Errorf("invalid saved Roles firewall policy: %w", e)
	}
	out := slices.Clone(desired)
	for _, rule := range previous {
		rule = upgradeFirewallRule(rule)
		found := false
		for _, want := range out {
			if rule.Tool == want.Tool && slices.Equal(rule.Args, want.Args) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, rule)
		}
	}
	// Preserve reachability for old consumers until their old placements stop.
	slices.SortStableFunc(out, func(a, b Rule) int {
		drop := func(r Rule) bool { return len(r.Args) > 0 && r.Args[len(r.Args)-1] == "DROP" }
		if drop(a) == drop(b) {
			return 0
		}
		if drop(a) {
			return 1
		}
		return -1
	})
	return out, nil
}

func upgradeFirewallRule(r Rule) Rule {
	if !slices.Contains(r.Args, "--ctorigdst") {
		return r
	}
	out := []string{}
	for i := 0; i < len(r.Args); i++ {
		a := r.Args[i]
		switch a {
		case "--ctorigdst":
			a = "-d"
		case "--ctorigdstport":
			a = "--dport"
		case "ACCEPT":
			a = "RETURN"
		}
		out = append(out, a)
	}
	out = append(out, "--ctstate", "NEW", "--ctdir", "ORIGINAL")
	r.Args = out
	return r
}
