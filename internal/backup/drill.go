package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Labels on every drill container, volume and network. Drill objects never
// carry yoho.app / yoho.destination, so nothing that manages the App (deploy,
// restore isolation, Backups, `app ps`) mistakes them for the App's own.
const (
	LabelDrill            = "yoho.drill" // value: the drill project name
	LabelDrillApp         = "yoho.drill.app"
	LabelDrillDestination = "yoho.drill.destination"
)

// DrillProject is the throwaway compose project of one drill:
// yoho-<app>-<destination>-drill-<suffix>. Its volumes are
// <project>_<key>, never the App's <app project>_<key>.
func DrillProject(app, dest, suffix string) string {
	return release.ProjectName(app, dest) + "-drill-" + suffix
}

var drillSuffixRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// ValidDrillProject reports whether project is a drill project of app/dest.
// It is never the App's own project.
func ValidDrillProject(app, dest, project string) bool {
	prefix := release.ProjectName(app, dest) + "-drill-"
	if !nameRe.MatchString(app) || !nameRe.MatchString(dest) || !strings.HasPrefix(project, prefix) {
		return false
	}
	return drillSuffixRe.MatchString(strings.TrimPrefix(project, prefix)) && project != release.ProjectName(app, dest)
}

func newDrillProject(app, dest string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("drill project nonce: %w", err)
	}
	return DrillProject(app, dest, hex.EncodeToString(b[:])), nil
}

func drillLabels(app, dest, project string) map[string]any {
	return map[string]any{LabelDrill: project, LabelDrillApp: app, LabelDrillDestination: dest}
}

// Service keys copied from the deployed Release into the drill. Everything
// else is dropped: published ports, networks and network_mode (the drill
// gets one internal network), Proxy labels, container_name, depends_on,
// devices, privileges, deploy settings and restart policies.
var drillServiceKeys = []string{
	"command", "entrypoint", "env_file", "environment", "healthcheck", "image", "init",
	"platform", "read_only", "shm_size", "stop_grace_period", "stop_signal", "tmpfs",
	"ulimits", "user", "working_dir",
}

