// Package setup provisions a Debian/Ubuntu Server for Yoho: Docker with
// the compose and buildx plugins, the deploy user, Yoho directories, ufw,
// unattended-upgrades, optional swap, timezone and containerd image store.
//
// Plan inspects the Server and lists every step with what would change;
// Apply runs the needed steps one by one after an injected confirmation.
// Every change needs root or passwordless sudo; without it, steps report
// "needs sudo" instead of failing midway.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Config is the resolved input of a setup run.
type Config struct {
	Setup config.SetupConfig
	// Server.Sudo: run privileged commands with sudo -n.
	Sudo bool
	// Published Proxy ports to allow in the firewall (e.g. 80, 443).
	ProxyPorts []int
	// Public key lines for the deploy user (defaults resolved by the CLI).
	AuthorizedKeys []string
	// Opt-in: enable Docker's containerd image store (needed for pussh).
	ContainerdImageStore bool
}

// OSInfo is parsed from /etc/os-release.
type OSInfo struct {
	ID, VersionID, Codename, PrettyName string
}

// Step is one provisioning step.
type Step struct {
	ID    string
	Title string
	// checkSudo: Check itself needs privileges. Apply always does.
	checkSudo bool
	check     func(ctx context.Context, h remote.Host) (needed bool, detail string, err error)
	apply     func(ctx context.Context, h remote.Host) error
}

// Check reports whether the step would change the Server, and what.
func (s *Step) Check(ctx context.Context, h remote.Host) (bool, string, error) {
	return s.check(ctx, h)
}

// Apply makes the change.
func (s *Step) Apply(ctx context.Context, h remote.Host) error { return s.apply(ctx, h) }

// PlannedStep is a Step with its Check result.
type PlannedStep struct {
	Step   *Step
	Needed bool
	Detail string
	// Non-empty when the step cannot be applied, e.g. "needs sudo".
	Blocked string
}

var (
	userRe    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	pkgRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	swapRe    = regexp.MustCompile(`^[1-9][0-9]*[MG]$`)
	tzRe      = regexp.MustCompile(`^[A-Za-z0-9_+/-]+$`)
	sshPortRe = regexp.MustCompile(`(?m)^port ([0-9]+)$`)
)

// ParseOSRelease parses /etc/os-release content.
func ParseOSRelease(b []byte) OSInfo {
	kv := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'"`)
		}
		kv[k] = v
	}
	o := OSInfo{ID: kv["ID"], VersionID: kv["VERSION_ID"], Codename: kv["VERSION_CODENAME"], PrettyName: kv["PRETTY_NAME"]}
	if o.ID == "ubuntu" && kv["UBUNTU_CODENAME"] != "" {
		o.Codename = kv["UBUNTU_CODENAME"]
	}
	return o
}

// DetectOS reads /etc/os-release and refuses anything but Debian/Ubuntu.
func DetectOS(ctx context.Context, h remote.Host) (OSInfo, error) {
	b, err := h.ReadFile(ctx, "/etc/os-release", false)
	if err != nil {
		return OSInfo{}, fmt.Errorf("read /etc/os-release: %w", err)
	}
	o := ParseOSRelease(b)
	if o.ID != "debian" && o.ID != "ubuntu" {
		name := o.PrettyName
		if name == "" {
			name = o.ID
		}
		return o, fmt.Errorf("yoho setup supports Debian and Ubuntu only; %s on %s is not supported (install Docker with the compose plugin yourself, then deploy)", name, h.Name())
	}
	if o.Codename == "" {
		return o, fmt.Errorf("%s: VERSION_CODENAME missing from /etc/os-release", o.PrettyName)
	}
	return o, nil
}

// privilege reports whether privileged commands can run, and why not.
func privilege(ctx context.Context, h remote.Host, sudo bool) (bool, string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: `if [ "$(id -u)" = 0 ]; then echo root; elif sudo -n true 2>/dev/null; then echo sudo; else echo none; fi`})
	if err != nil {
		return false, "", err
	}
	switch out {
	case "root":
		return true, "", nil
	case "sudo":
		if sudo {
			return true, "", nil
		}
		return false, "needs sudo: set `sudo: true` for this Server", nil
	default:
		return false, "needs sudo: connect as root or configure passwordless sudo (NOPASSWD) for this user", nil
	}
}

