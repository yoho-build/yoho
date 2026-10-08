// Package schedule installs Scheduled Jobs (Backups) on a Server as one
// systemd timer per job running the yoho binary (ADR 0003), and runs them.
//
// Two modes:
//   - system (Server.Sudo): units in /etc/systemd/system with User=<deploy
//     user>, binary at /usr/local/libexec/yoho.
//   - user (no sudo): `systemctl --user` units in ~/.config/systemd/user,
//     binary at <root>/bin/yoho. Needs linger to run while logged out.
//
// Job specs live at <root>/jobs/<app>-<destination>-<job>.json and point
// at secret files under <root>/apps/<app>/<destination>/scheduled/, so a
// job runs without the operator's machine.
package schedule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

const (
	specVersion    = 1
	systemBinPath  = "/usr/local/libexec/yoho"
	systemUnitDir  = "/etc/systemd/system"
	defaultUser    = "yoho"
	defaultTimeout = 6 * time.Hour
)

var (
	nameRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// Paths embedded in unit files: no whitespace, quotes or % specifiers.
	safePathRe = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
)

// Job is one Scheduled Job to install, with secret values already resolved.
type Job struct {
	App, Destination, Name string
	// Compose project; default release.ProjectName.
	Project    string
	Services   map[string]config.ServiceBackup
	TargetName string
	Target     config.BackupTarget
	// systemd OnCalendar expression.
	Schedule string
	// Resolved values for Target.PasswordSecret and Target.EnvSecrets.
	Secrets map[string]string
	// Default 6h.
	Timeout time.Duration
	// Destination runtime: "compose" (default) or "swarm".
	Runtime string
	// Keyed fingerprints of Secrets (key -> fingerprint), stored in the spec
	// so `yoho plan` can see a rotated credential without reading the secret
	// files. Never the values.
	SecretFingerprints map[string]string
}

// Spec is the job file read by `yoho schedule run --job`. It holds paths
// to secret files, never secret values.
type Spec struct {
	Version     int    `json:"version"`
	App         string `json:"app"`
	Destination string `json:"destination"`
	Job         string `json:"job"`
	Project     string `json:"project"`
	// Runtime is the Destination runtime; empty in specs written before it
	// existed, which means compose.
	Runtime    string                          `json:"runtime,omitempty"`
	Root       string                          `json:"root"`
	Services   map[string]config.ServiceBackup `json:"services"`
	TargetName string                          `json:"target_name,omitempty"`
	Target     config.BackupTarget             `json:"target"`
	Schedule   string                          `json:"schedule"`
	// Secret key -> file path on the Server (0600).
	SecretFiles map[string]string `json:"secret_files,omitempty"`
	// Secret key -> keyed fingerprint of the value in SecretFiles.
	SecretFingerprints map[string]string `json:"secret_fingerprints,omitempty"`
	TimeoutSec         int               `json:"timeout_sec"`
}

// Mode selects system or user systemd units.
type Mode struct {
	// Sudo: system units (Server.Sudo). Otherwise systemctl --user.
	Sudo bool
	// Deploy user for User= and file ownership in system mode. Default yoho.
	User string
}

func (m Mode) user() string {
	if m.User == "" {
		return defaultUser
	}
	return m.User
}

func (m Mode) binPath() string {
	if m.Sudo {
		return systemBinPath
	}
	return path.Join(release.Root, "bin", "yoho")
}

func (m Mode) systemctl() string {
	if m.Sudo {
		return "systemctl"
	}
	return "systemctl --user"
}

// userEnv makes `systemctl --user` work from non-login ssh sessions.
const userEnv = `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"` + "\n"

func (m Mode) prelude() string {
	if m.Sudo {
		return "set -eu\n"
	}
	return "set -eu\n" + userEnv
}

// unitDir returns the systemd unit directory on the Server.
func (m Mode) unitDir(ctx context.Context, h remote.Host) (string, error) {
	if m.Sudo {
		return systemUnitDir, nil
	}
	home, err := h.Output(ctx, remote.Cmd{Script: `printf '%s' "$HOME"`})
	if err != nil {
		return "", err
	}
	if !safePathRe.MatchString(home) {
		return "", fmt.Errorf("unsupported HOME %q", home)
	}
	return path.Join(home, ".config/systemd/user"), nil
}

