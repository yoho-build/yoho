package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Staging kinds that can outlive a killed CLI with decrypted Backup data:
// drill staging (the whole Backup) and restore staging (the whole Backup
// plus prior-state.json, the recovery record of an interrupted restore).
const (
	StagingDrill   = "drill"
	StagingRestore = "restore"
)

const priorStateFile = "prior-state.json"

// stagingRe matches the names newStagingName gives drill and restore
// staging: <kind>-<UTC timestamp>-<32 hex>.
var stagingRe = regexp.MustCompile(`^(drill|restore)-([0-9]{8}T[0-9]{6}Z)-[0-9a-f]{32}$`)

// StagingDir is a drill or restore staging directory left in the
// Destination's Backup directory, proven Yoho-owned by its private
// ownership marker.
type StagingDir struct {
	Kind, Name, Path string
	// Created is parsed from the name (when the operation started).
	Created time.Time
	// PriorState: the directory holds prior-state.json (restore only): the
	// Services that were running before an interrupted or failed restore.
	PriorState bool
}

// Age is how long ago the staging directory was created.
func (d StagingDir) Age(now time.Time) time.Duration { return now.Sub(d.Created).Truncate(time.Second) }

// stagingProof proves a staging directory is a Yoho-owned private staging
// directory without knowing its token: a private, unlinked directory owned by
// the deploy user with a private marker holding exactly 32 hex characters.
// The caller defines d (the directory). It exits through yoho_bad otherwise.
const stagingProof = `yoho_private "$d"
m="$d/` + checkOwnerMarker + `"
yoho_secret "$m"
[ "$(wc -c < "$m" | tr -d '[:space:]')" -eq 32 ] || yoho_bad "$m"
t=$(cat "$m")
case "$t" in *[!0-9a-f]*|'') yoho_bad "$m";; esac
`

// ListStaging lists the drill or restore staging directories (kind) of
// app/dest. Directories that do not match the naming or cannot be proven
// Yoho-owned are never returned (and never removed). Read-only.
func ListStaging(ctx context.Context, h remote.Host, app, dest, kind string) ([]StagingDir, error) {
	if kind != StagingDrill && kind != StagingRestore {
		return nil, fmt.Errorf("staging: unknown kind %q", kind)
	}
	if !nameRe.MatchString(app) || !nameRe.MatchString(dest) {
		return nil, fmt.Errorf("staging: invalid App or Destination")
	}
	base := release.BackupDir(app, dest)
	script := "set -eu\n" + pathSafety +
		"b=" + remote.Quote(base) + "\n" +
		"if [ ! -e \"$b\" ] && [ ! -L \"$b\" ]; then exit 0; fi\n" +
		"yoho_private \"$b\"\n" +
		"for d in \"$b\"/" + kind + "-*; do\n" +
		"  [ -d \"$d\" ] && [ ! -L \"$d\" ] || continue\n" +
		"  (\n" + stagingProof + "  ) >/dev/null 2>&1 || continue\n" +
		"  p=no; if [ -f \"$d/" + priorStateFile + "\" ] && [ ! -L \"$d/" + priorStateFile + "\" ]; then p=yes; fi\n" +
		"  printf 'yoho-staging %s %s\\n' \"${d##*/}\" \"$p\"\n" +
		"done\n"
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil, fmt.Errorf("list %s staging: %w", kind, err)
	}
	var dirs []StagingDir
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "yoho-staging" {
			continue
		}
		m := stagingRe.FindStringSubmatch(f[1])
		if m == nil || m[1] != kind {
			continue
		}
		created, err := time.Parse(tsLayout, m[2])
		if err != nil {
			continue
		}
		dirs = append(dirs, StagingDir{Kind: kind, Name: f[1], Path: path.Join(base, f[1]), Created: created, PriorState: f[2] == "yes"})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	return dirs, nil
}

