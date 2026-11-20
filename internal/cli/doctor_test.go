package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/schedule"
	"github.com/yoho-build/yoho/internal/transport"
	"github.com/yoho-build/yoho/internal/ui"
)

// doctorFakeHost is a configurable fake remote.Host for doctor unit tests.
type doctorFakeHost struct {
	name  string
	files map[string][]byte
	out   func(cmd remote.Cmd) (string, error)
}

func (f *doctorFakeHost) Target() string { return "yoho@test" }

func (f *doctorFakeHost) Name() string {
	if f.name != "" {
		return f.name
	}
	return "primary"
}

func (f *doctorFakeHost) Run(_ context.Context, cmd remote.Cmd) error {
	if f.out != nil {
		_, err := f.out(cmd)
		return err
	}
	return nil
}

func (f *doctorFakeHost) Output(_ context.Context, cmd remote.Cmd) (string, error) {
	if f.out != nil {
		return f.out(cmd)
	}
	return "", nil
}

func (f *doctorFakeHost) WriteFile(_ context.Context, _ string, _ []byte, _ os.FileMode, _ bool) error {
	return errors.New("doctor must be read-only")
}

func (f *doctorFakeHost) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	if f.files != nil {
		if b, ok := f.files[p]; ok {
			return b, nil
		}
	}
	return nil, os.ErrNotExist
}

func (f *doctorFakeHost) Close() error { return nil }

func makeDoctorApp(t *testing.T, composeYAML string, mutate func(*config.Config)) *app {
	t.Helper()
	p := appDir(t, "compose", composeYAML)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Builder.Engine = "docker"
	if mutate != nil {
		mutate(cfg)
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest := cfg.Destinations["production"]
	return &app{
		g:        &globals{configPath: p},
		dir:      filepath.Dir(p),
		path:     p,
		cfg:      cfg,
		destName: "production",
		dest:     dest,
		ui:       ui.Discard(),
	}
}

func TestDoctorConfigCheck(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })

	t.Run("valid config", func(t *testing.T) {
		a := makeDoctorApp(t, "services:\n  web:\n    image: nginx\n    restart: unless-stopped\n", nil)
		_, fs := checkDoctorConfig(context.Background(), a)
		if len(fs) != 1 || fs[0].Severity != severityInfo || !strings.Contains(fs[0].Message, "config valid") {
			t.Fatalf("got %+v", fs)
		}
	})

	t.Run("stateful service without backup comes before non-stateful", func(t *testing.T) {
		src := `services:
  db:
    image: postgres:16
    restart: unless-stopped
    volumes: [pgdata:/var/lib/postgresql/data]
    x-yoho:
      stateful: true
  cache:
    image: redis:7
    restart: unless-stopped
    x-yoho:
      backup: {volumes: []}
volumes:
  pgdata: {}
`
		a := makeDoctorApp(t, src, nil)
		_, fs := checkDoctorConfig(context.Background(), a)
		var backupSubjects []string
		for _, f := range fs {
			if f.Check == "backup" {
				backupSubjects = append(backupSubjects, f.Subject)
				if f.Severity != severityWarning || f.Hint == "" {
					t.Errorf("backup finding should be warning with hint: %+v", f)
				}
			}
		}
		if len(backupSubjects) != 2 || backupSubjects[0] != "db" || backupSubjects[1] != "cache" {
			t.Fatalf("want [db cache], got %v (all: %+v)", backupSubjects, fs)
		}
	})

	t.Run("non-proxied service publishing public port", func(t *testing.T) {
		src := `services:
  api:
    image: nginx
    restart: unless-stopped
    ports: ["8080:8080", "127.0.0.1:9090:9090"]
`
		a := makeDoctorApp(t, src, nil)
		_, fs := checkDoctorConfig(context.Background(), a)
		found := false
		for _, f := range fs {
			if f.Check == "ports" && f.Subject == "api" && strings.Contains(f.Message, "8080:8080") && !strings.Contains(f.Message, "9090") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected public port warning for api, got %+v", fs)
		}
	})

	t.Run("missing secret key is an error", func(t *testing.T) {
		src := "services:\n  web: {image: nginx, restart: unless-stopped, x-yoho: {secrets: {API_KEY: MISSING}}}\n"
		a := makeDoctorApp(t, src, nil)
		_, fs := checkDoctorConfig(context.Background(), a)
		hasErr := false
		for _, f := range fs {
			if f.Severity == severityError && strings.Contains(f.Message, "MISSING") {
				hasErr = true
			}
		}
		if !hasErr {
			t.Fatalf("expected error finding for MISSING secret, got %+v", fs)
		}
	})
}

