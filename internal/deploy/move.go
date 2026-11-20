package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// MovePlan is a stop-first relocation. Volume names are Docker names, not keys.
type MovePlan struct {
	Service   string   `json:"service"`
	Source    string   `json:"source"`
	Target    string   `json:"target"`
	Volumes   []string `json:"volumes"`
	Consumers []string `json:"consumers"`
}

// PlanMove rejects resources a safe volume move cannot own or copy.
func PlanMove(d *plan.Deploy, service, target string) (MovePlan, error) {
	m := MovePlan{Service: service, Target: target}
	if d.Roles.Roles == nil {
		return m, fmt.Errorf("service move requires Roles")
	}
	svc, ok := d.Project.Services[service]
	if !ok {
		return m, fmt.Errorf("unknown Service %s", service)
	}
	if !d.Ext[service].Stateful {
		return m, fmt.Errorf("Service %s must be marked x-yoho.stateful", service)
	}
	hs := d.Roles.ServiceServers(service)
	if len(hs) != 1 {
		return m, fmt.Errorf("Stateful Service must have exactly one owner")
	}
	m.Source = hs[0]
	if !slices.Contains(d.Roles.Servers, target) || target == m.Source {
		return m, fmt.Errorf("--to must name another Server in this Destination")
	}
	if svc.HealthCheck == nil || svc.HealthCheck.Disable {
		return m, fmt.Errorf("Service move requires a Docker healthcheck; configure healthcheck before moving")
	}
	ext := d.Ext[service]
	if ext.Backup == nil || len(ext.Backup.Volumes) == 0 && len(ext.Backup.Dump) == 0 {
		return m, fmt.Errorf("Service move requires a configured Backup covering named volumes or a consistent dump")
	}

	for _, v := range svc.Volumes {
		if v.Type != types.VolumeTypeVolume {
			return m, fmt.Errorf("Service move supports owned named volumes only")
		}
		cfg, ok := d.Project.Volumes[v.Source]
		if !ok || bool(cfg.External) || cfg.Driver != "" && cfg.Driver != "local" || len(cfg.DriverOpts) > 0 {
			return m, fmt.Errorf("volume %s must be a Yoho-owned plain local volume", v.Source)
		}
		if len(ext.Backup.Dump) == 0 && !slices.Contains(ext.Backup.Volumes, v.Source) {
			return m, fmt.Errorf("Backup must include volume %s", v.Source)
		}
		for other, s := range d.Project.Services {
			if other == service {
				continue
			}
			for _, mounted := range s.Volumes {
				if mounted.Type == types.VolumeTypeVolume && mounted.Source == v.Source {
					return m, fmt.Errorf("volume %s is shared with Service %s", v.Source, other)
				}
			}
		}
		name := cfg.Name
		if name == "" {
			name = release.ProjectName(d.App, d.Destination) + "_" + v.Source
		}
		m.Volumes = append(m.Volumes, name)
	}
	if len(m.Volumes) == 0 {
		return m, fmt.Errorf("Service has no named volumes to move")
	}
	for _, c := range d.Roles.Connections {
		if c.Service == service {
			m.Consumers = append(m.Consumers, c.Consumers...)
		}
	}
	slices.Sort(m.Volumes)
	m.Volumes = slices.Compact(m.Volumes)
	slices.Sort(m.Consumers)
	m.Consumers = slices.Compact(m.Consumers)
	return m, nil
}

// MovedDestination returns the config edit without mutating the user's config.
func MovedDestination(dest config.Destination, service, target string) config.Destination {
	dest.Roles = maps.Clone(dest.Roles)
	for name, role := range dest.Roles {
		if !slices.Contains(role.Services, service) {
			continue
		}
		role.Services = slices.Clone(role.Services)
		if len(role.Services) == 1 {
			role.Servers = []string{target}
			dest.Roles[name] = role
			return dest
		}
		role.Services = slices.DeleteFunc(role.Services, func(s string) bool { return s == service })
		dest.Roles[name] = role
		moveName := service + "-moved"
		for {
			if _, ok := dest.Roles[moveName]; !ok {
				break
			}
			moveName += "-moved"
		}
		dest.Roles[moveName] = config.Role{Services: []string{service}, Servers: []string{target}}
		return dest
	}
	return dest
}

type MoveJournal struct {
	MovePlan
	State       string            `json:"state"`
	Steps       map[string]string `json:"steps"`
	OperationID string            `json:"operation_id,omitempty"`
}

func moveJournalPath(d *plan.Deploy, service string) string {
	return path.Join(release.AppDir(d.App, d.Destination), "moves", service+".json")
}

