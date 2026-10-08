package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// ListOptions selects the Backups of one App/Destination in a Backup Target.
type ListOptions struct {
	App, Destination, Project string
	Target                    config.BackupTarget
	Password                  string
	TargetEnv                 map[string]string
	Host                      remote.Host
}

// List returns stored Backups, newest first.
func List(ctx context.Context, o ListOptions) ([]Entry, error) {
	if o.Host == nil {
		return nil, errors.New("backup: Host is required")
	}
	project := o.Project
	if project == "" {
		project = release.ProjectName(o.App, o.Destination)
	}
	t, err := newTarget(o.App, o.Destination, project, o.Target, o.Password, o.TargetEnv)
	if err != nil {
		return nil, err
	}
	return t.list(ctx, o.Host, release.BackupDir(o.App, o.Destination))
}

// RestoreOptions describes a restore. Restoring replaces volume contents
// and requires Confirm.
type RestoreOptions struct {
	App, Destination, Project string
	// Services to restore with their backup config (PostRestore,
	// RestoreDump). Services absent from the Backup are skipped. Empty:
	// every Service in the Backup, without PostRestore / RestoreDump.
	Services  map[string]config.ServiceBackup
	Target    config.BackupTarget
	Password  string
	TargetEnv map[string]string
	Host      remote.Host
	Out       io.Writer
	Lock      LockFunc
	// restic snapshot id or "latest"; archive file name.
	ID      string
	Confirm bool
	Now     func() time.Time
	// Runtime is "compose" (default) or "swarm".
	Runtime string
}

// RestoreResult reports what was restored.
type RestoreResult struct {
	Manifest Manifest
	// Volumes restored, as "<service>/<volume>".
	Volumes []string
	// Dumps loaded with the Service's RestoreDump command, as "<service>".
	Dumps []string
	// Services listed in RestoreOptions.Services but absent from the Backup.
	Skipped []string
	// Dumps without a RestoreDump command are not loaded; they are left at
	// these Server paths for the operator.
	DumpFiles []string
	// Directory kept on the Server when DumpFiles is non-empty.
	StagingDir string
	// Generated secrets written back to the Server (names only).
	Generated []string
	// GeneratedChanged are generated secrets whose Server value differed
	// and was replaced. Services keep reading secrets/<generation> until
	// the next deploy.
	GeneratedChanged []string
}