func TestDoctorScheduleMaxAge(t *testing.T) {
	for _, tc := range []struct {
		expr, fixture string
		gap           time.Duration
	}{
		{"Monday *-*-* 03:00:00", "weekly", 7 * 24 * time.Hour},
		{"Mon *-*-01..07 03:00:00", "monthly", 35 * 24 * time.Hour},
		{"*-*-* 1/3:00:00", "step", 3 * time.Hour},
		{"daily", "dst", 23 * time.Hour},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			recorded, err := os.ReadFile(filepath.Join("testdata", "systemd-calendar-"+tc.fixture+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			h := &doctorFakeHost{out: func(c remote.Cmd) (string, error) {
				if !strings.Contains(c.Script, "--iterations=2 "+remote.Quote(tc.expr)) {
					t.Fatalf("wrong calendar command: %s", c.Script)
				}
				return string(recorded), nil
			}}
			got, fallback, err := scheduleMaxAge(context.Background(), h, tc.expr)
			if err != nil || fallback || got != 2*tc.gap+time.Hour {
				t.Fatalf("got %s, fallback=%v, err=%v", got, fallback, err)
			}
		})
	}
	for _, tc := range []struct {
		name, output      string
		commandErr        error
		fallback, wantErr bool
	}{
		{name: "unavailable", output: "yoho-systemd-analyze-unavailable\n", fallback: true},
		{name: "invalid calendar", commandErr: errors.New("Invalid argument"), wantErr: true},
		{name: "no future elapse", output: "Next elapse: never\n", wantErr: true},
		{name: "unreadable output", output: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &doctorFakeHost{out: func(remote.Cmd) (string, error) { return tc.output, tc.commandErr }}
			got, fallback, err := scheduleMaxAge(context.Background(), h, "daily")
			if (err != nil) != tc.wantErr || fallback != tc.fallback || (fallback && got != 8*24*time.Hour) {
				t.Fatalf("got %s %v %v", got, fallback, err)
			}
		})
	}
}