// Plan inspects the Server and returns every step with its Check result.
func Plan(ctx context.Context, h remote.Host, cfg Config) ([]PlannedStep, error) {
	osi, err := DetectOS(ctx, h)
	if err != nil {
		return nil, err
	}
	priv, why, err := privilege(ctx, h, cfg.Sudo)
	if err != nil {
		return nil, fmt.Errorf("probe privileges: %w", err)
	}
	steps, err := Steps(ctx, h, cfg, osi, priv)
	if err != nil {
		return nil, err
	}
	var plan []PlannedStep
	for _, s := range steps {
		ps := PlannedStep{Step: s}
		if s.checkSudo && !priv {
			ps.Needed, ps.Detail = true, "cannot inspect without privileges"
		} else {
			ch := h
			if !priv {
				// sudo -n would fail; inspect as the SSH user.
				ch = noSudo{h}
			}
			ps.Needed, ps.Detail, err = s.Check(ctx, ch)
			if err != nil {
				return nil, fmt.Errorf("check %s: %w", s.ID, err)
			}
		}
		if ps.Needed && !priv {
			ps.Blocked = why
		}
		plan = append(plan, ps)
	}
	return plan, nil
}

// noSudo runs every command as the connecting user.
type noSudo struct{ remote.Host }

func (n noSudo) Run(ctx context.Context, c remote.Cmd) error {
	c.Sudo = false
	return n.Host.Run(ctx, c)
}

func (n noSudo) Output(ctx context.Context, c remote.Cmd) (string, error) {
	c.Sudo = false
	return n.Host.Output(ctx, c)
}

func (n noSudo) ReadFile(ctx context.Context, p string, _ bool) ([]byte, error) {
	return n.Host.ReadFile(ctx, p, false)
}

// Apply runs needed steps in order, asking confirm for each. It stops at
// the first failure because later steps depend on earlier ones.
func Apply(ctx context.Context, h remote.Host, steps []PlannedStep, confirm func(PlannedStep) bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	var blocked []string
	for _, ps := range steps {
		if !ps.Needed {
			continue
		}
		if ps.Blocked != "" {
			fmt.Fprintf(out, "skip %s: %s\n", ps.Step.Title, ps.Blocked)
			blocked = append(blocked, ps.Step.ID)
			continue
		}
		if confirm != nil && !confirm(ps) {
			fmt.Fprintf(out, "skip %s\n", ps.Step.Title)
			continue
		}
		fmt.Fprintf(out, "applying %s\n", ps.Step.Title)
		if err := ps.Step.Apply(ctx, h); err != nil {
			return fmt.Errorf("%s: %w", ps.Step.Title, err)
		}
	}
	if len(blocked) > 0 {
		return fmt.Errorf("not applied, need sudo: %s", strings.Join(blocked, ", "))
	}
	return nil
}

// Steps returns the provisioning steps for cfg in dependency order.
func Steps(ctx context.Context, h remote.Host, cfg Config, osi OSInfo, priv bool) ([]*Step, error) {
	sc := cfg.Setup
	user := sc.User
	if user == "" {
		user = "yoho"
	}
	if !userRe.MatchString(user) {
		return nil, fmt.Errorf("invalid setup.user %q", user)
	}
	for _, p := range sc.Packages {
		if !pkgRe.MatchString(p) {
			return nil, fmt.Errorf("invalid package name %q", p)
		}
	}
	for _, k := range cfg.AuthorizedKeys {
		if strings.ContainsAny(k, "\n\r") || !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") && !strings.HasPrefix(k, "sk-") {
			return nil, fmt.Errorf("invalid authorized key %.30q", k)
		}
	}
	steps := []*Step{dockerStep(osi)}
	if len(sc.Packages) > 0 {
		steps = append(steps, packagesStep(sc.Packages))
	}
	steps = append(steps, userStep(user, cfg.AuthorizedKeys), dirsStep(user))
	if sc.Firewall == nil || *sc.Firewall {
		ports := append([]int(nil), cfg.ProxyPorts...)
		ports = append(ports, sc.AllowPorts...)
		ports = append(ports, sshPorts(ctx, h, priv)...)
		steps = append(steps, firewallStep(ports))
	}
	if sc.AutoUpdates == nil || *sc.AutoUpdates {
		steps = append(steps, autoUpdatesStep())
	}
	if sc.Swap != "" {
		if !swapRe.MatchString(sc.Swap) {
			return nil, fmt.Errorf("invalid swap size %q (e.g. 2G, 512M)", sc.Swap)
		}
		steps = append(steps, swapStep(sc.Swap))
	}
	if sc.Timezone != "" {
		if !tzRe.MatchString(sc.Timezone) {
			return nil, fmt.Errorf("invalid timezone %q", sc.Timezone)
		}
		steps = append(steps, timezoneStep(sc.Timezone))
	}
	if cfg.ContainerdImageStore {
		steps = append(steps, containerdStep())
	}
	return steps, nil
}