// DrillCompose turns the deployed Release's compiled compose file into the
// drill project's file: only services, isolated as described at
// drillServiceKeys. Named volumes become fresh drill volumes (no name,
// external, driver or driver_opts, so they can never alias the App's or a
// host path); bind mounts must be read-only; secrets and configs keep only
// their file (or inline content) source.
func DrillCompose(compiled []byte, app, dest, project string, services []string) ([]byte, error) {
	if !ValidDrillProject(app, dest, project) {
		return nil, fmt.Errorf("drill: invalid project %q", project)
	}
	var in map[string]any
	if err := yaml.Unmarshal(compiled, &in); err != nil {
		return nil, fmt.Errorf("drill: parse deployed compose file: %w", err)
	}
	inSvcs, _ := in["services"].(map[string]any)
	inVols, _ := in["volumes"].(map[string]any)
	inSecrets, _ := in["secrets"].(map[string]any)
	inConfigs, _ := in["configs"].(map[string]any)
	labels := drillLabels(app, dest, project)

	outSvcs := map[string]any{}
	outVols := map[string]any{}
	outSecrets := map[string]any{}
	outConfigs := map[string]any{}
	for _, name := range services {
		raw, ok := inSvcs[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("drill: Service %s is not in the deployed Release; deploy first", name)
		}
		if img, _ := raw["image"].(string); img == "" {
			return nil, fmt.Errorf("drill: Service %s has no image in the deployed Release", name)
		}
		s := map[string]any{}
		for _, k := range drillServiceKeys {
			if v, ok := raw[k]; ok {
				s[k] = v
			}
		}
		s["labels"] = labels
		s["restart"] = "no"
		s["networks"] = map[string]any{"default": nil}

		if vs, ok := raw["volumes"]; ok {
			list, ok := vs.([]any)
			if !ok {
				return nil, fmt.Errorf("drill: Service %s: unexpected volumes", name)
			}
			var mounts []any
			for _, item := range list {
				m, err := drillMount(name, item, inVols, outVols, labels)
				if err != nil {
					return nil, err
				}
				mounts = append(mounts, m)
			}
			if len(mounts) > 0 {
				s["volumes"] = mounts
			}
		}
		for _, kind := range []string{"secrets", "configs"} {
			refs, ok := raw[kind]
			if !ok {
				continue
			}
			list, ok := refs.([]any)
			if !ok {
				return nil, fmt.Errorf("drill: Service %s: unexpected %s", name, kind)
			}
			top, out := inSecrets, outSecrets
			if kind == "configs" {
				top, out = inConfigs, outConfigs
			}
			var kept []any
			for _, r := range list {
				var src string
				switch v := r.(type) {
				case string:
					src = v
					kept = append(kept, v)
				case map[string]any:
					src, _ = v["source"].(string)
					ref := map[string]any{"source": src}
					for _, k := range []string{"target", "uid", "gid", "mode"} {
						if x, ok := v[k]; ok {
							ref[k] = x
						}
					}
					kept = append(kept, ref)
				default:
					return nil, fmt.Errorf("drill: Service %s: unexpected %s entry", name, kind)
				}
				def, ok := top[src].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("drill: Service %s: %s %q is not defined in the deployed Release", name, kind, src)
				}
				clean := map[string]any{}
				if f, ok := def["file"].(string); ok && f != "" {
					clean["file"] = f
				} else if c, ok := def["content"].(string); ok && kind == "configs" {
					clean["content"] = c
				} else {
					return nil, fmt.Errorf("drill: Service %s: %s %q must be file based to run in a drill", name, kind, src)
				}
				out[src] = clean
			}
			if len(kept) > 0 {
				s[kind] = kept
			}
		}
		outSvcs[name] = s
	}
	doc := map[string]any{
		"name":     project,
		"services": outSvcs,
		// internal: no route in or out of the drill except docker exec.
		"networks": map[string]any{"default": map[string]any{"internal": true, "labels": labels}},
	}
	if len(outVols) > 0 {
		doc["volumes"] = outVols
	}
	if len(outSecrets) > 0 {
		doc["secrets"] = outSecrets
	}
	if len(outConfigs) > 0 {
		doc["configs"] = outConfigs
	}
	return yaml.Marshal(doc)
}

func drillMount(svc string, item any, inVols, outVols map[string]any, labels map[string]any) (any, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("drill: Service %s: unexpected short volume syntax in the deployed Release", svc)
	}
	typ, _ := m["type"].(string)
	target, _ := m["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("drill: Service %s: volume without target", svc)
	}
	out := map[string]any{"type": typ, "target": target}
	if ro, ok := m["read_only"].(bool); ok && ro {
		out["read_only"] = true
	}
	switch typ {
	case "volume":
		src, _ := m["source"].(string)
		if src == "" {
			// Anonymous: Docker creates it with the container; it is
			// removed with the container (docker rm -v).
			return out, nil
		}
		if _, ok := inVols[src]; !ok {
			return nil, fmt.Errorf("drill: Service %s: volume %s is not a named volume of the deployed Release", svc, src)
		}
		if !nameRe.MatchString(src) {
			return nil, fmt.Errorf("drill: Service %s: invalid volume name %q", svc, src)
		}
		out["source"] = src
		// Only labels: no name/external/driver/driver_opts, so the drill
		// volume is always a fresh local <project>_<key>.
		outVols[src] = map[string]any{"labels": labels}
	case "bind":
		if out["read_only"] != true {
			src, _ := m["source"].(string)
			return nil, fmt.Errorf("drill: Service %s mounts %s read-write from the Server; a drill would share it with the running App", svc, src)
		}
		src, _ := m["source"].(string)
		if !strings.HasPrefix(src, "/") {
			return nil, fmt.Errorf("drill: Service %s: bind source %q must be absolute", svc, src)
		}
		out["source"] = src
		out["bind"] = map[string]any{"create_host_path": false}
	case "tmpfs":
		if t, ok := m["tmpfs"]; ok {
			out["tmpfs"] = t
		}
	default:
		return nil, fmt.Errorf("drill: Service %s: %s mounts are not supported in a drill", svc, typ)
	}
	return out, nil
}