// UnitName is the systemd unit base name of a job.
func UnitName(app, dest, job string) string { return "yoho-" + app + "-" + dest + "-" + job }

// SpecPath is the job spec path on the Server.
func SpecPath(app, dest, job string) string {
	return path.Join(release.JobsDir(), app+"-"+dest+"-"+job+".json")
}

// SecretDir holds secret files for Scheduled Jobs of an App/Destination.
func SecretDir(app, dest string) string {
	return path.Join(release.AppDir(app, dest), "scheduled")
}

// StatePath is the run state of all jobs of an App/Destination.
func StatePath(app, dest string) string {
	return path.Join(release.AppDir(app, dest), "schedule-state.json")
}

// NewSpec builds the spec for j; secret files are named but not written.
func NewSpec(j Job) (Spec, error) {
	for _, n := range []string{j.App, j.Destination, j.Name} {
		if !nameRe.MatchString(n) {
			return Spec{}, fmt.Errorf("invalid name %q (letters, digits, - and _)", n)
		}
	}
	if j.Schedule == "" || strings.ContainsAny(j.Schedule, "\n\r%") {
		return Spec{}, fmt.Errorf("job %s: invalid schedule %q", j.Name, j.Schedule)
	}
	if !safePathRe.MatchString(release.Root) {
		return Spec{}, fmt.Errorf("unsupported Server root %q", release.Root)
	}
	if len(j.Services) == 0 {
		return Spec{}, fmt.Errorf("job %s: no Services to back up", j.Name)
	}
	s := Spec{
		Version: specVersion, App: j.App, Destination: j.Destination, Job: j.Name,
		Project: j.Project, Root: release.Root, Services: j.Services,
		TargetName: j.TargetName, Target: j.Target, Schedule: j.Schedule,
		SecretFiles: map[string]string{}, TimeoutSec: int(defaultTimeout / time.Second),
	}
	switch j.Runtime {
	case "", "compose":
	case "swarm":
		s.Runtime = "swarm"
	default:
		return Spec{}, fmt.Errorf("job %s: unknown runtime %q", j.Name, j.Runtime)
	}
	if s.Project == "" {
		s.Project = release.ProjectName(j.App, j.Destination)
	}
	if j.Timeout > 0 {
		s.TimeoutSec = int(j.Timeout / time.Second)
	}
	keys := append([]string(nil), j.Target.EnvSecrets...)
	if j.Target.PasswordSecret != "" {
		keys = append(keys, j.Target.PasswordSecret)
	}
	for _, k := range keys {
		if !envNameRe.MatchString(k) {
			return Spec{}, fmt.Errorf("job %s: invalid secret key %q", j.Name, k)
		}
		if _, ok := j.Secrets[k]; !ok {
			return Spec{}, fmt.Errorf("job %s: secret %s not resolved", j.Name, k)
		}
		s.SecretFiles[k] = path.Join(SecretDir(j.App, j.Destination), k)
		if fp := j.SecretFingerprints[k]; fp != "" {
			if s.SecretFingerprints == nil {
				s.SecretFingerprints = map[string]string{}
			}
			s.SecretFingerprints[k] = fp
		}
	}
	return s, nil
}

// RenderService renders the oneshot service unit. Pure.
func RenderService(s Spec, m Mode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by yoho. Scheduled Job %s of %s (%s).\n[Unit]\n", s.Job, s.App, s.Destination)
	fmt.Fprintf(&b, "Description=yoho %s %s %s\n", s.App, s.Destination, s.Job)
	if m.Sudo {
		b.WriteString("Wants=network-online.target\nAfter=network-online.target docker.service\n")
	}
	b.WriteString("\n[Service]\nType=oneshot\n")
	if m.Sudo {
		fmt.Fprintf(&b, "User=%s\n", m.user())
	}
	fmt.Fprintf(&b, "ExecStart=%s schedule run --job %s\n", m.binPath(), SpecPath(s.App, s.Destination, s.Job))
	fmt.Fprintf(&b, "TimeoutStartSec=%d\nNice=10\n", s.TimeoutSec)
	if m.Sudo {
		b.WriteString("IOSchedulingClass=idle\n")
	}
	return b.String()
}