// sshPorts detects sshd's ports so the firewall never locks us out.
func sshPorts(ctx context.Context, h remote.Host, priv bool) []int {
	out := ""
	if priv {
		out, _ = h.Output(ctx, remote.Cmd{Script: "sshd -T 2>/dev/null || /usr/sbin/sshd -T 2>/dev/null || true", Sudo: true})
	}
	var ports []int
	for _, m := range sshPortRe.FindAllStringSubmatch(out, -1) {
		if p, err := strconv.Atoi(m[1]); err == nil {
			ports = append(ports, p)
		}
	}
	if len(ports) == 0 {
		ports = []int{22}
	}
	return ports
}

func run(ctx context.Context, h remote.Host, script string) error {
	return h.Run(ctx, remote.Cmd{Script: "set -eu\nexport DEBIAN_FRONTEND=noninteractive\n" + script, Sudo: true})
}

// lines turns check script output into (needed, detail).
func lines(out string) (bool, string) {
	out = strings.TrimSpace(out)
	if out == "" {
		return false, "ok"
	}
	return true, strings.ReplaceAll(out, "\n", "; ")
}

func dockerStep(osi OSInfo) *Step {
	return &Step{
		ID: "docker", Title: "Docker Engine + compose + buildx plugins (apt.docker.com)",
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: `command -v docker >/dev/null 2>&1 || { echo "docker not installed"; exit 0; }
docker compose version >/dev/null 2>&1 || echo "docker compose plugin missing"
docker buildx version >/dev/null 2>&1 || echo "docker buildx plugin missing"
systemctl is-active --quiet docker 2>/dev/null || echo "docker service not running"`})
			if err != nil {
				return false, "", err
			}
			n, d := lines(out)
			return n, d, nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			// Official apt repository per docs.docker.com/engine/install/{debian,ubuntu}.
			return run(ctx, h, `if dpkg-query -W -f='${Status}' docker.io 2>/dev/null | grep -q 'ok installed'; then
  echo "distribution package docker.io is installed; remove it (apt-get remove docker.io) or install docker-compose-v2 yourself" >&2; exit 1
fi
apt-get update -q
apt-get install -y -q ca-certificates curl
install -m 0755 -d /etc/apt/keyrings
curl -fsSL `+remote.Quote("https://download.docker.com/linux/"+osi.ID+"/gpg")+` -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] `+"https://download.docker.com/linux/"+osi.ID+" "+osi.Codename+` stable" > /etc/apt/sources.list.d/docker.list
apt-get update -q
apt-get install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker`)
		},
	}
}

func packagesStep(pkgs []string) *Step {
	return &Step{
		ID: "packages", Title: "Extra packages: " + strings.Join(pkgs, " "),
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: "for p in " + remote.QuoteArgs(pkgs...) + `; do dpkg-query -W -f='${Status}' "$p" 2>/dev/null | grep -q 'ok installed' || echo "install $p"; done`})
			if err != nil {
				return false, "", err
			}
			n, d := lines(out)
			return n, d, nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			return run(ctx, h, "apt-get update -q\napt-get install -y -q "+remote.QuoteArgs(pkgs...))
		},
	}
}

