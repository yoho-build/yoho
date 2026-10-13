package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/schedule"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestReliabilityCLIBadInputBeforeConnection(t *testing.T) {
	for _, args := range [][]string{{"--max-age", "0"}, {"--max-age", "-1h"}, {"--max-age", "weekly"}, {"unexpected"}} {
		c := scheduleStatusCmd(&globals{configPath: "/does-not-exist"})
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		err := c.Execute()
		if err == nil || strings.Contains(err.Error(), "does-not-exist") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--id", ""}, {"--id", "latest", "--read-data"}, {"one", "two"}, {"--read-data=not-bool"}} {
		c := backupCheckCmd(&globals{configPath: "/does-not-exist"})
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(args)
		err := c.Execute()
		if err == nil || strings.Contains(err.Error(), "does-not-exist") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
}
func TestBackupCheckTargetFlagsRejectedWithoutRemote(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	for _, tc := range []struct {
		typ  string
		args []string
		want string
	}{{"archive", []string{"--read-data"}, "only supported for restic"}, {"restic", []string{"--id", "latest"}, "only supported for archive"}} {
		dir := t.TempDir()
		p := filepath.Join(dir, "yoho.json")
		cfg := config.Config{App: "shop", Servers: map[string]config.Server{"test": {SSH: "invalid@no-server", Root: dir}}, Destinations: map[string]config.Destination{"production": {Servers: []string{"test"}}}, Backups: config.BackupsConfig{Targets: map[string]config.BackupTarget{"local": {Type: tc.typ, Repository: "/not-a-real-target"}}}}
		b, _ := json.Marshal(cfg)
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		c := backupCheckCmd(&globals{configPath: p})
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs(tc.args)
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.typ, err)
		}
	}
}
func TestScheduleStatusNormalAndExplicitExitFacts(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	week := now.Add(-6 * 24 * time.Hour)
	future := now.Add(time.Second)
	for _, tc := range []struct {
		name  string
		stamp *time.Time
		age   time.Duration
		want  string
		fail  bool
	}{
		{"normal-failed", &week, 0, "", false}, {"healthy-with-last-failure", &recent, 48 * time.Hour, "healthy", false}, {"never", nil, 48 * time.Hour, "never-success", true}, {"stale", &week, 48 * time.Hour, "stale", true}, {"weekly-explicit-margin", &week, 192 * time.Hour, "healthy", false}, {"future", &future, 48 * time.Hour, "future-success", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			u := ui.New(&out, ui.JSON, false)
			err := writeScheduleStatus(u, []schedule.JobStatus{{Job: "nightly", Schedule: "weekly", State: schedule.JobState{LastSuccess: tc.stamp, Error: "last attempt failed"}}}, tc.age, now)
			if (err != nil) != tc.fail {
				t.Fatalf("%v %s", err, out.String())
			}
			var row map[string]any
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				var e map[string]any
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatal(err)
				}
				if e["event"] == "row" {
					row = e
				}
			}
			if tc.age > 0 {
				if row["success_health"] != tc.want || row["max_age"] != tc.age.String() || row["last_success"] == nil {
					t.Fatal(row)
				}
			} else if _, ok := row["success_health"]; ok {
				t.Fatal("default inferred freshness", row)
			}
		})
	}
	for _, age := range []time.Duration{0, time.Hour} {
		if err := writeScheduleStatus(ui.Discard(), nil, age, now); (err != nil) != (age > 0) {
			t.Fatalf("no jobs age %s: %v", age, err)
		}
	}
}
func TestJobDiffIncludesRetryPolicyAndPreservesZeroDefaults(t *testing.T) {
	j := &backupJob{cfg: config.BackupJob{Schedule: "daily"}, targetName: "offsite"}
	s := schedule.Spec{Schedule: "daily", TargetName: "offsite"}
	if got := jobDiff(s, j, ""); len(got) != 0 {
		t.Fatal(got)
	}
	j.cfg.Retries = 2
	if got := jobDiff(s, j, ""); len(got) != 1 || got[0] != "retry policy changed" {
		t.Fatal(got)
	}
	s.Retries = 2
	s.RetryDelaySec = 30
	if got := jobDiff(s, j, ""); len(got) != 0 {
		t.Fatal(got)
	}
	j.cfg.RetryDelaySec = 60
	if got := jobDiff(s, j, ""); len(got) != 1 {
		t.Fatal(got)
	}
}

type scheduleClockHost struct {
	files        map[string][]byte
	clock        string
	clockErr     error
	clockQueries int
	statusRead   bool
}

