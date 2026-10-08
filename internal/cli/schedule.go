package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/dist"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/schedule"
	"github.com/yoho-build/yoho/internal/secrets"
)

func init() {
	extraCommands = append(extraCommands, scheduleCmd)
}

func scheduleCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "schedule",
		Short: "Install Scheduled Jobs (Backups) as systemd timers on the Server",
		Long: `Scheduled Jobs run Backup jobs that have a schedule (systemd OnCalendar)
on the Server itself, without the operator's machine. Install uploads a
Linux yoho binary, the job spec and the Backup Target's secrets (0600 files
under <root>/apps/<app>/<destination>/scheduled/) and enables one timer per
job. Without sudo, user timers (systemctl --user) are used; they need
linger (sudo loginctl enable-linger <user>) to run while logged out.`,
	}
	c.AddCommand(scheduleInstallCmd(g), scheduleStatusCmd(g), scheduleRemoveCmd(g), scheduleRunCmd(g))
	return c
}

// scheduleMode picks system units with sudo, user units otherwise.
func (a *app) scheduleMode() schedule.Mode {
	srv := a.cfg.Servers[a.dest.Servers[0]]
	m := schedule.Mode{Sudo: srv.Sudo, User: a.cfg.Setup.User}
	// The SSH user runs deploys and owns the Server root, so it runs jobs too.
	if user, _, _, err := remote.ParseTarget(srv.SSH); err == nil && user != "" && user != "root" {
		m.User = user
	}
	return m
}

func scheduleInstallCmd(g *globals) *cobra.Command {
	var binary string
	c := &cobra.Command{
		Use:   "install [JOB...]",
		Short: "Install timers for Backup jobs with a schedule (default: all of the Destination)",
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			u := a.ui
			defer func() {
				if err != nil {
					u.Finished(err, "")
				}
			}()
			names, err := a.scheduledJobNames(args)
			if err != nil {
				return err
			}
			u.Title("Installing Scheduled Jobs of %s (%s): %s", a.cfg.App, a.destName, strings.Join(names, ", "))
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			if err := a.installJobs(ctx, hosts, names, binary, nil); err != nil {
				return err
			}
			u.Finished(nil, fmt.Sprintf("Installed %d Scheduled Job(s); check them with `yoho schedule status`", len(names)))
			return nil
		},
	}
	c.Flags().StringVar(&binary, "binary", "", "yoho binary for the Server's OS/arch (default: this binary, a release download, or a cross-build from source)")
	return c
}

