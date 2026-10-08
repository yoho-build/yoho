package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/schedule"
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
			var jobs []schedule.Job
			var loaded bool
			for _, n := range names {
				j, jerr := a.newBackupJob(n, a.cfg.Backups.Jobs[n])
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
					if jerr = a.resolveTargetSecrets(ctx, j); jerr != nil {
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

			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			h := hosts[0]

			step := u.Step(h.Name, "Prepare yoho binary")
			platform, err := build.ServerPlatform(ctx, h.Host)
			if err == nil {
				binary, err = linuxBinary(ctx, binary, platform, step.Output())
			}
			if err != nil {
				step.Fail(err, "pass --binary with a yoho binary built for the Server (e.g. `make dist`)")
				return &silentError{err}
			}
			step.Done(platform)

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
			u.Finished(nil, fmt.Sprintf("Installed %d Scheduled Job(s); check them with `yoho schedule status`", len(res.Timers)))
			return nil
		},
	}
	c.Flags().StringVar(&binary, "binary", "", "yoho binary for the Server's OS/arch (default: this binary, or a cross-build from source)")
	return c
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

// linuxBinary returns a yoho binary for platform (linux/<arch>): explicit,
// the running binary when it matches, or a cross-build of the source this
// binary was built from (development). Release downloads come later.
func linuxBinary(ctx context.Context, explicit, platform string, out interface{ Write([]byte) (int, error) }) (string, error) {
	osName, arch, _ := strings.Cut(platform, "/")
	if osName != "linux" {
		return "", fmt.Errorf("Scheduled Jobs need a Linux Server, not %s", platform)
	}
	if explicit != "" {
		return explicit, nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		return os.Executable()
	}
	src := yohoSourceDir()
	gobin, gerr := exec.LookPath("go")
	if src == "" || gerr != nil {
		return "", errors.New("this yoho is not built for " + platform + " and release downloads are not available yet; build one with `make dist` and pass --binary")
	}
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("yoho-%s-%s", osName, arch))
	fmt.Fprintf(out, "cross-building %s from %s\n", platform, src)
	c := exec.CommandContext(ctx, gobin, "build", "-trimpath", "-ldflags", "-s -w -X github.com/yoho-build/yoho/internal/cli.Version="+Version, "-o", tmp, "./cmd/yoho")
	c.Dir = src
	c.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	c.Stdout, c.Stderr = out, out
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("cross-build yoho for %s: %w", platform, err)
	}
	return tmp, nil
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
			step := a.ui.Step(hosts[0].Name, "Remove Scheduled Jobs")
			removed, err := schedule.Remove(ctx, hosts[0].Host, schedule.RemoveOptions{
				App: a.cfg.App, Destination: a.destName, Jobs: args, Mode: a.scheduleMode(),
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
		},
	}
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
