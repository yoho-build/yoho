package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/ui"
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

func TestRestoreSummaryMentionsGeneratedSecrets(t *testing.T) {
	var b strings.Builder
	e := &backup.Entry{ID: "abc", Time: time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC)}
	services := map[string]config.ServiceBackup{
		"db": {Volumes: []string{"pgdata"}, Dump: []string{"pg_dump"}, RestoreDump: []string{"psql"}},
	}
	writeRestoreSummary(&b, "shop", "production", "primary", e, services, 2, true)
	if !strings.Contains(b.String(), "  - generated secrets: 2\n") {
		t.Fatalf("summary:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "db: volume pgdata") {
		t.Fatalf("summary:\n%s", b.String())
	}
	b.Reset()
	writeRestoreSummary(&b, "shop", "production", "primary", e, services, 2, false)
	if strings.Contains(b.String(), "generated secrets") {
		t.Fatalf("unencrypted target must not list generated secrets:\n%s", b.String())
	}
}

func TestWarnGeneratedSecretsChanged(t *testing.T) {
	var b strings.Builder
	u := ui.New(&b, ui.Human, false)
	warnIfGeneratedChanged(u, []string{"PG", "API_KEY"})
	want := "generated secrets PG, API_KEY changed; run `yoho deploy` so Services use the restored values"
	if !strings.Contains(b.String(), want) {
		t.Fatalf("warning:\n%s", b.String())
	}
	b.Reset()
	warnIfGeneratedChanged(u, nil)
	if b.Len() != 0 {
		t.Fatalf("unchanged secrets must not warn:\n%s", b.String())
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