func TestDoctorBackupsAndDrill(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	release.Root = "/var/lib/yoho"

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clockStr := fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond())

	makeHost := func(jobName, sched, timerState string, state schedule.JobState, auditLines string) *doctorFakeHost {
		spec := schedule.Spec{
			Version: 1, App: "shop", Destination: "production", Job: jobName,
			Schedule: sched,
		}
		specBytes, _ := json.Marshal(spec)
		stateBytes, _ := json.Marshal(map[string]schedule.JobState{jobName: state})
		files := map[string][]byte{
			schedule.SpecPath("shop", "production", jobName): specBytes,
			schedule.StatePath("shop", "production"):         stateBytes,
		}
		return &doctorFakeHost{
			name:  "primary",
			files: files,
			out: func(cmd remote.Cmd) (string, error) {
				s := cmd.Script
				switch {
				case strings.Contains(s, "ls -1"):
					return fmt.Sprintf("shop-production-%s.json\n", jobName), nil
				case strings.Contains(s, "systemctl") && strings.Contains(s, "show"):
					return fmt.Sprintf("ActiveState=%s\nNextElapseUSecRealtime=tomorrow\nLastTriggerUSec=today\nResult=success\n", timerState), nil
				case strings.Contains(s, "systemd-analyze"):
					return "Next elapse: Sat 2026-11-21 03:00:00 UTC\nIter. #2: Sun 2026-11-22 03:00:00 UTC\n", nil
				case strings.Contains(s, "date -u +%s.%N"):
					return clockStr, nil
				case strings.Contains(s, "audit"):
					return auditLines, nil
				default:
					return "", nil
				}
			},
		}
	}

	okCode := 0
	failCode := 1
	drillRecord := func(at time.Time, exit *int, cleanup bool) string {
		r := audit.Record{
			Time: at, Run: "run1", Phase: audit.Finished,
			Command: "backup drill", App: "shop", Destination: "production", Exit: exit,
		}
		if cleanup {
			r.Args = map[string]any{"cleanup": true}
		}
		b, _ := r.Line()
		return string(b) + "\n"
	}

	t.Run("healthy backup and recent drill", func(t *testing.T) {
		a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", func(c *config.Config) {
			c.App = "shop"
			c.Backups = config.BackupsConfig{
				Targets: map[string]config.BackupTarget{"local": {Type: "archive", Repository: "/backups"}},
				Jobs:    map[string]config.BackupJob{"nightly": {Destination: "production", Target: "local", Schedule: "*-*-* 03:00:00"}},
			}
		})
		lastOK := now.Add(-6 * time.Hour)
		h := makeHost("nightly", "*-*-* 03:00:00", "active",
			schedule.JobState{LastRun: lastOK, LastSuccess: &lastOK, BackupID: "snap1"},
			drillRecord(now.Add(-10*24*time.Hour), &okCode, false),
		)
		fs := checkDoctorBackups(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, 0, true, now)
		for _, f := range fs {
			if f.Severity != severityInfo {
				t.Fatalf("expected only info findings, got %+v", fs)
			}
		}
		if len(fs) != 2 {
			t.Fatalf("expected 2 info findings (backup + drill), got %+v", fs)
		}
	})

	t.Run("stale backup and inactive timer and no valid drill", func(t *testing.T) {
		a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", func(c *config.Config) {
			c.App = "shop"
			c.Backups = config.BackupsConfig{
				Targets: map[string]config.BackupTarget{"local": {Type: "archive", Repository: "/backups"}},
				Jobs: map[string]config.BackupJob{
					"nightly": {Destination: "production", Target: "local", Schedule: "*-*-* 03:00:00"},
					"manual":  {Destination: "production", Target: "local"},
				},
			}
		})
		old := now.Add(-72 * time.Hour)
		// Only a failed drill and a --cleanup drill exist: neither counts as a successful drill.
		auditData := drillRecord(now.Add(-24*time.Hour), &failCode, false) + drillRecord(now.Add(-12*time.Hour), &okCode, true)
		h := makeHost("nightly", "*-*-* 03:00:00", "inactive",
			schedule.JobState{LastRun: now.Add(-1 * time.Hour), LastSuccess: &old, Error: "disk full"},
			auditData,
		)
		fs := checkDoctorBackups(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, 0, true, now)
		var hasStale, hasInactive, hasDrillWarn, hasManualWarn bool
		for _, f := range fs {
			if f.Check == "backup" && f.Severity == severityError && strings.Contains(f.Message, "older than") {
				hasStale = true
			}
			if f.Check == "backup" && f.Severity == severityError && strings.Contains(f.Message, "inactive") {
				hasInactive = true
			}
			if f.Check == "drill" && f.Severity == severityWarning && strings.Contains(f.Message, "no successful restore drill") {
				hasDrillWarn = true
			}
			if f.Check == "backup" && f.Severity == severityWarning && f.Subject == "manual" {
				hasManualWarn = true
			}
		}
		if !hasStale || !hasInactive || !hasDrillWarn || !hasManualWarn {
			t.Fatalf("missing expected findings: stale=%v inactive=%v drill=%v manual=%v\nall: %+v", hasStale, hasInactive, hasDrillWarn, hasManualWarn, fs)
		}
	})

	t.Run("never-success and stale drill (>90d) and max-age override", func(t *testing.T) {
		a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", func(c *config.Config) {
			c.App = "shop"
		})
		h := makeHost("nightly", "*-*-* 03:00:00", "active",
			schedule.JobState{},
			drillRecord(now.Add(-120*24*time.Hour), &okCode, false),
		)
		fs := checkDoctorBackups(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, 0, true, now)
		var hasNever, hasOldDrill bool
		for _, f := range fs {
			if f.Check == "backup" && f.Severity == severityError && strings.Contains(f.Message, "no successful Backup yet") {
				hasNever = true
			}
			if f.Check == "drill" && f.Severity == severityWarning && strings.Contains(f.Message, "last successful restore drill was on") {
				hasOldDrill = true
			}
		}
		if !hasNever || !hasOldDrill {
			t.Fatalf("want never-success and old drill warning, got %+v", fs)
		}
	})
}

func TestDoctorFirewall(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", nil)

	t.Run("active ufw with required ports", func(t *testing.T) {
		h := &doctorFakeHost{
			name: "primary",
			out: func(cmd remote.Cmd) (string, error) {
				switch {
				case strings.Contains(cmd.Script, "id -u"):
					return "root", nil
				case strings.Contains(cmd.Script, "sshd -T"):
					return "port 22\n", nil
				case strings.Contains(cmd.Script, "ufw status"):
					return "Status: active\n\nTo Action From\n22/tcp ALLOW Anywhere\n80/tcp ALLOW Anywhere\n443/tcp ALLOW Anywhere\n", nil
				default:
					return "", nil
				}
			},
		}
		fs := checkDoctorFirewall(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
		if len(fs) != 1 || fs[0].Severity != severityInfo || !strings.Contains(fs[0].Message, "ok") {
			t.Fatalf("got %+v", fs)
		}
	})

	t.Run("inactive ufw", func(t *testing.T) {
		h := &doctorFakeHost{
			name: "primary",
			out: func(cmd remote.Cmd) (string, error) {
				switch {
				case strings.Contains(cmd.Script, "id -u"):
					return "root", nil
				case strings.Contains(cmd.Script, "ufw status"):
					return "Status: inactive\n", nil
				default:
					return "", nil
				}
			},
		}
		fs := checkDoctorFirewall(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
		if len(fs) != 1 || fs[0].Severity != severityWarning || !strings.Contains(fs[0].Message, "enable ufw") || fs[0].Hint == "" {
			t.Fatalf("got %+v", fs)
		}
	})

	t.Run("firewall disabled in config", func(t *testing.T) {
		off := false
		aOff := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", func(c *config.Config) {
			c.Setup.Firewall = &off
		})
		fs := checkDoctorFirewall(context.Background(), aOff, plan.NamedHost{Name: "primary", Host: &doctorFakeHost{}})
		if len(fs) != 1 || fs[0].Severity != severityInfo || !strings.Contains(fs[0].Message, "skipped") {
			t.Fatalf("got %+v", fs)
		}
	})
}

