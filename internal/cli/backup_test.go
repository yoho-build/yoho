package cli

import (
	"strings"
	"testing"

	"github.com/yoho-dev/yoho/internal/config"
)

func TestSelectBackupJob(t *testing.T) {
	cfg := &config.Config{Backups: config.BackupsConfig{
		Targets: map[string]config.BackupTarget{"local": {Type: "archive", Repository: "/b"}, "s3": {Repository: "s3:x"}},
		Jobs: map[string]config.BackupJob{
			"nightly": {Destination: "production", Target: "local", Schedule: "daily"},
			"weekly":  {Destination: "production", Target: "s3"},
			"stage":   {Destination: "staging", Target: "local"},
		},
	}}
	a := &app{cfg: cfg, destName: "production"}
	if _, err := a.selectBackupJob(""); err == nil || !strings.Contains(err.Error(), "nightly, weekly") {
		t.Fatalf("want ambiguity error listing jobs, got %v", err)
	}
	j, err := a.selectBackupJob("weekly")
	if err != nil || j.targetName != "s3" {
		t.Fatalf("got %+v %v", j, err)
	}
	if _, err := a.selectBackupJob("stage"); err == nil || !strings.Contains(err.Error(), "-d staging") {
		t.Fatalf("want Destination hint, got %v", err)
	}
	a.destName = "staging"
	if j, err := a.selectBackupJob(""); err != nil || j.name != "stage" {
		t.Fatalf("only job of staging: %+v %v", j, err)
	}
	names, err := (&app{cfg: cfg, destName: "production"}).scheduledJobNames(nil)
	if err != nil || len(names) != 1 || names[0] != "nightly" {
		t.Fatalf("scheduled jobs %v %v", names, err)
	}
	if _, err := (&app{cfg: cfg, destName: "production"}).scheduledJobNames([]string{"weekly"}); err == nil {
		t.Fatal("job without schedule must be refused")
	}
	// No jobs, one target: ad-hoc job.
	b := &app{cfg: &config.Config{Backups: config.BackupsConfig{Targets: map[string]config.BackupTarget{"local": {Repository: "/b"}}}}, destName: "production"}
	if j, err := b.selectBackupJob(""); err != nil || j.targetName != "local" || j.name != "" {
		t.Fatalf("ad-hoc: %+v %v", j, err)
	}
}

func TestLinuxBinaryRefusesNonLinux(t *testing.T) {
	if _, err := linuxBinary(t.Context(), "", "darwin/arm64", &strings.Builder{}); err == nil {
		t.Fatal("want error")
	}
	if p, err := linuxBinary(t.Context(), "/x/yoho", "linux/amd64", &strings.Builder{}); err != nil || p != "/x/yoho" {
		t.Fatalf("explicit binary: %q %v", p, err)
	}
}