// Restore fetches a Backup, stops the Services, replaces their volume
// contents, starts them, loads dumps through RestoreDump and runs
// PostRestore. On failure after the stop,
// Services are left stopped so a half-restored volume is not served.
func Restore(ctx context.Context, o RestoreOptions) (res *RestoreResult, err error) {
	if !o.Confirm {
		return nil, errors.New("restore replaces volume data; confirmation required")
	}
	if o.Host == nil {
		return nil, errors.New("backup: Host is required")
	}
	if o.ID == "" {
		return nil, errors.New("restore: Backup id required")
	}
	rt, err := normalizeRuntime(o.Runtime)
	if err != nil {
		return nil, err
	}
	project := o.Project
	if project == "" {
		project = release.ProjectName(o.App, o.Destination)
	}
	logf := func(format string, a ...any) {
		if o.Out != nil {
			fmt.Fprintf(o.Out, format+"\n", a...)
		}
	}
	t, err := newTarget(o.App, o.Destination, project, o.Target, o.Password, o.TargetEnv)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	if o.Lock != nil {
		unlock, err := o.Lock(ctx)
		if err != nil {
			return nil, fmt.Errorf("lock: %w", err)
		}
		defer func() {
			uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if uerr := unlock(uctx); uerr != nil && err == nil {
				err = fmt.Errorf("unlock: %w", uerr)
			}
		}()
	}

	base := release.BackupDir(o.App, o.Destination)
	name := "restore-" + now().UTC().Format(tsLayout)
	dir := path.Join(base, name)
	if err := o.Host.Run(ctx, remote.Cmd{Script: "set -eu; umask 077; mkdir -p " + remote.Quote(base) +
		" && chmod 0700 " + remote.Quote(base) + " && mkdir -m 0700 " + remote.Quote(dir)}); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			cleanup(ctx, o.Host, base, name)
		}
	}()

	logf("fetching Backup %s", o.ID)
	data, err := t.fetch(ctx, o.Host, base, dir, o.ID)
	if err != nil {
		return nil, err
	}
	mb, err := o.Host.ReadFile(ctx, path.Join(data, "manifest.json"), false)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	res = &RestoreResult{}
	if err := json.Unmarshal(mb, &res.Manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	m := res.Manifest
	if m.App != o.App || m.Destination != o.Destination {
		return nil, fmt.Errorf("Backup belongs to %s/%s, not %s/%s", m.App, m.Destination, o.App, o.Destination)
	}

	var services []string
	if len(o.Services) == 0 {
		services = sortedKeys(m.Services)
	} else {
		for _, svc := range sortedKeys(o.Services) {
			if _, ok := m.Services[svc]; ok {
				services = append(services, svc)
			} else {
				res.Skipped = append(res.Skipped, svc)
				logf("Service %s is not in this Backup, skipping", svc)
			}
		}
		if len(services) == 0 {
			return nil, fmt.Errorf("none of the Services %s are in this Backup", strings.Join(sortedKeys(o.Services), ", "))
		}
	}
	type vol struct{ svc, key, file, docker string }
	var vols []vol
	var withVolumes, dumps []string
	for _, svc := range services {
		ms, ok := m.Services[svc]
		if !ok {
			return nil, fmt.Errorf("Service %s is not in this Backup", svc)
		}
		if ms.Dump != "" {
			if !nameRe.MatchString(ms.Dump) {
				return nil, fmt.Errorf("manifest: invalid file name %q", ms.Dump)
			}
			if len(o.Services[svc].RestoreDump) > 0 {
				dumps = append(dumps, svc)
			} else {
				res.DumpFiles = append(res.DumpFiles, path.Join(data, ms.Dump))
			}
		}
		if len(ms.Volumes) > 0 {
			withVolumes = append(withVolumes, svc)
		}
		for _, key := range sortedKeys(ms.Volumes) {
			file := ms.Volumes[key]
			if !nameRe.MatchString(file) {
				return nil, fmt.Errorf("manifest: invalid file name %q", file)
			}
			dn, err := volumeName(ctx, o.Host, project, key, rt)
			if err != nil {
				return nil, fmt.Errorf("%w (deploy the App before restoring)", err)
			}
			vols = append(vols, vol{svc, key, file, dn})
		}
	}

	// Check every archive before anything is stopped or wiped, so a
	// truncated or corrupt one fails the restore with the App still running.
	for _, v := range vols {
		if err := o.Host.Run(ctx, remote.Cmd{Script: volumeScript(v.docker, data, verifyVolumeSh, v.file)}); err != nil {
			return nil, fmt.Errorf("Backup archive for volume %s of %s is unreadable, nothing was changed: %w", v.key, v.svc, err)
		}
	}

	var replicas map[string]int
	if len(withVolumes) > 0 {
		logf("stopping %s", strings.Join(withVolumes, ", "))
		replicas, err = stopServices(ctx, o.Host, project, rt, withVolumes)
		if err != nil {
			return nil, fmt.Errorf("stop Services: %w", err)
		}
		for _, v := range vols {
			logf("restoring volume %s of %s", v.key, v.svc)
			script := volumeScript(v.docker, data, restoreVolumeSh, v.file)
			if err := o.Host.Run(ctx, remote.Cmd{Script: script}); err != nil {
				keep = true
				return nil, fmt.Errorf("restore volume %s of %s (Services left stopped, Backup kept at %s): %w", v.key, v.svc, data, err)
			}
			res.Volumes = append(res.Volumes, v.svc+"/"+v.key)
		}
	}
	// Generated secrets go back after volumes and before Services start,
	// so the data and the passwords it was created with land together.
	// The next deploy keeps these files (ensureFileOnce never overwrites).
	restored, changed, err := restoreGenerated(ctx, o.Host, o.App, o.Destination, data, m.GeneratedSecrets, logf)
	if err != nil {
		if len(withVolumes) > 0 {
			keep = true
			return nil, fmt.Errorf("restore generated secrets (Services left stopped, Backup kept at %s): %w", data, err)
		}
		return nil, fmt.Errorf("restore generated secrets: %w", err)
	}
	res.Generated = restored
	res.GeneratedChanged = changed
	if len(withVolumes) > 0 {
		logf("starting %s", strings.Join(withVolumes, ", "))
		if err := startServices(ctx, o.Host, project, rt, withVolumes, replicas); err != nil {
			return nil, fmt.Errorf("start Services: %w", err)
		}
	}
	// Dumps load after volumes, into the started container, so a dump can
	// overwrite (or complement) restored volume data.
	for _, svc := range dumps {
		cid, err := containerID(ctx, o.Host, project, svc, rt)
		if err != nil {
			return nil, err
		}
		if err := waitReady(ctx, o.Host, cid, readyTimeout); err != nil {
			return nil, fmt.Errorf("Service %s: %w", svc, err)
		}
		logf("loading dump into %s", svc)
		file := path.Join(data, m.Services[svc].Dump)
		script := "set -eu; docker exec -i " + remote.Quote(cid) + " " + remote.QuoteArgs(o.Services[svc].RestoreDump...) + " < " + remote.Quote(file)
		if err := o.Host.Run(ctx, remote.Cmd{Script: script}); err != nil {
			keep = true
			return nil, fmt.Errorf("restore dump of %s (Backup kept at %s): %w", svc, data, err)
		}
		res.Dumps = append(res.Dumps, svc)
	}
	for _, svc := range services {
		post := o.Services[svc].PostRestore
		if len(post) == 0 {
			continue
		}
		cid, err := containerID(ctx, o.Host, project, svc, rt)
		if err != nil {
			return nil, err
		}
		logf("running post-restore for %s", svc)
		if err := o.Host.Run(ctx, remote.Cmd{Script: "set -eu; docker exec " + remote.Quote(cid) + " " + remote.QuoteArgs(post...)}); err != nil {
			return nil, fmt.Errorf("post-restore %s: %w", svc, err)
		}
	}
	if len(res.DumpFiles) > 0 {
		keep = true
		res.StagingDir = dir
		logf("dumps without restore_dump are not loaded; restore them from: %s", strings.Join(res.DumpFiles, " "))
	}
	return res, nil
}