// DrillCleanupScript removes the containers (with their anonymous volumes),
// volumes and networks of one drill project. It refuses, before removing
// anything, when an object of the project lacks the drill labels, carries the
// App's ownership labels, is named outside <project>_, or when a volume is
// used by a container outside the drill. The App's project is never a valid
// drill project.
func DrillCleanupScript(app, dest, project string) (string, error) {
	if !ValidDrillProject(app, dest, project) {
		return "", fmt.Errorf("drill: refusing to clean up %q: not a drill project of %s/%s", project, app, dest)
	}
	q := remote.Quote
	return `set -eu
p=` + q(project) + `
app=` + q(app) + `
dest=` + q(dest) + `
real=` + q(release.ProjectName(app, dest)) + `
case "$p" in "$real-drill-"*) ;; *) echo "not a drill project: $p" >&2; exit 1;; esac
want="$p|$app|$dest||"
bad() { echo "drill cleanup: $1 is not owned by drill $p; nothing removed" >&2; exit 1; }
cs=$(docker ps -aq --no-trunc --filter "label=com.docker.compose.project=$p")
for c in $cs; do
  l=$(docker inspect -f '{{index .Config.Labels "yoho.drill"}}|{{index .Config.Labels "yoho.drill.app"}}|{{index .Config.Labels "yoho.drill.destination"}}|{{index .Config.Labels "yoho.app"}}|{{index .Config.Labels "yoho.destination"}}' "$c")
  [ "$l" = "$want" ] || bad "container $c"
done
vs=$(docker volume ls -q --filter "label=com.docker.compose.project=$p")
for v in $vs; do
  case "$v" in "$real"_*) bad "volume $v";; "$p"_*) ;; *) bad "volume $v";; esac
  l=$(docker volume inspect -f '{{index .Labels "yoho.drill"}}|{{index .Labels "yoho.drill.app"}}|{{index .Labels "yoho.drill.destination"}}|{{index .Labels "yoho.app"}}|{{index .Labels "yoho.destination"}}' "$v")
  [ "$l" = "$want" ] || bad "volume $v"
  for u in $(docker ps -aq --no-trunc --filter "volume=$v"); do
    case " $(echo $cs) " in *" $u "*) ;; *) bad "volume $v (used by container $u)";; esac
  done
done
ns=$(docker network ls -q --no-trunc --filter "label=com.docker.compose.project=$p")
for n in $ns; do
  nn=$(docker network inspect -f '{{.Name}}' "$n")
  case "$nn" in "$p"_*) ;; *) bad "network $nn";; esac
  l=$(docker network inspect -f '{{index .Labels "yoho.drill"}}|{{index .Labels "yoho.drill.app"}}|{{index .Labels "yoho.drill.destination"}}|{{index .Labels "yoho.app"}}|{{index .Labels "yoho.destination"}}' "$n")
  [ "$l" = "$want" ] || bad "network $nn"
done
if [ -n "$cs" ]; then docker rm -f -v $cs >/dev/null; fi
for v in $vs; do docker volume rm "$v" >/dev/null; done
for n in $ns; do docker network rm "$n" >/dev/null; done
`, nil
}

// RemoveDrill removes one drill project's containers, volumes and network.
func RemoveDrill(ctx context.Context, h remote.Host, app, dest, project string) error {
	script, err := DrillCleanupScript(app, dest, project)
	if err != nil {
		return err
	}
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("remove drill %s: %w", project, err)
	}
	return nil
}

