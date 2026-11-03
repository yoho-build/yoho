package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/swarm"
	"github.com/yoho-build/yoho/internal/ui"
)

func init() { extraCommands = append(extraCommands, removeCmd) }

// removeFlags are the flags of `yoho remove`.
type removeFlags struct {
	volumes bool
	confirm string
	yes     bool
}

func removeCmd(g *globals) *cobra.Command {
	var f removeFlags
	c := &cobra.Command{
		Use:   "remove [--volumes [--confirm APP]] [-y]",
		Short: "Remove the Destination's containers, Proxy routes, networks, Scheduled Jobs and Releases from its Servers (volumes and Backups kept)",
		Long: `Take the App's Destination off its Servers: stop and remove its
yoho-<app>-<destination> containers (swarm: docker stack rm), its Proxy
routes, its networks, its Scheduled Jobs, and the App directory with its
Releases and secrets. Docker objects are selected by their exact compose
project / stack label, so other Apps, other Destinations and drill projects
are never touched. The shared Proxy and the yoho network stay.

Volumes and Backups are kept: a later deploy starts on the same data.
--volumes also deletes the Destination's named volumes; you confirm it by
typing the App name at the prompt, or with --confirm <app> without a
terminal. Backups (the local spool and every Backup Target) are never
deleted by remove.

On a terminal remove shows what it removes and asks for "yes" unless -y;
without a terminal (and with --json) -y is required.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.remove(cmd, f)
		},
	}
	c.Flags().BoolVar(&f.volumes, "volumes", false, "Also delete the Destination's named volumes (asks for the App name)")
	c.Flags().StringVar(&f.confirm, "confirm", "", "With --volumes: the App name, to confirm without a terminal")
	c.Flags().BoolVarP(&f.yes, "yes", "y", false, "Do not ask for confirmation")
	return c
}

// checkRemoveConfirm refuses before connecting when no one could confirm.
func checkRemoveConfirm(f removeFlags, app string, tty, jsonOut bool) error {
	if f.confirm != "" && !f.volumes {
		return &hintError{"--confirm applies to --volumes only", "drop --confirm, or add --volumes to delete the volumes too"}
	}
	if f.volumes && f.confirm != "" && f.confirm != app {
		return &hintError{fmt.Sprintf("--confirm %q is not the App name", f.confirm), "pass --confirm " + app}
	}
	interactive := tty && !jsonOut
	if !f.yes && jsonOut {
		return &hintError{"--json requires -y (there is no one to confirm)", "pass -y"}
	}
	if !f.yes && !interactive {
		return &hintError{"refusing to remove without confirmation: stdin is not a terminal", "pass -y (and --volumes --confirm " + app + " to delete volumes)"}
	}
	if f.volumes && f.confirm == "" && !interactive {
		return &hintError{"--volumes without a terminal needs --confirm " + app, "pass --volumes --confirm " + app}
	}
	return nil
}

// askRemove asks on the terminal: "yes" to remove (unless -y), then the App
// name to delete volumes (unless --confirm).
func askRemove(in io.Reader, out io.Writer, f removeFlags, app string, volumes int) error {
	r := bufio.NewReader(in)
	if !f.yes {
		fmt.Fprint(out, "\nRemove these from the Server? Only 'yes' will be accepted: ")
		line, _ := r.ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return errors.New("remove cancelled")
		}
	}
	if f.volumes && f.confirm == "" {
		fmt.Fprintf(out, "\nThis deletes %s and their data. Type the App name (%s) to confirm: ", ui.Count(volumes, "volume", ""), app)
		line, _ := r.ReadString('\n')
		if strings.TrimSpace(line) != app {
			return errors.New("remove cancelled: App name did not match, nothing was removed")
		}
	}
	return nil
}

func (a *app) removeOptions(h plan.NamedHost, svcs []string, volumes bool) deploy.RemoveOptions {
	return deploy.RemoveOptions{App: a.cfg.App, Destination: a.destName, Swarm: a.dest.Runtime == "swarm",
		Volumes: volumes, Services: svcs, Sudo: h.Server.Sudo}
}

// removeHosts are the Servers remove works on: compose runs on the first
// Server only; swarm tasks, routes and volumes may be on every Server.
func (a *app) removeHosts(hosts []plan.NamedHost) []plan.NamedHost {
	if a.dest.Runtime == "swarm" {
		return hosts
	}
	return hosts[:1]
}

func (a *app) showRemovePlan(name string, inv *deploy.RemoveInventory, volumes bool) {
	u := a.ui
	u.Event("remove_plan", "", map[string]any{"server": name, "containers": inv.Containers, "routes": inv.Routes,
		"networks": inv.Networks, "volumes": inv.Volumes, "remove_volumes": volumes, "secrets": inv.Secrets, "app_dir": inv.AppDir})
	if a.g.json {
		return
	}
	line := func(sym, kind string, items []string) {
		for _, it := range items {
			u.Info("  %s %s %s [%s]", sym, kind, it, name)
		}
	}
	line("-", "container", inv.Containers)
	line("-", "route", inv.Routes)
	line("-", "network", inv.Networks)
	line("-", "swarm secret", inv.Secrets)
	if volumes {
		line("-", "volume", inv.Volumes)
	} else {
		line("=", "volume (kept)", inv.Volumes)
	}
	if inv.AppDir != "" {
		u.Info("  - App directory %s (Releases, secrets, files) [%s]", inv.AppDir, name)
	}
}

func (a *app) remove(cmd *cobra.Command, f removeFlags) (err error) {
	ctx := cmd.Context()
	u := a.ui
	tty := isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout())
	if err := checkRemoveConfirm(f, a.cfg.App, tty, a.g.json); err != nil {
		return a.fail(err)
	}
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	defer func() {
		if err != nil {
			var se *silentError
			if !errors.As(err, &se) {
				u.Error(err, "")
				err = &silentError{err}
			}
		}
	}()
	targets := a.removeHosts(hosts)
	// Best effort: the Release plans on the Server name the other Services.
	svcs, _ := a.serviceNames(ctx)
	u.Title("Removing %s from %s (%s)", a.cfg.App, a.destName, runtimeName(a.dest))
	volumes := 0
	for _, h := range targets {
		inv, err := deploy.InspectRemove(ctx, h.Host, a.removeOptions(h, svcs, f.volumes))
		if err != nil {
			return err
		}
		a.showRemovePlan(h.Name, inv, f.volumes)
		volumes += len(inv.Volumes)
	}
	u.Info("  - Scheduled Jobs of %s", a.destName)
	u.Info("  = Backups in %s and in every Backup Target (kept)", release.BackupDir(a.cfg.App, a.destName))
	u.Info("  = Audit log %s (kept; delete it by hand if wanted, also with --volumes)", audit.Path(a.cfg.App, a.destName))
	if !f.volumes && volumes > 0 {
		u.Info("Volumes are kept; a later deploy uses them again. Pass --volumes to delete them.")
	}
	if !f.yes || (f.volumes && f.confirm == "") {
		if err := askRemove(cmd.InOrStdin(), cmd.OutOrStdout(), f, a.cfg.App, volumes); err != nil {
			return a.fail(err)
		}
	}

	mgr := hosts[0]
	step := u.Step(mgr.Name, "Lock %s/%s", a.cfg.App, a.destName)
	unlock, err := deploy.AcquireLock(ctx, mgr.Host, a.cfg.App, a.destName, deploy.LockInfo{Performer: performer(), Command: "remove"})
	if err != nil {
		step.Fail(err, "wait for the running deploy or Backup, or remove a stale lock as the message says")
		return &silentError{err}
	}
	step.Done()
	defer func() { _ = unlock(context.WithoutCancel(ctx)) }()

	if err := a.removeJobs(ctx, hosts, nil); err != nil {
		return err
	}
	removed := map[string]int{}
	run := func(h plan.NamedHost, label string, fn func() error) error {
		step := u.Step(h.Name, "%s", label)
		if err := fn(); err != nil {
			step.Fail(err, "rerun `yoho remove`; it continues where it stopped")
			return &silentError{err}
		}
		step.Done()
		return nil
	}
	invs := map[string]*deploy.RemoveInventory{}
	for _, h := range targets {
		inv, err := deploy.InspectRemove(ctx, h.Host, a.removeOptions(h, svcs, f.volumes))
		if err != nil {
			return err
		}
		invs[h.Name] = inv
		if len(inv.Routes) > 0 {
			if err := run(h, "Remove Proxy routes "+strings.Join(inv.Routes, ", "), func() error { return deploy.RemoveRoutes(ctx, h.Host, inv.Routes) }); err != nil {
				return err
			}
			removed["routes"] += len(inv.Routes)
		}
	}
	if a.dest.Runtime == "swarm" {
		if err := run(mgr, "Remove stack "+a.project(), func() error {
			return swarm.RemoveStack(ctx, hosts, a.cfg.App, a.destName, func(format string, args ...any) { fmt.Fprintf(u.Progress(), format+"\n", args...) })
		}); err != nil {
			return err
		}
	}
	for _, h := range targets {
		inv, o := invs[h.Name], a.removeOptions(h, svcs, f.volumes)
		if !o.Swarm && len(inv.Containers) > 0 {
			if err := run(h, fmt.Sprintf("Stop and remove %s", ui.Count(len(inv.Containers), "container", "")), func() error {
				return h.Host.Run(ctx, remote.Cmd{Script: deploy.RemoveContainersScript(o)})
			}); err != nil {
				return err
			}
		}
		removed["containers"] += len(inv.Containers)
		if len(inv.Networks) > 0 && (h.Name == mgr.Name || !o.Swarm) {
			if err := run(h, "Remove networks "+strings.Join(inv.Networks, ", "), func() error {
				return h.Host.Run(ctx, remote.Cmd{Script: deploy.RemoveNetworksScript(o)})
			}); err != nil {
				return err
			}
			removed["networks"] += len(inv.Networks)
		}
		if f.volumes && len(inv.Volumes) > 0 {
			if err := run(h, "Delete volumes "+strings.Join(inv.Volumes, ", "), func() error {
				return h.Host.Run(ctx, remote.Cmd{Script: deploy.RemoveVolumesScript(o)})
			}); err != nil {
				return err
			}
			removed["volumes"] += len(inv.Volumes)
		}
	}
	for _, h := range targets {
		o := a.removeOptions(h, svcs, f.volumes)
		if err := run(h, "Remove App directory "+release.AppDir(a.cfg.App, a.destName), func() error { return deploy.RemoveAppDir(ctx, h.Host, o) }); err != nil {
			return err
		}
	}
	// Images built for this App that no other Destination's Release keeps.
	keep, kerr := deploy.RetainedImageRefs(ctx, mgr.Host, a.cfg.App)
	for _, h := range targets {
		if kerr != nil {
			u.Warn("not pruning images: %v", kerr)
			break
		}
		deploy.PruneImages(ctx, h.Host, a.cfg.App, func(format string, args ...any) { fmt.Fprintf(u.Progress(), format+"\n", args...) }, deploy.PruneOptions{Keep: keep})
	}
	kept := 0
	if !f.volumes {
		kept = volumes
	}
	u.Event("removed", "", map[string]any{"app": a.cfg.App, "destination": a.destName, "containers": removed["containers"],
		"routes": removed["routes"], "networks": removed["networks"], "volumes_removed": removed["volumes"], "volumes_kept": kept})
	summary := fmt.Sprintf("Removed %s from %s", a.cfg.App, a.destName)
	if kept > 0 {
		summary += fmt.Sprintf("; kept %s and Backups", ui.Count(kept, "volume", ""))
	} else {
		summary += "; Backups kept"
	}
	u.Finished(nil, summary)
	return nil
}