const (
	verifyVolumeSh = `set -eu; tar -tzf "/in/$1" >/dev/null`
	// restoreVolumeSh extracts into a scratch directory inside the volume
	// first and only then replaces the live data, so a failing tar (corrupt
	// archive, disk full) leaves the volume as it was. The archive's "./"
	// entry sets the scratch directory's owner and mode, which are copied to
	// the volume root.
	restoreVolumeSh = `set -eu
t=/data/.yoho-restore-tmp
trap 'rm -rf "$t"' EXIT
rm -rf "$t"; mkdir "$t"
tar -tzf "/in/$1" >/dev/null
tar -C "$t" -xzf "/in/$1"
own=$(stat -c %u:%g "$t"); mode=$(stat -c %a "$t")
find /data -mindepth 1 -maxdepth 1 ! -name .yoho-restore-tmp -exec rm -rf {} +
find "$t" -mindepth 1 -maxdepth 1 -exec mv {} /data/ \;
chown "$own" /data; chmod "$mode" /data`
)

// volumeScript runs sh script (with file as $1) in the helper image with the
// volume at /data and the staged Backup at /in.
func volumeScript(volume, data, sh, file string) string {
	return "set -eu; docker run --rm --network none -v " + remote.Quote(volume+":/data") + " -v " + remote.Quote(data+":/in:ro") +
		" " + remote.Quote(HelperImage) + " sh -c " + remote.Quote(sh) + " sh " + remote.Quote(file)
}

