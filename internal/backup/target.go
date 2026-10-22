package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// Env names carrying secrets into scripts. Values travel in remote.Cmd.Env
// and are forwarded to containers with `-e NAME` (no value on argv).
const (
	envResticPassword = "RESTIC_PASSWORD"
	envResticRepo     = "RESTIC_REPOSITORY"
	envArchivePass    = "YOHO_ARCHIVE_PASSWORD"
)

// target stores, lists and fetches Backups for one Backup Target.
type target struct {
	app, dest, project string
	cfg                config.BackupTarget
	typ, format        string
	env                map[string]string
}

func newTarget(app, dest, project string, cfg config.BackupTarget, password string, targetEnv map[string]string) (*target, error) {
	if !nameRe.MatchString(app) || !nameRe.MatchString(dest) || !nameRe.MatchString(project) {
		return nil, errors.New("backup target: invalid App, Destination or project name")
	}
	if (cfg.Type == "" || cfg.Type == "restic") && strings.HasPrefix(cfg.Repository, "local:") {
		cfg.Repository = strings.TrimPrefix(cfg.Repository, "local:")
	}
	if strings.HasPrefix(cfg.Repository, "/") {
		if strings.Contains(cfg.Repository, ":") {
			return nil, errors.New("backup target: local path cannot contain a colon")
		}
		for _, part := range strings.Split(cfg.Repository, "/") {
			if part == ".." {
				return nil, errors.New("backup target: local path cannot contain parent traversal")
			}
		}
		cfg.Repository = path.Clean(cfg.Repository)
	}
	t := &target{app: app, dest: dest, project: project, cfg: cfg, typ: cfg.Type, format: cfg.Format, env: map[string]string{}}
	if t.typ == "" {
		t.typ = "restic"
	}
	if !strings.HasPrefix(cfg.Repository, "/") && !strings.Contains(cfg.Repository, ":") && cfg.Repository != "" {
		return nil, errors.New("backup target: local repository must be an absolute path")
	}
	if cfg.Repository == "" {
		return nil, errors.New("backup target: repository is required")
	}
	if strings.ContainsAny(cfg.Repository, "\n\r") {
		return nil, errors.New("backup target: invalid repository")
	}
	for k, v := range targetEnv {
		if !envNameRe.MatchString(k) {
			return nil, fmt.Errorf("backup target: invalid env name %q", k)
		}
		t.env[k] = v
	}
	for _, k := range cfg.EnvSecrets {
		if _, ok := targetEnv[k]; !ok {
			return nil, fmt.Errorf("backup target: env secret %s not resolved", k)
		}
	}
	if k := cfg.RcloneConfigSecret; k != "" {
		if !envNameRe.MatchString(k) {
			return nil, fmt.Errorf("backup target: invalid rclone_config_secret %q", k)
		}
		if !cfg.UsesRclone() {
			return nil, errors.New("backup target: rclone_config_secret requires an rclone repository (archive remote:path or restic rclone:)")
		}
	}
	switch t.typ {
	case "restic":
		if password == "" {
			return nil, errors.New("backup target: restic requires password_secret")
		}
		t.env[envResticPassword] = password
		t.env[envResticRepo] = cfg.Repository
	case "archive":
		if t.format == "" {
			t.format = "tar.gz"
		}
		switch t.format {
		case "tar.gz":
			if password != "" {
				return nil, errors.New("backup target: tar.gz cannot be encrypted; use format zip or 7z (AES-256) with password_secret")
			}
		case "zip", "7z":
			if password != "" {
				t.env[envArchivePass] = password
			}
		default:
			return nil, fmt.Errorf("backup target: unknown archive format %q", t.format)
		}
		if !t.isRclone() && !strings.HasPrefix(cfg.Repository, "/") {
			return nil, fmt.Errorf("backup target: archive repository %q must be an absolute path or rclone remote:path", cfg.Repository)
		}
	default:
		return nil, fmt.Errorf("backup target: unknown type %q", t.typ)
	}
	return t, nil
}

func (t *target) method() string {
	if t.typ == "restic" {
		return "restic"
	}
	return "archive:" + t.format
}

func (t *target) isRclone() bool {
	r := t.cfg.Repository
	return !strings.HasPrefix(r, "/") && strings.Contains(r, ":")
}

func (t *target) tags() string {
	return "yoho,app=" + t.app + ",destination=" + t.dest
}

