package cli

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/roles"
)

func (a *app) rotateRoles(ctx context.Context, services, generated []string, dry bool) (err error) {
	if !dry {
		if err = a.hookFunc("current", "secrets rotate", nil)(ctx, "pre-connect", nil); err != nil {
			return err
		}
	}
	s, err := a.openSession(ctx, "rotate", true, false)
	if err != nil {
		return err
	}
	defer s.close()
	if len(services) > 0 {
		if err = checkServices(s.r.Project.ServiceNames(), services); err != nil {
			return &validationError{err}
		}
	}
	if !dry {
		unlock, e := a.operationLocks(ctx, s.hosts, deploy.LockInfo{Command: "secrets rotate"}, true)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()
	}
	images := map[string]string{}
	built := []string{}
	current := map[string]*release.Release{}
	version := ""
	for _, h := range s.hosts {
		rel, e := deploy.CurrentRelease(ctx, h.Host, a.cfg.App, a.destName)
		if e != nil {
			return e
		}
		if rel == nil {
			return fmt.Errorf("deploy every Server before rotating secrets")
		}
		if version != "" && version != rel.Version {
			return fmt.Errorf("Servers have mixed Releases; converge with yoho apply before rotating")
		}
		version = rel.Version
		current[h.Name] = rel
		for name, img := range rel.Images {
			images[name] = img
		}
		built = append(built, rel.Built...)
	}
	if err = a.checkRotateVersion(version); err != nil {
		return err
	}
	s.ver = version

	d, err := a.newDeploy(s, images, true)
	if err != nil {
		return err
	}
	slices.Sort(built)
	d.Built = slices.Compact(built)
	var rotations []*deploy.GeneratedRotation
	for _, key := range generated {
		g, e := deploy.PlanGeneratedRotation(d, key)
		if e != nil {
			return e
		}
		rotations = append(rotations, g)
	}
	if err = a.checkRotateConfig(ctx, d); err != nil {
		return err
	}
	if err = a.refusePendingRotations(ctx, s.hosts, generated); err != nil {
		return err
	}
	if !dry {
		if err = beginOperation(ctx); err != nil {
			return err
		}
	}
	for _, g := range rotations {
		hs := a.dest.ServiceServers(g.Service)
		if len(hs) == 0 {
			return fmt.Errorf("no generator placement for %s", g.Key)
		}
		var h plan.NamedHost
		for _, candidate := range s.hosts {
			if candidate.Name == hs[0] {
				h = candidate
			}
		}
		a.ui.Event("generated_rotation_plan", "", map[string]any{"key": g.Key, "service": g.Service, "server": h.Name, "consumers": g.Consumers})
		if dry {
			continue
		}
		swapped, e := deploy.RotationSwapped(ctx, h.Host, a.cfg.App, a.destName, current[h.Name], g.Key)
		if e != nil {
			return e
		}
		if !swapped {
			if e = deploy.RotateGeneratedSecret(ctx, h.Host, a.cfg.App, a.destName, performer(), g, a.ui.Info, a.ui.Warn); e != nil {
				return e
			}
		}
		owner := deploy.GeneratedOwner(d, g.Key)
		if owner != h.Name {
			b, e := h.Host.ReadFile(ctx, path.Join(release.AppDir(d.App, d.Destination), "generated", g.Key), false)
			if e != nil {
				return e
			}
			for _, target := range s.hosts {
				if target.Name == owner {
					if e = target.Host.WriteFile(ctx, path.Join(release.AppDir(d.App, d.Destination), "generated", g.Key), b, 0600, false); e != nil {
						return e
					}
				}
			}
		}
	}
	changes, err := (deploy.Roles{}).Diff(ctx, d)
	if err != nil {
		return err
	}
	// Rotation is only safe against the deployed Compose configuration. The
	// normal diff also reports unrelated changes; refuse those before rollout.
	var restarts []string
	for _, c := range changes {
		if c.Kind == "service" && c.Action != plan.ActionNoop {
			if len(services) > 0 && !slices.Contains(services, c.Name) {
				return fmt.Errorf("Service %s also changed; rotate all consumers", c.Name)
			}
			restarts = append(restarts, c.Name)
		}
	}
	if dry {
		a.ui.Plan(changes)
		a.ui.Finished(nil, "Dry run; nothing changed")
		return nil
	}

	// Keep each Server's image IDs and current secret generation as the restart
	// base, rather than comparing all Servers against the first one's Release.
	for _, h := range s.hosts {
		p, e := roles.Render(d, a.dest, h.Name, d.PrivateAddresses)
		if e != nil {
			return e
		}
		if e = deploy.CheckRestartBase(ctx, h.Host, p); e != nil {
			return e
		}
	}
	d.Restart = restarts
	d.Hook = a.hookFunc(version, "secrets rotate", s.hosts)
	d.Out = s.store.Redactor(a.ui.Progress())
	err = deployRolesRotation(ctx, d, restarts)
	if f, ok := d.Out.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
	if err != nil {
		return err
	}
	for _, h := range s.hosts {
		if e := deploy.ClearRotationSwapped(ctx, h.Host, d.App, d.Destination, generated); e != nil {
			return e
		}
	}
	a.ui.Finished(nil, "Rotated secrets across consumer Servers")
	return nil
}

// An empty Restart means a full rollout in the Runtime contract. Rotation
// must never pass an empty selection through to that API.
func deployRolesRotation(ctx context.Context, d *plan.Deploy, services []string) error {
	if len(services) == 0 {
		return nil
	}
	d.Restart = services
	_, e := (deploy.Roles{LockHeld: true}).Deploy(ctx, d)
	return e
}