// RenderTimer renders the timer unit. Pure.
func RenderTimer(s Spec) string {
	return fmt.Sprintf("# Managed by yoho.\n[Unit]\nDescription=yoho %s %s %s timer\n\n[Timer]\nOnCalendar=%s\nPersistent=true\nRandomizedDelaySec=300\n\n[Install]\nWantedBy=timers.target\n",
		s.App, s.Destination, s.Job, s.Schedule)
}

// InstallOptions configures Install.
type InstallOptions struct {
	Jobs []Job
	// yoho binary built for the Server's OS/arch.
	YohoBinaryLocalPath string
	Mode                Mode
	Out                 io.Writer
}

// InstallResult reports installed units and warnings (e.g. linger off).
type InstallResult struct {
	Timers   []string
	Warnings []string
}

// Install uploads the binary, writes secrets, specs and units, and enables
// the timers. Secret values only travel through WriteFile (stdin).
func Install(ctx context.Context, h remote.Host, o InstallOptions) (*InstallResult, error) {
	if len(o.Jobs) == 0 {
		return nil, errors.New("schedule: no jobs")
	}
	m := o.Mode
	if m.Sudo && !nameRe.MatchString(m.user()) {
		return nil, fmt.Errorf("invalid user %q", m.user())
	}
	logf := func(format string, a ...any) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, format+"\n", a...)
		}
	}
	specs := make([]Spec, len(o.Jobs))
	for i, j := range o.Jobs {
		s, err := NewSpec(j)
		if err != nil {
			return nil, err
		}
		specs[i] = s
	}
	unitDir, err := m.unitDir(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("locate unit dir: %w", err)
	}

	for _, s := range specs {
		if err := h.Run(ctx, remote.Cmd{Script: "systemd-analyze calendar " + remote.Quote(s.Schedule) + " >/dev/null"}); err != nil {
			return nil, fmt.Errorf("job %s: invalid schedule %q: %w", s.Job, s.Schedule, err)
		}
	}

	if err := uploadBinary(ctx, h, o.YohoBinaryLocalPath, m, logf); err != nil {
		return nil, err
	}

	// Directories. In system mode files are written as root then chowned
	// to the deploy user, who runs the job.
	dirs := []string{release.JobsDir()}
	for _, s := range specs {
		dirs = append(dirs, SecretDir(s.App, s.Destination))
	}
	script := m.prelude() + "umask 077\nmkdir -p " + remote.QuoteArgs(dirs...) + " " + remote.Quote(unitDir) + "\nchmod 0700 " + remote.QuoteArgs(dirs...) + "\n"
	if m.Sudo {
		script += "chown " + remote.Quote(m.user()+":") + " " + remote.QuoteArgs(dirs...) + "\n"
	}
	if err := h.Run(ctx, remote.Cmd{Script: script, Sudo: m.Sudo}); err != nil {
		return nil, fmt.Errorf("create directories: %w", err)
	}

	res := &InstallResult{}
	var owned []string
	for i, s := range specs {
		for _, k := range sortedKeys(s.SecretFiles) {
			p := s.SecretFiles[k]
			if err := h.WriteFile(ctx, p, []byte(o.Jobs[i].Secrets[k]), 0o600, m.Sudo); err != nil {
				return nil, fmt.Errorf("write secret %s: %w", k, err)
			}
			owned = append(owned, p)
		}
		sj, _ := json.MarshalIndent(s, "", "  ")
		sp := SpecPath(s.App, s.Destination, s.Job)
		if err := h.WriteFile(ctx, sp, sj, 0o600, m.Sudo); err != nil {
			return nil, fmt.Errorf("write job spec: %w", err)
		}
		owned = append(owned, sp)
		unit := UnitName(s.App, s.Destination, s.Job)
		if err := h.WriteFile(ctx, path.Join(unitDir, unit+".service"), []byte(RenderService(s, m)), 0o644, m.Sudo); err != nil {
			return nil, fmt.Errorf("write unit: %w", err)
		}
		if err := h.WriteFile(ctx, path.Join(unitDir, unit+".timer"), []byte(RenderTimer(s)), 0o644, m.Sudo); err != nil {
			return nil, fmt.Errorf("write timer: %w", err)
		}
		res.Timers = append(res.Timers, unit+".timer")
		logf("installed %s (%s)", unit+".timer", s.Schedule)
	}
	script = m.prelude()
	if m.Sudo {
		script += "chown " + remote.Quote(m.user()+":") + " " + remote.QuoteArgs(owned...) + "\n"
	}
	script += m.systemctl() + " daemon-reload\n" + m.systemctl() + " enable --now " + remote.QuoteArgs(res.Timers...) + "\n"
	if err := h.Run(ctx, remote.Cmd{Script: script, Sudo: m.Sudo}); err != nil {
		return nil, fmt.Errorf("enable timers: %w", err)
	}

	if !m.Sudo {
		// The marker file works without a login session, unlike loginctl show-user.
		out, _ := h.Output(ctx, remote.Cmd{Script: `u="$(id -un)"; if [ -e "/var/lib/systemd/linger/$u" ]; then echo "on $u"; else echo "off $u"; fi`})
		if state, u, _ := strings.Cut(strings.TrimSpace(out), " "); state != "on" {
			if u == "" {
				u, _ = h.Output(ctx, remote.Cmd{Script: "id -un"})
				u = strings.TrimSpace(u)
			}
			w := fmt.Sprintf("linger is off for %s: user timers stop when the user logs out. Run `yoho setup` (it enables linger), or on the Server: sudo loginctl enable-linger %s", u, u)
			res.Warnings = append(res.Warnings, w)
			logf("warning: %s", w)
		}
	}
	return res, nil
}