// envFlags forwards every secret env name into a container.
func (t *target) envFlags() string {
	names := sortedKeys(t.env)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(" -e " + n)
	}
	return b.String()
}

func mountFlags(mounts []string) string {
	var b strings.Builder
	for _, m := range mounts {
		b.WriteString(" -v " + remote.Quote(m))
	}
	return b.String()
}

// resticPrelude defines r(): restic on the Server if installed, else the
// pinned container with the given extra -v mount specs. Host paths are
// mounted at the same path so snapshot paths match the Server.
func (t *target) resticPrelude(base string, extra ...string) string {
	cache := path.Join(base, ".restic-cache")
	mounts := []string{cache + ":/cache"}
	pre := safeDir(cache, true, true)
	if strings.HasPrefix(t.cfg.Repository, "/") {
		mounts = append(mounts, t.cfg.Repository+":"+t.cfg.Repository)
		pre += safeDir(path.Clean(t.cfg.Repository), false, true)
	}
	mounts = append(mounts, extra...)
	native, docker := "restic", "docker run --rm --user \"$(id -u):$(id -g)\""
	if rc := t.rcloneConfig(); rc != nil {
		pre += rc.check
		native = rc.native + native
		docker += rc.dockerEnv
		mounts = append(mounts, rc.mount)
	}
	return pre + "\n" +
		"if command -v restic >/dev/null 2>&1; then r() { " + native + " \"$@\"; }; else r() { " + docker +
		t.envFlags() + " -e RESTIC_CACHE_DIR=/cache" + mountFlags(mounts) + " " + remote.Quote(ResticImage) +
		" \"$@\"; }; fi\n"
}

// rclonePrelude defines rc(): rclone on the Server if installed, else the
// pinned container. RCLONE_CONFIG_* env and the persistent rclone config
// (rclone_config_secret) configure remotes.
func (t *target) rclonePrelude(mounts ...string) string {
	pre, native, docker := "set -eu\n", "rclone", "docker run --rm"
	if rc := t.rcloneConfig(); rc != nil {
		pre = rc.check
		native = rc.native + native
		docker += rc.docker
		mounts = append(mounts, rc.mount)
	}
	return pre + "if command -v rclone >/dev/null 2>&1; then rc() { " + native + " \"$@\"; }; else rc() { " + docker +
		t.envFlags() + mountFlags(mounts) + " " + remote.Quote(RcloneImage) + " \"$@\"; }; fi\n"
}

// sevenZip runs a 7-Zip command line inside the helper container. The
// password is expanded from env inside the container's shell, so it never
// appears in the docker CLI argv. It does appear in the 7-Zip process
// argv, which host root can read in /proc; mount /proc with hidepid=2 to
// hide it from other users. 7-Zip offers no non-interactive stdin input.
func (t *target) sevenZip(mounts []string, inner string) string {
	return "set -eu\ndocker run --rm --network none" + t.envFlags() + mountFlags(mounts) + " --entrypoint sh " +
		remote.Quote(SevenZipImage) + " -c " + remote.Quote("set -eu; umask 077; z=$(command -v 7zz || command -v 7z || command -v 7za); "+inner)
}

func (t *target) passFlag() string {
	if _, ok := t.env[envArchivePass]; ok {
		return ` -p"$` + envArchivePass + `"`
	}
	return ""
}

func (t *target) archiveExt() string { return t.format }

func (t *target) archiveRe() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(t.project) + `-([0-9]{8}T[0-9]{6}Z)\.` + regexp.QuoteMeta(t.archiveExt()) + `$`)
}

func (t *target) remoteFile(name string) string {
	r := strings.TrimSuffix(t.cfg.Repository, "/")
	if strings.HasSuffix(r, ":") {
		return r + name
	}
	return r + "/" + name
}

// store saves base/<ts> to the Backup Target.
func (t *target) store(ctx context.Context, h remote.Host, cacheBase, base, ts string) (*Result, error) {
	staging := path.Join(base, ts)
	if t.typ == "restic" {
		return t.storeRestic(ctx, h, cacheBase, staging)
	}
	return t.storeArchive(ctx, h, base, ts)
}