// RemoveStaging removes one leftover staging directory of app/dest after
// proving again that it is Yoho-owned. Call it with the Destination lock
// held so a running drill or restore never loses its staging. rm -rf does
// not follow symlinks inside the directory.
func RemoveStaging(ctx context.Context, h remote.Host, app, dest string, d StagingDir) error {
	base := release.BackupDir(app, dest)
	if !stagingRe.MatchString(d.Name) || !strings.HasPrefix(d.Name, d.Kind+"-") || d.Path != path.Join(base, d.Name) {
		return fmt.Errorf("staging: refusing to remove %q: not a drill or restore staging directory of %s/%s", d.Path, app, dest)
	}
	script := "set -eu\n" + pathSafety +
		"yoho_private " + remote.Quote(base) + "\n" +
		"d=" + remote.Quote(d.Path) + "\n" +
		"if [ ! -e \"$d\" ] && [ ! -L \"$d\" ]; then exit 0; fi\n" +
		stagingProof +
		"rm -rf \"$d\"\n"
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("remove %s staging %s: %w", d.Kind, d.Path, err)
	}
	return nil
}

// Leftovers are the drill projects and staging directories an interrupted
// (killed) drill or restore left on the Server.
type Leftovers struct {
	Drills  []string
	Staging []StagingDir
}

// Empty reports whether nothing was left behind.
func (l Leftovers) Empty() bool { return len(l.Drills) == 0 && len(l.Staging) == 0 }

// FindLeftovers lists leftover drill projects and drill/restore staging of
// app/dest. Read-only. exclude names a staging directory of the running
// operation, which is not a leftover.
func FindLeftovers(ctx context.Context, h remote.Host, app, dest, exclude string) (Leftovers, error) {
	var l Leftovers
	drills, err := ListDrills(ctx, h, app, dest)
	if err != nil {
		return l, err
	}
	l.Drills = drills
	for _, kind := range []string{StagingDrill, StagingRestore} {
		dirs, err := ListStaging(ctx, h, app, dest, kind)
		if err != nil {
			return l, err
		}
		for _, d := range dirs {
			if d.Name != exclude {
				l.Staging = append(l.Staging, d)
			}
		}
	}
	return l, nil
}

// reportLeftovers calls fn with what interrupted drills and restores left
// behind; a listing failure is logged, never fatal.
func reportLeftovers(ctx context.Context, h remote.Host, app, dest, exclude string, fn func(Leftovers), logf func(string, ...any)) {
	if fn == nil {
		return
	}
	l, err := FindLeftovers(ctx, h, app, dest, exclude)
	if err != nil {
		logf("could not check for leftover drills and restore staging: %v", err)
		return
	}
	if !l.Empty() {
		fn(l)
	}
}

// scrubStaging removes the decrypted Backup payload (generated secrets,
// dumps, volume archives, fetched archive) from a private staging directory
// it created (token), keeping the ownership marker, prior-state.json when
// keepState, and the files keep (paths inside dataDir, e.g. dumps an operator
// still needs).
func scrubStaging(ctx context.Context, h remote.Host, dir, token, dataDir string, keep []string, keepState bool) error {
	qdir, marker := remote.Quote(dir), remote.Quote(path.Join(dir, checkOwnerMarker))
	var s strings.Builder
	s.WriteString("set -eu\n" + pathSafety + "yoho_path " + remote.Quote(path.Dir(dir)) + "\n")
	s.WriteString("if [ ! -e " + qdir + " ] && [ ! -L " + qdir + " ]; then exit 0; fi\n")
	s.WriteString("yoho_private " + qdir + "\nyoho_secret " + marker + "\n")
	s.WriteString("test \"$(cat " + marker + ")\" = " + remote.Quote(token) + "\n")
	keepData := false
	var names []string
	inData := dataDir == path.Join(dir, "data") || strings.HasPrefix(dataDir, path.Join(dir, "data")+"/")
	for _, k := range keep {
		if !inData || path.Dir(k) != dataDir || !nameRe.MatchString(path.Base(k)) {
			return fmt.Errorf("scrub staging: refusing to keep %q outside %s", k, dataDir)
		}
		names = append(names, remote.Quote(path.Base(k)))
		keepData = true
	}
	top := checkOwnerMarker
	if keepState {
		top += "|" + priorStateFile
	}
	if keepData {
		top += "|data"
	}
	s.WriteString("for f in " + qdir + "/* " + qdir + "/.[!.]* " + qdir + "/..?*; do\n" +
		"  [ -e \"$f\" ] || [ -L \"$f\" ] || continue\n" +
		"  case \"${f##*/}\" in " + top + ") continue;; esac\n" +
		"  rm -rf \"$f\"\n" +
		"done\n")
	if keepData {
		// Only the directories leading to dataDir and the kept regular files
		// survive inside data/.
		rel := strings.TrimPrefix(dataDir, path.Join(dir, "data"))
		cur := path.Join(dir, "data")
		parts := strings.Split(strings.Trim(rel, "/"), "/")
		if rel == "" {
			parts = nil
		}
		for i := 0; i <= len(parts); i++ {
			keepHere := ""
			if i < len(parts) {
				keepHere = remote.Quote(parts[i])
			} else {
				keepHere = strings.Join(names, "|")
			}
			qcur := remote.Quote(cur)
			s.WriteString("if [ -d " + qcur + " ] && [ ! -L " + qcur + " ]; then\n" +
				"for f in " + qcur + "/* " + qcur + "/.[!.]* " + qcur + "/..?*; do\n" +
				"  [ -e \"$f\" ] || [ -L \"$f\" ] || continue\n")
			if i < len(parts) {
				s.WriteString("  case \"${f##*/}\" in " + keepHere + ") [ -d \"$f\" ] && [ ! -L \"$f\" ] && continue;; esac\n")
			} else {
				s.WriteString("  case \"${f##*/}\" in " + keepHere + ") [ -f \"$f\" ] && [ ! -L \"$f\" ] && continue;; esac\n")
			}
			s.WriteString("  rm -rf \"$f\"\ndone\nfi\n")
			if i < len(parts) {
				cur = path.Join(cur, parts[i])
			}
		}
	}
	return h.Run(ctx, remote.Cmd{Script: s.String()})
}