// uploadBinary copies the yoho binary unless the same sha256 is present,
// then verifies the checksum on the Server.
func uploadBinary(ctx context.Context, h remote.Host, local string, m Mode, logf func(string, ...any)) error {
	data, err := os.ReadFile(local)
	if err != nil {
		return fmt.Errorf("read yoho binary: %w", err)
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	bin := m.binPath()
	shaCmd := "sha256sum " + remote.Quote(bin) + " 2>/dev/null | cut -d' ' -f1"
	if got, _ := h.Output(ctx, remote.Cmd{Script: shaCmd, Sudo: m.Sudo}); got == want {
		return nil
	}
	if m.Sudo {
		// WriteFile creates missing parents 0700, which would hide the binary
		// from the deploy user.
		if err := h.Run(ctx, remote.Cmd{Script: "mkdir -p " + remote.Quote(path.Dir(bin)) + " && chmod 0755 " + remote.Quote(path.Dir(bin)), Sudo: true}); err != nil {
			return fmt.Errorf("create %s: %w", path.Dir(bin), err)
		}
	}
	logf("uploading yoho binary to %s (%d MB)", bin, len(data)>>20)
	if err := h.WriteFile(ctx, bin, data, 0o755, m.Sudo); err != nil {
		return fmt.Errorf("upload yoho binary: %w", err)
	}
	got, err := h.Output(ctx, remote.Cmd{Script: shaCmd, Sudo: m.Sudo})
	if err != nil || got != want {
		return fmt.Errorf("yoho binary checksum mismatch on Server (got %q, want %s): %v", got, want, err)
	}
	return nil
}

// RemoveOptions selects jobs to remove. Empty Jobs: all of App/Destination.
type RemoveOptions struct {
	App, Destination string
	Jobs             []string
	Mode             Mode
}

// Remove disables and deletes timers, units and specs. When no job of the
// App/Destination remains, its scheduled secret files and run state are
// deleted too. The shared yoho binary stays (other Apps may use it).
func Remove(ctx context.Context, h remote.Host, o RemoveOptions) ([]string, error) {
	specs, err := loadSpecs(ctx, h, o.App, o.Destination, o.Mode.Sudo)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, j := range o.Jobs {
		want[j] = true
	}
	unitDir, err := o.Mode.unitDir(ctx, h)
	if err != nil {
		return nil, err
	}
	var removed []string
	var b strings.Builder
	b.WriteString(o.Mode.prelude())
	for _, s := range specs {
		if len(want) > 0 && !want[s.Job] {
			continue
		}
		unit := UnitName(s.App, s.Destination, s.Job)
		fmt.Fprintf(&b, "%s disable --now %s 2>/dev/null || true\nrm -f %s %s %s\n", o.Mode.systemctl(), remote.Quote(unit+".timer"),
			remote.Quote(path.Join(unitDir, unit+".timer")), remote.Quote(path.Join(unitDir, unit+".service")),
			remote.Quote(SpecPath(s.App, s.Destination, s.Job)))
		removed = append(removed, s.Job)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if len(removed) == len(specs) {
		b.WriteString("rm -rf " + remote.Quote(SecretDir(o.App, o.Destination)) + "\nrm -f " + remote.Quote(StatePath(o.App, o.Destination)) + "\n")
	}
	b.WriteString(o.Mode.systemctl() + " daemon-reload\n")
	if err := h.Run(ctx, remote.Cmd{Script: b.String(), Sudo: o.Mode.Sudo}); err != nil {
		return nil, fmt.Errorf("remove jobs: %w", err)
	}
	return removed, nil
}

// loadSpecs reads job specs of one App/Destination. Filtering on the
// parsed spec avoids ambiguity of dashed names in file prefixes.
func loadSpecs(ctx context.Context, h remote.Host, app, dest string, sudo bool) ([]Spec, error) {
	dir := release.JobsDir()
	out, err := h.Output(ctx, remote.Cmd{Script: "ls -1 " + remote.Quote(dir) + " 2>/dev/null || true", Sudo: sudo})
	if err != nil {
		return nil, fmt.Errorf("list job specs: %w", err)
	}
	var specs []Spec
	prefix := app + "-" + dest + "-"
	for _, f := range strings.Fields(out) {
		if !strings.HasPrefix(f, prefix) || !strings.HasSuffix(f, ".json") {
			continue
		}
		b, err := h.ReadFile(ctx, path.Join(dir, f), sudo)
		if err != nil {
			return nil, err
		}
		var s Spec
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		if s.App == app && s.Destination == dest {
			specs = append(specs, s)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Job < specs[j].Job })
	return specs, nil
}

// JobState is the last run of a job, written by RunJob.
type JobState struct {
	LastRun     time.Time  `json:"last_run"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	Error       string     `json:"error,omitempty"`
	BackupID    string     `json:"backup_id,omitempty"`
	DurationSec float64    `json:"duration_sec,omitempty"`
}

// JobStatus combines systemd and RunJob state.
type JobStatus struct {
	Job      string
	Schedule string
	Timer    string
	// systemd timer ActiveState (active, inactive) and next/last trigger.
	TimerState  string
	NextRun     string
	LastTrigger string
	// Result of the last service run (success, exit-code, timeout...).
	ServiceResult string
	State         JobState
}

// Status reports installed jobs of an App/Destination.
func Status(ctx context.Context, h remote.Host, app, dest string, m Mode) ([]JobStatus, error) {
	specs, err := loadSpecs(ctx, h, app, dest, m.Sudo)
	if err != nil {
		return nil, err
	}
	states := map[string]JobState{}
	if b, err := h.ReadFile(ctx, StatePath(app, dest), m.Sudo); err == nil {
		_ = json.Unmarshal(b, &states)
	}
	var out []JobStatus
	for _, s := range specs {
		unit := UnitName(s.App, s.Destination, s.Job)
		js := JobStatus{Job: s.Job, Schedule: s.Schedule, Timer: unit + ".timer", State: states[s.Job]}
		props, err := h.Output(ctx, remote.Cmd{Script: m.prelude() +
			m.systemctl() + " show " + remote.Quote(unit+".timer") + " -p ActiveState -p NextElapseUSecRealtime -p LastTriggerUSec\n" +
			m.systemctl() + " show " + remote.Quote(unit+".service") + " -p Result\n"})
		if err == nil {
			kv := parseProps(props)
			js.TimerState, js.NextRun, js.LastTrigger, js.ServiceResult = kv["ActiveState"], kv["NextElapseUSecRealtime"], kv["LastTriggerUSec"], kv["Result"]
		}
		out = append(out, js)
	}
	return out, nil
}

func parseProps(s string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// marshalState is split out for tests.
func marshalState(states map[string]JobState) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	_ = enc.Encode(states)
	return b.Bytes()
}