func (t *target) storeRestic(ctx context.Context, h remote.Host, base, staging string) (*Result, error) {
	script := t.resticPrelude(base, staging+":"+staging+":ro") +
		"if ! out=$(r cat config 2>&1 >/dev/null); then\n" +
		"  if printf '%s' \"$out\" | grep -qiE 'does not exist|is there a repository|no such file'; then r init >&2; else printf '%s\\n' \"$out\" >&2; exit 1; fi\n" +
		"fi\n" +
		"r backup --json --host " + remote.Quote(t.project) + " --tag " + remote.Quote(t.tags()) + " " + remote.Quote(staging) + "\n"
	out, err := h.Output(ctx, remote.Cmd{Script: script, Env: t.env})
	if err != nil {
		return nil, fmt.Errorf("restic backup: %w", err)
	}
	res := &Result{Method: t.method()}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var s struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
			Total       int64  `json:"total_bytes_processed"`
		}
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.MessageType == "summary" {
			res.ID, res.Size = s.SnapshotID, s.Total
		}
	}
	if res.ID == "" {
		return nil, errors.New("restic backup: no snapshot id in output")
	}
	if len(res.ID) > 8 {
		res.ID = res.ID[:8]
	}
	if t.cfg.KeepLast > 0 {
		fs := t.resticPrelude(base) + fmt.Sprintf("r forget --tag %s --group-by tags --keep-last %d --prune >&2\n", remote.Quote(t.tags()), t.cfg.KeepLast)
		if err := h.Run(ctx, remote.Cmd{Script: fs, Env: t.env}); err != nil {
			return res, fmt.Errorf("restic forget (Backup %s was stored): %w", res.ID, err)
		}
	}
	return res, nil
}

func (t *target) storeArchive(ctx context.Context, h remote.Host, base, ts string) (*Result, error) {
	staging := path.Join(base, ts)
	name := t.project + "-" + ts + "." + t.archiveExt()
	local := path.Join(base, name)
	// Archives are created by the deploy UID in this call's exclusive staging,
	// never at a shared spool filename. Cleanup is owned by the staging marker.

	var create string
	switch t.format {
	case "tar.gz":
		create = "set -eu\ndocker run --rm --network none" + mountFlags([]string{staging + ":/in:ro", base + ":/out"}) + " " +
			remote.Quote(HelperImage) + " sh -c " + remote.Quote("umask 077; tar -C /in -czf "+remote.Quote("/out/"+name)+" .")
	case "zip":
		// AES-256, never ZipCrypto.
		create = t.sevenZip([]string{staging + ":/in:ro", base + ":/out"},
			`cd /in && "$z" a -bd -tzip -mem=AES256`+t.passFlag()+" "+remote.Quote("/out/"+name)+" .")
	case "7z":
		create = t.sevenZip([]string{staging + ":/in:ro", base + ":/out"},
			`cd /in && "$z" a -bd -t7z -mhe=on`+t.passFlag()+" "+remote.Quote("/out/"+name)+" .")
	}
	create = safeDir(base, true, false) + "test ! -e " + remote.Quote(local) + " && test ! -L " + remote.Quote(local) + "\n" +
		strings.Replace(create, "docker run --rm", `docker run --rm --user "$(id -u):$(id -g)"`, 1)
	if err := h.Run(ctx, remote.Cmd{Script: create, Env: t.env}); err != nil {
		return nil, fmt.Errorf("create %s archive: %w", t.format, err)
	}
	size, err := h.Output(ctx, remote.Cmd{Script: "wc -c < " + remote.Quote(local) + " | tr -d ' '"})
	if err != nil {
		return nil, fmt.Errorf("stat archive: %w", err)
	}
	res := &Result{ID: name, Size: parseInt(size), Method: t.method()}

	var copyErr error
	if t.isRclone() {
		copyScript := t.rclonePrelude(base+":"+base+":ro") + "rc copyto " + remote.Quote(local) + " " + remote.Quote(t.remoteFile(name)) + "\n"
		copyErr = t.rcloneAuthHint(h.Run(ctx, remote.Cmd{Script: copyScript, Env: t.env}))
	} else {
		copyErr = publishArchive(ctx, h, local, path.Clean(t.cfg.Repository), name)
	}
	if copyErr != nil {
		return nil, fmt.Errorf("copy archive to %s: %w", t.cfg.Repository, copyErr)
	}
	if t.cfg.KeepLast > 0 {
		if err := t.pruneArchives(ctx, h); err != nil {
			return res, fmt.Errorf("retention (Backup %s was stored): %w", name, err)
		}
	}
	return res, nil
}

