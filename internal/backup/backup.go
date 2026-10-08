// Package backup takes, lists and restores Backups of an App's Services on a
// Server, following the ONCE contract: dump, then pre-backup command or
// `docker pause` while volumes are copied, then post-restore command.
//
// Everything runs through a remote.Host, so the same code works over SSH
// from the operator's machine and with remote.Local inside a Scheduled Job.
// Only Docker is required on the Server: tar, restic, rclone and 7-Zip run
// in helper containers when not installed.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Helper images. Pinned so Backups are reproducible; override in tests or
// when mirroring images.
var (
	HelperImage   = "alpine:3.22"
	ResticImage   = "restic/restic:0.18.0"
	RcloneImage   = "rclone/rclone:1.71"
	SevenZipImage = "crazymax/7zip:latest" // TODO: pin a digest once verified.
)

// LockFunc takes the Destination lock shared with deploys. The returned
// unlock is always called, with a context that is not cancelled.
type LockFunc func(ctx context.Context) (unlock func(ctx context.Context) error, err error)

// RunOptions describes one Backup.
type RunOptions struct {
	App         string
	Destination string
	// Compose project; default release.ProjectName(App, Destination).
	Project  string
	Services map[string]config.ServiceBackup
	Target   config.BackupTarget
	// Resolved Target.PasswordSecret value; empty when none.
	Password string
	// Resolved Target.EnvSecrets values (name -> value).
	TargetEnv   map[string]string
	Host        remote.Host
	Out         io.Writer
	Lock        LockFunc
	YohoVersion string
	// Now is overridable for tests.
	Now func() time.Time
	// Runtime is "compose" (default) or "swarm". On swarm, the Service's
	// container is the running task on this host and volumes are stack
	// volumes named <project>_<volume>. Stateful Services are pinned to the
	// first Server, which is the host the CLI connects for Backups.
	Runtime string
}

// Result describes a stored Backup.
type Result struct {
	// restic snapshot id or archive file name.
	ID       string
	Size     int64
	Duration time.Duration
	// restic, or archive:<format>.
	Method string
}

// Manifest is written as manifest.json into every Backup.
type Manifest struct {
	App         string                     `json:"app"`
	Destination string                     `json:"destination"`
	Project     string                     `json:"project"`
	Version     string                     `json:"version,omitempty"`
	CreatedAt   time.Time                  `json:"created_at"`
	YohoVersion string                     `json:"yoho_version,omitempty"`
	Services    map[string]ManifestService `json:"services"`
}

// ManifestService records what was captured for one Service.
type ManifestService struct {
	// Compose volume key -> archive file name inside the Backup.
	Volumes map[string]string `json:"volumes,omitempty"`
	// Dump file name inside the Backup.
	Dump string `json:"dump,omitempty"`
	// How the Service was quiesced: pre_backup, pause or none.
	Quiesce string `json:"quiesce"`
}

const (
	cleanupTimeout = 2 * time.Minute
	unpauseTimeout = time.Minute
	tsLayout       = "20060102T150405Z"
)

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	nameRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

// Run takes a Backup and stores it in the Backup Target. The staging
// directory is always removed and paused containers are always unpaused,
// also on error or cancellation.
func Run(ctx context.Context, o RunOptions) (res *Result, err error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	t, err := newTarget(o.App, o.Destination, o.project(), o.Target, o.Password, o.TargetEnv)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	start := now()
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

	ts := start.UTC().Format(tsLayout)
	base := release.BackupDir(o.App, o.Destination)
	staging := path.Join(base, ts)
	if err := run(ctx, o.Host, remote.Cmd{Script: "set -eu; umask 077; mkdir -p " + remote.Quote(base) +
		" && chmod 0700 " + remote.Quote(base) + " && mkdir -m 0700 " + remote.Quote(staging)}); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	defer cleanup(ctx, o.Host, base, ts)

	m := Manifest{
		App: o.App, Destination: o.Destination, Project: o.project(),
		Version: currentVersion(ctx, o.Host, o.App, o.Destination), CreatedAt: start.UTC(),
		YohoVersion: o.YohoVersion, Services: map[string]ManifestService{},
	}
	for _, svc := range sortedKeys(o.Services) {
		o.logf("backing up %s", svc)
		ms, err := backupService(ctx, o, staging, svc, o.Services[svc])
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", svc, err)
		}
		m.Services[svc] = ms
	}
	mj, _ := json.MarshalIndent(m, "", "  ")
	if err := o.Host.WriteFile(ctx, path.Join(staging, "manifest.json"), mj, 0o600, false); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}

	o.logf("storing in %s target", t.method())
	res, err = t.store(ctx, o.Host, base, ts)
	if err != nil {
		return nil, err
	}
	res.Duration = now().Sub(start)
	o.logf("Backup %s stored (%d bytes, %s)", res.ID, res.Size, res.Duration.Round(time.Second))
	return res, nil
}

