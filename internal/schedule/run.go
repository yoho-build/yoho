package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// RunJobOptions are optional collaborators of RunJob.
type RunJobOptions struct {
	// Default &remote.Local{}.
	Host remote.Host
	Out  io.Writer
	// Destination lock shared with deploys (provided by the deploy package).
	Lock        backup.LockFunc
	YohoVersion string
	Now         func() time.Time
}

// RunJob runs the job described by the spec file on this Server and
// records its state. A non-nil error means the CLI must exit non-zero.
func RunJob(ctx context.Context, specPath string, o RunJobOptions) error {
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
	if s.Root != "" {
		// This process serves one job; the spec carries the Server root.
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
	start := now()
	res, runErr := runSpec(ctx, h, s, o)

	st := JobState{LastRun: start.UTC(), DurationSec: now().Sub(start).Seconds()}
	if runErr != nil {
		st.Error = runErr.Error()
	} else {
		t := now().UTC()
		st.LastSuccess, st.BackupID = &t, res.ID
	}
	if err := writeState(ctx, h, s, st); err != nil {
		if runErr != nil {
			return fmt.Errorf("%w (also failed to record state: %v)", runErr, err)
		}
		return fmt.Errorf("record state: %w", err)
	}
	return runErr
}

func runSpec(ctx context.Context, h remote.Host, s Spec, o RunJobOptions) (*backup.Result, error) {
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
	ro := backup.RunOptions{
		App: s.App, Destination: s.Destination, Project: s.Project,
		Services: s.Services, Target: s.Target, Host: h, Out: o.Out,
		Lock: o.Lock, YohoVersion: o.YohoVersion, Now: o.Now,
		TargetEnv: map[string]string{},
	}
	if k := s.Target.PasswordSecret; k != "" {
		v, err := secret(k)
		if err != nil {
			return nil, err
		}
		ro.Password = v
	}
	for _, k := range s.Target.EnvSecrets {
		v, err := secret(k)
		if err != nil {
			return nil, err
		}
		ro.TargetEnv[k] = v
	}
	if o.Out != nil {
		fmt.Fprintf(o.Out, "Scheduled Job %s: Backup of %s (%s) to %s\n", s.Job, s.App, s.Destination, s.TargetName)
	}
	return backup.Run(ctx, ro)
}

// writeState merges st into the App/Destination state file.
func writeState(ctx context.Context, h remote.Host, s Spec, st JobState) error {
	p := StatePath(s.App, s.Destination)
	states := map[string]JobState{}
	if b, err := h.ReadFile(ctx, p, false); err == nil {
		_ = json.Unmarshal(b, &states)
	}
	if prev, ok := states[s.Job]; ok && st.LastSuccess == nil {
		st.LastSuccess, st.BackupID = prev.LastSuccess, prev.BackupID
	}
	states[s.Job] = st
	return h.WriteFile(ctx, p, marshalState(states), 0o600, false)
}