// ListDrills returns the drill projects of app/dest that still have
// containers, volumes or networks on the Server.
func ListDrills(ctx context.Context, h remote.Host, app, dest string) ([]string, error) {
	f := " --filter " + remote.Quote("label="+LabelDrillApp+"="+app) + " --filter " + remote.Quote("label="+LabelDrillDestination+"="+dest)
	script := "set -eu\ndocker ps -a" + f + ` --format '{{.Label "yoho.drill"}}'` + "\n" +
		"for v in $(docker volume ls -q" + f + `); do docker volume inspect -f '{{index .Labels "yoho.drill"}}' "$v"; done` + "\n" +
		"for n in $(docker network ls -q" + f + `); do docker network inspect -f '{{index .Labels "yoho.drill"}}' "$n"; done` + "\n"
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil, fmt.Errorf("list drills: %w", err)
	}
	seen := map[string]bool{}
	var projects []string
	for _, l := range strings.Fields(out) {
		if ValidDrillProject(app, dest, l) && !seen[l] {
			seen[l] = true
			projects = append(projects, l)
		}
	}
	sort.Strings(projects)
	return projects, nil
}

// DrillOptions describes a restore drill.
type DrillOptions struct {
	App, Destination string
	// Services to restore with their backup config; see RestoreOptions.
	Services  map[string]config.ServiceBackup
	Target    config.BackupTarget
	Password  string
	TargetEnv map[string]string
	// Resolved Target.RcloneConfigSecret; see RunOptions.RcloneConfig.
	RcloneConfig string
	Host         remote.Host
	Out          io.Writer
	Lock         LockFunc
	// restic snapshot id or "latest"; archive file name.
	ID string
	// Keep leaves the drill project running for inspection.
	Keep bool
	// Runtime is "compose" (default). Swarm Destinations are refused.
	Runtime string
	// Phase is called as each phase ends, for UI events.
	Phase func(DrillPhase)
	Now   func() time.Time
	// ReadyTimeout bounds each container's wait for health (default 5m).
	ReadyTimeout time.Duration
}

// DrillPhase is one timed drill step: download, restore, healthy, verify,
// cleanup.
type DrillPhase struct {
	Name     string
	Duration time.Duration
	Detail   string
}

// DrillResult reports a drill.
type DrillResult struct {
	Project  string
	Manifest Manifest
	Services []string
	Volumes  []string
	Dumps    []string
	Verified []string
	// Dumps without restore_dump: not loaded in the drill.
	Unloaded []string
	Phases   []DrillPhase
	// RTO is download + restore + healthy: the time a real restore of this
	// Backup onto this Server would take before the data is usable.
	RTO  time.Duration
	Kept bool
}

// drillReadyTimeout is the default health wait for drill containers.
var drillReadyTimeout = 5 * time.Minute

