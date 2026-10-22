package cli

import (
	"github.com/yoho-build/yoho/internal/config"
	"testing"
)

func TestScheduleModeJobIdentity(t *testing.T) {
	for _, tc := range []struct {
		ssh      string
		sudo     bool
		wantUser string
		wantSudo bool
	}{
		{"root@example", true, "yoho", true},
		{"root@example", false, "yoho", true},
		{"deploy@example", true, "deploy", true},
		{"deploy@example", false, "deploy", false},
	} {
		t.Run(tc.ssh, func(t *testing.T) {
			dest := config.Destination{Servers: []string{"server"}}
			a := &app{cfg: &config.Config{Servers: map[string]config.Server{"server": {SSH: tc.ssh, Sudo: tc.sudo}}, Setup: config.SetupConfig{User: "yoho"}}, dest: dest}
			mode := a.scheduleMode()
			if mode.User != tc.wantUser || mode.Sudo != tc.wantSudo {
				t.Fatalf("mode: %+v", mode)
			}
		})
	}
}