// scaleAttempts is how many seconds restore waits for a Swarm Service to
// reach the requested number of running tasks.
const scaleAttempts = 180

// stopServices stops compose Services, or scales Swarm Services to 0 and
// waits until no task is running. The returned map is the previous replica
// count (swarm only).
func stopServices(ctx context.Context, h remote.Host, project, runtime string, svcs []string) (map[string]int, error) {
	if runtime != "swarm" {
		script := "set -eu; docker compose -p " + remote.Quote(project) + " stop " + remote.QuoteArgs(svcs...)
		if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	prev := map[string]int{}
	var stopped []string
	for _, svc := range svcs {
		name := project + "_" + svc
		out, err := h.Output(ctx, remote.Cmd{Script: "docker service inspect -f '{{.Spec.Mode.Replicated.Replicas}}' " + remote.Quote(name)})
		if err != nil {
			return prev, fmt.Errorf("inspect replicas of %s: %w", name, err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			return prev, fmt.Errorf("replicas of %s: %q", name, strings.TrimSpace(out))
		}
		prev[svc] = n
		if err := scaleService(ctx, h, name, 0); err != nil {
			// Nothing was restored yet: put back what was already stopped.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			back := append(stopped, svc)
			if rerr := startServices(rctx, h, project, runtime, back, prev); rerr != nil {
				err = fmt.Errorf("%w (also failed to scale back %s: %v)", err, strings.Join(back, ", "), rerr)
			}
			return prev, err
		}
		stopped = append(stopped, svc)
	}
	return prev, nil
}

// startServices starts compose Services, or scales Swarm Services back and
// waits until that many tasks are running.
func startServices(ctx context.Context, h remote.Host, project, runtime string, svcs []string, replicas map[string]int) error {
	if runtime != "swarm" {
		return h.Run(ctx, remote.Cmd{Script: "set -eu; docker compose -p " + remote.Quote(project) + " start " + remote.QuoteArgs(svcs...)})
	}
	for _, svc := range svcs {
		n := replicas[svc]
		if n < 1 {
			n = 1
		}
		if err := scaleService(ctx, h, project+"_"+svc, n); err != nil {
			return err
		}
	}
	return nil
}

func scaleService(ctx context.Context, h remote.Host, name string, replicas int) error {
	script := fmt.Sprintf(`set -eu
docker service scale %s >/dev/null
i=0
while :; do
  n=$(docker ps -q --filter %s --filter status=running | wc -l | tr -d ' ')
  if [ "$n" = %d ]; then exit 0; fi
  i=$((i+1))
  if [ "$i" -ge %d ]; then echo "service %s did not converge to %d running tasks (have $n)" >&2; exit 1; fi
  sleep 1
done`, remote.Quote(fmt.Sprintf("%s=%d", name, replicas)), remote.Quote("label=com.docker.swarm.service.name="+name), replicas, scaleAttempts, name, replicas)
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("scale %s to %d: %w", name, replicas, err)
	}
	return nil
}

// readyTimeout bounds waiting for a started container before loading a dump.
var readyTimeout = 2 * time.Minute

// waitReady waits until the container runs and, when it has a healthcheck,
// reports healthy. Without a healthcheck a database may still be starting;
// the dump command then fails and the Backup is kept for a retry.
func waitReady(ctx context.Context, h remote.Host, cid string, timeout time.Duration) error {
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	script := fmt.Sprintf(`set -eu
i=0
while :; do
  s=$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' %s)
  case "$s" in
    "running healthy"|"running none") exit 0 ;;
  esac
  i=$((i+1))
  if [ "$i" -ge %d ]; then echo "container not ready after %ds: $s" >&2; exit 1; fi
  sleep 1
done`, remote.Quote(cid), secs, secs)
	return h.Run(ctx, remote.Cmd{Script: script})
}