func TestDoctorPublishedPorts(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", nil)
	h := &doctorFakeHost{
		name: "primary",
		out: func(cmd remote.Cmd) (string, error) {
			return strings.Join([]string{
				proxy.ContainerName + "\t0.0.0.0:80->80/tcp, 0.0.0.0:443->443/tcp",
				"app-db-1\t0.0.0.0:5432->5432/tcp, :::5432->5432/tcp",
				"app-redis-1\t127.0.0.1:6379->6379/tcp",
				"app-worker-1\t",
			}, "\n"), nil
		},
	}
	fs := checkDoctorPublishedPorts(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding (db only), got %+v", fs)
	}
	if fs[0].Severity != severityWarning || !strings.Contains(fs[0].Message, "app-db-1") || !strings.Contains(fs[0].Message, "0.0.0.0:5432->5432/tcp") {
		t.Fatalf("unexpected finding: %+v", fs[0])
	}
}

func TestDoctorDisk(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", nil)

	t.Run("shared mount healthy", func(t *testing.T) {
		h := &doctorFakeHost{
			name: "primary",
			out: func(cmd remote.Cmd) (string, error) {
				// 100 GiB total, 40 GiB used, 60 GiB avail (in 1K blocks)
				return "docker\t/var/lib/docker\t/\t104857600\t41943040\t62914560\n" +
					"backup-cache\t/var/lib/yoho/backups/app/production/.restic-cache\t/\t104857600\t41943040\t62914560\n", nil
			},
		}
		fs := checkDoctorDisk(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
		if len(fs) != 1 || fs[0].Severity != severityInfo || !strings.Contains(fs[0].Message, "Docker data root and Backup cache") {
			t.Fatalf("got %+v", fs)
		}
	})

	t.Run("separate mounts warning and error", func(t *testing.T) {
		h := &doctorFakeHost{
			name: "primary",
			out: func(cmd remote.Cmd) (string, error) {
				// docker: 100 GiB total, 88 GiB used, 12 GiB avail -> warning (88% >= 85%)
				// backup-cache: 100 GiB total, 96 GiB used, 4 GiB avail -> error (96% >= 95%)
				return "docker\t/var/lib/docker\t/var/lib/docker\t104857600\t92274688\t12582912\n" +
					"backup-cache\t/mnt/backups/.restic-cache\t/mnt/backups\t104857600\t100663296\t4194304\n", nil
			},
		}
		fs := checkDoctorDisk(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
		if len(fs) != 2 {
			t.Fatalf("expected 2 findings, got %+v", fs)
		}
		if fs[0].Severity != severityWarning || fs[0].Hint == "" {
			t.Errorf("docker disk should warn: %+v", fs[0])
		}
		if fs[1].Severity != severityError || fs[1].Hint == "" {
			t.Errorf("backup-cache disk should error: %+v", fs[1])
		}
	})
}

func TestDoctorProxy(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", nil)
	wantImg := proxy.Image(a.proxyConfig())

	cases := []struct {
		name       string
		inspectOut string
		needsProxy bool
		wantSev    string
		wantSubstr string
	}{
		{"running pinned", "abc|true|" + wantImg, true, severityInfo, "up to date"},
		{"running older", "abc|true|basecamp/kamal-proxy:v0.9.0", true, severityWarning, "pinned version"},
		{"running newer", "abc|true|basecamp/kamal-proxy:v9.9.9", true, severityInfo, "newer than pinned"},
		{"stopped with proxied service", "abc|false|" + wantImg, true, severityError, "stopped"},
		{"missing with proxied service", "", true, severityError, "not installed"},
		{"missing without proxied service", "", false, severityInfo, "no proxied Services"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &doctorFakeHost{
				name: "primary",
				out: func(cmd remote.Cmd) (string, error) {
					return tc.inspectOut, nil
				},
			}
			fs := checkDoctorProxy(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, tc.needsProxy)
			if len(fs) != 1 || fs[0].Severity != tc.wantSev || !strings.Contains(fs[0].Message, tc.wantSubstr) {
				t.Fatalf("got %+v, want sev=%s substr=%q", fs, tc.wantSev, tc.wantSubstr)
			}
		})
	}
}

func TestDoctorPendingRotations(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", nil)

	t.Run("none pending", func(t *testing.T) {
		h := &doctorFakeHost{name: "primary", out: func(remote.Cmd) (string, error) { return "", nil }}
		fs := checkDoctorPendingRotations(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
		if len(fs) != 1 || fs[0].Severity != severityInfo {
			t.Fatalf("got %+v", fs)
		}
	})

	t.Run("pending key", func(t *testing.T) {
		h := &doctorFakeHost{name: "primary", out: func(remote.Cmd) (string, error) { return "POSTGRES_PASSWORD\n", nil }}
		fs := checkDoctorPendingRotations(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
		if len(fs) != 1 || fs[0].Severity != severityError || !strings.Contains(fs[0].Message, "POSTGRES_PASSWORD") || !strings.Contains(fs[0].Hint, "--generated POSTGRES_PASSWORD") {
			t.Fatalf("got %+v", fs)
		}
	})
}

func TestDoctorTransport(t *testing.T) {
	cases := []struct {
		name       string
		builderLoc string
		mode       string
		pusshOK    bool
		driverJSON string
		wantSev    string
		wantSubstr string
	}{
		{"auto pussh ready", "local", "auto", true, `[["driver-type","io.containerd.snapshotter.v1"]]`, severityInfo, "transport: pussh"},
		{"auto missing pussh", "local", "auto", false, `[["driver-type","io.containerd.snapshotter.v1"]]`, severityInfo, "docker pussh"},
		{"auto missing containerd", "local", "auto", true, `[["Backing Filesystem","extfs"]]`, severityInfo, "containerd image store"},
		{"explicit pussh missing containerd", "local", "pussh", true, `[["Backing Filesystem","extfs"]]`, severityError, "unavailable"},
		{"explicit load", "local", "load", true, "", severityInfo, "full archive"},
		{"explicit registry", "local", "registry", false, "", severityInfo, "registry"},
		{"server builder", "server", "auto", false, "", severityInfo, "built on Server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", func(c *config.Config) {
				c.Builder.Location = tc.builderLoc
				c.Transport.Mode = tc.mode
			})
			h := &doctorFakeHost{
				name: "primary",
				out:  func(remote.Cmd) (string, error) { return tc.driverJSON, nil },
			}
			fs := checkDoctorTransport(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, tc.pusshOK)
			if len(fs) != 1 || fs[0].Severity != tc.wantSev || !strings.Contains(fs[0].Message, tc.wantSubstr) {
				t.Fatalf("got %+v, want sev=%s substr=%q", fs, tc.wantSev, tc.wantSubstr)
			}
		})
	}
}

func TestDoctorEndToEndJSONAndExitCodes(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })

	compose := `services:
  web:
    image: nginx
    restart: unless-stopped
    healthcheck: {test: ["CMD", "true"]}
    x-yoho:
      proxy: {hosts: [app.example.com], port: 80}
`
	p := appDir(t, "compose", compose)
	wantProxyImg := proxy.Image(config.ProxyConfig{})

	run := func(t *testing.T, jsonOut bool, proxyRunning bool, pendingKey string) (string, error) {
		t.Helper()
		h := &doctorFakeHost{
			name: "primary",
			out: func(cmd remote.Cmd) (string, error) {
				s := cmd.Script
				switch {
				case strings.Contains(s, "ls -1"):
					return "", nil
				case strings.Contains(s, "generated"):
					return pendingKey, nil
				case strings.Contains(s, "id -u"):
					return "root", nil
				case strings.Contains(s, "sshd -T"):
					return "port 22\n", nil
				case strings.Contains(s, "ufw status"):
					return "Status: active\n22/tcp ALLOW Anywhere\n80/tcp ALLOW Anywhere\n443/tcp ALLOW Anywhere\n", nil
				case strings.Contains(s, "docker ps --filter"):
					return "", nil
				case strings.Contains(s, "df -Pk"):
					return "docker\t/var/lib/docker\t/\t104857600\t41943040\t62914560\n" +
						"backup-cache\t/var/lib/yoho/backups/app/production/.restic-cache\t/\t104857600\t41943040\t62914560\n", nil
				case strings.Contains(s, "container inspect"):
					if proxyRunning {
						return "abc|true|" + wantProxyImg, nil
					}
					return "abc|false|" + wantProxyImg, nil
				case strings.Contains(s, "DriverStatus"):
					return `[["driver-type","io.containerd.snapshotter.v1"]]`, nil
				default:
					return "", nil
				}
			},
		}
		g := &globals{configPath: p, json: jsonOut}
		cmd := doctorCmd(g)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetContext(context.Background())
		err := runDoctor(cmd, g, doctorOptions{
			connect: func(_ context.Context, _ *app) ([]plan.NamedHost, func(), error) {
				return []plan.NamedHost{{Name: "primary", Host: h}}, func() {}, nil
			},
			localExec: func(_ context.Context, _ transport.Command) error { return nil },
			now:       func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) },
		})
		return buf.String(), err
	}

	t.Run("healthy exits 0 and emits findings then doctor summary", func(t *testing.T) {
		out, err := run(t, true, true, "")
		if exitCodeOf(err) != 0 {
			t.Fatalf("want exit 0, got %d (%v)\n%s", exitCodeOf(err), err, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var findings int
		var last map[string]any
		for _, line := range lines {
			var ev map[string]any
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("invalid NDJSON %q: %v", line, err)
			}
			if ev["event"] == "finding" {
				findings++
			}
			last = ev
		}
		if findings == 0 {
			t.Fatalf("expected finding events:\n%s", out)
		}
		if last["event"] != "doctor" || last["ok"] != true || last["errors"] != float64(0) {
			t.Fatalf("unexpected closing event: %v", last)
		}
	})

	t.Run("errors exit 1 and report error count", func(t *testing.T) {
		out, err := run(t, true, false, "POSTGRES_PASSWORD\n")
		if exitCodeOf(err) != 1 {
			t.Fatalf("want exit 1, got %d (%v)\n%s", exitCodeOf(err), err, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var last map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
			t.Fatalf("invalid closing NDJSON: %v", err)
		}
		if last["event"] != "doctor" || last["ok"] != false || last["errors"].(float64) < 2 {
			t.Fatalf("unexpected closing event: %v\nfull:\n%s", last, out)
		}
	})

	t.Run("human output includes findings and summary", func(t *testing.T) {
		out, err := run(t, false, false, "")
		if exitCodeOf(err) != 1 {
			t.Fatalf("want exit 1, got %d", exitCodeOf(err))
		}
		if !strings.Contains(out, "Doctor (production):") || !strings.Contains(out, "hint:") {
			t.Fatalf("unexpected human output:\n%s", out)
		}
	})
}

func TestDoctorMissingScheduledBackupAndUnreadableTimer(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx, restart: unless-stopped}\n", func(c *config.Config) {
		c.App = "shop"
		c.Backups.Jobs = map[string]config.BackupJob{"nightly": {Destination: "production", Schedule: "daily"}}
	})
	h := &doctorFakeHost{}
	fs := checkDoctorBackups(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, 48*time.Hour, false, time.Now())
	found := false
	for _, f := range fs {
		if f.Subject == "nightly" && strings.Contains(f.Message, "not installed") {
			found = f.Severity == severityError && strings.Contains(f.Hint, "yoho schedule install")
		}
	}
	if !found || exitCodeOf(emitDoctorResults(a, fs)) != 1 {
		t.Fatalf("missing job must fail doctor: %+v", fs)
	}

	spec, _ := json.Marshal(schedule.Spec{App: "shop", Destination: "production", Job: "nightly", Schedule: "daily"})
	now := time.Now().UTC().Truncate(time.Second)
	state, _ := json.Marshal(map[string]schedule.JobState{"nightly": {LastSuccess: &now}})
	h.files = map[string][]byte{schedule.SpecPath("shop", "production", "nightly"): spec, schedule.StatePath("shop", "production"): state}
	h.out = func(c remote.Cmd) (string, error) {
		switch {
		case strings.Contains(c.Script, "ls -1"):
			return "shop-production-nightly.json\n", nil
		case strings.Contains(c.Script, "systemctl"):
			return "", errors.New("show failed")
		case strings.Contains(c.Script, "date -u"):
			return fmt.Sprintf("%d.000000000\n", now.Unix()), nil
		}
		return "", nil
	}
	fs = checkDoctorBackups(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, 48*time.Hour, false, now)
	for _, f := range fs {
		if f.Subject == "nightly" && f.Message == "timer state unreadable" && f.Severity == severityWarning {
			return
		}
	}
	t.Fatalf("missing unreadable timer warning: %+v", fs)
}

