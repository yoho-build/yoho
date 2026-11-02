package cli

import (
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

func TestContainerStatusValue(t *testing.T) {
	const hint = "run `yoho builder setup` to upgrade (needs sudo)"
	cases := []struct {
		name        string
		installed   string
		installedOK bool
		latest      string
		latestOK    bool
		want        string
	}{
		{"outdated", "1.0.0", true, "1.5.0", true, "1.0.0 · latest 1.5.0; " + hint},
		{"semantic", "1.5.0", true, "1.10.0", true, "1.5.0 · latest 1.10.0; " + hint},
		{"same", "1.5.0", true, "1.5.0", true, "1.5.0 · latest 1.5.0"},
		{"newer", "1.10.0", true, "1.5.0", true, "1.10.0 · latest 1.5.0"},
		{"offline old", "1.0.0", true, "", false, "1.0.0 (older than 1.5.0; run `yoho builder setup`) · latest unknown (offline?)"},
		{"offline current", "1.5.0", true, "", false, "1.5.0 · latest unknown (offline?)"},
		{"missing", "", false, "1.5.0", true, "not installed · latest 1.5.0"},
	}
	for _, c := range cases {
		got := containerStatusValue(c.installed, c.installedOK, c.latest, c.latestOK)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDirectSetupHints(t *testing.T) {
	hop := remote.Hop{User: "root", Host: "203.0.113.9", Port: 2200, Resolved: true}
	text := strings.Join(directSetupHints("hostinger", "build@10.0.0.2:2022", "hostinger", hop, true), "\n")
	for _, want := range []string{
		"ssh -p 2022 build@10.0.0.2 'test -f ~/.ssh/id_ed25519 || ssh-keygen -t ed25519 -N \"\" -f ~/.ssh/id_ed25519'",
		"ssh -p 2022 build@10.0.0.2 'cat ~/.ssh/id_ed25519.pub' | ssh hostinger 'mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys'",
		"ssh-keyscan -p 2200 203.0.113.9 >> ~/.ssh/known_hosts'",
		"ssh-keygen -l -F '[203.0.113.9]:2200'",
		"yoho builder check",
		"root@203.0.113.9:2200",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("hints lack %q:\n%s", want, text)
		}
	}
	cf := strings.Join(directSetupHints("web", "b@host", "web", remote.Hop{}, false), "\n")
	if !strings.Contains(cf, "Cloudflare") || strings.Contains(cf, "ssh-keygen") {
		t.Errorf("cloudflare hint: %s", cf)
	}
}
