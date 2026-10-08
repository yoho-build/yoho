// Package deploy is the compose Runtime: it deploys an App to one Server as a
// compose project with a compiled, secret-free compose file, versioned
// secret files, release commands, Hooks, and zero-downtime Proxy cutover.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Compose implements plan.Runtime with `docker compose` on a single Server.
type Compose struct{}

var _ plan.Runtime = Compose{}

const defaultRetain = 5

// now is replaceable in tests.
var now = func() time.Time { return time.Now().UTC() }

func newRunner(d *plan.Deploy, composeFile string) *runner {
	s := d.Servers[0]
	out := d.Out
	if out == nil {
		out = io.Discard
	}
	return &runner{
		h: s.Host, out: out, server: s.Name, app: d.App, dest: d.Destination,
		cc: composeCLI{project: release.ProjectName(d.App, d.Destination), dir: release.AppDir(d.App, d.Destination), file: composeFile},
	}
}

func (Compose) hook(ctx context.Context, d *plan.Deploy, name string, extra map[string]string) error {
	if d.Hook == nil {
		return nil
	}
	env := map[string]string{
		"YOHO_APP":             d.App,
		"YOHO_DESTINATION":     d.Destination,
		"YOHO_VERSION":         d.Version,
		"YOHO_SERVICE_VERSION": d.App + "@" + d.Version,
		"YOHO_PERFORMER":       d.Performer,
		"YOHO_HOSTS":           hostsList(d),
	}
	for k, v := range extra {
		env[k] = v
	}
	return d.Hook(ctx, name, env)
}

// hostsList is the comma-separated Server addresses (ssh target without user).
func hostsList(d *plan.Deploy) string {
	var hs []string
	for _, s := range d.Servers {
		h := s.Server.SSH
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = h[i+1:]
		}
		if h == "" {
			h = s.Name
		}
		hs = append(hs, h)
	}
	return strings.Join(hs, ",")
}

// needsProxy reports whether the Proxy (and the yoho network) must be up:
// proxied Services, or Services joining the yoho network themselves (e.g.
// cloudflared targeting http://yoho-proxy:80).
func needsProxy(plans []servicePlan) bool {
	return slices.ContainsFunc(plans, func(sp servicePlan) bool { return sp.Proxy != nil || sp.ProxyNetwork })
}

// Deploy implements plan.Runtime.
func (c Compose) Deploy(ctx context.Context, d *plan.Deploy) (_ *release.Release, err error) {
	if err := validate(d); err != nil {
		return nil, err
	}
	start := now()
	srv := d.Servers[0]
	h := srv.Host
	dir := release.AppDir(d.App, d.Destination)
	relDir := release.Dir(d.App, d.Destination, d.Version)
	composeFile := path.Join(relDir, "compose.yaml")
	r := newRunner(d, composeFile)

	unlock, err := AcquireLock(ctx, h, d.App, d.Destination, LockInfo{Performer: d.Performer, Version: d.Version, Command: "deploy", Time: start})
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := unlock(ctx); uerr != nil && err == nil {
			err = uerr
		}
	}()

	r.logf("preparing release %s", d.Version)
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\numask 077\nmkdir -p " + remote.QuoteArgs(dir, path.Join(dir, "releases"), path.Join(dir, "secrets"), path.Join(dir, "generated"), relDir)}); err != nil {
		return nil, fmt.Errorf("prepare %s: %w", dir, err)
	}
	key, err := ensureHMACKey(ctx, h, dir)
	if err != nil {
		return nil, err
	}
	kinds, err := generatedDecls(d)
	if err != nil {
		return nil, err
	}
	generated, err := ensureGenerated(ctx, h, dir, kinds)
	if err != nil {
		return nil, err
	}
	svcSecrets, err := serviceSecrets(d, generated, r.logf)
	if err != nil {
		return nil, err
	}

	generation := start.Format("20060102T150405Z") + "-" + d.Version
	genDir := release.SecretsDir(d.App, d.Destination, generation)
	asEnv := map[string]bool{}
	for svc, ext := range d.Ext {
		asEnv[svc] = ext.SecretsAsEnv
	}
	r.logf("writing secrets generation %s", generation)
	if err := writeSecrets(ctx, h, genDir, svcSecrets, asEnv); err != nil {
		return nil, err
	}

	composeYAML, plans, err := compile(d, srv.Name, genDir, svcSecrets, key)
	if err != nil {
		return nil, err
	}
	if err := writeReleaseFiles(ctx, h, relDir, composeYAML, plans); err != nil {
		return nil, err
	}

	rel := &release.Release{
		App: d.App, Destination: d.Destination, Server: srv.Name, Version: d.Version,
		Runtime: "compose", Role: "all", DeployedAt: start, Performer: d.Performer,
		Images: map[string]string{}, SecretsGeneration: generation,
		Secrets:       secretAudit(d, svcSecrets, generated, key),
		ComposeSHA256: sha256Hex(composeYAML), Status: "failed",
	}
	for _, sp := range plans {
		if sp.Image != "" {
			rel.Images[sp.Name] = sp.Image
		}
	}
	defer func() {
		if err != nil {
			// Best effort: keep a record of the failed attempt.
			_ = writeJSON(context.WithoutCancel(ctx), h, path.Join(relDir, "release.json"), rel)
		}
	}()

	if err := c.hook(ctx, d, "pre-deploy", nil); err != nil {
		return nil, err
	}
	if needsProxy(plans) {
		if err := proxy.Boot(ctx, h, d.Proxy, r.out); err != nil {
			return nil, err
		}
	}
	if err := r.startDependencies(ctx, plans); err != nil {
		return nil, err
	}
	if err := r.releaseCommands(ctx, plans); err != nil {
		return nil, err
	}
	if err := r.cutover(ctx, plans); err != nil {
		return nil, err
	}
	r.removeStaleRoutes(ctx, plans)

	rel.Status = "deployed"
	if err := c.finish(ctx, d, r, rel); err != nil {
		return nil, err
	}
	pruneImages(ctx, r, d)
	runtime := int(now().Sub(start).Round(time.Second) / time.Second)
	if err := c.hook(ctx, d, "post-deploy", map[string]string{"YOHO_RUNTIME": strconv.Itoa(runtime)}); err != nil {
		return rel, err
	}
	r.logf("deployed %s in %ds", d.Version, runtime)
	return rel, nil
}