// pruneArchives keeps the newest KeepLast archives of this App/Destination.
// Names sort by timestamp, so lexical order is chronological.
func (t *target) pruneArchives(ctx context.Context, h remote.Host) error {
	entries, err := t.list(ctx, h, "")
	if err != nil {
		return err
	}
	if len(entries) <= t.cfg.KeepLast {
		return nil
	}
	old := entries[t.cfg.KeepLast:] // list is newest first
	var b strings.Builder
	if t.isRclone() {
		b.WriteString(t.rclonePrelude())
		for _, e := range old {
			b.WriteString("rc deletefile " + remote.Quote(t.remoteFile(e.ID)) + " </dev/null\n")
		}
	} else {
		b.WriteString(safeDir(path.Clean(t.cfg.Repository), false, false))
		for _, e := range old {
			file := path.Join(t.cfg.Repository, e.ID)
			b.WriteString("yoho_file " + remote.Quote(file) + "\n" +
				"test -n \"$(find " + remote.Quote(file) + " -prune -user \"$yoho_uid\" -print)\"\n" +
				"rm -f " + remote.Quote(file) + "\n")
		}
	}
	return h.Run(ctx, remote.Cmd{Script: b.String(), Env: t.env})
}

// Entry is a stored Backup.
type Entry struct {
	ID   string
	Time time.Time
	Size int64 // 0 when unknown
}

// list returns Backups of this App/Destination, newest first.
func (t *target) list(ctx context.Context, h remote.Host, base string) ([]Entry, error) {
	var entries []Entry
	switch {
	case t.typ == "restic":
		out, err := h.Output(ctx, remote.Cmd{Script: t.resticPrelude(base) + "r snapshots --json --tag " + remote.Quote(t.tags()) + "\n", Env: t.env})
		if err != nil {
			return nil, fmt.Errorf("restic snapshots: %w", err)
		}
		var snaps []struct {
			ShortID string    `json:"short_id"`
			Time    time.Time `json:"time"`
			Summary *struct {
				Total int64 `json:"total_bytes_processed"`
			} `json:"summary"`
		}
		if err := json.Unmarshal([]byte(out), &snaps); err != nil {
			return nil, fmt.Errorf("restic snapshots: parse: %w", err)
		}
		for _, s := range snaps {
			e := Entry{ID: s.ShortID, Time: s.Time}
			if s.Summary != nil {
				e.Size = s.Summary.Total
			}
			entries = append(entries, e)
		}
	default:
		var script string
		if t.isRclone() {
			script = t.rclonePrelude() + "rc lsf --files-only --format ps --separator ' ' " + remote.Quote(t.cfg.Repository) + "\n"
		} else {
			// POSIX only: find -printf is GNU-specific and absent on BusyBox.
			dir := remote.Quote(path.Clean(t.cfg.Repository))
			script = safeDir(path.Clean(t.cfg.Repository), false, false) + "[ -d " + dir + " ] || exit 0\nfor f in " + dir + "/*; do\n" +
				"  case \"${f##*/}\" in " + remote.Quote(t.project) + "-*." + remote.Quote(t.archiveExt()) + ") ;; *) continue;; esac\n" +
				"  yoho_file \"$f\"\n  printf '%s %s\\n' \"${f##*/}\" \"$(wc -c < \"$f\" | tr -d ' ')\"\ndone\n"
		}
		out, err := h.Output(ctx, remote.Cmd{Script: script, Env: t.env})
		if err != nil {
			return nil, fmt.Errorf("list archives: %w", t.rcloneAuthHint(err))
		}
		re := t.archiveRe()
		for _, line := range strings.Split(out, "\n") {
			name, size, _ := strings.Cut(strings.TrimSpace(line), " ")
			m := re.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			ts, _ := time.Parse(tsLayout, m[1])
			entries = append(entries, Entry{ID: name, Time: ts, Size: parseInt(size)})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.After(entries[j].Time) })
	return entries, nil
}

