package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

func TestPullRootResolution(t *testing.T) {
	t.Setenv("YOHO_BACKUP_PULL_DIR", "/env/dir")
	if got, _ := pullRoot("/flag"); got != "/flag" {
		t.Fatalf("flag wins: %s", got)
	}
	if got, _ := pullRoot(""); got != "/env/dir" {
		t.Fatalf("env: %s", got)
	}
	t.Setenv("YOHO_BACKUP_PULL_DIR", "")
	t.Setenv("HOME", "/home/op")
	if got, _ := pullRoot(""); got != "/home/op/yoho-backups" {
		t.Fatalf("default: %s", got)
	}
}

func TestReportPullJSONEvents(t *testing.T) {
	var b strings.Builder
	u := ui.New(&b, ui.JSON, false)
	copied, skipped, conflicts, failed, n := reportPull(u, []backup.PulledArchive{
		{ID: "a.tar.gz", Server: "s1", Status: backup.PullCopied, Size: 10, SHA256: "ab", Path: "/m/a.tar.gz"},
		{ID: "b.tar.gz", Server: "s1", Status: backup.PullSkipped, Size: 5},
		{ID: "c.tar.gz", Server: "s1", Status: backup.PullConflict, Error: "differs"},
	})
	if copied != 1 || skipped != 1 || conflicts != 1 || failed != 0 || n != 10 {
		t.Fatalf("counts %d %d %d %d %d", copied, skipped, conflicts, failed, n)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"event":"backup_pull"`) || !strings.Contains(lines[0], `"sha256":"ab"`) ||
		!strings.Contains(lines[0], `"status":"copied"`) || !strings.Contains(lines[2], `"error":"differs"`) {
		t.Fatalf("events:\n%s", b.String())
	}
}

func TestBackupPullFlags(t *testing.T) {
	c := backupPullCmd(&globals{})
	for _, f := range []string{"to", "limit"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("missing --%s", f)
		}
	}
	if c.Flags().Lookup("delete") != nil {
		t.Error("pull must have no delete flag")
	}
}

// restore --from SRC reads the Backup Target's secrets from SRC's store.
func TestRestoreFromResolvesSourceTargetSecrets(t *testing.T) {
	dir := t.TempDir()
	yoho := "app: shop\nservers:\n  primary:\n    ssh: yoho@203.0.113.10\ndestinations:\n  production:\n    servers: [primary]\n  staging:\n    servers: [primary]\n" +
		"backups:\n  targets:\n    local: {repository: /tmp/yoho-test-backups, password_secret: BACKUP_PASSWORD}\n  jobs:\n    nightly: {destination: production, target: local}\n    stage: {destination: staging, target: local}\n"
	for name, body := range map[string]string{
		"yoho.yml":                 yoho,
		"compose.yaml":             "services:\n  db: {image: postgres, x-yoho: {stateful: true, backup: {volumes: [data]}}, volumes: [data:/d]}\nvolumes:\n  data: {}\n",
		".yoho/secrets.production": "BACKUP_PASSWORD=prod-pass\n",
		".yoho/secrets.staging":    "BACKUP_PASSWORD=staging-pass\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	g := &globals{configPath: filepath.Join(dir, "yoho.yml"), destination: "staging", json: true}
	a, err := g.load(backupRestoreCmd(g))
	if err != nil {
		t.Fatal(err)
	}
	a.ui = ui.New(io.Discard, ui.JSON, false)
	for dest, want := range map[string]string{"production": "prod-pass", "staging": "staging-pass"} {
		j, err := a.selectBackupJobFor("", dest)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.resolveTargetSecrets(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		if j.password != want {
			t.Errorf("job of %s restoring into staging: password %q, want %q", dest, j.password, want)
		}
	}
}

func TestRestoreFromAcceptsDestinationEnv(t *testing.T) {
	t.Setenv("YOHO_DESTINATION", "")
	g := &globals{configPath: filepath.Join(t.TempDir(), "missing.yml")}
	c := backupRestoreCmd(g)
	c.SetArgs([]string{"latest", "--from", "production"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "--from needs -d") {
		t.Fatalf("no Destination: %v", err)
	}
	t.Setenv("YOHO_DESTINATION", "staging")
	c = backupRestoreCmd(g)
	c.SetArgs([]string{"latest", "--from", "production"})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.Execute(); err == nil || strings.Contains(err.Error(), "--from needs -d") {
		t.Fatalf("$YOHO_DESTINATION must name the target: %v", err)
	}
}