func (h *scheduleClockHost) Name() string { return "fake" }
func (h *scheduleClockHost) Close() error { return nil }
func (h *scheduleClockHost) Run(context.Context, remote.Cmd) error {
	return errors.New("unexpected Run")
}
func (h *scheduleClockHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error {
	return errors.New("unexpected WriteFile")
}
func (h *scheduleClockHost) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	if p == schedule.StatePath("shop", "production") {
		h.statusRead = true
	}
	b, ok := h.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}
func (h *scheduleClockHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	if strings.HasPrefix(c.Script, "ls -1") {
		return "shop-production-nightly.json", nil
	}
	if strings.Contains(c.Script, " show ") {
		return "ActiveState=active\nResult=success", nil
	}
	if strings.Contains(c.Script, "date -u +%s.%N") {
		h.clockQueries++
		if !h.statusRead {
			return "", errors.New("clock queried before reading state")
		}
		return h.clock, h.clockErr
	}
	return "", errors.New("unexpected Output")
}
func scheduleClockFixture(t *testing.T, serverNow, success time.Time) *scheduleClockHost {
	t.Helper()
	spec, _ := json.Marshal(schedule.Spec{App: "shop", Destination: "production", Job: "nightly", Schedule: "daily"})
	state, _ := json.Marshal(map[string]schedule.JobState{"nightly": {LastSuccess: &success}})
	return &scheduleClockHost{clock: fmt.Sprintf("%d.%09d", serverNow.Unix(), serverNow.Nanosecond()), files: map[string][]byte{schedule.SpecPath("shop", "production", "nightly"): spec, schedule.StatePath("shop", "production"): state}}
}
func TestScheduleFreshnessUsesServerClockAfterState(t *testing.T) {
	local := time.Date(2026, 10, 13, 0, 0, 0, 800000000, time.UTC)
	for _, tc := range []struct {
		name   string
		skew   time.Duration
		future bool
	}{{"server-ahead", 2 * time.Minute, false}, {"server-behind", -2 * time.Minute, false}, {"genuine-future", 2 * time.Minute, true}} {
		t.Run(tc.name, func(t *testing.T) {
			serverNow := local.Add(tc.skew)
			success := serverNow.Add(-time.Nanosecond)
			if tc.future {
				success = serverNow.Add(time.Nanosecond)
			}
			h := scheduleClockFixture(t, serverNow, success)
			sts, now, err := readScheduleStatus(context.Background(), h, "shop", "production", schedule.Mode{}, 48*time.Hour)
			if err != nil || !now.Equal(serverNow) || h.clockQueries != 1 {
				t.Fatalf("%v now %s want %s queries %d", err, now, serverNow, h.clockQueries)
			}
			err = writeScheduleStatus(ui.Discard(), sts, 48*time.Hour, now)
			if (err != nil) != tc.future {
				t.Fatalf("unexpected freshness result: %v", err)
			}
		})
	}
}
func TestScheduleClockFailureHasNoLocalFallback(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 800000000, time.UTC)
	for _, clock := range []string{"", "1791849600", "1791849600.%N", "1791849600.123", "1791849600.1234567890", "-1.123456789", "1e3.123456789", "1791849600.123456789\nextra", "9223372036854775808.000000001"} {
		h := scheduleClockFixture(t, now, now.Add(-time.Hour))
		h.clock = clock
		_, sample, err := readScheduleStatus(context.Background(), h, "shop", "production", schedule.Mode{}, 48*time.Hour)
		if err == nil || !sample.IsZero() || h.clockQueries != 1 {
			t.Fatalf("clock %q: sample %s err %v", clock, sample, err)
		}
	}
	h := scheduleClockFixture(t, now, now)
	h.clockErr = context.Canceled
	_, sample, err := readScheduleStatus(context.Background(), h, "shop", "production", schedule.Mode{}, 48*time.Hour)
	if !errors.Is(err, context.Canceled) || !sample.IsZero() {
		t.Fatalf("%s %v", sample, err)
	}
	// Informational status must not gain a clock-query dependency or freshness.
	h = scheduleClockFixture(t, now, now.Add(time.Hour))
	h.clockErr = errors.New("date unavailable")
	sts, sample, err := readScheduleStatus(context.Background(), h, "shop", "production", schedule.Mode{}, 0)
	if err != nil || len(sts) != 1 || !sample.IsZero() || h.clockQueries != 0 {
		t.Fatalf("%+v %s %v queries=%d", sts, sample, err, h.clockQueries)
	}
	if err := writeScheduleStatus(ui.Discard(), sts, 0, sample); err != nil {
		t.Fatal(err)
	}
}
