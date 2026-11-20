package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

func init() { extraCommands = append(extraCommands, serviceCmd) }
func serviceCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "service", Short: "Manage Service placement"}
	var target string
	var yes, cleanup, show bool
	move := &cobra.Command{Use: "move SERVICE --to SERVER", Short: "Move a Stateful Service with a fresh Backup and stop-first cutover", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, e := g.load(cmd)
		if e != nil {
			return e
		}
		if target == "" {
			return &validationError{fmt.Errorf("--to SERVER is required")}
		}
		return a.moveService(cmd.Context(), args[0], target, yes, cleanup, show)
	}}
	move.Flags().StringVar(&target, "to", "", "Destination Server name")
	move.Flags().BoolVar(&yes, "auto-approve", false, "Approve the displayed stop-first move")
	move.Flags().BoolVar(&show, "plan", false, "Show the move without changing Servers")
	move.Flags().BoolVar(&cleanup, "cleanup-source", false, "Remove retained source volumes from a previously successful move")
	addLockWaitFlag(move)
	c.AddCommand(move)
	return c
}
func (a *app) moveService(ctx context.Context, service, target string, yes, cleanup, show bool) (err error) {
	defer func() {
		if err != nil {
			a.ui.Finished(classifyMutation(err, !preMutation(ctx)), "")
		}
	}()
	if a.dest.Roles == nil {
		return &validationError{fmt.Errorf("service move requires a Roles Destination")}
	}
	s, err := a.openSession(ctx, "move", true, false)
	if err != nil {
		return err
	}
	defer s.close()
	d, err := a.newDeploy(s, nil, true)
	if err != nil {
		return err
	}
	if cleanup {
		if _, ok := s.r.Project.Services[service]; !ok {
			return &validationError{fmt.Errorf("unknown Service %s", service)}
		}
		if show {
			return &validationError{fmt.Errorf("--cleanup-source cannot be combined with --plan")}
		}
		a.ui.Plan([]plan.Change{{Kind: "volume", Name: service + " retained source volumes", Action: plan.ActionDelete, Reasons: []string{"successful move journal and ownership must be verified"}}})
		if err = a.confirmApply(applyInput, isTTY(), yes); err != nil {
			return err
		}
		unlock, e := a.operationLocks(ctx, s.hosts, deploy.LockInfo{Command: "service move --cleanup-source"}, true)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()
		if err = beginOperation(ctx); err != nil {
			return err
		}
		if err = deploy.CleanupMovedSource(ctx, d, service, target); err != nil {
			return err
		}
		a.ui.Finished(nil, "Removed retained source volumes")
		return nil
	}
	m, err := deploy.PlanMove(d, service, target)
	if err != nil {
		return &validationError{err}
	}
	j, err := a.moveBackupJob(ctx, s, service)
	if err != nil {
		return &validationError{err}
	}
	a.ui.Plan([]plan.Change{{Kind: "service", Name: service, Server: target, Action: plan.ActionReplace, Downtime: true, Reasons: []string{"move from " + m.Source + " to " + target, "fresh Backup; stop source; restore empty volumes: " + strings.Join(m.Volumes, ", "), "repoint consumers: " + strings.Join(m.Consumers, ", ")}}})
	if show {
		return nil
	}
	if err = a.confirmApply(applyInput, isTTY(), yes); err != nil {
		return err
	}
	unlock, err := a.operationLocks(ctx, s.hosts, deploy.LockInfo{Command: "service move"}, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()
	// Reload current images under every lock; never build or silently upgrade data.
	images := map[string]string{}
	built := []string{}
	version := ""
	sourceImageID := ""
	for _, h := range s.hosts {
		rel, e := deploy.CurrentRelease(ctx, h.Host, d.App, d.Destination)
		if e != nil {
			return e
		}
		if rel == nil {
			continue
		}
		if version != "" && version != rel.Version {
			return fmt.Errorf("Servers have mixed Releases; converge with yoho apply before moving")
		}
		version = rel.Version
		if h.Name == m.Source {
			sourceImageID = rel.ImageIDs[service]
		}
		for svc, img := range rel.Images {
			if prev := images[svc]; prev != "" && prev != img {
				return fmt.Errorf("Service %s images differ across Servers", svc)
			}
			images[svc] = img
		}
		built = append(built, rel.Built...)
	}
	if version == "" {
		return fmt.Errorf("deploy the Destination before moving a Service")
	}
	if err = a.checkRotateVersion(version); err != nil {
		return err
	}
	s.ver = version
	// newDeploy already stripped build on its copy, so set the current images.
	d.Version = version
	for name, svc := range d.Project.Services {
		if images[name] == "" {
			return fmt.Errorf("Service %s has no deployed image", name)
		}
		svc.Image = images[name]
		svc.Build = nil
		d.Project.Services[name] = svc
	}
	slices.Sort(built)
	d.Built = slices.Compact(built)
	if err = a.refusePendingRotations(ctx, s.hosts, nil); err != nil {
		return err
	}
	if err = a.checkMoveConfig(ctx, d); err != nil {
		return err
	}
	for _, h := range s.hosts {
		if h.Name == target {
			image := d.Project.Services[service].Image
			id, e := h.Host.Output(ctx, remote.Cmd{Script: "set -eu\ndocker image inspect --format '{{.Id}}' " + remote.Quote(image)})
			if e != nil || strings.TrimSpace(id) == "" || sourceImageID == "" || strings.TrimSpace(id) != sourceImageID {
				return fmt.Errorf("target Server lacks the deployed Version's image %s; ship that image before moving", image)
			}
		}
	}
	if err = (deploy.Roles{}).Preflight(ctx, d); err != nil {
		return err
	}
	if err = beginOperation(ctx); err != nil {
		return err
	}
	var source plan.NamedHost
	for _, h := range s.hosts {
		if h.Name == m.Source {
			source = h
		}
	}
	runBackup := func(ctx context.Context) error {
		_, e := backup.Run(ctx, backup.RunOptions{App: d.App, Destination: d.Destination, Project: a.project(), Services: j.services, Target: j.target, Password: j.password, TargetEnv: j.env, RcloneConfig: j.rcloneConf, TargetName: j.targetName, Host: source.Host, Out: d.Out, YohoVersion: Version})
		return e
	}
	d.Out = s.store.Redactor(a.ui.Progress())
	d.Hook = a.hookFunc(version, "service move", s.hosts)
	err = deploy.MoveService(ctx, d, m, runBackup, func(step, state string) {
		a.progress(ctx, source.Name+"/move/"+service+"/"+step, state)
		a.ui.Event("service_move", "", map[string]any{"service": service, "source": m.Source, "target": target, "step": step, "state": state})
	})
	if f, ok := d.Out.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	if err != nil {
		var partial *deploy.RolesFailure
		if !errors.As(err, &partial) {
			return err
		}
		a.ui.Warn("Move is partial: target data is retained and source remains stopped. Inspect the move journal and converge using this target placement.")
	}
	edit := deploy.MovedDestination(a.dest, service, target)
	data, e := yaml.Marshal(map[string]any{"destinations": map[string]any{a.destName: map[string]any{"roles": edit.Roles}}})
	if e != nil {
		return e
	}
	a.ui.Info("Update and commit this Role placement in %s before the next apply:\n%s", a.path, string(data))
	if err != nil {
		return err
	}
	a.ui.Finished(nil, "Moved "+service+" to "+target+"; source volumes retained")
	return nil
}
func (a *app) moveBackupJob(ctx context.Context, s *session, service string) (*backupJob, error) {
	var job *backupJob
	for _, n := range sortedKeys(a.cfg.Backups.Jobs) {
		jc := a.cfg.Backups.Jobs[n]
		if jc.Destination == a.destName && (len(jc.Services) == 0 || slices.Contains(jc.Services, service)) {
			var e error
			job, e = a.newBackupJob(n, jc)
			if e != nil {
				return nil, e
			}
			break
		}
	}
	if job == nil && len(a.cfg.Backups.Targets) == 1 {
		for name := range a.cfg.Backups.Targets {
			var e error
			job, e = a.newBackupJob("", config.BackupJob{Destination: a.destName, Target: name})
			if e != nil {
				return nil, e
			}
		}
	}
	if job == nil {
		return nil, fmt.Errorf("configure a Backup job and Backup Target for Service %s before moving", service)
	}
	job.cfg.Services = []string{service}
	if e := a.resolveServices(ctx, job); e != nil {
		return nil, e
	}
	if e := secretsFromStore(job, s.store); e != nil {
		return nil, e
	}
	return job, nil
}

func (a *app) checkMoveConfig(ctx context.Context, d *plan.Deploy) error {
	changes, e := (deploy.Roles{}).Diff(ctx, d)
	if e != nil {
		return e
	}
	for _, c := range changes {
		if c.Kind == "service" && c.Action != plan.ActionNoop {
			return fmt.Errorf("local Compose or secrets differ from the deployed Version on %s/%s; deploy or undo those changes before moving", c.Server, c.Name)
		}
	}
	return nil
}
