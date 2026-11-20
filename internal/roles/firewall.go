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
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// Rule describes a Docker forwarding rule using the pre-DNAT destination.
type Rule struct {
	Tool string
	Args []string
}

func (r Rule) command(action string) string {
	return remote.Quote(r.Tool) + " " + action + " DOCKER-USER " + remote.QuoteArgs(r.Args...)
}

func FirewallRules(app, dest, server string, d config.Destination, addresses map[string]string) ([]Rule, error) {
	var out []Rule
	names := make([]string, 0, len(d.Connections))
	for n := range d.Connections {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		c := d.Connections[n]
		hs := d.ServiceServers(c.Service)
		if len(hs) != 1 {
			return nil, fmt.Errorf("connection %s must be singleton", n)
		}
		if hs[0] != server {
			continue
		}
		addr, err := netip.ParseAddr(addresses[server])
		if err != nil {
			return nil, fmt.Errorf("connection %s missing provider private address", n)
		}
		tool := "iptables"
		if addr.Is6() {
			tool = "ip6tables"
		}
		base := []string{"-p", "tcp", "-m", "conntrack", "--ctorigdst", addr.String(), "--ctorigdstport", fmt.Sprint(c.Port), "-m", "comment", "--comment", "yoho-roles " + app + "/" + dest + "/" + n}
		sources := []string{addr.String()}
		for _, svc := range c.Consumers {
			for _, h := range d.ServiceServers(svc) {
				ip, e := netip.ParseAddr(addresses[h])
				if e != nil || ip.Is4() != addr.Is4() {
					return nil, fmt.Errorf("connection %s consumer %s needs a private address of the provider's IP family", n, h)
				}
				sources = append(sources, ip.String())
			}
		}
		slices.Sort(sources)
		sources = slices.Compact(sources)
		for _, src := range sources {
			args := append(slices.Clone(base), "-s", src, "-j", "ACCEPT")
			out = append(out, Rule{tool, args})
		}
		out = append(out, Rule{tool, append(slices.Clone(base), "-j", "DROP")})
	}
	return out, nil
}

// FirewallScript installs the complete desired rules before deleting older
// owned rules. Insertion is reversed so DROP is always present before ACCEPT.
// No untagged rule is removed, even on a Server shared with other Apps.
func FirewallScript(app, dest string, rules []Rule) string {
	prefix := "yoho-roles " + app + "/" + dest + "/"
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n")
	for _, tool := range []string{"iptables", "ip6tables"} {
		b.WriteString("command -v " + tool + " >/dev/null\n" + tool + " -w -S DOCKER-USER >/dev/null\n")
		count := 0
		for _, r := range rules {
			if r.Tool == tool {
				count++
			}
		}
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].Tool == tool {
				b.WriteString(rules[i].command("-w -I") + "\n")
			}
		}
		// The first count owned rules are the rules inserted above. Remove the
		// remainder by descending rule number, preserving all unrelated rules.
		b.WriteString(tool + " -w -L DOCKER-USER -n --line-numbers | awk -v tag=" + remote.Quote("/* "+prefix) + " -v keep=" + fmt.Sprint(count) + " 'index($0,tag) { n++; if(n>keep) print $1 }' | sort -rn | while IFS= read -r n; do " + tool + " -w -D DOCKER-USER \"$n\"; done\n")
	}
	for _, r := range rules {
		b.WriteString(r.command("-w -C") + "\n")
	}
	return b.String()
}

// Protect persists only Yoho-owned scripts and a oneshot unit; the unit
// replays every App's firewall after Docker starts, without a Yoho daemon.
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
	script := FirewallScript(app, dest, rules)
	// Verify tool/chain support before replacing the persisted policy.
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\ncommand -v iptables >/dev/null\ncommand -v ip6tables >/dev/null\niptables -w -S DOCKER-USER >/dev/null\nip6tables -w -S DOCKER-USER >/dev/null\ncommand -v systemctl >/dev/null", Sudo: true}); err != nil {
		return fmt.Errorf("Roles firewall unavailable: %w", err)
	}
	file := FirewallPath(app, dest)
	if err := h.Run(ctx, remote.Cmd{Script: script, Sudo: true}); err != nil {
		return fmt.Errorf("Roles firewall reconciliation failed: %w", err)
	}
	metadata, e := json.Marshal(rules)
	if e != nil {
		return e
	}
	if e = h.WriteFile(ctx, file+".json", metadata, 0600, true); e != nil {
		return e
	}
	if err := h.WriteFile(ctx, file, []byte(script), 0700, true); err != nil {
		return err
	}
	unit := "[Unit]\nDescription=Yoho Docker connection firewall\nAfter=docker.service\nRequires=docker.service\nPartOf=docker.service\n[Service]\nType=oneshot\nExecStart=/bin/sh -ec 'for f in /etc/yoho/firewall/*.sh; do [ ! -f \"$f\" ] || /bin/sh \"$f\"; done'\nRemainAfterExit=yes\n[Install]\nWantedBy=docker.service\n"
	if err := h.WriteFile(ctx, "/etc/systemd/system/yoho-firewall.service", []byte(unit), 0644, true); err != nil {
		return err
	}
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\nsystemctl daemon-reload\nsystemctl enable yoho-firewall.service\n", Sudo: true}); err != nil {
		return err
	}
	return CheckFirewall(ctx, h, rules)
}

// CheckFirewall is read-only and refuses missing protection.
func CheckFirewall(ctx context.Context, h remote.Host, rules []Rule) error {
	script := "set -eu\n"
	prefix := ""
	for _, r := range rules {
		script += r.command("-w -C") + "\n"
		for i, a := range r.Args {
			if a == "--comment" && i+1 < len(r.Args) {
				tag := r.Args[i+1]
				idx := strings.LastIndex(tag, "/")
				if idx > 0 {
					prefix = "/* " + tag[:idx+1]
				}
			}
		}
	}
	if prefix != "" {
		for _, tool := range []string{"iptables", "ip6tables"} {
			n := 0
			for _, r := range rules {
				if r.Tool == tool {
					n++
				}
			}
			script += tool + " -w -L DOCKER-USER -n --line-numbers | awk -v tag=" + remote.Quote(prefix) + " -v want=" + fmt.Sprint(n) + " 'index($0,tag) { n++; if($1> want) bad=1 } END { if(bad || n!=want) exit 1 }'\n"
		}
	}
	return h.Run(ctx, remote.Cmd{Script: script, Sudo: true})
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