func (o *RunOptions) project() string {
	if o.Project != "" {
		return o.Project
	}
	return release.ProjectName(o.App, o.Destination)
}

func (o *RunOptions) logf(format string, a ...any) {
	if o.Out != nil {
		fmt.Fprintf(o.Out, format+"\n", a...)
	}
}

func (o *RunOptions) validate() error {
	if o.Host == nil {
		return errors.New("backup: Host is required")
	}
	if !nameRe.MatchString(o.App) || !nameRe.MatchString(o.Destination) {
		return errors.New("backup: invalid App or Destination name")
	}
	if len(o.Services) == 0 {
		return errors.New("backup: no Services with x-yoho.backup selected")
	}
	if _, err := normalizeRuntime(o.Runtime); err != nil {
		return err
	}
	for svc, sb := range o.Services {
		if !nameRe.MatchString(svc) {
			return fmt.Errorf("backup: invalid Service name %q", svc)
		}
		if len(sb.Volumes) == 0 && len(sb.Dump) == 0 {
			return fmt.Errorf("backup: Service %s has neither volumes nor dump", svc)
		}
		for _, v := range sb.Volumes {
			if !nameRe.MatchString(v) {
				return fmt.Errorf("backup: invalid volume name %q", v)
			}
		}
	}
	return nil
}

// backupService dumps, quiesces and copies the volumes of one Service.
func backupService(ctx context.Context, o RunOptions, staging, svc string, sb config.ServiceBackup) (ManifestService, error) {
	ms := ManifestService{Quiesce: "none"}
	rt, err := normalizeRuntime(o.Runtime)
	if err != nil {
		return ms, err
	}
	cid, err := containerID(ctx, o.Host, o.project(), svc, rt)
	if err != nil {
		return ms, err
	}
	if len(sb.Dump) > 0 {
		ms.Dump = svc + ".dump"
		script := "set -eu; umask 077; docker exec " + remote.Quote(cid) + " " + remote.QuoteArgs(sb.Dump...) +
			" > " + remote.Quote(path.Join(staging, ms.Dump))
		if err := run(ctx, o.Host, remote.Cmd{Script: script}); err != nil {
			return ms, fmt.Errorf("dump: %w", err)
		}
	}
	if len(sb.Volumes) == 0 {
		return ms, nil
	}
	vols := map[string]string{} // compose key -> Docker volume name
	for _, v := range sb.Volumes {
		name, err := volumeName(ctx, o.Host, o.project(), v, rt)
		if err != nil {
			return ms, err
		}
		vols[v] = name
	}
	ms.Volumes = map[string]string{}
	err = withQuiesced(ctx, o, cid, sb.PreBackup, &ms, func() error {
		for _, v := range sb.Volumes {
			file := svc + "-" + v + ".tar.gz"
			script := "set -eu; docker run --rm --network none -v " + remote.Quote(vols[v]+":/data:ro") +
				" -v " + remote.Quote(staging+":/out") + " " + remote.Quote(HelperImage) +
				" sh -c " + remote.Quote(`umask 077; tar -C /data -czf "/out/$1" .`) + " sh " + remote.Quote(file)
			if err := run(ctx, o.Host, remote.Cmd{Script: script}); err != nil {
				return fmt.Errorf("copy volume %s: %w", v, err)
			}
			ms.Volumes[v] = file
		}
		return nil
	})
	return ms, err
}

