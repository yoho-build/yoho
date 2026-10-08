package cli

import "testing"

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
