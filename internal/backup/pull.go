package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Pull statuses.
const (
	PullCopied   = "copied"
	PullSkipped  = "skipped"
	PullConflict = "conflict" // same name, different size; never overwritten
	PullFailed   = "failed"
)

// PullOptions describes one mirror run for one Server.
type PullOptions struct {
	App, Destination, Project string
	Target                    config.BackupTarget
	Host                      remote.Host
	// Server names the Server for results and the optional subdirectory.
	Server string
	// Dir is the local root; files go to Dir/<app>/<destination>[/<server>]/.
	Dir string
	// ServerSubdir adds the Server name as a directory, for Destinations
	// with several Servers (Swarm writes archives on the node that ran the job).
	ServerSubdir bool
	// Limit pulls only the newest Limit archives (0: all).
	Limit int
	// Done is called after each archive is handled (optional).
	Done func(PulledArchive)
	// wrap lets tests corrupt the stream between the Server and the file.
	wrap func(io.Writer) io.Writer
}

// PulledArchive reports one archive.
type PulledArchive struct {
	ID     string
	Server string
	Status string
	Size   int64
	SHA256 string
	Path   string
	Error  string
}

// PullDir is the local directory for one Server's archives.
func PullDir(root, app, dest, server string, subdir bool) string {
	p := filepath.Join(root, app, dest)
	if subdir {
		p = filepath.Join(p, server)
	}
	return p
}

// CheckPullable refuses Backup Targets that are not a directory on the Server.
func CheckPullable(cfg config.BackupTarget) error {
	typ := cfg.Type
	if typ == "" || typ == "restic" {
		return errors.New("restic repositories are not files; use `restic copy` or `rclone sync` to mirror them")
	}
	if typ != "archive" {
		return fmt.Errorf("unknown Backup Target type %q", typ)
	}
	if !strings.HasPrefix(cfg.Repository, "/") && strings.Contains(cfg.Repository, ":") {
		return errors.New("rclone remotes are not on the Server; mirror them with `rclone sync`")
	}
	return nil
}

var shaRe = regexp.MustCompile(`(?i)\b([0-9a-f]{64})\b`)

// remoteHash is POSIX sh: sha256sum (GNU, BusyBox), shasum (macOS, Perl), openssl.
func remoteHash(file string) string {
	f := remote.Quote(file)
	return "sha256sum " + f + " 2>/dev/null || shasum -a 256 " + f + " 2>/dev/null || openssl dgst -sha256 " + f + " 2>/dev/null || true\n"
}