func userStep(user string, keys []string) *Step {
	q := remote.Quote(user)
	checkScript := "id -u " + q + " >/dev/null 2>&1 || echo " + remote.Quote("create user "+user) + "\n" +
		"id -nG " + q + " 2>/dev/null | tr ' ' '\\n' | grep -qx docker || echo " + remote.Quote("add "+user+" to docker group (root-equivalent)") + "\n" +
		"f=\"$(getent passwd " + q + " | cut -d: -f6)/.ssh/authorized_keys\"\n"
	applyScript := "id -u " + q + " >/dev/null 2>&1 || useradd -m -s /bin/bash " + q + "\n" +
		"getent group docker >/dev/null || groupadd docker\n" +
		"usermod -aG docker " + q + "\n" +
		"home=$(getent passwd " + q + " | cut -d: -f6)\n" +
		"install -d -m 0700 -o " + q + " -g \"$(id -gn " + q + ")\" \"$home/.ssh\"\n" +
		"f=\"$home/.ssh/authorized_keys\"\ntouch \"$f\"\n"
	// Unprivileged checks work for the SSH user's own keys; another user's
	// unreadable file is reported instead of guessed.
	keyChecks := ""
	for _, k := range keys {
		fields := strings.Fields(k)
		label := fields[0]
		if len(fields) > 2 {
			label = fields[2]
		}
		keyChecks += "grep -qxF " + remote.Quote(k) + " \"$f\" 2>/dev/null || echo " + remote.Quote("authorize key "+label) + "\n"
		applyScript += "grep -qxF " + remote.Quote(k) + " \"$f\" || printf '%s\\n' " + remote.Quote(k) + " >> \"$f\"\n"
	}
	if keyChecks != "" {
		checkScript += "if [ -e \"$f\" ] && [ ! -r \"$f\" ]; then echo \"cannot read $f without privileges\"; else\n" + keyChecks + "fi\n"
	}
	applyScript += "chmod 0600 \"$f\"\nchown " + remote.Quote(user+":") + " \"$f\"\n"
	return &Step{
		ID: "user", Title: "Deploy user " + user,
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: checkScript, Sudo: true})
			if err != nil {
				return false, "", err
			}
			n, d := lines(out)
			if len(keys) == 0 {
				d += "; warning: no authorized keys given"
			}
			return n, d, nil
		},
		apply: func(ctx context.Context, h remote.Host) error { return run(ctx, h, applyScript) },
	}
}

func dirsStep(user string) *Step {
	root := release.Root
	dirs := []string{root, path.Join(root, "apps"), path.Join(root, "backups"), path.Join(root, "jobs")}
	return &Step{
		ID: "dirs", Title: "Yoho directories under " + root + " (owner " + user + ", 0700)",
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: "for d in " + remote.QuoteArgs(dirs...) + `; do s=$(stat -c '%U %a' "$d" 2>/dev/null || true); [ "$s" = ` +
				remote.Quote(user+" 700") + ` ] || echo "create $d"; done`, Sudo: true})
			if err != nil {
				return false, "", err
			}
			n, d := lines(out)
			return n, d, nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			return run(ctx, h, "install -d -m 0700 -o "+remote.Quote(user)+" -g \"$(id -gn "+remote.Quote(user)+")\" "+remote.QuoteArgs(dirs...))
		},
	}
}

func firewallStep(ports []int) *Step {
	ports = uniqueSorted(ports)
	var allow []string
	for _, p := range ports {
		allow = append(allow, fmt.Sprintf("%d/tcp", p))
	}
	return &Step{
		ID: "firewall", Title: "ufw firewall allowing " + strings.Join(allow, ", "), checkSudo: true,
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: "ufw status 2>/dev/null || true", Sudo: true})
			if err != nil {
				return false, "", err
			}
			var todo []string
			if !strings.Contains(out, "Status: active") {
				todo = append(todo, "enable ufw")
			}
			for _, a := range allow {
				if !ufwAllows(out, a) {
					todo = append(todo, "allow "+a)
				}
			}
			if len(todo) == 0 {
				return false, "ok", nil
			}
			return true, strings.Join(todo, "; ") + " (note: ports published by Docker bypass ufw; bind private ports to 127.0.0.1)", nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			script := "command -v ufw >/dev/null 2>&1 || { apt-get update -q; apt-get install -y -q ufw; }\n"
			for _, a := range allow {
				script += "ufw allow " + a + "\n"
			}
			// Rules first, so enabling never cuts the SSH session.
			return run(ctx, h, script+"ufw --force enable\n")
		},
	}
}

// ufwAllows reports whether `ufw status` lists an ALLOW rule for port.
func ufwAllows(status, port string) bool {
	for _, line := range strings.Split(status, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == port && f[1] == "ALLOW" {
			return true
		}
	}
	return false
}

