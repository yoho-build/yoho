package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

func init() {
	extraCommands = append(extraCommands, backupCmd)
}

// backupJob is a Backup job resolved against the Yoho file and compose
// file: which Services, which Backup Target, and the target's secrets.
type backupJob struct {
	name       string // empty for an ad-hoc job (single Backup Target, no jobs)
	cfg        config.BackupJob
	targetName string
	target     config.BackupTarget
	services   map[string]config.ServiceBackup
	password   string
	env        map[string]string
	store      *secrets.Store // nil when the target needs no secrets
}

func (j *backupJob) label() string {
	if j.name == "" {
		return "target " + j.targetName
	}
	return j.name
}

// loadForJob loads the App for Backup job name (any job when empty). A job
// pins its Destination, so -d may be omitted when the job names it.
func (g *globals) loadForJob(cmd *cobra.Command, name string) (*app, error) {
	if name != "" && g.destination == "" {
		p := g.configPath
		if p == "" {
			if wd, err := os.Getwd(); err == nil {
				p, _ = config.Find(wd)
			}
		}
		if p != "" {
			if cfg, err := config.Load(p); err == nil {
				if j, ok := cfg.Backups.Jobs[name]; ok {
					g.destination = j.Destination
				}
			}
		}
	}
	return g.load(cmd)
}

// selectBackupJob picks job name, the only job of the Destination, or,
// with no jobs at all, the only Backup Target.
func (a *app) selectBackupJob(name string) (*backupJob, error) {
	jobs := a.cfg.Backups.Jobs
	if name != "" {
		jc, ok := jobs[name]
		if !ok {
			return nil, fmt.Errorf("no Backup job %q in %s%s", name, a.path, jobList(jobs))
		}
		if jc.Destination != a.destName {
			return nil, fmt.Errorf("Backup job %s belongs to Destination %s, not %s\nhint: pass -d %s", name, jc.Destination, a.destName, jc.Destination)
		}
		return a.newBackupJob(name, jc)
	}
	var mine []string
	for _, n := range sortedKeys(jobs) {
		if jobs[n].Destination == a.destName {
			mine = append(mine, n)
		}
	}
	switch {
	case len(mine) == 1:
		return a.newBackupJob(mine[0], jobs[mine[0]])
	case len(mine) > 1:
		return nil, fmt.Errorf("several Backup jobs for %s; name one: %s", a.destName, strings.Join(mine, ", "))
	case len(a.cfg.Backups.Targets) == 1:
		for tn := range a.cfg.Backups.Targets {
			return a.newBackupJob("", config.BackupJob{Destination: a.destName, Target: tn})
		}
	}
	return nil, fmt.Errorf("no Backup job for Destination %s\nhint: add backups.targets and backups.jobs to %s", a.destName, a.path)
}

func jobList(jobs map[string]config.BackupJob) string {
	if len(jobs) == 0 {
		return " (no jobs defined)"
	}
	return "; jobs: " + strings.Join(sortedKeys(jobs), ", ")
}

func (a *app) newBackupJob(name string, jc config.BackupJob) (*backupJob, error) {
	t, ok := a.cfg.Backups.Targets[jc.Target]
	if !ok {
		return nil, fmt.Errorf("Backup job %s: no Backup Target %q", name, jc.Target)
	}
	return &backupJob{name: name, cfg: jc, targetName: jc.Target, target: t}, nil
}

// resolveServices fills j.services from x-yoho.backup in the compose file.
func (a *app) resolveServices(ctx context.Context, j *backupJob) error {
	r, err := a.compose(ctx)
	if err != nil {
		return err
	}
	all := map[string]config.ServiceBackup{}
	for svc, ext := range r.Ext {
		if ext.Backup != nil {
			all[svc] = *ext.Backup
		}
	}
	j.services = map[string]config.ServiceBackup{}
	if len(j.cfg.Services) == 0 {
		j.services = all
	}
	for _, svc := range j.cfg.Services {
		sb, ok := all[svc]
		if !ok {
			return fmt.Errorf("Service %s of job %s has no x-yoho.backup in the compose file", svc, j.label())
		}
		j.services[svc] = sb
	}
	if len(j.services) == 0 {
		return errors.New("no Service has x-yoho.backup; add e.g. `x-yoho: {backup: {volumes: [data]}}` to a Service")
	}
	return nil
}

