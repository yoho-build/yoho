package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseKeepWithin(t *testing.T) {
	for in, want := range map[string]string{"30d": "30d", "12h": "12h", "1y6m": "1y6m", "2m3d4h": "2m3d4h", "007d": "7d"} {
		k, err := ParseKeepWithin(in)
		if err != nil || k.String() != want {
			t.Errorf("%s: %v %q", in, err, k.String())
		}
	}
	for _, in := range []string{"", "0d", "30", "30 d", "d", "6m1y", "30s", "-1d", "1w"} {
		if _, err := ParseKeepWithin(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	k, _ := ParseKeepWithin("1m2d3h")
	newest := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	if got := k.Cutoff(newest); !got.Equal(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("cutoff %v", got)
	}
}

func TestValidateKeepWithin(t *testing.T) {
	c := &Config{App: "shop", Backups: BackupsConfig{Targets: map[string]BackupTarget{
		"ok":  {Repository: "/b", KeepWithin: "30d"},
		"bad": {Repository: "/b", KeepWithin: "30 days", KeepLast: -1},
	}}}
	var msgs []string
	for _, err := range Validate(c) {
		msgs = append(msgs, err.Error())
	}
	all := strings.Join(msgs, "\n")
	if !strings.Contains(all, "backups.targets.bad.keep_within") || !strings.Contains(all, "backups.targets.bad.keep_last must not be negative") {
		t.Fatalf("errors:\n%s", all)
	}
	if strings.Contains(all, "backups.targets.ok.") {
		t.Fatalf("valid target rejected:\n%s", all)
	}
}