func autoUpdatesStep() *Step {
	const conf = "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	return &Step{
		ID: "auto-updates", Title: "unattended-upgrades (security updates)",
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: `dpkg-query -W -f='${Status}' unattended-upgrades 2>/dev/null | grep -q 'ok installed' || echo "install unattended-upgrades"
grep -qs 'Unattended-Upgrade "1"' /etc/apt/apt.conf.d/20auto-upgrades || echo "enable periodic upgrades"`})
			if err != nil {
				return false, "", err
			}
			n, d := lines(out)
			return n, d, nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			return run(ctx, h, "apt-get update -q\napt-get install -y -q unattended-upgrades\nprintf '%s' "+remote.Quote(conf)+" > /etc/apt/apt.conf.d/20auto-upgrades\n")
		},
	}
}

func swapStep(size string) *Step {
	return &Step{
		ID: "swap", Title: "Swap file /swapfile (" + size + ")",
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: "swapon --noheadings --show=NAME 2>/dev/null | head -n 1"})
			if err != nil {
				return false, "", err
			}
			if out != "" {
				return false, "swap already active: " + out, nil
			}
			return true, "create and enable /swapfile of " + size, nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			return run(ctx, h, `[ ! -e /swapfile ] || { echo "/swapfile exists but is not active" >&2; exit 1; }
fallocate -l `+size+` /swapfile
chmod 0600 /swapfile
mkswap /swapfile >/dev/null
swapon /swapfile
grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
`)
		},
	}
}

func timezoneStep(tz string) *Step {
	return &Step{
		ID: "timezone", Title: "Timezone " + tz,
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			out, err := h.Output(ctx, remote.Cmd{Script: "timedatectl show -p Timezone --value 2>/dev/null || cat /etc/timezone 2>/dev/null || true"})
			if err != nil {
				return false, "", err
			}
			if out == tz {
				return false, "ok", nil
			}
			return true, fmt.Sprintf("change %q -> %q", out, tz), nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			return run(ctx, h, "timedatectl set-timezone "+remote.Quote(tz))
		},
	}
}

const daemonJSON = "/etc/docker/daemon.json"

func containerdStep() *Step {
	return &Step{
		ID: "containerd-image-store", Title: "Docker containerd image store (for pussh image transport)", checkSudo: true,
		check: func(ctx context.Context, h remote.Host) (bool, string, error) {
			cur, err := readDaemonJSON(ctx, h)
			if err != nil {
				return false, "", err
			}
			if f, ok := cur["features"].(map[string]any); ok && f["containerd-snapshotter"] == true {
				return false, "ok", nil
			}
			return true, "set features.containerd-snapshotter in " + daemonJSON + " and RESTART Docker: running containers restart, " +
				"and images in the previous store are hidden until pulled or pushed again", nil
		},
		apply: func(ctx context.Context, h remote.Host) error {
			cur, err := readDaemonJSON(ctx, h)
			if err != nil {
				return err
			}
			b, err := MergeContainerdSnapshotter(cur)
			if err != nil {
				return err
			}
			if err := run(ctx, h, "install -d -m 0755 /etc/docker\n[ ! -f "+daemonJSON+" ] || cp -p "+daemonJSON+" "+daemonJSON+".yoho-bak"); err != nil {
				return err
			}
			if err := h.WriteFile(ctx, daemonJSON, b, 0o644, true); err != nil {
				return err
			}
			return run(ctx, h, "systemctl restart docker")
		},
	}
}

func readDaemonJSON(ctx context.Context, h remote.Host) (map[string]any, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "[ ! -f " + daemonJSON + " ] || cat " + daemonJSON, Sudo: true})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", daemonJSON, err)
	}
	cur := map[string]any{}
	if strings.TrimSpace(out) == "" {
		return cur, nil
	}
	if err := json.Unmarshal([]byte(out), &cur); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON; fix it by hand: %w", daemonJSON, err)
	}
	return cur, nil
}

// MergeContainerdSnapshotter enables the containerd image store while
// keeping every other daemon.json setting.
func MergeContainerdSnapshotter(cur map[string]any) ([]byte, error) {
	if cur == nil {
		cur = map[string]any{}
	}
	f, _ := cur["features"].(map[string]any)
	if f == nil {
		if _, exists := cur["features"]; exists {
			return nil, errors.New("daemon.json: features is not an object")
		}
		f = map[string]any{}
	}
	f["containerd-snapshotter"] = true
	cur["features"] = f
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func uniqueSorted(ps []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ps {
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}