// Pull copies archives not yet in the local directory from one Server. It
// never deletes anything. Each archive is streamed over the Host's own
// transport (no rsync) into a .part file, verified by size and SHA-256
// against the Server's copy, then renamed into place. The returned error
// joins per-archive failures; results still list every archive handled.
func Pull(ctx context.Context, o PullOptions) ([]PulledArchive, error) {
	if o.Host == nil {
		return nil, errors.New("backup: Host is required")
	}
	if o.Dir == "" {
		return nil, errors.New("backup pull: local directory is required")
	}
	if err := CheckPullable(o.Target); err != nil {
		return nil, err
	}
	project := o.Project
	if project == "" {
		project = release.ProjectName(o.App, o.Destination)
	}
	// Pull reads files with the deploy user's own access and needs no
	// credentials; give newTarget placeholders so env_secrets do not block it.
	placeholder := map[string]string{}
	for _, k := range o.Target.EnvSecrets {
		placeholder[k] = ""
	}
	t, err := newTarget(o.App, o.Destination, project, o.Target, "", placeholder)
	if err != nil {
		return nil, err
	}
	entries, err := t.list(ctx, o.Host, "")
	if err != nil {
		return nil, err
	}
	// list is newest first; pull oldest first, so an interruption leaves a
	// contiguous mirror. Limit keeps the newest N.
	if o.Limit > 0 && len(entries) > o.Limit {
		entries = entries[:o.Limit]
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time.Before(entries[j].Time) })

	dir := PullDir(o.Dir, o.App, o.Destination, o.Server, o.ServerSubdir)
	if len(entries) > 0 {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	var results []PulledArchive
	var errs []error
	for _, e := range entries {
		r := PulledArchive{ID: e.ID, Server: o.Server, Size: e.Size, Path: filepath.Join(dir, e.ID)}
		if fi, serr := os.Lstat(r.Path); serr == nil {
			if fi.Mode().IsRegular() && fi.Size() == e.Size {
				r.Status = PullSkipped
			} else {
				r.Status = PullConflict
				r.Error = fmt.Sprintf("%s exists locally with a different size (%d, Server has %d); not overwritten", r.Path, fi.Size(), e.Size)
				errs = append(errs, errors.New(r.Error))
			}
		} else if !errors.Is(serr, os.ErrNotExist) {
			r.Status, r.Error = PullFailed, serr.Error()
			errs = append(errs, fmt.Errorf("%s: %w", e.ID, serr))
		} else if sum, perr := t.pullOne(ctx, o, dir, e); perr != nil {
			r.Status, r.Error = PullFailed, perr.Error()
			errs = append(errs, fmt.Errorf("%s: %w", e.ID, perr))
		} else {
			r.Status, r.SHA256 = PullCopied, sum
		}
		results = append(results, r)
		if o.Done != nil {
			o.Done(r)
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
	}
	return results, errors.Join(errs...)
}

func (t *target) pullOne(ctx context.Context, o PullOptions, dir string, e Entry) (sum string, err error) {
	file := path.Join(path.Clean(t.cfg.Repository), e.ID)
	// Exclusive per-run temp file (0600): a planted symlink or hard link at a
	// predictable name cannot redirect the write, and overlapping pulls do
	// not share a file.
	f, err := os.CreateTemp(dir, "."+e.ID+".*.part")
	if err != nil {
		return "", err
	}
	part := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(part)
		}
	}()
	var w io.Writer = f
	if o.wrap != nil {
		w = o.wrap(w)
	}
	cw := &countWriter{w: w}
	if err = o.Host.Run(ctx, remote.Cmd{Script: safeFile(file, false) + "cat " + remote.Quote(file) + "\n", Stdout: cw}); err != nil {
		return "", fmt.Errorf("read from Server: %w", err)
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	// Hash what landed on disk, not the stream, so write corruption counts.
	sum, n, err := hashFile(part)
	if err != nil {
		return "", err
	}
	if e.Size > 0 && (cw.n != e.Size || n != e.Size) {
		return "", fmt.Errorf("size mismatch: streamed %d bytes, wrote %d, Server listed %d", cw.n, n, e.Size)
	}
	out, err := o.Host.Output(ctx, remote.Cmd{Script: safeFile(file, false) + remoteHash(file)})
	if err != nil {
		return "", fmt.Errorf("checksum on Server: %w", err)
	}
	m := shaRe.FindStringSubmatch(out)
	if m == nil {
		return "", errors.New("checksum on Server: no SHA-256 tool (sha256sum, shasum, openssl) produced a digest")
	}
	if !strings.EqualFold(m[1], sum) {
		return "", fmt.Errorf("checksum mismatch: local %s, Server %s", sum, strings.ToLower(m[1]))
	}
	if err = publishLocal(part, filepath.Join(dir, e.ID)); err != nil {
		return "", err
	}
	return sum, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func hashFile(name string) (string, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// publishLocal moves part to final without replacing an existing final file.
// A hard link fails if final exists; filesystems without links fall back to
// an Lstat check and rename.
func publishLocal(part, final string) error {
	err := os.Link(part, final)
	switch {
	case err == nil:
		return os.Remove(part)
	case errors.Is(err, os.ErrExist):
		return fmt.Errorf("%s appeared during the copy; not overwritten", final)
	}
	if _, serr := os.Lstat(final); serr == nil {
		return fmt.Errorf("%s appeared during the copy; not overwritten", final)
	}
	return os.Rename(part, final)
}
