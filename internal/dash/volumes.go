package dash

import (
	"bufio"
	"context"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
)

// DiskUsage is the slow, on-demand part of the dashboards: sizes of the App
// directory and the App's volumes on each Server. Sizing walks every file,
// so it is collected apart from the Snapshot and much less often.
type DiskUsage struct {
	CollectedAt time.Time    `json:"collected_at"`
	Servers     []ServerDisk `json:"servers"`
}

// ServerDisk is the disk usage of the App on one Server.
type ServerDisk struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
	// Access is how volumes were read: root (the SSH user is root), sudo
	// (passwordless sudo for du), none (listed, not sized), no-timeout
	// (no timeout command, so du does not run as root) or scoped (not
	// available through the Gate).
	Access        string   `json:"access"`
	User          string   `json:"user,omitempty"`
	Note          string   `json:"note,omitempty"`
	AppDir        string   `json:"app_dir"`
	AppDirSized   bool     `json:"app_dir_sized"`
	AppDirPartial bool     `json:"app_dir_partial"`
	AppDirBytes   uint64   `json:"app_dir_bytes"`
	Volumes       []Volume `json:"volumes"`
	Elapsed       float64  `json:"elapsed_seconds"`
}

// Volume is one of the App's named volumes on a Server.
type Volume struct {
	Name     string   `json:"name"`
	Key      string   `json:"key"` // compose volume key
	Driver   string   `json:"driver"`
	Services []string `json:"services"`
	// Sized is set when du ran; Partial when it reported errors or was
	// stopped, so Bytes is a lower bound.
	Sized   bool       `json:"sized"`
	Partial bool       `json:"partial"`
	Bytes   uint64     `json:"bytes"`
	Top     []DirEntry `json:"top"` // largest direct children
	Error   string     `json:"error,omitempty"`
}

