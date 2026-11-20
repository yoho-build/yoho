package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	cases := []struct {
		sched string
		want  time.Duration
	}{
		{"minutely", time.Hour},
		{"hourly", 3 * time.Hour},
		{"daily", 48 * time.Hour},
		{"weekly", 8 * 24 * time.Hour},
		{"monthly", 35 * 24 * time.Hour},
		{"yearly", 370 * 24 * time.Hour},
		{"Mon *-*-* 03:00:00", 8 * 24 * time.Hour},
		{"*-*-* 03:00:00", 48 * time.Hour},
		{"*-*-* *:00:00", 3 * time.Hour},
		{"*-*-* */6:00:00", 13 * time.Hour},
		{"*-*-01 03:00:00", 35 * 24 * time.Hour},
		{"custom-unknown", 48 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.sched, func(t *testing.T) {
			if got := scheduleMaxAge(tc.sched); got != tc.want {
				t.Errorf("scheduleMaxAge(%q) = %s, want %s", tc.sched, got, tc.want)
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
