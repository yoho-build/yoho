package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// RunJobOptions are optional collaborators of RunJob.
type RunJobOptions struct {
	// Default &remote.Local{}.
	Host remote.Host
	Out  io.Writer
	// Destination lock shared with deploys; held through attempts and state write.
	Lock        backup.LockFunc
	YohoVersion string
	Now         func() time.Time
	// Package-local test collaborator; production always calls backup.Run.
	runBackup func(context.Context, backup.RunOptions) (*backup.Result, error)
}

const (
	maxRetries           = 5
	defaultRetryDelaySec = 30
	maxRetryDelaySec     = 300
	stateTimeout         = 30 * time.Second
)

func validateRetries(count, delay int, services map[string]config.ServiceBackup) error {
	if count < 0 || count > maxRetries {
		return fmt.Errorf("retries must be between 0 and %d", maxRetries)
	}
	if delay < 0 || delay > maxRetryDelaySec {
		return fmt.Errorf("retry_delay_sec must be between 0 and %d (0 means %d)", maxRetryDelaySec, defaultRetryDelaySec)
	}
	if count > 0 {
		for _, svc := range sortedKeys(services) {
			sb := services[svc]
			if len(sb.Dump) > 0 || len(sb.PreBackup) > 0 {
				return fmt.Errorf("retries require volume-only Backups without dump or pre_backup commands (Service %s)", svc)
			}
		}
	}
	return nil
}

// RunJob runs a stored Backup spec and records the actual attempt outcomes.
// Zero retries preserves one attempt. The total spec timeout includes lock
// acquisition, all attempts and delays; caller cancellation always wins.
func RunJob(ctx context.Context, specPath string, o RunJobOptions) (err error) {
	var values []string
	defer func() { err = secrets.RedactError(err, values...) }()
	b, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("read job spec: %w", err)
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("parse job spec %s: %w", specPath, err)
	}
	if s.Version != specVersion {
		return fmt.Errorf("job spec %s: unsupported version %d (reinstall with this yoho)", specPath, s.Version)
	}
	if !nameRe.MatchString(s.App) || !nameRe.MatchString(s.Destination) || !nameRe.MatchString(s.Job) {
		return errors.New("job spec: invalid App, Destination or job name")
	}
	if err := validateRetries(s.Retries, s.RetryDelaySec, s.Services); err != nil {
		return fmt.Errorf("job spec: %w", err)
	}
	if s.TimeoutSec < 0 || int64(s.TimeoutSec) > int64((1<<63-1)/int64(time.Second)) {
		return errors.New("job spec: invalid timeout_sec")
	}
	if s.Root != "" {
		if !safePathRe.MatchString(s.Root) {
			return errors.New("job spec: unsupported Server root")
		}
		release.Root = s.Root
	}
	h := o.Host
	if h == nil {
		h = &remote.Local{}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	timeout := defaultTimeout
	if s.TimeoutSec > 0 {
		timeout = time.Duration(s.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if o.Lock != nil {
		unlock, lockErr := o.Lock(ctx)
		if lockErr != nil {
			return fmt.Errorf("lock: %w", lockErr)
		}
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateTimeout)
			defer cancel()
			err = errors.Join(err, unlock(cctx))
		}()
	}
	start := now()
	st := JobState{LastRun: start.UTC()}
	ro, prepErr := prepareRun(ctx, h, s, o)
	values = []string{ro.Password}
	for _, v := range ro.TargetEnv {
		values = append(values, v)
	}
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	redactor := secrets.NewDiagnosticRedactor(out, values...)
	defer redactor.Flush()
	ro.Out, ro.Lock = redactor, nil
	run := backup.Run
	if o.runBackup != nil {
		run = o.runBackup
	}
	runErr := prepErr
	if prepErr == nil {
		delay := s.RetryDelaySec
		if delay == 0 {
			delay = defaultRetryDelaySec
		}
		for attempt := 1; attempt <= s.Retries+1; attempt++ {
			if runErr = ctx.Err(); runErr != nil {
				break
			}
			at := now()
			fmt.Fprintf(redactor, "Scheduled Job %s: Backup attempt %d/%d\n", s.Job, attempt, s.Retries+1)
			res, aerr := run(ctx, ro)
			if ctx.Err() != nil {
				aerr = errors.Join(aerr, ctx.Err())
			}
			if aerr == nil && (res == nil || res.ID == "") {
				aerr = errors.New("Backup completed without a stored ID")
			}
			aerr = secrets.RedactError(aerr, values...)
			ast := AttemptState{Number: attempt, StartedAt: at.UTC(), DurationSec: now().Sub(at).Seconds(), Outcome: "success"}
			if res != nil {
				ast.BackupID = res.ID
			}
			if aerr != nil {
				ast.Error, ast.Outcome = aerr.Error(), "failed"
				if errors.Is(aerr, context.Canceled) {
					ast.Outcome = "cancelled"
				}
				if errors.Is(aerr, context.DeadlineExceeded) {
					ast.Outcome = "timeout"
				}
				fmt.Fprintf(redactor, "attempt %d %s: %v\n", attempt, ast.Outcome, aerr)
			}
			st.Attempts = append(st.Attempts, ast)
			runErr = aerr
			if runErr == nil {
				t := now().UTC()
				st.LastSuccess, st.BackupID = &t, res.ID
				break
			}
			// A stored Backup followed by failed retention is not repeated. Nor is a
			// cancelled/timeout attempt or an uncertain unpause cleanup failure.
			if attempt > s.Retries || ctx.Err() != nil || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || (res != nil && res.ID != "") || backup.UnsafeToRetry(runErr) {
				break
			}
			fmt.Fprintf(redactor, "retrying in %ds\n", delay)
			timer := time.NewTimer(time.Duration(delay) * time.Second)
			select {
			case <-ctx.Done():
				runErr = ctx.Err()
			case <-timer.C:
			}
			timer.Stop()
		}
	}
	runErr = secrets.RedactError(runErr, values...)
	st.DurationSec = now().Sub(start).Seconds()
	if runErr != nil {
		st.Error = runErr.Error()
	}
	// Cancellation must still record the outcome and preserve prior success.
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), stateTimeout)
	defer ccancel()
	if err := writeState(cctx, h, s, st); err != nil {
		return errors.Join(runErr, fmt.Errorf("record state: %w", err))
	}
	return runErr
}