// finish records the Release, points `current` at it and prunes.
func (c Compose) finish(ctx context.Context, d *plan.Deploy, r *runner, rel *release.Release) error {
	dir := release.AppDir(d.App, d.Destination)
	if err := writeJSON(ctx, r.h, path.Join(release.Dir(d.App, d.Destination, rel.Version), "release.json"), rel); err != nil {
		return err
	}
	if err := r.h.Run(ctx, remote.Cmd{Script: "ln -sfn " + remote.Quote("releases/"+rel.Version) + " " + remote.Quote(path.Join(dir, "current"))}); err != nil {
		return fmt.Errorf("update current release: %w", err)
	}
	if err := prune(ctx, r, d, rel.Version); err != nil {
		r.logf("warning: pruning old releases failed: %v", err)
	}
	return nil
}

func writeReleaseFiles(ctx context.Context, h remote.Host, relDir string, composeYAML []byte, plans []servicePlan) error {
	if err := h.WriteFile(ctx, path.Join(relDir, "compose.yaml"), composeYAML, 0o600, false); err != nil {
		return fmt.Errorf("write compose.yaml: %w", err)
	}
	if err := h.WriteFile(ctx, path.Join(relDir, "compose_sha256"), []byte(sha256Hex(composeYAML)+"\n"), 0o600, false); err != nil {
		return fmt.Errorf("write compose_sha256: %w", err)
	}
	return writeJSON(ctx, h, path.Join(relDir, "plan.json"), plans)
}

func writeJSON(ctx context.Context, h remote.Host, p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := h.WriteFile(ctx, p, append(b, '\n'), 0o600, false); err != nil {
		return fmt.Errorf("write %s: %w", path.Base(p), err)
	}
	return nil
}

// Rollback implements plan.Runtime: it redeploys a retained Release's
// compiled compose (and its secrets generation) through the same
// zero-downtime cutover. No build, no release_command.
func (c Compose) Rollback(ctx context.Context, d *plan.Deploy, version string) (_ *release.Release, err error) {
	if len(d.Servers) != 1 || d.Servers[0].Host == nil {
		return nil, fmt.Errorf("compose runtime rolls back exactly one Server, destination %s has %d", d.Destination, len(d.Servers))
	}
	if !versionRe.MatchString(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	start := now()
	srv := d.Servers[0]
	h := srv.Host
	dir := release.AppDir(d.App, d.Destination)
	relDir := release.Dir(d.App, d.Destination, version)
	composeFile := path.Join(relDir, "compose.yaml")
	r := newRunner(d, composeFile)

	unlock, err := AcquireLock(ctx, h, d.App, d.Destination, LockInfo{Performer: d.Performer, Version: version, Command: "rollback", Time: start})
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := unlock(ctx); uerr != nil && err == nil {
			err = uerr
		}
	}()

	rel, err := readRelease(ctx, h, path.Join(relDir, "release.json"))
	if err != nil {
		return nil, fmt.Errorf("release %s not found on %s: %w", version, srv.Name, err)
	}
	composeYAML, err := h.ReadFile(ctx, composeFile, false)
	if err != nil {
		return nil, fmt.Errorf("release %s has no compose.yaml: %w", version, err)
	}
	if rel.ComposeSHA256 != "" && sha256Hex(composeYAML) != rel.ComposeSHA256 {
		return nil, fmt.Errorf("release %s: compose.yaml does not match its recorded sha256", version)
	}
	planJSON, err := h.ReadFile(ctx, path.Join(relDir, "plan.json"), false)
	if err != nil {
		return nil, fmt.Errorf("release %s has no plan.json: %w", version, err)
	}
	var plans []servicePlan
	if err := json.Unmarshal(planJSON, &plans); err != nil {
		return nil, fmt.Errorf("release %s: bad plan.json: %w", version, err)
	}
	if rel.SecretsGeneration != "" {
		if err := h.Run(ctx, remote.Cmd{Script: "test -d " + remote.Quote(release.SecretsDir(d.App, d.Destination, rel.SecretsGeneration))}); err != nil {
			return nil, fmt.Errorf("release %s: secrets generation %s is gone", version, rel.SecretsGeneration)
		}
	}
	prev, _ := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	prevVersion := path.Base(strings.TrimSpace(prev))

	r.logf("rolling back to %s (volumes, data and migrations are not reverted)", version)
	if needsProxy(plans) {
		if err := proxy.Boot(ctx, h, d.Proxy, r.out); err != nil {
			return nil, err
		}
	}
	if err := r.cutover(ctx, plans); err != nil {
		return nil, err
	}

	if prevVersion != "" && prevVersion != "." && prevVersion != version {
		prevPath := path.Join(release.Dir(d.App, d.Destination, prevVersion), "release.json")
		if pr, err := readRelease(ctx, h, prevPath); err == nil {
			pr.Status = "rolled_back"
			_ = writeJSON(ctx, h, prevPath, pr)
		}
	}
	// The rolled-back-to Release becomes the newest, so pruning keeps it.
	rel.Status = "deployed"
	rel.DeployedAt = start
	rel.Performer = d.Performer
	if err := c.finish(ctx, d, r, rel); err != nil {
		return nil, err
	}
	r.logf("rolled back to %s", version)
	return rel, nil
}

