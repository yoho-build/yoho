package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/schedule"
	"github.com/yoho-build/yoho/internal/secrets"
)

// planSchedules returns the changes apply would make to Scheduled Jobs on the
// Destination's Servers: jobs with a schedule in config but not installed
// (create), installed with a different spec (update), installed but no longer
// in config (delete). Read-only.
func (a *app) planSchedules(ctx context.Context, hosts []plan.NamedHost, store *secrets.Store) ([]plan.Change, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no Servers")
	}
	h := hosts[0]
	desired := map[string]*backupJob{}
	var loaded bool
	for _, n := range sortedKeys(a.cfg.Backups.Jobs) {
		jc := a.cfg.Backups.Jobs[n]
		if jc.Destination != a.destName || jc.Schedule == "" {
			continue
		}
		j, err := a.newBackupJob(n, jc)
		if err != nil {
			return nil, err
		}
		if !loaded {
			if err := a.resolveServices(ctx, j); err != nil {
				return nil, err
			}
			loaded = true
		} else if err := a.resolveServices(ctx, j); err != nil {
			return nil, err
		}
		desired[n] = j
	}
	// Keyed fingerprints of the Backup Target credentials, so a rotated
	// secret under the same key name still shows up as a change. Without the
	// Destination key (never deployed) there is nothing to compare against.
	var fpKey []byte
	if store != nil {
		fpKey = a.readFingerprintKey(ctx, h.Host)
	}
	mode := a.scheduleMode()
	sts, err := schedule.Status(ctx, h.Host, a.cfg.App, a.destName, mode)
	if err != nil {
		return nil, err
	}
	installed := map[string]schedule.JobStatus{}
	for _, s := range sts {
		installed[s.Job] = s
	}
	var changes []plan.Change
	for _, n := range sortedKeys(desired) {
		j := desired[n]
		st, ok := installed[n]
		if !ok {
			changes = append(changes, plan.Change{
				Kind: "job", Name: n, Server: h.Name, Action: plan.ActionCreate,
				Reasons: []string{"not installed"},
			})
			continue
		}
		spec, err := a.readInstalledSpec(ctx, h.Host, n, mode.Sudo)
		if err != nil {
			return nil, err
		}
		if spec.Schedule == "" {
			spec.Schedule = st.Schedule
		}
		reasons := jobDiff(spec, j, a.dest.Runtime)
		if fpKey != nil {
			if err := secretsFromStore(j, store); err != nil {
				return nil, err
			}
			if r := secretDiff(spec, targetFingerprints(fpKey, j)); r != "" {
				reasons = append(reasons, r)
			}
		}
		ch := plan.Change{Kind: "job", Name: n, Server: h.Name, Action: plan.ActionNoop}
		if len(reasons) > 0 {
			ch.Action = plan.ActionUpdate
			ch.Reasons = reasons
		}
		changes = append(changes, ch)
	}
	var removed []string
	for name := range installed {
		if _, ok := desired[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, n := range removed {
		changes = append(changes, plan.Change{
			Kind: "job", Name: n, Server: h.Name, Action: plan.ActionDelete,
			Reasons: []string{"removed from config"},
		})
	}
	return changes, nil
}

// applySchedules converges Scheduled Jobs to config (install/update/remove).
func (a *app) applySchedules(ctx context.Context, hosts []plan.NamedHost, store *secrets.Store) error {
	changes, err := a.planSchedules(ctx, hosts, store)
	if err != nil {
		return err
	}
	var install, remove []string
	for _, c := range changes {
		switch c.Action {
		case plan.ActionCreate, plan.ActionUpdate:
			install = append(install, c.Name)
		case plan.ActionDelete:
			remove = append(remove, c.Name)
		}
	}
	if len(install) == 0 && len(remove) == 0 {
		return nil
	}
	if len(install) > 0 {
		if err := a.installJobs(ctx, hosts, install, "", store); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		if err := a.removeJobs(ctx, hosts, remove); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) readInstalledSpec(ctx context.Context, h interface {
	ReadFile(context.Context, string, bool) ([]byte, error)
}, job string, sudo bool) (schedule.Spec, error) {
	p := schedule.SpecPath(a.cfg.App, a.destName, job)
	b, err := h.ReadFile(ctx, p, sudo)
	if err != nil {
		return schedule.Spec{}, fmt.Errorf("read installed spec %s: %w", job, err)
	}
	var s schedule.Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return schedule.Spec{}, fmt.Errorf("parse installed spec %s: %w", job, err)
	}
	return s, nil
}

// jobDiff compares non-secret fields of an installed spec with the desired job.
func jobDiff(spec schedule.Spec, j *backupJob, runtime string) []string {
	var reasons []string
	if normRuntime(spec.Runtime) != normRuntime(runtime) {
		reasons = append(reasons, fmt.Sprintf("runtime %s → %s", normRuntime(spec.Runtime), normRuntime(runtime)))
	}
	if spec.Schedule != j.cfg.Schedule {
		reasons = append(reasons, fmt.Sprintf("schedule %q → %q", spec.Schedule, j.cfg.Schedule))
	}
	if spec.TargetName != j.targetName {
		reasons = append(reasons, fmt.Sprintf("target %q → %q", spec.TargetName, j.targetName))
	} else if !targetsEqual(spec.Target, j.target) {
		reasons = append(reasons, "target changed")
	}
	if !servicesEqual(spec.Services, j.services) {
		reasons = append(reasons, serviceReason(spec.Services, j.services))
	}
	return reasons
}

func targetsEqual(a, b config.BackupTarget) bool {
	a.EnvSecrets = nilIfEmpty(a.EnvSecrets)
	b.EnvSecrets = nilIfEmpty(b.EnvSecrets)
	return reflect.DeepEqual(a, b)
}

func servicesEqual(a, b map[string]config.ServiceBackup) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !reflect.DeepEqual(normService(av), normService(bv)) {
			return false
		}
	}
	return true
}