func TestDoctorDrillScansAllHistoryAndPairsRuns(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx}\n", nil)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var log strings.Builder
	add := func(r audit.Record) {
		b, err := r.Line()
		if err != nil {
			t.Fatal(err)
		}
		log.Write(b)
		log.WriteByte('\n')
	}
	zero := 0
	add(audit.Record{Time: now.Add(-48 * time.Hour), Run: "good", Command: "backup drill", Phase: audit.Started})
	add(audit.Record{Time: now.Add(-47 * time.Hour), Run: "good", Command: "backup drill", Phase: audit.Finished, Exit: &zero})
	add(audit.Record{Time: now.Add(-24 * time.Hour), Run: "cleanup", Command: "backup drill", Phase: audit.Started, Args: map[string]any{"cleanup": true}})
	for i := 0; i < 600; i++ {
		add(audit.Record{Time: now.Add(-12 * time.Hour), Run: fmt.Sprint(i), Command: "deploy", Phase: audit.Finished, Exit: &zero})
	}
	add(audit.Record{Time: now.Add(-time.Hour), Run: "cleanup", Command: "backup drill", Phase: audit.Finished, Exit: &zero})
	h := &doctorFakeHost{out: func(c remote.Cmd) (string, error) {
		if strings.Contains(c.Script, "tail -n") || !strings.Contains(c.Script, "cat ") {
			t.Fatalf("audit scan truncated history: %s", c.Script)
		}
		return log.String(), nil
	}}
	f := checkDoctorDrill(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, now)
	if f.Severity != severityInfo || !strings.Contains(f.Message, "2026-11-18") {
		t.Fatalf("wrong successful drill: %+v", f)
	}
}

