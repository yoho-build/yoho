package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

type fakeHost struct {
	cmds    []remote.Cmd
	files   map[string][]byte
	respond func(script string) string
}

func (f *fakeHost) Name() string { return "fake" }
func (f *fakeHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}
func (f *fakeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	f.cmds = append(f.cmds, c)
	if f.respond != nil {
		return f.respond(c.Script), nil
	}
	return "", nil
}
func (f *fakeHost) WriteFile(_ context.Context, p string, data []byte, _ os.FileMode, _ bool) error {
	f.files[p] = data
	return nil
}
func (f *fakeHost) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	if b, ok := f.files[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (f *fakeHost) Close() error { return nil }

const ubuntu = `PRETTY_NAME="Ubuntu 22.04.5 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION_CODENAME=jammy
ID=ubuntu
ID_LIKE=debian
UBUNTU_CODENAME=jammy
`

func host(osRelease, priv string) *fakeHost {
	return &fakeHost{
		files: map[string][]byte{"/etc/os-release": []byte(osRelease)},
		respond: func(s string) string {
			switch {
			case strings.Contains(s, "sudo -n true"):
				return priv
			case strings.HasPrefix(s, "sshd -T"):
				return "port 2222\naddressfamily any"
			case strings.HasPrefix(s, "command -v docker"):
				return "docker not installed"
			case strings.HasPrefix(s, "ufw status"):
				return "Status: active\n\nTo Action From\n2222/tcp ALLOW Anywhere\n80/tcp ALLOW Anywhere"
			case strings.HasPrefix(s, "id -u 'yoho'"):
				return "create user yoho"
			case strings.HasPrefix(s, "for d in"):
				return "create /var/lib/yoho"
			case strings.HasPrefix(s, "timedatectl"):
				return "Etc/UTC"
			}
			return ""
		},
	}
}

func TestParseOSRelease(t *testing.T) {
	o := ParseOSRelease([]byte(ubuntu))
	if o.ID != "ubuntu" || o.Codename != "jammy" || o.PrettyName != "Ubuntu 22.04.5 LTS" {
		t.Fatalf("%+v", o)
	}
}

func TestPlanRefusesNonDebian(t *testing.T) {
	h := host("ID=fedora\nPRETTY_NAME=\"Fedora Linux 40\"\nVERSION_ID=40\n", "root")
	_, err := Plan(context.Background(), h, Config{})
	if err == nil || !strings.Contains(err.Error(), "Debian and Ubuntu only") || !strings.Contains(err.Error(), "Fedora Linux 40") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanAsRoot(t *testing.T) {
	h := host(ubuntu, "root")
	fw := true
	plan, err := Plan(context.Background(), h, Config{
		Setup:          config.SetupConfig{Firewall: &fw, AllowPorts: []int{8443}, Timezone: "Etc/UTC", Swap: "2G"},
		ProxyPorts:     []int{80, 443},
		AuthorizedKeys: []string{"ssh-ed25519 AAAAC3Nz op@laptop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PlannedStep{}
	var ids []string
	for _, p := range plan {
		got[p.Step.ID] = p
		ids = append(ids, p.Step.ID)
	}
	if strings.Join(ids, ",") != "docker,user,dirs,firewall,auto-updates,swap,timezone" {
		t.Fatalf("steps %v", ids)
	}
	if !got["docker"].Needed || got["docker"].Blocked != "" {
		t.Fatalf("docker %+v", got["docker"])
	}
	fwp := got["firewall"]
	if !fwp.Needed || !strings.Contains(fwp.Detail, "allow 443/tcp") || !strings.Contains(fwp.Detail, "allow 8443/tcp") ||
		strings.Contains(fwp.Detail, "allow 2222/tcp") || strings.Contains(fwp.Detail, "enable ufw") {
		t.Fatalf("firewall %+v", fwp)
	}
	if !strings.Contains(fwp.Step.Title, "2222/tcp") {
		t.Fatalf("detected SSH port missing: %s", fwp.Step.Title)
	}
	sawStatus := false
	for _, c := range h.cmds {
		if strings.HasPrefix(c.Script, "ufw status") {
			sawStatus = true
			if !strings.Contains(c.Script, "/usr/sbin/ufw") || !strings.Contains(c.Script, "/sbin/ufw") {
				t.Fatalf("privileged ufw lookup: %s", c.Script)
			}
		}
	}
	if !sawStatus {
		t.Fatal("root plan did not run ufw status")
	}
	if got["timezone"].Needed {
		t.Fatal("timezone already set")
	}

	// Apply with a confirm that declines swap.
	h.cmds = nil
	var out bytes.Buffer
	err = Apply(context.Background(), h, plan, func(p PlannedStep) bool { return p.Step.ID != "swap" }, &out)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, c := range h.cmds {
		if !c.Sudo {
			t.Fatalf("apply without sudo: %s", c.Script)
		}
		all.WriteString(c.Script)
		if out, err := exec.Command("sh", "-n", "-c", c.Script).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v %s\n%s", err, out, c.Script)
		}
	}
	s := all.String()
	for _, want := range []string{"docker-compose-plugin", "https://download.docker.com/linux/ubuntu jammy stable", "useradd -m -s /bin/bash 'yoho'",
		"ssh-ed25519 AAAAC3Nz op@laptop", "ufw allow 2222/tcp", "ufw --force enable", "/var/lib/yoho/backups"} {
		if !strings.Contains(s, want) {
			t.Errorf("apply missing %q", want)
		}
	}
	if strings.Contains(s, "fallocate") || !strings.Contains(out.String(), "skip Swap") {
		t.Fatal("declined step applied")
	}
	if strings.Index(s, "ufw allow 22") > strings.Index(s, "ufw --force enable") {
		t.Fatal("must allow SSH before enabling ufw")
	}
}

func TestPlanWithoutSudoIsBlocked(t *testing.T) {
	h := host(ubuntu, "none")
	plan, err := Plan(context.Background(), h, Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan {
		if p.Needed && !strings.Contains(p.Blocked, "needs sudo") {
			t.Fatalf("%s needed but not blocked: %+v", p.Step.ID, p)
		}
	}
	h.cmds = nil
	err = Apply(context.Background(), h, plan, func(PlannedStep) bool { return true }, nil)
	if err == nil || !strings.Contains(err.Error(), "need sudo") {
		t.Fatalf("err = %v", err)
	}
	if len(h.cmds) != 0 {
		t.Fatal("blocked steps must not run")
	}
}

func TestFirewallWithoutPrivileges(t *testing.T) {
	const confYes = "# Set to yes to start on boot. Eg: 'ufw allow 22/tcp'\n# ENABLED=no\nENABLED=yes\nLOGLEVEL=low\n"
	cases := []struct {
		name       string
		locate     string
		conf       string
		wantNeeded bool
		detail     string
	}{
		{name: "not installed", locate: "missing", wantNeeded: true, detail: "ufw not installed"},
		{name: "disabled", locate: "found", conf: "ENABLED=no\n", wantNeeded: true, detail: "ufw installed but disabled"},
		{name: "enabled", locate: "found", conf: confYes, wantNeeded: false, detail: "enabled (rules not verified without sudo)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := host(ubuntu, "none")
			orig := h.respond
			h.respond = func(s string) string {
				if strings.Contains(s, "/usr/sbin/ufw") && strings.Contains(s, "echo found") {
					return tc.locate
				}
				return orig(s)
			}
			if tc.conf != "" {
				h.files["/etc/ufw/ufw.conf"] = []byte(tc.conf)
			}
			plan, err := Plan(context.Background(), h, Config{})
			if err != nil {
				t.Fatal(err)
			}
			var fw PlannedStep
			found := false
			for _, p := range plan {
				if p.Step.ID == "firewall" {
					fw = p
					found = true
				}
			}
			if !found {
				t.Fatal("no firewall step")
			}
			if fw.Needed != tc.wantNeeded || fw.Detail != tc.detail {
				t.Fatalf("needed=%v detail=%q blocked=%q", fw.Needed, fw.Detail, fw.Blocked)
			}
			if tc.wantNeeded && !strings.Contains(fw.Blocked, "needs sudo") {
				t.Fatalf("blocked %q", fw.Blocked)
			}
			if !tc.wantNeeded && fw.Blocked != "" {
				t.Fatalf("enabled firewall blocked the run: %q", fw.Blocked)
			}
			sawLocate := false
			for _, c := range h.cmds {
				if strings.Contains(c.Script, "ufw status") {
					t.Fatalf("unprivileged plan ran ufw status: %s", c.Script)
				}
				if strings.Contains(c.Script, "echo found") {
					sawLocate = true
					if !strings.Contains(c.Script, "/usr/sbin/ufw") || !strings.Contains(c.Script, "/sbin/ufw") {
						t.Fatalf("locate script: %s", c.Script)
					}
				}
			}
			if !sawLocate {
				t.Fatal("did not look for /usr/sbin/ufw or /sbin/ufw")
			}
			h.cmds = nil
			err = Apply(context.Background(), h, plan, func(PlannedStep) bool { return true }, nil)
			if err == nil || !strings.Contains(err.Error(), "need sudo") {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "firewall") != tc.wantNeeded {
				t.Fatalf("err = %v", err)
			}
			if len(h.cmds) != 0 {
				t.Fatal("blocked steps must not run")
			}
		})
	}
}

func TestPlanSudoAvailableButNotConfigured(t *testing.T) {
	h := host(ubuntu, "sudo")
	plan, err := Plan(context.Background(), h, Config{Sudo: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan[0].Blocked, "sudo: true") {
		t.Fatalf("%+v", plan[0])
	}
}

func TestMergeContainerdSnapshotter(t *testing.T) {
	cur := map[string]any{"log-driver": "local", "features": map[string]any{"buildkit": true}}
	b, err := MergeContainerdSnapshotter(cur)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	f := back["features"].(map[string]any)
	if back["log-driver"] != "local" || f["buildkit"] != true || f["containerd-snapshotter"] != true {
		t.Fatalf("%s", b)
	}
	if _, err := MergeContainerdSnapshotter(map[string]any{"features": "x"}); err == nil {
		t.Fatal("non-object features accepted")
	}
}

func TestInvalidInputsRejected(t *testing.T) {
	for _, c := range []Config{
		{Setup: config.SetupConfig{User: "Bad User"}},
		{Setup: config.SetupConfig{Swap: "2G; rm -rf /"}},
		{Setup: config.SetupConfig{Packages: []string{"vim;reboot"}}},
		{AuthorizedKeys: []string{"ssh-ed25519 AAA\nevil"}},
	} {
		if _, err := Plan(context.Background(), host(ubuntu, "root"), c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestApplySkipsDependentOfDeclinedStep(t *testing.T) {
	h := host(ubuntu, "root")
	plan, err := Plan(context.Background(), h, Config{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3Nz op@laptop"}})
	if err != nil {
		t.Fatal(err)
	}
	h.cmds = nil
	var asked []string
	var out bytes.Buffer
	err = Apply(context.Background(), h, plan, func(p PlannedStep) bool {
		asked = append(asked, p.Step.ID)
		return p.Step.ID != "user"
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range asked {
		if id == "dirs" {
			t.Fatalf("prompted for dirs after declined user: %v", asked)
		}
	}
	if !strings.Contains(out.String(), "skip Yoho directories under /var/lib/yoho (owner yoho, 0700): needs Deploy user yoho") {
		t.Fatalf("output:\n%s", out.String())
	}
	for _, c := range h.cmds {
		if strings.Contains(c.Script, "install -d") || strings.Contains(c.Script, "useradd") {
			t.Fatalf("ran skipped step: %s", c.Script)
		}
	}
}

func TestApplyAllRunsUserBeforeDirs(t *testing.T) {
	h := host(ubuntu, "root")
	plan, err := Plan(context.Background(), h, Config{AuthorizedKeys: []string{"ssh-ed25519 AAAAC3Nz op@laptop"}})
	if err != nil {
		t.Fatal(err)
	}
	h.cmds = nil
	if err := Apply(context.Background(), h, plan, func(PlannedStep) bool { return true }, nil); err != nil {
		t.Fatal(err)
	}
	u, d := -1, -1
	for i, c := range h.cmds {
		if strings.Contains(c.Script, "useradd") {
			u = i
		}
		if strings.Contains(c.Script, "install -d -m 0700 -o 'yoho'") {
			d = i
		}
	}
	if u < 0 || d < 0 || u > d {
		t.Fatalf("user at %d, dirs at %d", u, d)
	}
}

func TestPlanInstallsRsyncForServerBuilder(t *testing.T) {
	h := host(ubuntu, "root")
	base := h.respond
	missing := true
	h.respond = func(s string) string {
		if strings.HasPrefix(s, "for p in") {
			if missing {
				return "install rsync"
			}
			return ""
		}
		return base(s)
	}
	cfg := Config{Setup: config.SetupConfig{Packages: []string{"htop"}}, BuildOnServer: true}
	find := func() PlannedStep {
		plan, err := Plan(context.Background(), h, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range plan {
			if p.Step.ID == "packages" {
				return p
			}
		}
		t.Fatal("no packages step")
		return PlannedStep{}
	}
	p := find()
	if !p.Needed || p.Detail != "install rsync" || p.Step.Title != "Extra packages: htop rsync" {
		t.Fatalf("%+v", p)
	}
	missing = false
	if p := find(); p.Needed || p.Detail != "ok" {
		t.Fatalf("rsync present: %+v", p)
	}

	// Without the server builder there is no packages step; no duplicate when listed.
	plan, err := Plan(context.Background(), h, Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plan {
		if p.Step.ID == "packages" {
			t.Fatal("unexpected packages step")
		}
	}
	cfg.Setup.Packages = []string{"rsync"}
	if p := find(); p.Step.Title != "Extra packages: rsync" {
		t.Fatalf("dup: %s", p.Step.Title)
	}
}