// installJobs uploads the yoho binary and installs timers for names.
// store supplies Backup Target secrets when non-nil; otherwise they are loaded.
// It prints steps and does not print a title or a finished line.
func (a *app) installJobs(ctx context.Context, hosts []plan.NamedHost, names []string, binaryFlag string, store *secrets.Store) error {
	if len(names) == 0 {
		return nil
	}
	if len(hosts) == 0 {
		return errors.New("no Servers for Scheduled Jobs")
	}
	u := a.ui
	var jobs []schedule.Job
	var loaded bool
	for _, n := range names {
		jc, ok := a.cfg.Backups.Jobs[n]
		if !ok {
			return fmt.Errorf("no Backup job %q%s", n, jobList(a.cfg.Backups.Jobs))
		}
		j, jerr := a.newBackupJob(n, jc)
		if jerr == nil && !loaded {
			step := u.Step("", "Load compose")
			if jerr = a.resolveServices(ctx, j); jerr != nil {
				step.Fail(jerr, "run `yoho config check`")
				return &silentError{jerr}
			}
			step.Done()
			loaded = true
		} else if jerr == nil {
			jerr = a.resolveServices(ctx, j)
		}
		if jerr == nil && (j.target.PasswordSecret != "" || len(j.target.EnvSecrets) > 0) {
			step := u.Step("", "Resolve secrets of %s", j.targetName)
			if store != nil {
				jerr = secretsFromStore(j, store)
			} else {
				jerr = a.resolveTargetSecrets(ctx, j)
			}
			if jerr != nil {
				step.Fail(jerr, "check .yoho/secrets and that op / bw / bws are signed in")
				return &silentError{jerr}
			}
			step.Done()
		}
		if jerr != nil {
			return jerr
		}
		sec := map[string]string{}
		if k := j.target.PasswordSecret; k != "" {
			sec[k] = j.password
		}
		for k, v := range j.env {
			sec[k] = v
		}
		jobs = append(jobs, schedule.Job{
			App: a.cfg.App, Destination: a.destName, Name: n, Project: a.project(),
			Services: j.services, TargetName: j.targetName, Target: j.target,
			Schedule: j.cfg.Schedule, Secrets: sec,
		})
	}

	h := hosts[0]
	if len(hosts) > 1 {
		u.Info("Scheduled Jobs run only on %s (the first Server of %s)", h.Name, a.destName)
	}
	step := u.Step(h.Name, "Prepare yoho binary")
	platform, err := build.ServerPlatform(ctx, h.Host)
	var binary string
	var detail string
	if err == nil {
		sw := &sourceWriter{w: step.Output()}
		binary, err = linuxBinary(ctx, binaryFlag, platform, sw)
		if ferr := sw.flush(); err == nil && ferr != nil {
			err = ferr
		}
		detail = sw.detail
	}
	if err != nil {
		step.Fail(err, "pass --binary with a yoho binary built for the Server (e.g. `make dist`)")
		return &silentError{err}
	}
	if detail == "" {
		detail = platform
	}
	step.Done(detail)

	mode := a.scheduleMode()
	step = u.Step(h.Name, "Install timers (%s)", map[bool]string{true: "system units, sudo", false: "user units"}[mode.Sudo])
	res, err := schedule.Install(ctx, h.Host, schedule.InstallOptions{
		Jobs: jobs, YohoBinaryLocalPath: binary, Mode: mode, Out: step.Output(),
	})
	if err != nil {
		step.Fail(err, "check that systemd runs on the Server; without sudo the user needs a systemd user session (loginctl)")
		return &silentError{err}
	}
	step.Done(strings.Join(res.Timers, ", "))
	for _, w := range res.Warnings {
		u.Warn("%s", w)
	}
	return nil
}

// secretsFromStore fills the Backup Target password and env from store.
func secretsFromStore(j *backupJob, store *secrets.Store) error {
	j.env = map[string]string{}
	j.store = store
	if j.target.PasswordSecret == "" && len(j.target.EnvSecrets) == 0 {
		return nil
	}
	if store == nil {
		return fmt.Errorf("secret store is required for Backup Target %s", j.targetName)
	}
	get := func(k string) (string, error) {
		v, ok := store.Get(k)
		if !ok {
			return "", fmt.Errorf("secret %s for Backup Target %s is not defined in .yoho/secrets or secrets.values", k, j.targetName)
		}
		return v, nil
	}
	var err error
	if k := j.target.PasswordSecret; k != "" {
		if j.password, err = get(k); err != nil {
			return err
		}
	}
	for _, k := range j.target.EnvSecrets {
		if j.env[k], err = get(k); err != nil {
			return err
		}
	}
	return nil
}

// scheduledJobNames returns the named jobs, or every job of the
// Destination with a schedule.
func (a *app) scheduledJobNames(args []string) ([]string, error) {
	jobs := a.cfg.Backups.Jobs
	if len(args) > 0 {
		for _, n := range args {
			j, ok := jobs[n]
			if !ok {
				return nil, fmt.Errorf("no Backup job %q%s", n, jobList(jobs))
			}
			if j.Destination != a.destName {
				return nil, fmt.Errorf("Backup job %s belongs to Destination %s\nhint: pass -d %s", n, j.Destination, j.Destination)
			}
			if j.Schedule == "" {
				return nil, fmt.Errorf("Backup job %s has no schedule\nhint: set backups.jobs.%s.schedule, e.g. \"*-*-* 03:00:00\"", n, n)
			}
		}
		return args, nil
	}
	var names []string
	for _, n := range sortedKeys(jobs) {
		if jobs[n].Destination == a.destName && jobs[n].Schedule != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no Backup job with a schedule for Destination %s\nhint: set backups.jobs.<name>.schedule, e.g. \"*-*-* 03:00:00\"", a.destName)
	}
	return names, nil
}