func TestDoctorSwarmPublishedPorts(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  api:\n    image: nginx\n    ports: ['127.0.0.1:8080:80', '192.0.2.1:9090:90']\n", func(c *config.Config) {
		d := c.Destinations["production"]
		d.Runtime = "swarm"
		c.Destinations["production"] = d
	})
	_, fs := checkDoctorConfig(context.Background(), a)
	found := false
	for _, f := range fs {
		if f.Check == "ports" && strings.Contains(f.Message, "8080:80") && strings.Contains(f.Message, "9090:90") {
			found = true
			if strings.Contains(f.Hint, "127.0.0.1") || !strings.Contains(f.Message, "Swarm ingress") {
				t.Fatalf("incorrect Swarm guidance: %+v", f)
			}
		}
	}
	if !found {
		t.Fatalf("missed interface-specific Swarm ports: %+v", fs)
	}
	h := &doctorFakeHost{out: func(c remote.Cmd) (string, error) {
		if !strings.Contains(c.Script, "docker service ls --filter 'label=com.docker.stack.namespace="+a.project()+"'") || strings.Contains(c.Script, "docker ps") {
			t.Fatalf("wrong Swarm query: %s", c.Script)
		}
		return "app_api\t*:8080->80/tcp, *:9090->90/tcp\n", nil
	}}
	fs = checkDoctorPublishedPorts(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "8080") || strings.Contains(fs[0].Hint, "127.0.0.1") {
		t.Fatalf("wrong live Swarm finding: %+v", fs)
	}
}

