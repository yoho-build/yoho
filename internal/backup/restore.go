package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
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
	// AfterRestore runs after Service post_restore commands, while consumers
	// remain stopped. Failure keeps the App isolated. Intended for the CLI's
	// post-restore Hook; it must not start consumers or change exposure.
	AfterRestore func(context.Context) error
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

// Restore fetches and verifies a Backup, then stops this App's deployed
// Services before changing data. Only private dump/post_restore targets run
// temporarily; consumers resume their prior state after all restore work and
// Hooks succeed. Failure or cancellation keeps the App stopped (best effort if
// the Server is unreachable); volume changes are never automatically rolled back.
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
	name, token, err := newStagingName("restore-" + now().UTC().Format(tsLayout))
	if err != nil {
		return nil, err
	}
	dir := path.Join(base, name)
	keep := false
	var state *restoreState
	isolated := false
	defer func() {
		if !keep {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if cerr := removePrivateStaging(cctx, o.Host, dir, token); cerr != nil {
				err = errors.Join(err, fmt.Errorf("remove private restore staging %s: %w", dir, cerr))
				// This defer runs after the normal isolation guard. A cleanup
				// failure after resume is still a failed operation: fail closed,
				// but do not pretend a partly removed Backup is recoverable.
				if isolated {
					rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
					defer rcancel()
					if stopErr := state.stopBounded(rctx, o.Host); stopErr != nil {
						err = fmt.Errorf("%w; isolation cleanup failed: %v; verify and manually stop this App's Services immediately; staged files may be incomplete", err, stopErr)
					} else {
						err = fmt.Errorf("%w (Services left stopped; no data rollback; staged files may be incomplete)", err)
					}
				}
			}
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
	var dumps, hookRoots []string
	for _, svc := range services {
		ms, ok := m.Services[svc]
		if !ok {
			return nil, fmt.Errorf("Service %s is not in this Backup", svc)
		}
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
				res.DumpFiles = append(res.DumpFiles, path.Join(data, ms.Dump))
			}
		}
		if len(o.Services[svc].PostRestore) > 0 {
			hookRoots = append(hookRoots, svc)
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

	for _, name := range m.GeneratedSecrets {
		if !generatedNameRe.MatchString(name) {
			return nil, fmt.Errorf("manifest: invalid generated secret name %q", name)
		}
		if err := o.Host.Run(ctx, remote.Cmd{Script: safeFile(path.Join(data, generatedPrefix, name), true)}); err != nil {
			return nil, fmt.Errorf("unsafe Backup generated secret %s: %w", name, err)
		}
	}
	if len(m.GeneratedSecrets) > 0 {
		// Preflight existing destinations without creating or changing App files.
		d := generatedDir(o.App, o.Destination)
		pre := safeDir(d, false, false) + "if [ -d " + remote.Quote(d) + " ]; then yoho_private " + remote.Quote(d) + "; fi\n"
		for _, name := range m.GeneratedSecrets {
			f := path.Join(d, name)
			pre += "if [ -e " + remote.Quote(f) + " ] || [ -L " + remote.Quote(f) + " ]; then yoho_secret " + remote.Quote(f) + "; fi\n"
		}
		if err := o.Host.Run(ctx, remote.Cmd{Script: pre}); err != nil {
			return nil, fmt.Errorf("unsafe generated secret destinations: %w", err)
		}
	}

	if err := checkProjectOwnership(ctx, o.Host, project, o.App, o.Destination, rt); err != nil {
		return nil, err
	}

	// Check every archive before anything is stopped or wiped, so a
	// truncated or corrupt one fails the restore with the App still running.
	for _, v := range vols {
		if err := o.Host.Run(ctx, remote.Cmd{Script: volumeScript(v.docker, data, verifyVolumeSh, v.file)}); err != nil {
			return nil, fmt.Errorf("Backup archive for volume %s of %s is unreadable, nothing was changed: %w", v.key, v.svc, err)
		}
	}

	dumpTargets, requireHealth, err := restorePrerequisites(ctx, o.Host, o.App, o.Destination, rt, dumps)
	if err != nil {
		return nil, err
	}
	hookTargets, hookHealth, err := restorePrerequisites(ctx, o.Host, o.App, o.Destination, rt, hookRoots)
	if err != nil {
		return nil, err
	}
	for name := range hookHealth {
		if requireHealth == nil {
			requireHealth = map[string]bool{}
		}
		requireHealth[name] = true
	}
	// Starting a hook-only target runs its normal process too. Never let a
	// worker start while another Service's dump is still loading, even when
	// that worker is declared as a dump prerequisite.
	dumpSet, dumpRootSet := map[string]bool{}, map[string]bool{}
	for _, svc := range dumps {
		dumpRootSet[svc] = true
	}
	for _, svc := range dumpTargets {
		dumpSet[svc] = true
	}
	for _, svc := range hookRoots {
		if dumpSet[svc] && !dumpRootSet[svc] {
			return nil, fmt.Errorf("restore isolation: hook-only target %s is a dump prerequisite; its normal process cannot safely run while dumps load", svc)
		}
	}
	temporary := append([]string(nil), dumpTargets...)
	var afterDumps []string
	for _, svc := range hookTargets {
		if !dumpSet[svc] {
			temporary = append(temporary, svc)
			afterDumps = append(afterDumps, svc)
		}
	}
	state, err = snapshotRestoreState(ctx, o.Host, project, o.App, o.Destination, rt, services, temporary, requireHealth)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if err := checkRestoreVolumeUsers(ctx, o.Host, v.docker, project, o.App, o.Destination, rt); err != nil {
			return nil, err
		}
	}
	// Keep a names/status-only recovery record beside the staged Backup. It
	// survives errors and an interrupted process; no secret values are saved.
	stateFile := path.Join(dir, "prior-state.json")
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if err := o.Host.WriteFile(ctx, stateFile, stateJSON, 0600, false); err != nil {
		return nil, fmt.Errorf("write restore recovery state: %w", err)
	}
	// Install the fail-closed cleanup before the first stop: a stop/start can
	// partly succeed before SSH or the caller's context fails. Never recover
	// service availability automatically on a failed data operation.
	defer func() {
		if err == nil {
			return
		}
		keep = true
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		stopErr := state.stopBounded(rctx, o.Host)
		if stopErr != nil {
			err = fmt.Errorf("%w; isolation cleanup failed: %v; verify and manually stop this App's Services immediately (Backup kept at %s, prior state at %s)", err, stopErr, data, stateFile)
		} else {
			err = fmt.Errorf("%w (Services left stopped; no data rollback; Backup kept at %s, prior state at %s)", err, data, stateFile)
		}
	}()
	logf("isolating App Services during restore (availability interrupted)")
	isolated = true
	if err := state.stop(ctx, o.Host); err != nil {
		return nil, fmt.Errorf("stop Services: %w", err)
	}
	for _, v := range vols {
		logf("restoring volume %s of %s", v.key, v.svc)
		script := volumeScript(v.docker, data, restoreVolumeSh, v.file)
		if err := o.Host.Run(ctx, remote.Cmd{Script: script}); err != nil {
			return nil, fmt.Errorf("restore volume %s of %s: %w", v.key, v.svc, err)
		}
		res.Volumes = append(res.Volumes, v.svc+"/"+v.key)
	}
	// Generated secrets go back after volumes and before Services start,
	// so the data and the passwords it was created with land together.
	// The next deploy keeps these files (ensureFileOnce never overwrites).
	restored, changed, err := restoreGenerated(ctx, o.Host, o.App, o.Destination, data, m.GeneratedSecrets, logf)
	if err != nil {
		return nil, fmt.Errorf("restore generated secrets: %w", err)
	}
	res.Generated = restored
	res.GeneratedChanged = changed
	// Start prerequisites in dependency order and load each target's dump before
	// starting downstream processes that could otherwise consume partial data.
	for _, svc := range dumpTargets {
		logf("starting private dump prerequisite %s (consumers remain stopped)", svc)
		if err := state.startTemporary(ctx, o.Host, project, []string{svc}); err != nil {
			return nil, fmt.Errorf("start restore targets: %w", err)
		}
		if !dumpRootSet[svc] {
			continue
		}
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
	if len(afterDumps) > 0 {
		logf("dumps loaded; starting private post_restore targets %s", strings.Join(afterDumps, ", "))
		if err := state.startTemporary(ctx, o.Host, project, afterDumps); err != nil {
			return nil, fmt.Errorf("start post_restore targets: %w", err)
		}
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
		if err := waitReady(ctx, o.Host, cid, readyTimeout); err != nil {
			return nil, fmt.Errorf("Service %s: %w", svc, err)
		}
		logf("running post-restore for %s", svc)
		if err := o.Host.Run(ctx, remote.Cmd{Script: "set -eu; docker exec " + remote.Quote(cid) + " " + remote.QuoteArgs(post...)}); err != nil {
			return nil, fmt.Errorf("post-restore %s: %w", svc, err)
		}
	}
	if o.AfterRestore != nil {
		if err := o.AfterRestore(ctx); err != nil {
			return nil, fmt.Errorf("post-restore Hook: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logf("restore succeeded; resuming the App's prior running state")
	if err := state.resume(ctx, o.Host); err != nil {
		return nil, fmt.Errorf("resume Services: %w", err)
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
t=$(mktemp -d /data/.yoho-restore-XXXXXXXXXXXX)
n=${t##*/}
l=
trap 'if [ -n "$l" ]; then rm -f "$l"; fi; rm -rf "$t"' EXIT
l=$(mktemp /tmp/yoho-list-XXXXXXXXXXXX)
tar -tzf "/in/$1" >"$l"
if grep -qE "^(\./)?$n(/|$)" "$l"; then echo "archive contains the scratch name $n" >&2; exit 1; fi
tar -C "$t" -xzf "/in/$1"
own=$(stat -c %u:%g "$t"); mode=$(stat -c %a "$t")
find /data -mindepth 1 -maxdepth 1 ! -name "$n" -exec rm -rf {} +
# From here the scratch directory holds the only copy: keep it if a move fails.
trap 'rm -f "$l"' EXIT
for f in "$t"/* "$t"/.[!.]* "$t"/..?*; do
  if [ -e "$f" ] || [ -L "$f" ]; then mv "$f" /data/; fi
done
rmdir "$t"
chown "$own" /data; chmod "$mode" /data`
)

// volumeScript runs sh script (with file as $1) in the helper image with the
// volume at /data and the staged Backup at /in.
func volumeScript(volume, data, sh, file string) string {
	return safeDir(data, false, false) + "yoho_file " + remote.Quote(path.Join(data, file)) + "\ndocker run --rm --network none --read-only --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE --cap-add FSETID --cap-add MKNOD --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,nodev -v " + remote.Quote(volume+":/data") + " -v " + remote.Quote(data+":/in:ro") +
		" " + remote.Quote(HelperImage) + " sh -c " + remote.Quote(sh) + " sh " + remote.Quote(file)
}

// scaleAttempts is how many seconds restore waits for a Swarm Service to
// reach the requested number of running tasks.
const scaleAttempts = 180

func scaleService(ctx context.Context, h remote.Host, name string, replicas int) error {
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu; docker service scale " + remote.Quote(fmt.Sprintf("%s=%d", name, replicas)) + " --detach=true >/dev/null"}); err != nil {
		return fmt.Errorf("scale %s to %d: %w", name, replicas, err)
	}
	return waitServiceReplicas(ctx, h, name, replicas)
}

func waitServiceReplicas(ctx context.Context, h remote.Host, name string, replicas int) error {
	script := fmt.Sprintf(`set -eu
i=0
while :; do
  states=$(docker service ps --no-trunc --format '{{.CurrentState}}' %s)
  n=$(printf '%%s\n' "$states" | grep -c '^Running ' || true)
  active=$(printf '%%s\n' "$states" | grep -cvE '^$|^(Shutdown|Complete|Failed|Rejected) ' || true)
  if [ %d = 0 ]; then
    if [ "$active" = 0 ]; then exit 0; fi
  elif [ "$n" = %d ]; then exit 0; fi
  i=$((i+1))
  if [ "$i" -ge %d ]; then echo "service %s did not converge to %d running tasks (have $n)" >&2; exit 1; fi
  sleep 1
done`, remote.Quote(name), replicas, replicas, scaleAttempts, name, replicas)
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
