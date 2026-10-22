package schedule

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

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/config"
)

func retryFixture(t *testing.T, count int) (Spec, string, *fakeHost) {
	t.Helper()
	j := testJob()
	j.Retries = count
	j.RetryDelaySec = 1
	s, err := NewSpec(j)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "job.json")
	saveSpec(t, p, s)
	h := &fakeHost{files: map[string][]byte{s.SecretFiles["BACKUP_PASSWORD"]: []byte(pw), s.SecretFiles["AWS_SECRET_ACCESS_KEY"]: []byte("aws-secret")}}
	prev := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	h.files[StatePath(s.App, s.Destination)] = marshalState(map[string]JobState{s.Job: {LastSuccess: &prev, BackupID: "previous"}})
	return s, p, h
}
func saveSpec(t *testing.T, p string, s Spec) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func readTestState(t *testing.T, h *fakeHost, s Spec) JobState {
	t.Helper()
	var states map[string]JobState
	if err := json.Unmarshal(h.files[StatePath(s.App, s.Destination)], &states); err != nil {
		t.Fatal(err)
	}
	return states[s.Job]
}

func TestScheduledLockReleaseFailureIsReturned(t *testing.T) {
	for _, kind := range []string{"success", "failed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			s, p, h := retryFixture(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lockFailure := errors.New("release lock failed " + pw)
			backupFailure := errors.New("backup failed")
			err := RunJob(ctx, p, RunJobOptions{Host: h, Lock: func(context.Context) (func(context.Context) error, error) {
				return func(cleanup context.Context) error {
					if cleanup.Err() != nil {
						t.Fatal("unlock inherited cancelled context")
					}
					return lockFailure
				}, nil
			}, runBackup: func(context.Context, backup.RunOptions) (*backup.Result, error) {
				switch kind {
				case "failed":
					return nil, backupFailure
				case "cancelled":
					cancel()
					return nil, context.Canceled
				default:
					return &backup.Result{ID: "new-backup"}, nil
				}
			}})
			if !errors.Is(err, lockFailure) || strings.Contains(err.Error(), pw) {
				t.Fatalf("lock failure lost or leaked: %v", err)
			}
			if kind == "failed" && !errors.Is(err, backupFailure) {
				t.Fatalf("backup failure lost: %v", err)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if kind == "success" && readTestState(t, h, s).BackupID != "new-backup" {
				t.Fatal("successful Backup was not recorded")
			}
		})
	}
}

func TestRetriesSucceedAndRetainActualOutcomes(t *testing.T) {
	s, p, h := retryFixture(t, 2)
	var out bytes.Buffer
	attempts, locks, unlocks := 0, 0, 0
	err := RunJob(context.Background(), p, RunJobOptions{Host: h, Out: &out, Lock: func(context.Context) (func(context.Context) error, error) {
		locks++
		return func(ctx context.Context) error {
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			unlocks++
			return nil
		}, nil
	}, runBackup: func(ctx context.Context, o backup.RunOptions) (*backup.Result, error) {
		attempts++
		if o.Lock != nil {
			t.Fatal("nested lock")
		}
		if _, err := fmt.Fprintln(o.Out, "diagnostic", pw, "aws-secret"); err != nil {
			t.Fatal(err)
		}
		if attempts == 1 {
			return nil, errors.New("transient " + pw)
		}
		return &backup.Result{ID: "new-backup"}, nil
	}})
	st := readTestState(t, h, s)
	if err != nil || attempts != 2 || locks != 1 || unlocks != 1 || st.Error != "" || st.BackupID != "new-backup" || st.LastSuccess == nil || len(st.Attempts) != 2 || st.Attempts[0].Outcome != "failed" || st.Attempts[1].Outcome != "success" {
		t.Fatalf("err=%v attempts=%d locks=%d/%d state=%+v", err, attempts, locks, unlocks, st)
	}
	for _, secret := range []string{pw, "aws-secret"} {
		if strings.Contains(out.String(), secret) || strings.Contains(string(h.files[StatePath(s.App, s.Destination)]), secret) {
			t.Fatal("leaked secret")
		}
	}
}
func TestRetriesFailurePreservesSuccessAndRedacts(t *testing.T) {
	s, p, h := retryFixture(t, 1)
	attempts := 0
	err := RunJob(context.Background(), p, RunJobOptions{Host: h, runBackup: func(context.Context, backup.RunOptions) (*backup.Result, error) {
		attempts++
		return nil, errors.New("failed " + pw)
	}})
	st := readTestState(t, h, s)
	if err == nil || strings.Contains(err.Error(), pw) || attempts != 2 || len(st.Attempts) != 2 || st.LastSuccess == nil || st.LastSuccess.Day() != 12 || st.BackupID != "previous" || st.Error == "" {
		t.Fatalf("%v %+v attempts=%d", err, st, attempts)
	}
}
func TestRetryStopsForStoredBackupAndCancelledOrTimedOutAttempt(t *testing.T) {
	for _, kind := range []string{"stored", "cancel", "timeout", "default"} {
		t.Run(kind, func(t *testing.T) {
			count := 2
			if kind == "default" {
				count = 0
			}
			s, p, h := retryFixture(t, count)
			attempts := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := RunJob(ctx, p, RunJobOptions{Host: h, runBackup: func(ctx context.Context, o backup.RunOptions) (*backup.Result, error) {
				attempts++
				switch kind {
				case "stored":
					return &backup.Result{ID: "stored"}, errors.New("retention failed")
				case "cancel":
					cancel()
					return nil, errors.New("process killed")
				case "timeout":
					return nil, context.DeadlineExceeded
				default:
					return nil, errors.New("one attempt")
				}
			}})
			st := readTestState(t, h, s)
			if err == nil || attempts != 1 || len(st.Attempts) != 1 || st.LastSuccess == nil || st.BackupID != "previous" {
				t.Fatalf("%v %+v %d", err, st, attempts)
			}
			if kind == "stored" && st.Attempts[0].BackupID != "stored" {
				t.Fatal(st)
			}
			if kind == "cancel" && (!errors.Is(err, context.Canceled) || st.Attempts[0].Outcome != "cancelled") {
				t.Fatalf("%v %+v", err, st)
			}
			if kind == "timeout" && st.Attempts[0].Outcome != "timeout" {
				t.Fatal(st)
			}
		})
	}
}