// MoveService is called with all Destination locks held. Backup must make a
// fresh durable Backup before source shutdown. Data streams only over stdin.
func MoveService(ctx context.Context, d *plan.Deploy, m MovePlan, backup func(context.Context) error, progress func(string, string)) (err error) {
	hs := map[string]plan.NamedHost{}
	for _, h := range d.Servers {
		hs[h.Name] = h
	}
	source, target := hs[m.Source].Host, hs[m.Target].Host
	journal := MoveJournal{MovePlan: m, State: "running", Steps: map[string]string{}, OperationID: d.OperationID}
	save := func(ctx context.Context) error { return writeJSON(ctx, source, moveJournalPath(d, m.Service), journal) }
	step := func(name string, run func() error) error {
		journal.Steps[name] = "running"
		if err := save(ctx); err != nil {
			return err
		}
		progress(name, "running")
		e := run()
		if e != nil {
			journal.Steps[name] = "failed"
			journal.State = "failed"
		} else {
			journal.Steps[name] = "complete"
		}
		progress(name, journal.Steps[name])
		return errors.Join(e, save(context.WithoutCancel(ctx)))
	}
	project := release.ProjectName(d.App, d.Destination)
	selector := "docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+project) + " --filter " + remote.Quote("label=com.docker.compose.service="+m.Service)
	// Store a source recovery file outside mutable Release directories.
	cur, err := CurrentRelease(ctx, source, d.App, d.Destination)
	if err != nil || cur == nil {
		return errors.Join(err, fmt.Errorf("source requires a current Release"))
	}
	original, err := source.ReadFile(ctx, path.Join(release.Dir(d.App, d.Destination, cur.Version), "compose.yaml"), false)
	if err != nil {
		return err
	}
	recovery := path.Join(release.AppDir(d.App, d.Destination), "moves", m.Service+"-source.yaml")
	if err = source.WriteFile(ctx, recovery, original, 0600, false); err != nil {
		return err
	}
	if err = step("preflight", func() error {
		for _, v := range m.Volumes {
			if e := checkMoveVolume(ctx, source, v, project, false); e != nil {
				return e
			}
			if e := checkMoveVolume(ctx, target, v, project, true); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err = step("backup", func() error { return backup(ctx) }); err != nil {
		return err
	}
	stopped := false
	defer func() {
		if err == nil || !stopped {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		// Stop the target before allowing source writes again.
		stopErr := target.Run(cctx, remote.Cmd{Script: "set -eu\nids=$(" + selector + ")\n[ -z \"$ids\" ] || { docker update --restart=no $ids; docker stop $ids; }\n", Stdout: d.Out, Stderr: d.Out})
		if stopErr != nil {
			journal.State = "unknown"
			_ = save(cctx)
			err = errors.Join(err, fmt.Errorf("target could not be stopped; source restart refused: %w", stopErr))
			return
		}
		restart := source.Run(cctx, remote.Cmd{Script: "set -eu\ndocker compose -p " + remote.Quote(project) + " -f " + remote.Quote(recovery) + " up -d --force-recreate --no-deps --wait " + remote.Quote(m.Service), Stdout: d.Out, Stderr: d.Out})
		journal.Steps["restart-source"] = "complete"
		if restart != nil {
			journal.Steps["restart-source"] = "failed"
		}
		progress("restart-source", journal.Steps["restart-source"])
		if restart == nil {
			old := *d
			old.Hook = nil
			old.ServiceMove = true
			_, restart = (Roles{LockHeld: true}).Deploy(cctx, &old)
		}
		journal.State = "failed"
		err = errors.Join(err, restart, save(cctx))
	}()
	// Mark uncertainty before stop so disconnects also enter recovery.
	stopped = true
	if err = step("stop-source", func() error {
		return source.Run(ctx, remote.Cmd{Script: "set -eu\nids=$(" + selector + ")\n[ -n \"$ids\" ]\ndocker update --restart=no $ids\ndocker stop $ids\n", Stdout: d.Out, Stderr: d.Out})
	}); err != nil {
		return err
	}
	if err = step("restore-empty-target", func() error {
		for _, v := range m.Volumes {
			if e := createMoveVolume(ctx, target, v, project); e != nil {
				return e
			}
			if e := checkMoveVolume(ctx, target, v, project, true); e != nil {
				return e
			}
			reader, writer := io.Pipe()
			done := make(chan error, 1)
			go func() {
				e := source.Run(ctx, remote.Cmd{Script: "set -eu\ndocker run --rm --network none -v " + remote.Quote(v+":/data:ro") + " alpine:3.22 tar -C /data -cpf - .", Stdout: writer, Stderr: d.Out})
				writer.CloseWithError(e)
				done <- e
			}()
			e := target.Run(ctx, remote.Cmd{Script: "set -eu\ndocker run --rm -i --network none -v " + remote.Quote(v+":/data") + " alpine:3.22 tar -C /data -xpf -", Stdin: reader, Stdout: d.Out, Stderr: d.Out})
			reader.CloseWithError(e)
			e = errors.Join(e, <-done)
			if e != nil {
				return e
			}
		}
		// Preserve generated credentials before selecting the target as owner.
		kinds, e := generatedDecls(d)
		if e != nil {
			return e
		}
		for key := range kinds {
			needed := false
			ext := d.Ext[m.Service]
			if _, ok := ext.Generate[key]; ok {
				needed = true
			}
			for _, ref := range ext.Secrets {
				needed = needed || ref.Key == key
			}
			if !needed {
				continue
			}
			owner := generatedOwner(d, key)
			var ownerHost remote.Host
			for _, h := range d.Servers {
				if h.Name == owner {
					ownerHost = h.Host
				}
			}
			if ownerHost == nil {
				continue
			}
			b, e := ownerHost.ReadFile(ctx, path.Join(release.AppDir(d.App, d.Destination), "generated", key), false)
			if e != nil {
				return e
			}
			if e = target.WriteFile(ctx, path.Join(release.AppDir(d.App, d.Destination), "generated", key), b, 0600, false); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err = step("start-check-repoint", func() error {
		next := *d
		next.ServiceMove = true
		next.Roles = MovedDestination(d.Roles, m.Service, m.Target)
		_, e := (Roles{LockHeld: true}).Deploy(ctx, &next)
		return e
	}); err != nil {
		return err
	}
	journal.State = "complete"
	return save(ctx)
}

func checkMoveVolume(ctx context.Context, h remote.Host, volume, project string, empty bool) error {
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif docker volume inspect " + remote.Quote(volume) + " >/dev/null 2>&1; then docker volume inspect --format '{{json .}}' " + remote.Quote(volume) + "; fi"})
	if err != nil {
		return err
	}
	if out == "" {
		if empty {
			return nil
		}
		return fmt.Errorf("source volume %s is missing", volume)
	}
	var v struct {
		Driver  string
		Options map[string]string
		Labels  map[string]string
	}
	if err = json.Unmarshal([]byte(out), &v); err != nil {
		return err
	}
	if v.Driver != "local" || len(v.Options) > 0 || v.Labels[composeProjectLabel] != project {
		return fmt.Errorf("volume %s is not an owned plain local volume", volume)
	}
	if empty {
		users, e := h.Output(ctx, remote.Cmd{Script: "set -eu\ndocker ps -aq --filter " + remote.Quote("volume="+volume)})
		if e != nil {
			return e
		}
		if strings.TrimSpace(users) != "" {
			return fmt.Errorf("target volume %s is mounted by existing containers", volume)
		}
		return h.Run(ctx, remote.Cmd{Script: "set -eu\ndocker run --rm --network none -v " + remote.Quote(volume+":/data:ro") + " alpine:3.22 sh -ec 'test -z \"$(ls -A /data)\"'"})
	}
	return nil
}
func createMoveVolume(ctx context.Context, h remote.Host, volume, project string) error {
	return h.Run(ctx, remote.Cmd{Script: "set -eu\ndocker volume create --label " + remote.Quote(composeProjectLabel+"="+project) + " " + remote.Quote(volume) + " >/dev/null"})
}

// CleanupMovedSource requires a successful journal and an unmounted owned
// source volume. It never removes a target or another App's volume.
func CleanupMovedSource(ctx context.Context, d *plan.Deploy, service, target string) error {
	var journal MoveJournal
	var source remote.Host
	for _, h := range d.Servers {
		b, e := h.Host.ReadFile(ctx, moveJournalPath(d, service), false)
		if e != nil {
			continue
		}
		if e = json.Unmarshal(b, &journal); e != nil {
			return e
		}
		if journal.Service == service && journal.Target == target && journal.Source == h.Name && journal.State == "complete" {
			source = h.Host
			break
		}
	}
	if source == nil {
		return fmt.Errorf("no successful Service move journal; cleanup refused")
	}
	cur, e := CurrentRelease(ctx, source, d.App, d.Destination)
	if e != nil {
		return e
	}
	if cur != nil {
		if _, ok := cur.Images[service]; ok {
			return fmt.Errorf("source Release still contains Service %s; cleanup refused", service)
		}
	}
	var remaining []string
	for _, v := range journal.Volumes {
		exists, e := source.Output(ctx, remote.Cmd{Script: "set -eu\nif docker volume inspect " + remote.Quote(v) + " >/dev/null 2>&1; then printf exists; fi"})
		if e != nil {
			return e
		}
		if exists == "" {
			continue
		}
		if e = checkMoveVolume(ctx, source, v, release.ProjectName(d.App, d.Destination), false); e != nil {
			return e
		}
		if e = source.Run(ctx, remote.Cmd{Script: "set -eu\n[ -z \"$(docker ps -aq --filter " + remote.Quote("volume="+v) + ")\" ]"}); e != nil {
			return e
		}
		remaining = append(remaining, v)
	}
	for _, v := range remaining {
		if e := source.Run(ctx, remote.Cmd{Script: "set -eu\ndocker volume rm " + remote.Quote(v)}); e != nil {
			return e
		}
	}

	return nil
}