// linuxBinary returns a yoho binary for platform (linux/<arch>). Order:
// explicit --binary, the running binary when it matches, a release download
// when Version is not "dev", then a cross-build from source.
// It writes "yoho-binary-source <detail>" for the step detail.
func linuxBinary(ctx context.Context, explicit, platform string, out interface{ Write([]byte) (int, error) }) (string, error) {
	osName, arch, _ := strings.Cut(platform, "/")
	if osName != "linux" {
		return "", fmt.Errorf("Scheduled Jobs need a Linux Server, not %s", platform)
	}
	if explicit != "" {
		noteBinarySource(out, explicit)
		return explicit, nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		noteBinarySource(out, "this binary")
		return exe, nil
	}
	var dlErr error
	if Version != "" && Version != "dev" {
		fmt.Fprintf(out, "downloading release %s for %s\n", Version, platform)
		p, err := dist.Download(ctx, Version, platform, "")
		if err == nil {
			noteBinarySource(out, "release "+Version)
			return p, nil
		}
		dlErr = err
		fmt.Fprintf(out, "release download failed: %v\n", err)
	}
	src := yohoSourceDir()
	gobin, gerr := exec.LookPath("go")
	if src == "" || gerr != nil {
		hint := "pass --binary with a yoho binary built for the Server (e.g. `make dist`)"
		if dlErr != nil {
			return "", fmt.Errorf("no yoho binary for %s: %w\nhint: %s", platform, dlErr, hint)
		}
		return "", fmt.Errorf("this yoho is not built for %s\nhint: %s", platform, hint)
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("yoho-%s-%s", osName, arch))
	if dlErr != nil {
		fmt.Fprintf(out, "cross-building %s from %s after release download failed\n", platform, src)
	} else {
		fmt.Fprintf(out, "cross-building %s from %s\n", platform, src)
	}
	c := exec.CommandContext(ctx, gobin, "build", "-trimpath", "-ldflags", "-s -w -X github.com/yoho-build/yoho/internal/cli.Version="+Version, "-o", tmp, "./cmd/yoho")
	c.Dir = src
	c.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	c.Stdout, c.Stderr = out, out
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("cross-build yoho for %s: %w", platform, err)
	}
	noteBinarySource(out, "cross-built from source")
	return tmp, nil
}

func noteBinarySource(out interface{ Write([]byte) (int, error) }, detail string) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, "yoho-binary-source %s\n", detail)
}

// sourceWriter records a yoho-binary-source line and forwards the rest.
// go build writes stdout and stderr concurrently, so the buffer is locked.
type sourceWriter struct {
	mu     sync.Mutex
	w      interface{ Write([]byte) (int, error) }
	detail string
	buf    []byte
}

func (s *sourceWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := string(s.buf[:i])
		s.buf = s.buf[i+1:]
		if err := s.emit(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (s *sourceWriter) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	line := string(s.buf)
	s.buf = nil
	return s.emit(line)
}

func (s *sourceWriter) emit(line string) error {
	const pfx = "yoho-binary-source "
	if rest, ok := strings.CutPrefix(line, pfx); ok {
		s.detail = rest
		return nil
	}
	if s.w == nil {
		return nil
	}
	_, err := s.w.Write([]byte(line + "\n"))
	return err
}

// yohoSourceDir finds the yoho module source: $YOHO_SOURCE, or the
// directory this file was compiled from (non -trimpath dev builds).
func yohoSourceDir() string {
	isModule := func(dir string) bool {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		return err == nil && strings.Contains(string(b), "module github.com/yoho-build/yoho\n")
	}
	if d := os.Getenv("YOHO_SOURCE"); d != "" && isModule(d) {
		return d
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return ""
	}
	d := filepath.Dir(filepath.Dir(filepath.Dir(file))) // internal/cli/schedule.go -> module root
	if isModule(d) {
		return d
	}
	return ""
}

func scheduleStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show installed Scheduled Jobs, their timers and last runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			step := a.ui.Step(hosts[0].Name, "Read Scheduled Jobs")
			sts, err := schedule.Status(ctx, hosts[0].Host, a.cfg.App, a.destName, a.scheduleMode())
			if err != nil {
				step.Fail(err, "rerun with -v")
				return &silentError{err}
			}
			step.Done(fmt.Sprintf("%d jobs", len(sts)))
			if len(sts) == 0 {
				a.ui.Info("no Scheduled Jobs installed; run `yoho schedule install`")
				return nil
			}
			rows := make([][]string, 0, len(sts))
			for _, s := range sts {
				last, backupID := "-", s.State.BackupID
				if !s.State.LastRun.IsZero() {
					last = s.State.LastRun.Local().Format("2006-01-02 15:04:05")
					if s.State.Error != "" {
						last += " FAILED"
					} else {
						last += " ok"
					}
				}
				if backupID == "" {
					backupID = "-"
				}
				rows = append(rows, []string{s.Job, s.Schedule, s.TimerState, orDash(s.NextRun), last, backupID, orDash(s.ServiceResult)})
			}
			a.ui.Table([]string{"JOB", "SCHEDULE", "TIMER", "NEXT RUN", "LAST RUN", "LAST BACKUP", "RESULT"}, rows)
			for _, s := range sts {
				if s.State.Error != "" {
					a.ui.Warn("%s: last run failed: %s", s.Job, s.State.Error)
				}
			}
			return nil
		},
	}
}