type cancelRetryWriter struct{ cancel context.CancelFunc }

func (w cancelRetryWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "retrying in") {
		w.cancel()
	}
	return len(p), nil
}
func TestCancellationDuringRetryDelay(t *testing.T) {
	s, p, h := retryFixture(t, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	start := time.Now()
	err := RunJob(ctx, p, RunJobOptions{Host: h, Out: cancelRetryWriter{cancel}, runBackup: func(context.Context, backup.RunOptions) (*backup.Result, error) {
		attempts++
		return nil, errors.New("transient")
	}})
	if !errors.Is(err, context.Canceled) || attempts != 1 || time.Since(start) > time.Second {
		t.Fatalf("%v attempts=%d took %s", err, attempts, time.Since(start))
	}
	st := readTestState(t, h, s)
	if st.LastSuccess == nil || st.Error == "" {
		t.Fatal(st)
	}
}
func TestCallerAndSpecTimeoutBoundAttempts(t *testing.T) {
	for _, kind := range []string{"caller", "spec"} {
		t.Run(kind, func(t *testing.T) {
			s, p, h := retryFixture(t, 5)
			s.TimeoutSec = 1
			saveSpec(t, p, s)
			ctx := context.Background()
			cancel := func() {}
			if kind == "caller" {
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
			}
			defer cancel()
			attempts := 0
			start := time.Now()
			err := RunJob(ctx, p, RunJobOptions{Host: h, runBackup: func(ctx context.Context, o backup.RunOptions) (*backup.Result, error) {
				attempts++
				<-ctx.Done()
				return nil, errors.New("process killed")
			}})
			st := readTestState(t, h, s)
			if !errors.Is(err, context.DeadlineExceeded) || attempts != 1 || len(st.Attempts) != 1 || st.Attempts[0].Outcome != "timeout" || time.Since(start) > 2*time.Second {
				t.Fatalf("%v %+v attempts=%d duration=%s", err, st, attempts, time.Since(start))
			}
		})
	}
}
func TestRetryPolicyValidationAndLegacyZero(t *testing.T) {
	for _, tc := range []struct {
		count, delay int
		command      string
	}{{-1, 0, ""}, {6, 0, ""}, {1, -1, ""}, {1, 301, ""}, {1, 1, "dump"}, {1, 1, "pre"}} {
		j := testJob()
		j.Retries, j.RetryDelaySec = tc.count, tc.delay
		sb := j.Services["db"]
		if tc.command == "dump" {
			sb.Dump = []string{"non-idempotent"}
		}
		if tc.command == "pre" {
			sb.PreBackup = []string{"non-idempotent"}
		}
		j.Services["db"] = sb
		if _, err := NewSpec(j); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
		s, p, h := retryFixture(t, 0)
		s.Retries, s.RetryDelaySec, s.Services = j.Retries, j.RetryDelaySec, j.Services
		saveSpec(t, p, s)
		if err := RunJob(context.Background(), p, RunJobOptions{Host: h}); err == nil || len(h.cmds) != 0 {
			t.Fatalf("accepted invalid stored spec %+v err=%v", tc, err)
		}
	}
	s, p, h := retryFixture(t, 0)
	s.TimeoutSec = 0
	s.RetryDelaySec = 0
	s.Services["db"] = config.ServiceBackup{Dump: []string{"non-idempotent"}, PreBackup: []string{"non-idempotent"}, PostRestore: []string{"non-idempotent"}}
	saveSpec(t, p, s)
	calls := 0
	err := RunJob(context.Background(), p, RunJobOptions{Host: h, runBackup: func(context.Context, backup.RunOptions) (*backup.Result, error) {
		calls++
		return nil, errors.New("fail")
	}})
	if err == nil || calls != 1 {
		t.Fatalf("legacy/default policy: %d %v", calls, err)
	}
}
func TestScheduledDefaultRunsCommandOnceWithoutPostRestore(t *testing.T) {
	s, p, h := retryFixture(t, 0)
	s.Services = map[string]config.ServiceBackup{"db": {Dump: []string{"non-idempotent"}, PostRestore: []string{"never-run"}}}
	saveSpec(t, p, s)
	calls := 0
	h.respond = func(sc string) (string, error) {
		if strings.Contains(sc, " ps -q ") {
			return "cid", nil
		}
		if strings.Contains(sc, "docker exec") {
			calls++
			return "", errors.New("command failed")
		}
		return "", nil
	}
	if err := RunJob(context.Background(), p, RunJobOptions{Host: h}); err == nil || calls != 1 || strings.Contains(h.scripts(), "never-run") {
		t.Fatalf("calls=%d err=%v %s", calls, err, h.scripts())
	}
}
func TestFailedStateReadDoesNotErasePriorSuccess(t *testing.T) {
	s, p, h := retryFixture(t, 0)
	previous := append([]byte(nil), h.files[StatePath(s.App, s.Destination)]...)
	h.files[StatePath(s.App, s.Destination)] = []byte("invalid")
	err := RunJob(context.Background(), p, RunJobOptions{Host: h, runBackup: func(context.Context, backup.RunOptions) (*backup.Result, error) {
		return nil, errors.New("attempt failure")
	}})
	if err == nil || string(h.files[StatePath(s.App, s.Destination)]) != "invalid" {
		t.Fatal("overwrote broken prior state", err)
	}
	if len(previous) == 0 {
		t.Fatal("bad fixture")
	}
}
func TestSuccessHealthExplicitThreshold(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		age           time.Duration
		nilTime, zero bool
		want          string
	}{{0, true, false, "never-success"}, {0, false, true, "never-success"}, {-time.Second, false, false, "future-success"}, {48 * time.Hour, false, false, "healthy"}, {48*time.Hour + time.Nanosecond, false, false, "stale"}, {7 * 24 * time.Hour, false, false, "stale"}} {
		stamp := now.Add(-tc.age)
		st := JobState{LastSuccess: &stamp, Error: "latest attempt failed"}
		if tc.nilTime {
			st.LastSuccess = nil
		}
		if tc.zero {
			stamp = time.Time{}
		}
		if got := SuccessHealth(st, now, 48*time.Hour); got != tc.want {
			t.Fatalf("%+v got %s", tc, got)
		}
	}
}