// resolveTargetSecrets loads the Backup Target's password and env secrets.
func (a *app) resolveTargetSecrets(ctx context.Context, j *backupJob) error {
	j.env = map[string]string{}
	if j.target.PasswordSecret == "" && len(j.target.EnvSecrets) == 0 {
		return nil
	}
	store, err := a.loadSecrets(ctx)
	if err != nil {
		return err
	}
	j.store = store
	get := func(k string) (string, error) {
		v, ok := store.Get(k)
		if !ok {
			return "", fmt.Errorf("secret %s for Backup Target %s is not defined in .yoho/secrets or secrets.values", k, j.targetName)
		}
		return v, nil
	}
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

// redact wraps w in the secrets redactor when secrets were loaded.
func (j *backupJob) redact(w io.Writer) (io.Writer, func()) {
	if j.store == nil {
		return w, func() {}
	}
	r := j.store.Redactor(w)
	return r, func() { r.Flush() }
}

// prepareBackup resolves job, Services and secrets with UI steps.
func (a *app) prepareBackup(ctx context.Context, name string, needServices bool) (*backupJob, error) {
	j, err := a.selectBackupJob(name)
	if err != nil {
		return nil, err
	}
	if needServices {
		step := a.ui.Step("", "Load compose")
		if err := a.resolveServices(ctx, j); err != nil {
			step.Fail(err, "run `yoho config check`")
			return nil, &silentError{err}
		}
		step.Done(strings.Join(sortedKeys(j.services), ", "))
	}
	if j.target.PasswordSecret != "" || len(j.target.EnvSecrets) > 0 {
		step := a.ui.Step("", "Resolve Backup Target secrets")
		if err := a.resolveTargetSecrets(ctx, j); err != nil {
			step.Fail(err, "check .yoho/secrets and that op / bw / bws are signed in")
			return nil, &silentError{err}
		}
		step.Done()
	} else {
		j.env = map[string]string{}
	}
	return j, nil
}

func (a *app) backupLock(h remote.Host, command string) backup.LockFunc {
	return func(ctx context.Context) (func(context.Context) error, error) {
		return deploy.AcquireLock(ctx, h, a.cfg.App, a.destName, deploy.LockInfo{Performer: performer(), Command: command})
	}
}

// serverVersion reads the current Release version on the Server for Hooks.
func (a *app) serverVersion(ctx context.Context, h remote.Host) string {
	b, err := h.ReadFile(ctx, path.Join(release.AppDir(a.cfg.App, a.destName), "current", "release.json"), false)
	if err != nil {
		return ""
	}
	var r release.Release
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.Version
}

func backupCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Take, list and restore Backups of Stateful Services",
		Long: `Backups copy the volumes and dumps declared in x-yoho.backup to a Backup
Target defined in the Yoho file (backups.targets / backups.jobs).`,
	}
	c.AddCommand(backupRunCmd(g), backupListCmd(g), backupRestoreCmd(g))
	return c
}

func backupRunCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "run [JOB]",
		Short: "Take a Backup now (default: the Destination's only Backup job)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			a, err := g.loadForJob(cmd, name)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			u := a.ui
			start := time.Now()
			defer func() {
				if err != nil {
					u.Finished(err, "")
				}
			}()
			if sel, serr := a.selectBackupJob(name); serr == nil {
				u.Title("Backing up %s (%s) to %s", a.cfg.App, a.destName, sel.targetName)
			}
			j, err := a.prepareBackup(ctx, name, true)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			h := hosts[0]
			hook := a.hookFunc(a.serverVersion(ctx, h.Host), "backup", hosts)
			hookEnv := map[string]string{"YOHO_SUBCOMMAND": "run", "YOHO_BACKUP_JOB": j.name, "YOHO_BACKUP_TARGET": j.targetName}
			if err = hook(ctx, "pre-backup", hookEnv); err != nil {
				return err
			}
			step := u.Step(h.Name, "Back up %s", strings.Join(sortedKeys(j.services), ", "))
			out, flush := j.redact(step.Output())
			res, err := backup.Run(ctx, backup.RunOptions{
				App: a.cfg.App, Destination: a.destName, Project: a.project(),
				Services: j.services, Target: j.target, Password: j.password, TargetEnv: j.env,
				Host: h.Host, Out: out, Lock: a.backupLock(h.Host, "backup"), YohoVersion: Version,
				Runtime: a.dest.Runtime,
			})
			flush()
			if err != nil {
				step.Fail(err, backupHint(err))
				return &silentError{err}
			}
			step.Done(fmt.Sprintf("%s · %s · %s", res.ID, humanBytes(res.Size), res.Method))
			hookEnv["YOHO_BACKUP_ID"] = res.ID
			if err = hook(ctx, "post-backup", hookEnv); err != nil {
				return err
			}
			u.Finished(nil, fmt.Sprintf("Backup %s of %s (%s) stored in %s in %s", res.ID, a.cfg.App, a.destName, j.targetName, time.Since(start).Round(100*time.Millisecond)))
			return nil
		},
	}
}