// Releases implements plan.Runtime: Releases on the first Server, newest first.
func (Compose) Releases(ctx context.Context, d *plan.Deploy) ([]release.Release, error) {
	if len(d.Servers) == 0 || d.Servers[0].Host == nil {
		return nil, errors.New("no Server")
	}
	return listReleases(ctx, d.Servers[0].Host, d.App, d.Destination)
}

func readRelease(ctx context.Context, h remote.Host, p string) (*release.Release, error) {
	b, err := h.ReadFile(ctx, p, false)
	if err != nil {
		return nil, err
	}
	var rel release.Release
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &rel, nil
}

func listReleases(ctx context.Context, h remote.Host, app, dest string) ([]release.Release, error) {
	relRoot := path.Join(release.AppDir(app, dest), "releases")
	out, err := h.Output(ctx, remote.Cmd{Script: "for f in " + remote.Quote(relRoot) + "/*/release.json; do [ -f \"$f\" ] && cat \"$f\"; done; true"})
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	var rels []release.Release
	dec := json.NewDecoder(strings.NewReader(out))
	for dec.More() {
		var rel release.Release
		if err := dec.Decode(&rel); err != nil {
			return nil, fmt.Errorf("parse releases: %w", err)
		}
		rels = append(rels, rel)
	}
	slices.SortStableFunc(rels, func(a, b release.Release) int { return b.DeployedAt.Compare(a.DeployedAt) })
	return rels, nil
}

// prune keeps the newest RetainReleases Releases (always current) and the
// secrets generations they use or that existing containers still mount.
func prune(ctx context.Context, r *runner, d *plan.Deploy, current string) error {
	retain := d.RetainReleases
	if retain <= 0 {
		retain = defaultRetain
	}
	rels, err := listReleases(ctx, r.h, d.App, d.Destination)
	if err != nil {
		return err
	}
	dir := release.AppDir(d.App, d.Destination)
	keepGen := map[string]bool{}
	var drop []string
	for i, rel := range rels {
		if i < retain || rel.Version == current {
			keepGen[rel.SecretsGeneration] = true
			continue
		}
		drop = append(drop, path.Join(dir, "releases", rel.Version))
	}

	// Containers not recreated by later deploys still bind-mount older
	// generations; deleting those would break their next restart.
	mounts, err := r.h.Output(ctx, remote.Cmd{Script: "ids=$(docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+r.cc.project) + ")\n" +
		`[ -z "$ids" ] || docker inspect -f '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' $ids`})
	if err != nil {
		return err
	}
	secretsRoot := path.Join(dir, "secrets") + "/"
	for _, m := range lines(mounts) {
		if rest, ok := strings.CutPrefix(m, secretsRoot); ok {
			gen, _, _ := strings.Cut(rest, "/")
			keepGen[gen] = true
		}
	}
	gens, err := r.h.Output(ctx, remote.Cmd{Script: "ls -1 " + remote.Quote(path.Join(dir, "secrets")) + " 2>/dev/null || true"})
	if err != nil {
		return err
	}
	for _, g := range lines(gens) {
		if !keepGen[g] {
			drop = append(drop, path.Join(dir, "secrets", g))
		}
	}
	if len(drop) == 0 {
		return nil
	}
	r.logf("pruning %d old release/secrets director(ies)", len(drop))
	return r.h.Run(ctx, remote.Cmd{Script: "rm -rf " + remote.QuoteArgs(drop...)})
}
