package deploy

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// LabelFiles is set on Services that mount App files: a hash of the shipped
// files they use, so `yoho plan` and compose see content changes.
const LabelFiles = "yoho.files"

// maxAppFilesBytes caps the App files shipped with a Release.
const maxAppFilesBytes = 50 << 20

// appFile is one bind source (file or directory tree) from the App
// directory, shipped to <appDir>/files/<Hash>/<Name> on the Server. The
// directory is content-addressed, so unchanged files keep their path (no
// container recreate) and older Releases keep theirs for rollback.
type appFile struct {
	Local string // absolute local path (symlinks resolved)
	Name  string // base name on the Server
	Hash  string // content hash of Name's tree, 16 hex chars
	Size  int64
}

func (f appFile) serverPath(app, dest string) string {
	return path.Join(release.AppDir(app, dest), "files", f.Hash, f.Name)
}

// appFiles collects the read-only bind sources and config files that lie
// inside the App directory (compose-go resolved them to local absolute
// paths), keyed by that local path. Writable binds there are an error: state
// belongs in named volumes.
func appFiles(p *types.Project) (map[string]appFile, error) {
	out := map[string]appFile{}
	var total int64
	add := func(svc, src string) error {
		if _, ok := out[src]; ok {
			return nil
		}
		real, err := filepath.EvalSymlinks(src)
		if err != nil {
			if svc != "" {
				return fmt.Errorf("service %s: bind mount source %s: %w", svc, src, err)
			}
			return fmt.Errorf("config file %s: %w", src, err)
		}
		f := appFile{Local: real, Name: filepath.Base(src)}
		if f.Name == "." || f.Name == string(filepath.Separator) {
			f.Name = "app"
		}
		h := sha256.New()
		tw := tar.NewWriter(h)
		if f.Size, err = writeAppFile(tw, f, ""); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		f.Hash = hex.EncodeToString(h.Sum(nil))[:16]
		total += f.Size
		if total > maxAppFilesBytes {
			return fmt.Errorf("bind-mounted App files exceed %d MB (at %s); bake large files into an image or keep them in a named volume", maxAppFilesBytes>>20, src)
		}
		out[src] = f
		return nil
	}
	for _, name := range sortedKeys(p.Services) {
		for _, v := range p.Services[name].Volumes {
			if v.Type != types.VolumeTypeBind {
				continue
			}
			if _, in := composefile.InAppDir(p.WorkingDir, v.Source); !in {
				continue
			}
			if !v.ReadOnly {
				return nil, fmt.Errorf("service %s: writable bind mount %s is inside the App directory; use a named volume for state, or mount it :ro", name, v.Source)
			}
			if err := add(name, v.Source); err != nil {
				return nil, err
			}
		}
	}
	for _, k := range sortedKeys(p.Configs) {
		if c := p.Configs[k]; c.File != "" {
			if _, in := composefile.InAppDir(p.WorkingDir, c.File); in {
				if err := add("", c.File); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

// writeAppFile adds f to tw under prefix (prefix "" for the content hash)
// and returns the bytes of file content. Headers are
// normalized (no times or owners) so the hash only follows names, modes,
// link targets and contents. A .yoho directory is never shipped.
func writeAppFile(tw *tar.Writer, f appFile, prefix string) (int64, error) {
	var size int64
	err := filepath.WalkDir(f.Local, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(f.Local, p)
		if err != nil {
			return err
		}
		if de.IsDir() && de.Name() == ".yoho" && rel != "." {
			return filepath.SkipDir
		}
		name := path.Join(prefix, f.Name, filepath.ToSlash(rel))
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		switch {
		case info.Mode().IsRegular():
			hdr.Typeflag, hdr.Size = tar.TypeReg, info.Size()
		case info.IsDir():
			hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
		case info.Mode()&fs.ModeSymlink != 0:
			if hdr.Linkname, err = os.Readlink(p); err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
		default:
			return fmt.Errorf("%s: unsupported file type %s", p, info.Mode().Type())
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			fh, err := os.Open(p)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, fh)
			fh.Close()
			if err != nil {
				return err
			}
			size += n
			if size > maxAppFilesBytes {
				return fmt.Errorf("%s exceeds %d MB; bake large files into an image or keep them in a named volume", f.Local, maxAppFilesBytes>>20)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

// shipAppFiles uploads the App files that are not on the Server yet and
// returns the content hashes the Release uses (for retention).
func shipAppFiles(ctx context.Context, h remote.Host, d *plan.Deploy, logf func(string, ...any)) ([]string, error) {
	files, err := appFiles(d.Project)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	dir := path.Join(release.AppDir(d.App, d.Destination), "files")
	have, err := h.Output(ctx, remote.Cmd{Script: "for f in " + remote.Quote(dir) + "/*; do [ -d \"$f\" ] && echo \"${f##*/}\"; done; true"})
	if err != nil {
		return nil, fmt.Errorf("list App files: %w", err)
	}
	present := map[string]bool{}
	for _, l := range lines(have) {
		present[l] = true
	}
	var hashes []string
	var missing []appFile
	queued := map[string]bool{}
	for _, k := range sortedKeys(files) {
		f := files[k]
		if !slices.Contains(hashes, f.Hash) {
			hashes = append(hashes, f.Hash)
		}
		if !present[f.Hash] && !queued[f.Hash] {
			queued[f.Hash] = true
			missing = append(missing, f)
		}
	}
	slices.Sort(hashes)
	if len(missing) == 0 {
		return hashes, nil
	}
	var size int64
	for _, f := range missing {
		size += f.Size
	}
	logf("shipping %d App file(s) (%s)", len(missing), byteSize(size))
	pr, pw := io.Pipe()
	werr := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		var err error
		for _, f := range missing {
			if _, err = writeAppFile(tw, f, f.Hash); err != nil {
				break
			}
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
		werr <- err
	}()
	var mv strings.Builder
	for _, f := range missing {
		q := remote.Quote(f.Hash)
		mv.WriteString(`[ -e "$d"/` + q + ` ] || mv "$t"/` + q + ` "$d"/` + q + "\n")
	}
	script := "set -eu\numask 077\nd=" + remote.Quote(dir) + "\nmkdir -p \"$d\"\n" +
		"t=$(mktemp -d \"$d/.upload.XXXXXX\")\ntrap 'rm -rf \"$t\"' EXIT\n" +
		"tar -xpmf - -C \"$t\"\ncat >/dev/null\n" + mv.String()
	if err := h.Run(ctx, remote.Cmd{Script: script, Stdin: pr}); err != nil {
		pr.CloseWithError(err)
		<-werr
		return nil, fmt.Errorf("ship App files: %w", err)
	}
	pr.Close() // the script drains stdin; this only unblocks a stuck writer
	if err := <-werr; err != nil {
		// The stream ended early; a truncated tree may have been moved in.
		var rm []string
		for _, f := range missing {
			rm = append(rm, path.Join(dir, f.Hash))
		}
		_ = h.Run(context.WithoutCancel(ctx), remote.Cmd{Script: "rm -rf " + remote.QuoteArgs(rm...)})
		return nil, fmt.Errorf("ship App files: %w", err)
	}
	return hashes, nil
}

// rewriteAppFiles points bind sources and config files from the App
// directory at their shipped copies and labels Services with a hash of the
// files they use.
func rewriteAppFiles(p *types.Project, files map[string]appFile, app, dest string) {
	if len(files) == 0 {
		return
	}
	cfgHash := map[string]string{}
	for k, c := range p.Configs {
		if f, ok := files[c.File]; ok {
			cfgHash[k] = f.Hash
			c.File = f.serverPath(app, dest)
			p.Configs[k] = c
		}
	}
	for name, s := range p.Services {
		var used []string
		for i, v := range s.Volumes {
			if f, ok := files[v.Source]; ok && v.Type == types.VolumeTypeBind {
				v.Source = f.serverPath(app, dest)
				s.Volumes[i] = v
				used = append(used, f.Hash)
			}
		}
		for _, c := range s.Configs {
			if h, ok := cfgHash[c.Source]; ok {
				used = append(used, h)
			}
		}
		if len(used) == 0 {
			continue
		}
		slices.Sort(used)
		sum := sha256.Sum256([]byte(strings.Join(slices.Compact(used), ",")))
		if s.Labels == nil {
			s.Labels = types.Labels{}
		}
		s.Labels[LabelFiles] = hex.EncodeToString(sum[:])[:16]
		p.Services[name] = s
	}
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