// Drill restores a Backup into a throwaway compose project on the
// Destination's Server, starts the Stateful Services there, waits for their
// healthchecks, runs x-yoho.backup.verify and removes the drill again (unless
// Keep). The running App is never stopped or changed: the drill has its own
// fresh volumes, one internal network, no published ports and no Proxy route.
func Drill(ctx context.Context, o DrillOptions) (res *DrillResult, err error) {
	values := secrets.RcloneConfigValues(o.RcloneConfig)
	defer func() { err = secrets.RedactError(err, values...) }()
	if o.Out != nil {
		r := secrets.NewRedactor(o.Out, values...)
		defer r.Flush()
		o.Out = r
	}
	if o.Host == nil {
		return nil, errors.New("backup: Host is required")
	}
	if o.ID == "" {
		return nil, errors.New("drill: Backup id required")
	}
	rt, err := normalizeRuntime(o.Runtime)
	if err != nil {
		return nil, err
	}
	if rt == "swarm" {
		return nil, errors.New("drill: Swarm Destinations are not supported yet; run the drill against a compose Destination (e.g. restore --from into a staging Server)")
	}
	logf := func(format string, a ...any) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, format+"\n", a...)
		}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	ready := o.ReadyTimeout
	if ready <= 0 {
		ready = drillReadyTimeout
	}
	appProject := release.ProjectName(o.App, o.Destination)
	t, err := newTarget(o.App, o.Destination, appProject, o.Target, o.Password, o.TargetEnv)
	if err != nil {
		return nil, err
	}
	project, err := newDrillProject(o.App, o.Destination)
	if err != nil {
		return nil, err
	}
	res = &DrillResult{Project: project}
	mark := now()
	phase := func(name, detail string) {
		p := DrillPhase{Name: name, Duration: now().Sub(mark), Detail: detail}
		mark = now()
		res.Phases = append(res.Phases, p)
		if name == "download" || name == "restore" || name == "healthy" {
			res.RTO += p.Duration
		}
		if o.Phase != nil {
			o.Phase(p)
		}
	}

	if o.Lock != nil {
		unlock, lerr := o.Lock(ctx)
		if lerr != nil {
			return nil, fmt.Errorf("lock: %w", lerr)
		}
		defer func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if uerr := unlock(uctx); uerr != nil && err == nil {
				err = fmt.Errorf("unlock: %w", uerr)
			}
		}()
	}
	if err := t.prepareRclone(ctx, o.Host, o.RcloneConfig, now()); err != nil {
		return nil, err
	}
	base := release.BackupDir(o.App, o.Destination)
	name, token, err := newStagingName("drill-" + now().UTC().Format(tsLayout))
	if err != nil {
		return nil, err
	}
	dir := path.Join(base, name)
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := removePrivateStaging(cctx, o.Host, dir, token); cerr != nil {
			err = errors.Join(err, fmt.Errorf("remove private drill staging %s: %w", dir, cerr))
		}
	}()
	if err := createPrivateStaging(ctx, o.Host, base, dir, token); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}

	logf("fetching Backup %s", o.ID)
	data, err := t.fetch(ctx, o.Host, base, dir, o.ID)
	if err != nil {
		return nil, err
	}
	mb, err := o.Host.ReadFile(ctx, path.Join(data, "manifest.json"), false)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(mb, &res.Manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	m := res.Manifest
	if err := checkManifestOwner(m, o.App, o.Destination, o.Destination); err != nil {
		return nil, err
	}
	var services []string
	for _, svc := range sortedKeys(m.Services) {
		if len(o.Services) == 0 {
			services = append(services, svc)
		} else if _, ok := o.Services[svc]; ok {
			services = append(services, svc)
		}
	}
	if len(services) == 0 {
		return nil, errors.New("drill: none of the selected Services are in this Backup")
	}
	res.Services = services
	type vol struct{ svc, key, file string }
	var vols []vol
	var dumps []string
	for _, svc := range services {
		ms := m.Services[svc]
		if ms.Dump != "" {
			if !nameRe.MatchString(ms.Dump) {
				return nil, fmt.Errorf("manifest: invalid file name %q", ms.Dump)
			}
			if err := o.Host.Run(ctx, remote.Cmd{Script: safeFile(path.Join(data, ms.Dump), false)}); err != nil {
				return nil, fmt.Errorf("unsafe Backup dump for Service %s: %w", svc, err)
			}
			if len(o.Services[svc].RestoreDump) > 0 {
				dumps = append(dumps, svc)
			} else {
				res.Unloaded = append(res.Unloaded, svc)
			}
		}
		for _, key := range sortedKeys(ms.Volumes) {
			file := ms.Volumes[key]
			if !nameRe.MatchString(file) || !nameRe.MatchString(key) {
				return nil, fmt.Errorf("manifest: invalid volume %q / file name %q", key, file)
			}
			vols = append(vols, vol{svc, key, file})
		}
	}
	phase("download", o.ID)

	// The deployed Release, not today's local compose file, defines what runs.
	appDir := release.AppDir(o.App, o.Destination)
	if err := o.Host.Run(ctx, remote.Cmd{Script: appLink(o.App, o.Destination) + safeCurrentFile(appDir, "compose.yaml")}); err != nil {
		return nil, fmt.Errorf("drill: unsafe or missing deployed compose file (deploy first): %w", err)
	}
	compiled, err := o.Host.ReadFile(ctx, path.Join(appDir, "current", "compose.yaml"), false)
	if err != nil {
		return nil, fmt.Errorf("drill: read deployed compose file: %w", err)
	}
	drillYAML, err := DrillCompose(compiled, o.App, o.Destination, project, services)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if !composeDeclaresVolume(drillYAML, v.key) {
			return nil, fmt.Errorf("drill: volume %s of Service %s is in the Backup but not mounted by the deployed Release", v.key, v.svc)
		}
	}
	if err := drillFresh(ctx, o.Host, project); err != nil {
		return nil, err
	}
	composeFile := path.Join(dir, "compose.yaml")
	if err := o.Host.WriteFile(ctx, composeFile, drillYAML, 0o600, false); err != nil {
		return nil, fmt.Errorf("write drill compose file: %w", err)
	}
	compose := "set -eu; docker compose -p " + remote.Quote(project) + " --project-directory " + remote.Quote(dir) + " -f " + remote.Quote(composeFile)
	created := false
	defer func() {
		if !created || (o.Keep && err == nil) {
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := RemoveDrill(cctx, o.Host, o.App, o.Destination, project); cerr != nil {
			err = errors.Join(err, fmt.Errorf("%w; remove it with `yoho backup drill --cleanup`", cerr))
		}
	}()
	logf("creating drill project %s (internal network, no published ports)", project)
	created = true
	if err := o.Host.Run(ctx, remote.Cmd{Script: compose + " up --no-start >&2"}); err != nil {
		return nil, fmt.Errorf("create drill project: %w", err)
	}
	for _, v := range vols {
		dn, err := drillVolume(ctx, o.Host, o.App, o.Destination, project, v.key)
		if err != nil {
			return nil, err
		}
		if err := o.Host.Run(ctx, remote.Cmd{Script: volumeScript(dn, data, verifyVolumeSh, v.file)}); err != nil {
			return nil, fmt.Errorf("Backup archive for volume %s of %s is unreadable: %w", v.key, v.svc, err)
		}
		logf("restoring volume %s of %s into %s", v.key, v.svc, dn)
		if err := o.Host.Run(ctx, remote.Cmd{Script: volumeScript(dn, data, restoreVolumeSh, v.file)}); err != nil {
			return nil, fmt.Errorf("restore volume %s of %s: %w", v.key, v.svc, err)
		}
		res.Volumes = append(res.Volumes, v.svc+"/"+v.key)
	}
	for _, svc := range dumps {
		if err := o.Host.Run(ctx, remote.Cmd{Script: compose + " up -d --no-deps " + remote.Quote(svc) + " >&2"}); err != nil {
			return nil, fmt.Errorf("start %s: %w", svc, err)
		}
		cid, err := containerID(ctx, o.Host, project, svc, "compose")
		if err != nil {
			return nil, err
		}
		if err := waitReady(ctx, o.Host, cid, ready); err != nil {
			return nil, fmt.Errorf("Service %s: %w", svc, err)
		}
		logf("loading dump into %s", svc)
		file := path.Join(data, m.Services[svc].Dump)
		script := "set -eu; docker exec -i " + remote.Quote(cid) + " " + remote.QuoteArgs(o.Services[svc].RestoreDump...) + " < " + remote.Quote(file) + " >&2"
		if err := o.Host.Run(ctx, remote.Cmd{Script: script}); err != nil {
			return nil, fmt.Errorf("restore dump of %s: %w", svc, err)
		}
		res.Dumps = append(res.Dumps, svc)
	}
	phase("restore", strings.Join(append(append([]string(nil), res.Volumes...), res.Dumps...), ", "))

	if err := o.Host.Run(ctx, remote.Cmd{Script: compose + " up -d --no-deps >&2"}); err != nil {
		return nil, fmt.Errorf("start drill: %w", err)
	}
	cids := map[string]string{}
	for _, svc := range services {
		cid, err := containerID(ctx, o.Host, project, svc, "compose")
		if err != nil {
			return nil, err
		}
		if err := waitReady(ctx, o.Host, cid, ready); err != nil {
			return nil, fmt.Errorf("Service %s did not become healthy in the drill: %w", svc, err)
		}
		cids[svc] = cid
	}
	phase("healthy", strings.Join(services, ", "))

	for _, svc := range services {
		cmd := o.Services[svc].Verify
		if len(cmd) == 0 {
			continue
		}
		logf("verifying %s: %s", svc, strings.Join(cmd, " "))
		out, err := o.Host.Output(ctx, remote.Cmd{Script: "set -eu; docker exec " + remote.Quote(cids[svc]) + " " + remote.QuoteArgs(cmd...) + " </dev/null 2>&1"})
		if o.Out != nil && out != "" {
			if len(out) > 4096 {
				out = out[:4096] + "\n[...]"
			}
			fmt.Fprintln(o.Out, out)
		}
		if err != nil {
			return nil, fmt.Errorf("verify %s failed: %w", svc, err)
		}
		res.Verified = append(res.Verified, svc)
	}
	if len(res.Verified) > 0 {
		phase("verify", strings.Join(res.Verified, ", "))
	}
	if o.Keep {
		res.Kept = true
		return res, nil
	}
	logf("removing drill project %s", project)
	created = false
	if err := RemoveDrill(ctx, o.Host, o.App, o.Destination, project); err != nil {
		return nil, fmt.Errorf("%w; remove it with `yoho backup drill --cleanup`", err)
	}
	phase("cleanup", project)
	return res, nil
}