func orDash(s string) string {
	if s == "" || s == "n/a" {
		return "-"
	}
	return s
}

func scheduleRemoveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "remove [JOB...]",
		Short: "Disable and delete Scheduled Jobs (default: all of the Destination)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			return a.removeJobs(ctx, hosts, args)
		},
	}
}

// removeJobs disables and deletes Scheduled Jobs. Empty names removes every
// job of the Destination. It prints a step and does not print a title.
func (a *app) removeJobs(ctx context.Context, hosts []plan.NamedHost, names []string) error {
	if len(hosts) == 0 {
		return errors.New("no Servers for Scheduled Jobs")
	}
	h := hosts[0]
	step := a.ui.Step(h.Name, "Remove Scheduled Jobs")
	removed, err := schedule.Remove(ctx, h.Host, schedule.RemoveOptions{
		App: a.cfg.App, Destination: a.destName, Jobs: names, Mode: a.scheduleMode(),
	})
	if err != nil {
		step.Fail(err, "rerun with -v")
		return &silentError{err}
	}
	if len(removed) == 0 {
		step.Skip("none installed")
		return nil
	}
	step.Done(strings.Join(removed, ", "))
	return nil
}

// scheduleRunCmd is what the systemd service executes on the Server.
func scheduleRunCmd(g *globals) *cobra.Command {
	var specPath string
	c := &cobra.Command{
		Use:    "run --job SPEC",
		Short:  "Run a Scheduled Job on this Server (used by the systemd unit)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if specPath == "" {
				return errors.New("--job is required")
			}
			b, err := os.ReadFile(specPath)
			if err != nil {
				return fmt.Errorf("read job spec: %w", err)
			}
			var s schedule.Spec
			if err := json.Unmarshal(b, &s); err != nil {
				return fmt.Errorf("parse job spec: %w", err)
			}
			h := &remote.Local{}
			host, _ := os.Hostname()
			lock := func(ctx context.Context) (func(context.Context) error, error) {
				return deploy.AcquireLock(ctx, h, s.App, s.Destination, deploy.LockInfo{
					Performer: "schedule@" + host, Command: "backup " + s.Job,
				})
			}
			start := time.Now()
			out := cmd.OutOrStdout()
			err = schedule.RunJob(cmd.Context(), specPath, schedule.RunJobOptions{
				Host: h, Out: out, Lock: lock, YohoVersion: Version,
			})
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Scheduled Job %s failed after %s: %v\n", s.Job, time.Since(start).Round(time.Second), err)
				return &silentError{err}
			}
			fmt.Fprintf(out, "Scheduled Job %s finished in %s\n", s.Job, time.Since(start).Round(time.Second))
			return nil
		},
	}
	c.Flags().StringVar(&specPath, "job", "", "Job spec path on this Server")
	return c
}