// withQuiesced runs fn after the pre-backup command, or with the container
// paused. Unpause always runs, with its own deadline, even if ctx is done.
func withQuiesced(ctx context.Context, o RunOptions, cid string, preBackup []string, ms *ManifestService, fn func() error) (err error) {
	if len(preBackup) > 0 {
		ms.Quiesce = "pre_backup"
		if err := run(ctx, o.Host, remote.Cmd{Script: "set -eu; docker exec " + remote.Quote(cid) + " " + remote.QuoteArgs(preBackup...)}); err != nil {
			return fmt.Errorf("pre-backup: %w", err)
		}
		return fn()
	}
	ms.Quiesce = "pause"
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unpauseTimeout)
		defer cancel()
		// Unpause unconditionally: a cancelled pause may still have applied.
		script := "docker unpause " + remote.Quote(cid) + " >/dev/null 2>&1 || true; " +
			"test \"$(docker inspect -f '{{.State.Paused}}' " + remote.Quote(cid) + ")\" = false"
		if uerr := run(uctx, o.Host, remote.Cmd{Script: script}); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unpause %s FAILED, container may still be paused: %w", cid, uerr))
		}
	}()
	if err := run(ctx, o.Host, remote.Cmd{Script: "set -eu; docker pause " + remote.Quote(cid) + " >/dev/null"}); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	return fn()
}

func normalizeRuntime(runtime string) (string, error) {
	switch runtime {
	case "", "compose":
		return "compose", nil
	case "swarm":
		return "swarm", nil
	default:
		return "", fmt.Errorf("backup: unknown runtime %q", runtime)
	}
}

func containerID(ctx context.Context, h remote.Host, project, svc, runtime string) (string, error) {
	var script string
	if runtime == "swarm" {
		// First running task of the Service on this node. Stateful Services
		// are pinned to the host the CLI is connected to.
		script = "docker ps -q --filter " + remote.Quote("label=com.docker.swarm.service.name="+project+"_"+svc) + " --filter status=running"
	} else {
		script = "docker compose -p " + remote.Quote(project) + " ps -q " + remote.Quote(svc)
	}
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return "", fmt.Errorf("find container: %w", err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if id == "" {
		return "", fmt.Errorf("no running container for Service %s in project %s", svc, project)
	}
	return strings.TrimSpace(id), nil
}

// volumeName resolves a compose volume key to the Docker volume name.
// Compose uses compose labels (custom `name:` is honored). Swarm stack
// volumes are named <project>_<key> and labelled with the stack namespace.
func volumeName(ctx context.Context, h remote.Host, project, vol, runtime string) (string, error) {
	if runtime == "swarm" {
		out, err := h.Output(ctx, remote.Cmd{Script: "docker volume ls -q --filter " + remote.Quote("label=com.docker.stack.namespace="+project)})
		if err != nil {
			return "", fmt.Errorf("resolve volume %s: %w", vol, err)
		}
		want := project + "_" + vol
		var match []string
		for _, n := range strings.Fields(out) {
			if n == want {
				match = append(match, n)
			}
		}
		if len(match) != 1 {
			return "", fmt.Errorf("volume %s of project %s: found %d Docker volumes named %s, want 1", vol, project, len(match), want)
		}
		return match[0], nil
	}
	out, err := h.Output(ctx, remote.Cmd{Script: "docker volume ls -q --filter " +
		remote.Quote("label=com.docker.compose.project="+project) + " --filter " +
		remote.Quote("label=com.docker.compose.volume="+vol)})
	if err != nil {
		return "", fmt.Errorf("resolve volume %s: %w", vol, err)
	}
	names := strings.Fields(out)
	if len(names) != 1 {
		return "", fmt.Errorf("volume %s of project %s: found %d Docker volumes, want 1", vol, project, len(names))
	}
	return names[0], nil
}

// currentVersion reads the current Release version; empty when unknown.
func currentVersion(ctx context.Context, h remote.Host, app, dest string) string {
	b, err := h.ReadFile(ctx, path.Join(release.AppDir(app, dest), "current", "release.json"), false)
	if err != nil {
		return ""
	}
	var r release.Release
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.Version
}

// cleanup removes a staging dir under base. Files written by helper
// containers are root-owned, so fall back to removing them in a container.
func cleanup(ctx context.Context, h remote.Host, base, name string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	dir := path.Join(base, name)
	script := "rm -rf " + remote.Quote(dir) + " 2>/dev/null || docker run --rm --network none -v " +
		remote.Quote(base+":/b") + " " + remote.Quote(HelperImage) + " rm -rf " + remote.Quote("/b/"+name)
	_ = run(cctx, h, remote.Cmd{Script: script})
}

func run(ctx context.Context, h remote.Host, c remote.Cmd) error {
	return h.Run(ctx, c)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func parseInt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