// interruptedRestores reads the prior-state.json of earlier restores of
// app/dest that did not finish (killed, interrupted or failed), oldest first.
// exclude is the running restore's staging. Unreadable records are logged
// and skipped.
func interruptedRestores(ctx context.Context, h remote.Host, app, dest, exclude string, logf func(string, ...any)) ([]StagingDir, []*restoreState, error) {
	dirs, err := ListStaging(ctx, h, app, dest, StagingRestore)
	if err != nil {
		return nil, nil, err
	}
	var used []StagingDir
	var states []*restoreState
	for _, d := range dirs {
		if d.Name == exclude || !d.PriorState {
			continue
		}
		f := path.Join(d.Path, priorStateFile)
		if err := h.Run(ctx, remote.Cmd{Script: safeFile(f, true)}); err != nil {
			logf("ignoring unsafe restore recovery state %s: %v", f, err)
			continue
		}
		b, err := h.ReadFile(ctx, f, false)
		if err != nil {
			logf("ignoring unreadable restore recovery state %s: %v", f, err)
			continue
		}
		var s restoreState
		if err := json.Unmarshal(b, &s); err != nil {
			logf("ignoring invalid restore recovery state %s: %v", f, err)
			continue
		}
		used = append(used, d)
		states = append(states, &s)
	}
	return used, states, nil
}

// mergePrior marks objects that an earlier, interrupted restore stopped as
// running again (Compose: same container ID recorded running; Swarm: same
// Service recorded with replicas), so this restore resumes the App's state
// from before the FIRST attempt. It returns the Services revived, and the
// Services recorded running whose objects no longer match (still stopped).
func (s *restoreState) mergePrior(prior []*restoreState) (revived, unmatched []string) {
	seenRev, seenUn := map[string]bool{}, map[string]bool{}
	for i := range s.Objects {
		o := &s.Objects[i]
		if s.Runtime == "swarm" {
			if o.Replicas == nil || *o.Replicas > 0 {
				continue
			}
		} else if o.Status == "running" {
			continue
		}
		matched, sameService := false, false
		for _, p := range prior {
			if p == nil || p.Runtime != s.Runtime {
				continue
			}
			for _, po := range p.Objects {
				if po.Service != o.Service {
					continue
				}
				wasRunning := po.Status == "running"
				if s.Runtime == "swarm" {
					wasRunning = po.Replicas != nil && *po.Replicas > 0
				}
				if !wasRunning {
					continue
				}
				if po.ID != o.ID {
					sameService = true
					continue
				}
				if s.Runtime == "swarm" {
					n := *po.Replicas
					o.Replicas = &n
				} else {
					o.Status = "running"
				}
				matched = true
			}
		}
		if matched && !seenRev[o.Service] {
			seenRev[o.Service] = true
			revived = append(revived, o.Service)
		} else if !matched && sameService && !seenUn[o.Service] {
			seenUn[o.Service] = true
			unmatched = append(unmatched, o.Service)
		}
	}
	sort.Strings(revived)
	sort.Strings(unmatched)
	return revived, unmatched
}
