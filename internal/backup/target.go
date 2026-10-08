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
	t := &target{app: app, dest: dest, project: project, cfg: cfg, typ: cfg.Type, format: cfg.Format, env: map[string]string{}}
	if t.typ == "" {
		t.typ = "restic"
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
	pre := "mkdir -p " + remote.Quote(cache)
	if strings.HasPrefix(t.cfg.Repository, "/") {
		mounts = append(mounts, t.cfg.Repository+":"+t.cfg.Repository)
		pre += " " + remote.Quote(t.cfg.Repository)
	}
	mounts = append(mounts, extra...)
	return "set -eu\n" + pre + "\n" +
		"if command -v restic >/dev/null 2>&1; then r() { restic \"$@\"; }; else r() { docker run --rm" +
		t.envFlags() + " -e RESTIC_CACHE_DIR=/cache" + mountFlags(mounts) + " " + remote.Quote(ResticImage) +
		" \"$@\"; }; fi\n"
}

// rclonePrelude defines rc(): rclone on the Server if installed, else the
// pinned container. RCLONE_CONFIG_* env configures remotes.
func (t *target) rclonePrelude(mounts ...string) string {
	return "set -eu\nif command -v rclone >/dev/null 2>&1; then rc() { rclone \"$@\"; }; else rc() { docker run --rm" +
		t.envFlags() + mountFlags(mounts) + " " + remote.Quote(RcloneImage) + " \"$@\"; }; fi\n"
}

// sevenZip runs a 7-Zip command line inside the helper container. The
// password is expanded from env inside the container's shell, so it never
// appears in the docker CLI argv. It does appear in the 7-Zip process
// argv, which host root can read in /proc; mount /proc with hidepid=2 to
// hide it from other users. 7-Zip offers no non-interactive stdin input.
func (t *target) sevenZip(mounts []string, inner string) string {
	return "set -eu\ndocker run --rm --network none" + t.envFlags() + mountFlags(mounts) + " --entrypoint sh " +
		remote.Quote(SevenZipImage) + " -c " + remote.Quote("set -eu; z=$(command -v 7zz || command -v 7z || command -v 7za); "+inner)
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
func (t *target) store(ctx context.Context, h remote.Host, base, ts string) (*Result, error) {
	staging := path.Join(base, ts)
	if t.typ == "restic" {
		return t.storeRestic(ctx, h, base, staging)
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
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = h.Run(cctx, remote.Cmd{Script: "rm -f " + remote.Quote(local)})
	}()

	var create string
	switch t.format {
	case "tar.gz":
		create = "set -eu\ndocker run --rm --network none" + mountFlags([]string{staging + ":/in:ro", base + ":/out"}) + " " +
			remote.Quote(HelperImage) + " tar -C /in -czf " + remote.Quote("/out/"+name) + " ."
	case "zip":
		// AES-256, never ZipCrypto.
		create = t.sevenZip([]string{staging + ":/in:ro", base + ":/out"},
			`cd /in && "$z" a -bd -tzip -mem=AES256`+t.passFlag()+" "+remote.Quote("/out/"+name)+" .")
	case "7z":
		create = t.sevenZip([]string{staging + ":/in:ro", base + ":/out"},
			`cd /in && "$z" a -bd -t7z -mhe=on`+t.passFlag()+" "+remote.Quote("/out/"+name)+" .")
	}
	if err := h.Run(ctx, remote.Cmd{Script: create, Env: t.env}); err != nil {
		return nil, fmt.Errorf("create %s archive: %w", t.format, err)
	}
	size, err := h.Output(ctx, remote.Cmd{Script: "stat -c %s " + remote.Quote(local)})
	if err != nil {
		return nil, fmt.Errorf("stat archive: %w", err)
	}
	res := &Result{ID: name, Size: parseInt(size), Method: t.method()}

	var copyScript string
	if t.isRclone() {
		copyScript = t.rclonePrelude(base+":"+base+":ro") + "rc copyto " + remote.Quote(local) + " " + remote.Quote(t.remoteFile(name)) + "\n"
	} else {
		repo := t.cfg.Repository
		copyScript = "set -eu\numask 077\nmkdir -p " + remote.Quote(repo) + "\ncp " + remote.Quote(local) + " " +
			remote.Quote(path.Join(repo, name+".partial")) + "\nmv -f " + remote.Quote(path.Join(repo, name+".partial")) +
			" " + remote.Quote(path.Join(repo, name)) + "\n"
	}
	if err := h.Run(ctx, remote.Cmd{Script: copyScript, Env: t.env}); err != nil {
		return nil, fmt.Errorf("copy archive to %s: %w", t.cfg.Repository, err)
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
		b.WriteString("set -eu\n")
		for _, e := range old {
			b.WriteString("rm -f " + remote.Quote(path.Join(t.cfg.Repository, e.ID)) + "\n")
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
			script = "set -eu\n[ -d " + remote.Quote(t.cfg.Repository) + " ] || exit 0\nfind " + remote.Quote(t.cfg.Repository) +
				" -maxdepth 1 -type f -printf '%f %s\\n'\n"
		}
		out, err := h.Output(ctx, remote.Cmd{Script: script, Env: t.env})
		if err != nil {
			return nil, fmt.Errorf("list archives: %w", err)
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
		return path.Dir(out), nil
	}

	if !t.archiveRe().MatchString(id) {
		return "", fmt.Errorf("archive %q does not belong to %s (%s)", id, t.project, t.format)
	}
	local := path.Join(dir, id)
	var get string
	if t.isRclone() {
		get = t.rclonePrelude(dir+":"+dir) + "rc copyto " + remote.Quote(t.remoteFile(id)) + " " + remote.Quote(local) + "\n"
	} else {
		get = "set -eu\ncp " + remote.Quote(path.Join(t.cfg.Repository, id)) + " " + remote.Quote(local) + "\n"
	}
	if err := h.Run(ctx, remote.Cmd{Script: get, Env: t.env}); err != nil {
		return "", fmt.Errorf("fetch %s: %w", id, err)
	}
	var extract string
	switch t.format {
	case "tar.gz":
		extract = "set -eu\ndocker run --rm --network none" + mountFlags([]string{dir + ":/s"}) + " " + remote.Quote(HelperImage) +
			" sh -c " + remote.Quote(`mkdir -p /s/data && tar -C /s/data -xzf "/s/$1"`) + " sh " + remote.Quote(id)
	default:
		extract = t.sevenZip([]string{dir + ":/s"}, `"$z" x -bd -y`+t.passFlag()+" -o/s/data "+remote.Quote("/s/"+id))
	}
	if err := h.Run(ctx, remote.Cmd{Script: extract, Env: t.env}); err != nil {
		return "", fmt.Errorf("extract %s: %w", id, err)
	}
	return data, nil
}