func serviceReason(installed, desired map[string]config.ServiceBackup) string {
	a := strings.Join(sortedKeys(installed), ", ")
	b := strings.Join(sortedKeys(desired), ", ")
	if a != b {
		return fmt.Sprintf("services %q → %q", a, b)
	}
	return "services changed"
}

func normService(s config.ServiceBackup) config.ServiceBackup {
	s.Volumes = nilIfEmpty(s.Volumes)
	s.Dump = nilIfEmpty(s.Dump)
	s.PreBackup = nilIfEmpty(s.PreBackup)
	s.PostRestore = nilIfEmpty(s.PostRestore)
	s.RestoreDump = nilIfEmpty(s.RestoreDump)
	return s
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// normRuntime treats the empty runtime (older specs, unset Destinations) as compose.
func normRuntime(r string) string {
	if r == "" {
		return "compose"
	}
	return r
}

// readFingerprintKey returns the Destination's HMAC key without creating it,
// or nil when the Server has none yet.
func (a *app) readFingerprintKey(ctx context.Context, h interface {
	ReadFile(context.Context, string, bool) ([]byte, error)
}) []byte {
	b, err := h.ReadFile(ctx, path.Join(release.AppDir(a.cfg.App, a.destName), "hmac.key"), false)
	if err != nil {
		return nil
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < 16 {
		return nil
	}
	return key
}

// targetFingerprints fingerprints the resolved Backup Target secrets of j.
func targetFingerprints(key []byte, j *backupJob) map[string]string {
	out := map[string]string{}
	if k := j.target.PasswordSecret; k != "" {
		out[k] = secrets.Fingerprint(key, k+"\x00"+j.password)
	}
	for k, v := range j.env {
		out[k] = secrets.Fingerprint(key, k+"\x00"+v)
	}
	return out
}

// secretDiff reports credentials whose value changed since the spec was
// installed. A spec without fingerprints (installed by an older yoho) is
// refreshed once so the comparison works from then on.
func secretDiff(spec schedule.Spec, want map[string]string) string {
	if len(want) == 0 {
		return ""
	}
	if len(spec.SecretFingerprints) == 0 {
		return "record credential fingerprints"
	}
	var changed []string
	for _, k := range sortedKeys(want) {
		if spec.SecretFingerprints[k] != want[k] {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	return "credential changed: " + strings.Join(changed, ", ")
}