// fetch places Backup id under dir (inside base) and returns the directory
// holding manifest.json.
func (t *target) fetch(ctx context.Context, h remote.Host, base, dir, id string) (string, error) {
	data := path.Join(dir, "data")
	if t.typ == "restic" {
		args := "r restore " + remote.Quote(id) + " --target " + remote.Quote(data)
		if id == "latest" {
			args += " --tag " + remote.Quote(t.tags())
		}
		script := t.resticPrelude(base, dir+":"+dir) + args + " >&2\n" +
			"find " + remote.Quote(data) + " -name manifest.json -type f | head -n 1\n"
		out, err := h.Output(ctx, remote.Cmd{Script: script, Env: t.env})
		if err != nil {
			return "", fmt.Errorf("restic restore: %w", err)
		}
		if out == "" {
			return "", fmt.Errorf("snapshot %s has no manifest.json; not a Yoho Backup", id)
		}
		found := path.Dir(out)
		if !strings.HasPrefix(found+"/", data+"/") {
			return "", errors.New("restic restore: manifest outside private staging")
		}
		if err := h.Run(ctx, remote.Cmd{Script: safeFile(path.Join(found, "manifest.json"), false)}); err != nil {
			return "", fmt.Errorf("restic restore: unsafe manifest: %w", err)
		}
		return found, nil
	}

	return t.fetchArchive(ctx, h, dir, id, false)
}

// fetchArchive extracts only inside its new private staging directory. Check
// and Restore run the extractor as the deploy user with only data writable.
func (t *target) fetchArchive(ctx context.Context, h remote.Host, dir, id string, _ bool) (string, error) {
	data := path.Join(dir, "data")

	if !t.archiveRe().MatchString(id) {
		return "", fmt.Errorf("archive %q does not belong to %s (%s)", id, t.project, t.format)
	}
	local := path.Join(dir, id)
	var get string
	if t.isRclone() {
		get = t.rclonePrelude(dir+":"+dir) + "rc copyto " + remote.Quote(t.remoteFile(id)) + " " + remote.Quote(local) + "\n"
	} else {
		get = safeFile(path.Join(t.cfg.Repository, id), false) + "cp " + remote.Quote(path.Join(t.cfg.Repository, id)) + " " + remote.Quote(local) + "\n"
	}
	get = safeDir(dir, true, false) + "test ! -e " + remote.Quote(local) + " && test ! -L " + remote.Quote(local) + "\n" + get
	if err := h.Run(ctx, remote.Cmd{Script: get, Env: t.env}); err != nil {
		return "", fmt.Errorf("fetch %s: %w", id, t.rcloneAuthHint(err))
	}
	// Untrusted outer archives get one writable mount: a new data directory.
	// The archive and ownership marker cannot be overwritten by an extractor.
	// Running unprivileged with a read-only rootfs also confines absolute/link
	// targets outside /out; no App volume or host parent is mounted here.
	pre := safeDir(dir, true, false) + "mkdir -m 0700 " + remote.Quote(data) + "\n"
	var inner, image, envFlags string
	switch t.format {
	case "tar.gz":
		image = HelperImage
		// -o discards outer ownership even when the deploy UID is root and
		// the Backup was created by a different UID. Payload volume metadata
		// remains packed inside its own tar and is restored separately.
		inner = `set -eu; tar -C /out -xozf /archive`
	default:
		image = SevenZipImage
		envFlags = t.envFlags()
		inner = `set -eu; z=$(command -v 7zz || command -v 7z || command -v 7za); "$z" x -bd -y` + t.passFlag() + ` -o/out /archive`
	}
	// The outer format contains only regular payload files and directories.
	// Internal App symlinks/metadata remain inside each untouched volume tar.
	inner += `; links=$(find /out -type l -print); test -z "$links"; links=$(find /out -type f -links +1 -print); test -z "$links"; special=$(find /out ! -type f ! -type d -print); test -z "$special"`
	extract := pre + `docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges --user "$(id -u):$(id -g)" --tmpfs /tmp:rw,noexec,nosuid,nodev` + envFlags +
		mountFlags([]string{local + ":/archive:ro", data + ":/out"}) + " --entrypoint sh " + remote.Quote(image) + " -c " + remote.Quote(inner)
	if err := h.Run(ctx, remote.Cmd{Script: extract, Env: t.env}); err != nil {
		return "", fmt.Errorf("extract %s: %w", id, err)
	}
	manifest := path.Join(data, "manifest.json")
	if err := h.Run(ctx, remote.Cmd{Script: safeFile(manifest, false)}); err != nil {
		return "", fmt.Errorf("archive: manifest.json missing, linked or unsafe: %w", err)
	}
	return data, nil
}