func TestScheduledRcloneDiagnosticsRedactRefreshedConfig(t *testing.T) {
	s, p, h := retryFixture(t, 0)
	s.Target.RcloneConfigSecret = "RCLONE_CONF"
	s.Target.Type = "archive"
	s.Target.Format = "7z"
	s.Target.Repository = "od:backups"
	saveSpec(t, p, s)
	token := `{"refresh_token":"refreshed-sensitive-token"}`
	line := "token = " + token
	conf := backup.RcloneConfigDir(s.App, s.Destination, "RCLONE_CONF") + "/rclone.conf"
	h.files[conf] = []byte("[od]\n" + line + "\n")
	var out bytes.Buffer
	err := RunJob(context.Background(), p, RunJobOptions{Host: h, Out: &out,
		runBackup: func(_ context.Context, o backup.RunOptions) (*backup.Result, error) {
			if o.RcloneConfig != "" {
				t.Fatal("Scheduled Job must never re-seed refreshed tokens")
			}
			fmt.Fprintln(o.Out, "could not parse line: "+line)
			return nil, fmt.Errorf("could not parse token: %s", token)
		},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, diagnostic := range []string{err.Error(), out.String(), string(h.files[StatePath(s.App, s.Destination)])} {
		if strings.Contains(diagnostic, "refreshed-sensitive-token") {
			t.Fatalf("config leaked in diagnostic: %s", diagnostic)
		}
	}
}