func TestDoctorDiskReservedSpaceAndNoAvailableSpace(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  web: {image: nginx}\n", nil)
	for _, tc := range []struct{ name, df, severity, pct string }{
		{"reserved blocks", "104857600\t96468992\t4194304", severityError, "96%"},
		{"small full volume", "1024\t0\t0", severityError, "100%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &doctorFakeHost{out: func(remote.Cmd) (string, error) { return "docker\t/data\t/data\t" + tc.df + "\n", nil }}
			fs := checkDoctorDisk(context.Background(), a, plan.NamedHost{Name: "primary", Host: h})
			if len(fs) != 1 || fs[0].Severity != tc.severity || !strings.Contains(fs[0].Message, tc.pct) {
				t.Fatalf("wrong disk finding: %+v", fs)
			}
		})
	}
}

func TestDoctorContainerTransportAndLocalPussh(t *testing.T) {
	for _, mode := range []string{"auto", "load", "pussh", "registry"} {
		t.Run(mode, func(t *testing.T) {
			a := makeDoctorApp(t, "services:\n  web: {build: .}\n", func(c *config.Config) { c.Builder.Engine = "container"; c.Transport.Mode = mode })
			h := &doctorFakeHost{out: func(remote.Cmd) (string, error) {
				t.Fatal("Apple container should not probe Docker store")
				return "", nil
			}}
			fs := checkDoctorTransport(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, true)
			if len(fs) != 1 {
				t.Fatal(fs)
			}
			if mode == "auto" || mode == "load" {
				if fs[0].Severity != severityInfo || !strings.Contains(fs[0].Message, "archive load (Apple container)") || strings.Contains(fs[0].Hint, "pussh") {
					t.Fatal(fs)
				}
			} else if fs[0].Severity != severityError || !strings.Contains(fs[0].Message, "needs images in the local Docker") {
				t.Fatal(fs)
			}
		})
	}
	a := makeDoctorApp(t, "services:\n  web: {build: .}\n", func(c *config.Config) { c.Transport.Mode = "pussh" })
	fs := checkDoctorTransport(context.Background(), a, plan.NamedHost{Name: "local", Host: &remote.Local{}}, true)
	if len(fs) != 1 || fs[0].Severity != severityError || !strings.Contains(fs[0].Message, "no ssh target") {
		t.Fatalf("local pussh: %+v", fs)
	}
}