func backupHint(err error) string {
	var le *deploy.LockedError
	switch {
	case errors.As(err, &le):
		return "wait for the running deploy or Backup to finish"
	case strings.Contains(err.Error(), "no running container"):
		return "deploy the App first (`yoho deploy`), then check `yoho app ps`"
	case strings.Contains(err.Error(), "restic"):
		return "check the repository string, password_secret and env_secrets of the Backup Target; rerun with -v"
	}
	return "rerun with -v to see the remote output"
}

func backupListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list [JOB]",
		Short: "List stored Backups, newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			a, err := g.loadForJob(cmd, name)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			j, err := a.prepareBackup(ctx, name, false)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			step := a.ui.Step(hosts[0].Name, "List Backups in %s", j.targetName)
			entries, err := backup.List(ctx, backup.ListOptions{
				App: a.cfg.App, Destination: a.destName, Project: a.project(),
				Target: j.target, Password: j.password, TargetEnv: j.env, Host: hosts[0].Host,
			})
			if err != nil {
				step.Fail(err, backupHint(err))
				return &silentError{err}
			}
			step.Done(fmt.Sprintf("%d Backups", len(entries)))
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				rows = append(rows, []string{e.ID, e.Time.Local().Format("2006-01-02 15:04:05"), humanBytes(e.Size)})
			}
			if len(rows) == 0 {
				a.ui.Info("no Backups yet; take one with `yoho backup run %s`", name)
				return nil
			}
			a.ui.Table([]string{"ID", "CREATED", "SIZE"}, rows)
			return nil
		},
	}
}