func prepareRun(ctx context.Context, h remote.Host, s Spec, o RunJobOptions) (backup.RunOptions, error) {
	ro := backup.RunOptions{
		App: s.App, Destination: s.Destination, Project: s.Project,
		Services: s.Services, Target: s.Target, TargetName: s.TargetName,
		Host: h, Out: o.Out, YohoVersion: o.YohoVersion, Now: o.Now,
		Runtime: s.Runtime, TargetEnv: map[string]string{},
	}
	secret := func(key string) (string, error) {
		p, ok := s.SecretFiles[key]
		if !ok {
			return "", fmt.Errorf("secret %s missing from job spec", key)
		}
		b, err := h.ReadFile(ctx, p, false)
		if err != nil {
			return "", fmt.Errorf("read secret %s: %w", key, err)
		}
		return string(b), nil
	}
	var err error
	if k := s.Target.PasswordSecret; k != "" {
		ro.Password, err = secret(k)
		if err != nil {
			return ro, err
		}
	}
	for _, k := range s.Target.EnvSecrets {
		v, err := secret(k)
		if err != nil {
			return ro, err
		}
		ro.TargetEnv[k] = v
	}
	return ro, nil
}

// writeState merges st into the App/Destination state file under its lock.
func writeState(ctx context.Context, h remote.Host, s Spec, st JobState) error {
	p := StatePath(s.App, s.Destination)
	states := map[string]JobState{}
	if b, err := h.ReadFile(ctx, p, false); err == nil {
		if err := json.Unmarshal(b, &states); err != nil {
			return fmt.Errorf("parse previous state: %w", err)
		}
		if states == nil {
			states = map[string]JobState{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read previous state: %w", err)
	}
	if prev, ok := states[s.Job]; ok && st.LastSuccess == nil {
		st.LastSuccess, st.BackupID = prev.LastSuccess, prev.BackupID
	}
	states[s.Job] = st
	return h.WriteFile(ctx, p, marshalState(states), 0o600, false)
}