// DirEntry is a direct child of a volume (file or directory) with its size.
type DirEntry struct {
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

const (
	// TopEntries is how many children a Volume keeps.
	TopEntries = 10
	// duTimeout bounds one du run and duBudget the whole script, in
	// seconds; volumes past the budget are listed, not sized.
	duTimeout = 120
	duBudget  = 300
	// diskCollectTimeout bounds CollectDisk past the script budget.
	diskCollectTimeout = (duBudget + duTimeout + 60) * time.Second
)

// Records in the VolumesScript output.
const (
	markAccess   = "@@yoho-du-access"
	markAppDir   = "@@yoho-du-appdir"
	markMissing  = "@@yoho-du-missing"
	markLsErr    = "@@yoho-du-lserr"
	markVol      = "@@yoho-du-vol"
	markGone     = "@@yoho-du-gone"
	markSkip     = "@@yoho-du-skip"
	markExit     = "@@yoho-du-exit"
	markServices = "@@yoho-du-services"
)

// VolumesScript is the read-only shell script that sizes the App directory
// and the App's volumes (compose project or swarm stack labels).
//
// Volumes are read with du as root only: when the SSH user is root or
// `sudo -n du` works, and a timeout command can stop it. Otherwise they are
// listed without sizes. Only plain local volumes (driver local, no options)
// are sized: options mean NFS, CIFS or a bind of another host directory.
// With GNU du, records are NUL-terminated and newlines in names become '?',
// so a file name cannot forge a record. Errors go to stdout unsorted (fd 3);
// sizes are sorted and cut to the total plus the largest children.
func VolumesScript(project, appDir string) string {
	q := remote.Quote
	compose := q("label=com.docker.compose.project=" + project)
	stack := q("label=com.docker.stack.namespace=" + project)
	top := strconv.Itoa(TopEntries + 1)
	return `set -eu
LC_ALL=C; export LC_ALL
start=$(date +%s)
nl='
'
if [ "$(id -u)" = 0 ]; then a=root
elif command -v sudo >/dev/null 2>&1 && sudo -n du -sk /dev/null >/dev/null 2>&1; then a=sudo
else a=none; fi
lim=
if command -v timeout >/dev/null 2>&1; then
  if timeout -k 5 5 true 2>/dev/null; then lim="timeout -k 5 ` + strconv.Itoa(duTimeout) + `"
  elif timeout 5 true 2>/dev/null; then lim="timeout ` + strconv.Itoa(duTimeout) + `"; fi
fi
if [ -z "$lim" ] && [ "$a" != none ]; then a=no-timeout; fi
nul=
if du -0 -sk /dev/null >/dev/null 2>&1; then nul=1; fi
printf '` + markAccess + `\t%s\t%s\n' "$a" "$(id -un 2>/dev/null || :)"
low="nice -n 19"
if command -v ionice >/dev/null 2>&1 && ionice -c3 true 2>/dev/null; then low="$low ionice -c3"; fi
rdu() { if [ "$a" = sudo ]; then $low $lim sudo -n du "$@"; else $low $lim du "$@"; fi; }
sized() {
  if [ -n "$nul" ]; then
    { rdu -0 -a -k -x -d 1 -- "$1" 2>&3 || printf '` + markExit + `\t%s\n' "$?" >&3; } | tr '\n\0' '?\n' | sort -rn | head -n ` + top + `
  else
    { rdu -a -k -x -d 1 -- "$1" 2>&3 || printf '` + markExit + `\t%s\n' "$?" >&3; } | sort -rn | head -n ` + top + `
  fi
} 3>&1
echo ` + markAppDir + `
if d=$(cd ` + q(appDir) + ` 2>/dev/null && pwd -P); then
  rdu -s -k -x -- "$d" 2>/dev/null || printf '` + markExit + `\t%s\n' "$?"
else
  echo ` + markMissing + `
fi
if ! command -v docker >/dev/null 2>&1; then exit 0; fi
c=$(docker volume ls -q --filter ` + compose + ` 2>/dev/null) || { echo ` + markLsErr + `; c=; }
s=$(docker volume ls -q --filter ` + stack + ` 2>/dev/null) || { echo ` + markLsErr + `; s=; }
for v in $(printf '%s\n%s\n' "$c" "$s" | sort -u); do
  case $v in [A-Za-z0-9]*) ;; *) continue ;; esac
  case $v in *[!A-Za-z0-9_.-]*) continue ;; esac
  info=$(docker volume inspect --format '{{.Driver}}{{"\t"}}{{.Mountpoint}}{{"\t"}}{{range $k, $_ := .Options}}{{$k}},{{end}}{{"\t"}}{{index .Labels "com.docker.compose.volume"}}' "$v" 2>/dev/null) || { printf '` + markGone + `\t%s\n' "$v"; continue; }
  case $info in *"$nl"*) continue ;; esac
  printf '` + markVol + `\t%s\t%s\n' "$v" "$info"
  drv=$(printf '%s\n' "$info" | cut -f1)
  mp=$(printf '%s\n' "$info" | cut -f2)
  opts=$(printf '%s\n' "$info" | cut -f3)
  case $mp in /*) ;; *) continue ;; esac
  if [ "$drv" != local ] || [ -n "$opts" ]; then continue; fi
  case $a in root|sudo) ;; *) continue ;; esac
  if [ $(( $(date +%s) - start )) -ge ` + strconv.Itoa(duBudget) + ` ]; then echo ` + markSkip + `; continue; fi
  sized "$mp"
done
echo ` + markServices + `
{ docker ps -a --no-trunc --filter ` + compose + ` --format '{{.Label "com.docker.compose.service"}}{{"\t"}}{{.Mounts}}' 2>/dev/null || :
  docker ps -a --no-trunc --filter ` + stack + ` --format '{{.Label "com.docker.swarm.service.name"}}{{"\t"}}{{.Mounts}}' 2>/dev/null || :; } | sort -u
`
}

// ParseVolumes decodes VolumesScript output into d. Unknown or malformed
// lines are skipped so an odd record costs one entry, not the whole volume.
func ParseVolumes(out, project string, d *ServerDisk) {
	d.Volumes = []Volume{}
	var section string
	var cur *Volume
	var mount string
	var children []DirEntry
	var total uint64
	var haveTotal, partial, ran bool
	var errs []string
	flush := func() {
		if cur == nil {
			return
		}
		if ran {
			cur.Sized = true
			cur.Bytes = total
			if !haveTotal {
				// Stopped before the total line: the largest children
				// seen are a lower bound.
				partial = true
				for _, c := range children {
					cur.Bytes += c.Bytes
				}
			}
			cur.Partial = partial
			sort.SliceStable(children, func(i, j int) bool {
				if children[i].Bytes != children[j].Bytes {
					return children[i].Bytes > children[j].Bytes
				}
				return children[i].Name < children[j].Name
			})
			if len(children) > TopEntries {
				children = children[:TopEntries]
			}
			cur.Top = children
		}
		if len(errs) > 0 {
			cur.Error = strings.Join(errs, "; ")
		}
		d.Volumes = append(d.Volumes, *cur)
		cur, children, total, haveTotal, partial, ran, errs = nil, nil, 0, false, false, false, nil
	}
	var lsErr bool
	services := map[string]map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		f := strings.Split(line, "\t")
		switch f[0] {
		case markAccess:
			if len(f) >= 2 {
				d.Access = f[1]
			}
			if len(f) >= 3 {
				d.User = f[2]
			}
			continue
		case markAppDir, markServices:
			flush()
			section = f[0]
			continue
		case markLsErr:
			lsErr = true
			continue
		case markGone:
			flush()
			section = ""
			continue
		case markVol:
			flush()
			section = markVol
			if len(f) < 4 || !volumeNameRe.MatchString(f[1]) {
				continue
			}
			cur = &Volume{Name: f[1], Driver: f[2], Services: []string{}, Top: []DirEntry{}}
			mount = path.Clean(f[3])
			opts := ""
			if len(f) >= 5 {
				opts = strings.TrimSuffix(f[4], ",")
			}
			if len(f) >= 6 {
				cur.Key = f[5]
			}
			switch {
			case cur.Driver != "local":
				errs = append(errs, "driver "+cur.Driver+": not sized")
			case opts != "":
				errs = append(errs, "volume options ("+opts+"): not sized, may be remote or another host directory")
			}
			continue
		}
		switch section {
		case markAppDir:
			switch {
			case line == markMissing:
				// not deployed yet
			case strings.HasPrefix(line, markExit+"\t"):
				d.AppDirPartial = true
			default:
				if n, _, ok := duLine(line); ok {
					d.AppDirBytes, d.AppDirSized = n, true
				}
			}
		case markVol:
			if cur == nil {
				continue
			}
			if line == markSkip {
				errs = append(errs, "not sized: the scan took longer than "+strconv.Itoa(duBudget)+"s; rescan to size the rest")
				continue
			}
			if c, ok := strings.CutPrefix(line, markExit+"\t"); ok {
				ran, partial = true, true
				switch c {
				case "124", "137":
					errs = append(errs, "du stopped after "+strconv.Itoa(duTimeout)+"s")
				default:
					if len(errs) == 0 {
						errs = append(errs, "du exited "+c)
					}
				}
				continue
			}
			n, p, ok := duLine(line)
			if !ok {
				// Diagnostics (du:, sudo:, timeout:) become the error; a
				// malformed size record is dropped.
				if s := strings.TrimSpace(line); s != "" && (s[0] < '0' || s[0] > '9') && len(errs) < 3 {
					errs = append(errs, s)
				}
				continue
			}
			ran = true
			p = path.Clean(p)
			switch {
			case p == mount:
				total, haveTotal = n, true
			case path.Dir(p) == mount:
				children = append(children, DirEntry{Name: path.Base(p), Bytes: n})
			}
		case markServices:
			svc, mounts, ok := strings.Cut(line, "\t")
			if !ok || svc == "" {
				continue
			}
			svc = strings.TrimPrefix(svc, project+"_") // swarm service names
			for _, m := range strings.Split(mounts, ",") {
				m = strings.TrimSpace(m)
				if services[m] == nil {
					services[m] = map[string]bool{}
				}
				services[m][svc] = true
			}
		}
	}
	flush()
	for i := range d.Volumes {
		v := &d.Volumes[i]
		for s := range services[v.Name] {
			v.Services = append(v.Services, s)
		}
		sort.Strings(v.Services)
	}
	sort.SliceStable(d.Volumes, func(i, j int) bool {
		a, b := d.Volumes[i], d.Volumes[j]
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		return a.Name < b.Name
	})
	user := d.User
	if user == "" {
		user = "the SSH user"
	}
	switch d.Access {
	case "none":
		d.Note = "Volume sizes need root: connect as root, or allow passwordless sudo for du, e.g. `" + user + " ALL=(root) NOPASSWD: /usr/bin/du` in /etc/sudoers.d/yoho-du."
	case "no-timeout":
		d.Note = "Volume sizes need the timeout command (coreutils or busybox) so a scan as root cannot run unbounded."
	}
	if lsErr && d.Error == "" {
		d.Error = "docker volume ls failed; the volume list may be incomplete"
	}
}

var volumeNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// duLine parses "<KiB>\t<path>".
func duLine(line string) (uint64, string, bool) {
	k, p, ok := strings.Cut(line, "\t")
	if !ok || p == "" {
		return 0, "", false
	}
	n, err := strconv.ParseUint(k, 10, 64)
	if err != nil || n > 1<<53 {
		return 0, "", false
	}
	return n * 1024, p, true
}

// CollectDisk sizes the App directory and volumes on every Target in
// parallel. A failing Server is reported in its Error field, with whatever
// it printed before failing.
func CollectDisk(ctx context.Context, src *Source) (*DiskUsage, error) {
	du := &DiskUsage{CollectedAt: time.Now().UTC(), Servers: make([]ServerDisk, len(src.Targets))}
	appDir := path.Join(src.Root, "apps", src.App, src.Destination)
	var wg sync.WaitGroup
	for i, t := range src.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			d := ServerDisk{Name: t.Name, AppDir: appDir, Volumes: []Volume{}}
			defer func() { d.Elapsed = time.Since(start).Seconds(); du.Servers[i] = d }()
			if t.Reader != nil {
				d.Access = "scoped"
				d.Note = "Volume sizes are not available through scoped access yet."
				return
			}
			out, err := t.Host.Output(ctx, remote.Cmd{Script: VolumesScript(src.Project(), appDir)})
			ParseVolumes(out, src.Project(), &d)
			if err != nil {
				d.Error = err.Error()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return du, nil
}

// diskCall is one CollectDisk run that several callers wait for.
type diskCall struct {
	done chan struct{}
	du   *DiskUsage
	err  error
}

// Disk returns the cached DiskUsage when younger than DiskMaxAge (default
// 10 minutes), else joins or starts a collection. The collection runs on
// its own bounded context, so a caller that goes away (a closed browser
// tab) neither cancels it for others nor waits for it; and it has its own
// lock, so a slow du never delays Snapshot.
func (l *Live) Disk(ctx context.Context, fresh bool) (*DiskUsage, error) {
	maxAge := l.DiskMaxAge
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	l.diskMu.Lock()
	if !fresh && l.disk != nil && time.Since(l.diskAt) < maxAge {
		du := l.disk
		l.diskMu.Unlock()
		return du, nil
	}
	c := l.diskCall
	if c == nil {
		c = &diskCall{done: make(chan struct{})}
		l.diskCall = c
		go func() {
			cctx, cancel := context.WithTimeout(context.Background(), diskCollectTimeout)
			defer cancel()
			du, err := CollectDisk(cctx, l.Src)
			l.diskMu.Lock()
			c.du, c.err = du, err
			if err == nil {
				l.disk, l.diskAt = du, time.Now()
			}
			l.diskCall = nil
			l.diskMu.Unlock()
			close(c.done)
		}()
	}
	l.diskMu.Unlock()
	select {
	case <-c.done:
		return c.du, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