func TestDoctorInvalidMaxAgeJSON(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	p := appDir(t, "compose", "services:\n  web: {image: nginx}\n")
	for _, age := range []string{"0", "-1h", "invalid"} {
		t.Run(age, func(t *testing.T) {
			g := &globals{configPath: p, json: true}
			cmd := doctorCmd(g)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"--max-age", age})
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			err := cmd.Execute()
			if exitCodeOf(err) != 1 {
				t.Fatalf("got %v", err)
			}
			var events []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				var ev map[string]any
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					t.Fatal(err)
				}
				events = append(events, ev)
			}
			if len(events) != 2 || events[0]["event"] != "finding" || events[1]["event"] != "doctor" || events[1]["destination"] != "production" || events[1]["ok"] != false {
				t.Fatalf("bad JSON: %s", out.String())
			}
		})
	}
}

func TestDoctorOfflineValidationContinuesAfterSecretKeysError(t *testing.T) {
	a := makeDoctorApp(t, "services:\n  db:\n    image: postgres\n    x-yoho: {stateful: true}\n", nil)
	if err := os.MkdirAll(filepath.Join(a.dir, ".yoho", "secrets"), 0700); err != nil {
		t.Fatal(err)
	}
	_, fs := checkDoctorConfig(context.Background(), a)
	var secretError, backupWarning bool
	for _, f := range fs {
		if f.Check == "config" && f.Severity == severityError && strings.Contains(f.Message, "secrets") {
			secretError = true
		}
		if f.Check == "backup" && f.Subject == "db" {
			backupWarning = true
		}
	}
	if !secretError || !backupWarning {
		t.Fatalf("validation stopped on secret key error: %+v", fs)
	}
}

func TestDoctorValidatesPreviewTemplate(t *testing.T) {
	g := previewFixture(t, "")
	writePreviewTestFile(t, filepath.Join(filepath.Dir(g.configPath), "compose.staging.yaml"), "services:\n  api:\n    image: nginx\n    x-yoho:\n      proxy: {hosts: [api.example.com], port: 80}\n")
	cmd := doctorCmd(g)
	cmd.SetContext(context.Background())
	a, err := g.load(cmd)
	if err != nil {
		t.Fatal(err)
	}
	_, fs := checkDoctorConfig(context.Background(), a)
	for _, f := range fs {
		if f.Severity == severityError && strings.Contains(f.Message, "{service}") {
			return
		}
	}
	t.Fatalf("doctor missed invalid preview: %+v", fs)
}

func TestDoctorSkipsProxyAfterComposeLoadFailure(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	p := appDir(t, "compose", "services: [invalid")
	g := &globals{configPath: p, json: true}
	cmd := doctorCmd(g)
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	h := &doctorFakeHost{out: func(c remote.Cmd) (string, error) {
		if strings.Contains(c.Script, "container inspect") {
			t.Fatal("Proxy should be skipped")
		}
		return "", nil
	}}
	err := runDoctor(cmd, g, doctorOptions{connect: func(context.Context, *app) ([]plan.NamedHost, func(), error) {
		return []plan.NamedHost{{Name: "primary", Host: h}}, func() {}, nil
	}, localExec: func(context.Context, transport.Command) error { return nil }})
	if exitCodeOf(err) != 1 || !strings.Contains(out.String(), "Proxy check skipped") || strings.Contains(out.String(), "no proxied Services") {
		t.Fatalf("misleading proxy after config failure: %v\n%s", err, out.String())
	}
}

func TestDoctorAutoEngineOnAppleSilicon(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("Apple silicon engine selection")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "container"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	a := makeDoctorApp(t, "services:\n  web: {build: .}\n", func(c *config.Config) { c.Builder.Engine = "auto"; c.Transport.Mode = "auto" })
	h := &doctorFakeHost{out: func(remote.Cmd) (string, error) {
		t.Fatal("auto Apple container should not probe Docker")
		return "", nil
	}}
	fs := checkDoctorTransport(context.Background(), a, plan.NamedHost{Name: "primary", Host: h}, true)
	if len(fs) != 1 || fs[0].Severity != severityInfo || !strings.Contains(fs[0].Message, "archive load (Apple container)") {
		t.Fatalf("auto engine: %+v", fs)
	}
}
