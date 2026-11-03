package deploy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// RollbackWarnings reads what a rollback to version brings back that the
// volumes warning does not cover, both runtimes: Services that lose
// Yoho's default log rotation (a Release compiled before v0.6.0) and,
// with releaseSecrets (--with-release-secrets: the Release's own
// generation, not the current values), generated secrets rotated since
// the Release, whose old value the database no longer accepts.
// Unreadable files give no warning: these are notes, and the rollback
// itself checks the Release.
func RollbackWarnings(ctx context.Context, h remote.Host, app, dest, version string, releaseSecrets bool) []string {
	relDir := release.Dir(app, dest, version)
	target, err := readRelease(ctx, h, path.Join(relDir, "release.json"))
	if err != nil {
		return nil
	}
	var warns []string
	if w := loggingRollbackWarning(ctx, h, app, dest, version); w != "" {
		warns = append(warns, w)
	}
	if !releaseSecrets {
		return warns
	}
	// --with-release-secrets runs the values the Release was first deployed with.
	base := *target
	base.Secrets = BaseAudit(target)
	target = &base
	out, err := h.Output(ctx, remote.Cmd{Script: "cat " + remote.Quote(RotationsLog(app, dest)) + " 2>/dev/null || true"})
	if err == nil {
		warns = append(warns, RotatedSince(target, ParseRotations(out))...)
	}
	return warns
}

// loggingRollbackWarning names the Services whose current compiled
// compose has Yoho's default logging and version's has none.
func loggingRollbackWarning(ctx context.Context, h remote.Host, app, dest, version string) string {
	dir := release.AppDir(app, dest)
	cur, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	curVersion := path.Base(strings.TrimSpace(cur))
	if err != nil || curVersion == "" || curVersion == "." || curVersion == version {
		return ""
	}
	curYAML, err := h.ReadFile(ctx, path.Join(release.Dir(app, dest, curVersion), "compose.yaml"), false)
	if err != nil {
		return ""
	}
	oldYAML, err := h.ReadFile(ctx, path.Join(release.Dir(app, dest, version), "compose.yaml"), false)
	if err != nil {
		return ""
	}
	svcs := LostDefaultLogging(curYAML, oldYAML)
	if len(svcs) == 0 {
		return ""
	}
	return fmt.Sprintf("this Release predates Yoho's default log rotation (v0.6.0): rolling back runs %s without it and recreates %s (Stateful Services stop first); the next deploy recreates %s again",
		strings.Join(svcs, ", "), them(len(svcs)), them(len(svcs)))
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// LostDefaultLogging lists (sorted) the Services that have Yoho's default
// logging in the compiled compose cur and no logging in old.
func LostDefaultLogging(cur, old []byte) []string {
	var c, o struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if yaml.Unmarshal(cur, &c) != nil || yaml.Unmarshal(old, &o) != nil {
		return nil
	}
	var out []string
	for name, svc := range c.Services {
		prev, ok := o.Services[name]
		if !ok || !IsDefaultLogging(svc["logging"]) {
			continue
		}
		if _, has := prev["logging"]; !has {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// generatedFingerprints are the fingerprints rel records for the
// generated key: audit entries "<Service>/<key>" (or plain "<key>").
func generatedFingerprints(rel *release.Release, key string) []string {
	var fps []string
	for name, a := range rel.Secrets {
		if a.Fingerprint == "" || name != key && !strings.HasSuffix(name, "/"+key) {
			continue
		}
		if a.Ref != "" && a.Ref != "generated" {
			continue
		}
		fps = append(fps, a.Fingerprint)
	}
	return fps
}

// ParseRotations reads RotationsLog lines, skipping lines it cannot parse.
func ParseRotations(s string) []RotationRecord {
	var recs []RotationRecord
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r RotationRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Key != "" {
			recs = append(recs, r)
		}
	}
	return recs
}

// RotatedSince warns, per key (sorted), about generated secrets rotated
// since target last ran whose latest value target does not carry: its
// recorded fingerprint differs, or without one, the rotation is newer.
func RotatedSince(target *release.Release, recs []RotationRecord) []string {
	latest := map[string]RotationRecord{}
	for _, r := range recs {
		if l, ok := latest[r.Key]; !ok || !r.Time.Before(l.Time) {
			latest[r.Key] = r
		}
	}
	var keys []string
	for k, r := range latest {
		// The recorded fingerprint decides: a rollback re-records the
		// Release with a new DeployedAt but keeps its old value. Without
		// one, the times do.
		if fps := generatedFingerprints(target, k); len(fps) > 0 && r.Fingerprint != "" {
			if !slices.ContainsFunc(fps, func(fp string) bool { return fp != r.Fingerprint }) {
				continue
			}
		} else if !r.Time.After(target.DeployedAt) {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var warns []string
	for _, k := range keys {
		warns = append(warns, fmt.Sprintf("%s was rotated after this Release (%s); with --with-release-secrets its Services start with the old value the database no longer accepts. Run `yoho secrets rotate --generated %s` after the rollback, or roll back without --with-release-secrets",
			k, latest[k].Time.UTC().Format(time.RFC3339), k))
	}
	return warns
}