func backupRestoreCmd(g *globals) *cobra.Command {
	var jobName string
	var yes bool
	c := &cobra.Command{
		Use:   "restore ID",
		Short: "Restore a Backup, replacing the Services' volume data",
		Long: `Restore stops the Services in the Backup, replaces their volume contents,
starts them, loads dumps with x-yoho.backup.restore_dump and runs post_restore.
Current volume data is lost. Use "latest" as ID for the newest Backup.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			a, err := g.loadForJob(cmd, jobName)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			u := a.ui
			start := time.Now()
			defer func() {
				if err != nil {
					u.Finished(err, "")
				}
			}()
			j, err := a.prepareBackup(ctx, jobName, true)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			h := hosts[0]
			id := args[0]

			step := u.Step(h.Name, "Find Backup %s in %s", id, j.targetName)
			entries, err := backup.List(ctx, backup.ListOptions{
				App: a.cfg.App, Destination: a.destName, Project: a.project(),
				Target: j.target, Password: j.password, TargetEnv: j.env, Host: h.Host,
			})
			if err != nil {
				step.Fail(err, backupHint(err))
				return &silentError{err}
			}
			var found *backup.Entry
			for i, e := range entries {
				if e.ID == id || (id == "latest" && i == 0) {
					found = &entries[i]
					break
				}
			}
			if found == nil {
				err = fmt.Errorf("no Backup %q in %s", id, j.targetName)
				step.Fail(err, "see `yoho backup list`")
				return &silentError{err}
			}
			step.Done(found.Time.Local().Format("2006-01-02 15:04:05"))

			if !yes {
				if err = confirmRestore(cmd, a, j, h.Name, found); err != nil {
					return err
				}
			}

			hook := a.hookFunc(a.serverVersion(ctx, h.Host), "backup", hosts)
			hookEnv := map[string]string{"YOHO_SUBCOMMAND": "restore", "YOHO_BACKUP_JOB": j.name, "YOHO_BACKUP_TARGET": j.targetName, "YOHO_BACKUP_ID": found.ID}
			if err = hook(ctx, "pre-restore", hookEnv); err != nil {
				return err
			}
			step = u.Step(h.Name, "Restore %s", found.ID)
			out, flush := j.redact(step.Output())
			res, err := backup.Restore(ctx, backup.RestoreOptions{
				App: a.cfg.App, Destination: a.destName, Project: a.project(),
				Services: j.services, Target: j.target, Password: j.password, TargetEnv: j.env,
				Host: h.Host, Out: out, Lock: a.backupLock(h.Host, "restore"), ID: found.ID, Confirm: true,
				Runtime: a.dest.Runtime,
			})
			flush()
			if err != nil {
				step.Fail(err, restoreHint(err))
				return &silentError{err}
			}
			var detail []string
			if len(res.Volumes) > 0 {
				detail = append(detail, "volumes "+strings.Join(res.Volumes, ", "))
			}
			if len(res.Dumps) > 0 {
				detail = append(detail, "dumps "+strings.Join(res.Dumps, ", "))
			}
			step.Done(strings.Join(detail, " · "))
			for _, s := range res.Skipped {
				u.Warn("Service %s is not in this Backup; left unchanged", s)
			}
			if len(res.DumpFiles) > 0 {
				u.Warn("dumps not loaded (no x-yoho.backup.restore_dump); they are on the Server: %s", strings.Join(res.DumpFiles, " "))
			}
			if err = hook(ctx, "post-restore", hookEnv); err != nil {
				return err
			}
			u.Finished(nil, fmt.Sprintf("Restored %s (%s) from %s in %s", a.cfg.App, a.destName, found.ID, time.Since(start).Round(100*time.Millisecond)))
			return nil
		},
	}
	c.Flags().StringVar(&jobName, "job", "", "Backup job whose Backup Target holds the Backup (default: the only one)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return c
}

func restoreHint(err error) string {
	var le *deploy.LockedError
	switch {
	case errors.As(err, &le):
		return "wait for the running deploy or Backup to finish"
	case strings.Contains(err.Error(), "Services left stopped"):
		return "fix the cause and rerun the restore; start the Services with `yoho deploy` to serve the old data instead"
	case strings.Contains(err.Error(), "deploy the App"):
		return "deploy the App first so its volumes exist, then restore"
	case strings.Contains(err.Error(), "restore dump"):
		return "add a healthcheck to the Service so Yoho waits for it, or load the dump by hand with `yoho app exec`"
	}
	return "rerun with -v to see the remote output"
}

// confirmRestore shows what a restore overwrites and asks on a TTY.
func confirmRestore(cmd *cobra.Command, a *app, j *backupJob, server string, e *backup.Entry) error {
	if a.g.json || !stdinIsTTY() {
		return errors.New("restore replaces volume data; pass --yes to confirm non-interactively")
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "\nRestore Backup %s (taken %s) into %s (%s) on %s.\nThis overwrites:\n",
		e.ID, e.Time.Local().Format("2006-01-02 15:04:05"), a.cfg.App, a.destName, server)
	for _, svc := range sortedKeys(j.services) {
		sb := j.services[svc]
		for _, v := range sb.Volumes {
			fmt.Fprintf(w, "  - %s: volume %s (Service stopped during the restore)\n", svc, v)
		}
		if len(sb.Dump) > 0 {
			if len(sb.RestoreDump) > 0 {
				fmt.Fprintf(w, "  - %s: dump loaded with `%s`\n", svc, strings.Join(sb.RestoreDump, " "))
			} else {
				fmt.Fprintf(w, "  - %s: dump kept on the Server (no restore_dump)\n", svc)
			}
		}
	}
	fmt.Fprintln(w, "Data written since the Backup is lost.")
	ans, err := promptLine(w, "Restore? [y/N] ")
	if err != nil {
		return err
	}
	if ans != "y" && ans != "yes" {
		return errors.New("restore cancelled")
	}
	return nil
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