func composeDeclaresVolume(doc []byte, key string) bool {
	var d struct {
		Volumes map[string]any `yaml:"volumes"`
	}
	if yaml.Unmarshal(doc, &d) != nil {
		return false
	}
	_, ok := d.Volumes[key]
	return ok
}

// drillFresh refuses a project name that already has any Docker object.
func drillFresh(ctx context.Context, h remote.Host, project string) error {
	f := remote.Quote("label=com.docker.compose.project=" + project)
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker ps -aq --filter " + f + "; docker volume ls -q --filter " + f + "; docker network ls -q --filter " + f})
	if err != nil {
		return fmt.Errorf("drill: inspect project %s: %w", project, err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("drill: project %s already has Docker objects; refusing to reuse it", project)
	}
	return nil
}

// drillVolume resolves a drill volume and proves it is drill-owned: named
// <project>_<key>, with the drill labels and without the App's labels.
func drillVolume(ctx context.Context, h remote.Host, app, dest, project, key string) (string, error) {
	if !ValidDrillProject(app, dest, project) {
		return "", fmt.Errorf("drill: invalid project %q", project)
	}
	name, err := volumeName(ctx, h, project, key, "compose")
	if err != nil {
		return "", fmt.Errorf("drill: %w", err)
	}
	if name != project+"_"+key || strings.HasPrefix(name, release.ProjectName(app, dest)+"_") {
		return "", fmt.Errorf("drill: volume %s is not the drill volume %s_%s", name, project, key)
	}
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker volume inspect -f " +
		remote.Quote(`{{index .Labels "yoho.drill"}}|{{index .Labels "yoho.app"}}`) + " " + remote.Quote(name)})
	if err != nil {
		return "", fmt.Errorf("drill: inspect volume %s: %w", name, err)
	}
	if strings.TrimSpace(out) != project+"|" {
		return "", fmt.Errorf("drill: volume %s is not drill-owned; refusing to write to it", name)
	}
	return name, nil
}
