package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/hooks"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/transport"
	"github.com/yoho-build/yoho/internal/ui"
	"github.com/yoho-build/yoho/internal/version"
)

func init() {
	extraCommands = append(extraCommands, deployCmd, rollbackCmd, releasesCmd, proxyCmd, appCmds)
}

func performer() string {
	if u, err := user.Current(); err == nil {
		h, _ := os.Hostname()
		return u.Username + "@" + strings.TrimSuffix(h, ".local")
	}
	return "unknown"
}

// connect opens the Destination's Servers.
func (a *app) connect(ctx context.Context) ([]plan.NamedHost, func(), error) {
	var hosts []plan.NamedHost
	closeAll := func() {
		for _, h := range hosts {
			h.Host.Close()
		}
	}
	for _, name := range a.dest.Servers {
		srv := a.cfg.Servers[name]
		step := a.ui.Step(name, "Connect %s", srv.SSH)
		h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
		if err == nil {
			_, err = h.Output(ctx, remote.Cmd{Script: "docker version --format '{{.Server.Version}}'"})
		}
		if err != nil {
			hint := "check `" + sshCommand(srv.SSH) + "` works without a password and the user can run docker (run `yoho setup`)"
			if a.dest.SSH != nil {
				hint += "; through Cloudflare, log in to Cloudflare Access or set TUNNEL_SERVICE_TOKEN_ID/TUNNEL_SERVICE_TOKEN_SECRET"
			}
			if ce, ok := remote.AsConnectError(err); ok {
				hint = ce.Hint
			}
			step.Fail(err, hint)
			closeAll()
			return nil, nil, &silentError{err}
		}
		step.Done()
		hosts = append(hosts, plan.NamedHost{Name: name, Server: srv, Host: h})
	}
	return hosts, closeAll, nil
}

func (a *app) runtime() plan.Runtime {
	if a.dest.Runtime == "swarm" && swarmRuntime != nil {
		return swarmRuntime(false)
	}
	return deploy.Compose{}
}

// tunnelOptions: on Swarm the swarm runner owns the attachable overlay
// network, so the tunnel path must not create a node-local `yoho` bridge.
func (a *app) tunnelOptions() proxy.BootOptions {
	return proxy.BootOptions{SkipNetwork: a.dest.Runtime == "swarm", Owner: proxy.TunnelOwner(a.cfg.App, a.destName)}
}

// swarmRuntime is set by the swarm package wiring when available.
var swarmRuntime func(lockHeld bool) plan.Runtime

func (a *app) hookFunc(version, command string, hosts []plan.NamedHost) plan.HookFunc {
	dir := a.cfg.Hooks.Path
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.dir, dir)
	}
	return hooks.New(dir, map[string]string{
		"YOHO_APP":         a.cfg.App,
		"YOHO_DESTINATION": a.destName,
		"YOHO_VERSION":     version,
		"YOHO_HOSTS":       hooks.Hosts(hosts),
		"YOHO_COMMAND":     command,
		"YOHO_PERFORMER":   performer(),
	}, a.ui.Progress())
}

func deployCmd(g *globals) *cobra.Command {
	var o applyOptions
	c := &cobra.Command{
		Use:   "deploy",
		Short: "Build, ship and deploy the App with zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if a.scoped {
				if o.Refresh || o.Scan {
					return a.fail(&hintError{"--refresh and --scan require operator access", "deploy with operator access"})
				}
				if a.dest.Migrate != nil {
					return a.fail(&hintError{"destinations." + a.destName + ".migrate needs operator access: the cutover stops containers outside the App", "deploy with operator access, or remove the migrate block once `yoho migrate status` shows done"})
				}
				return a.scopedDeploy(cmd.Context(), o.Version, o.SkipBuild)
			}
			return a.deploy(cmd.Context(), o)
		},
	}
	c.Flags().StringVar(&o.Version, "version", "", "Version to deploy (default: git SHA)")
	c.Flags().BoolVar(&o.SkipBuild, "skip-build", false, "Use images already built for this version")
	addBuildFlags(c, &o)
	addCutoverFlags(c, &o)
	addLockWaitFlag(c)
	return c
}

// deploy is `yoho deploy`: apply --auto-approve without printing the plan.
func (a *app) deploy(ctx context.Context, o applyOptions) error {
	o.AutoApprove, o.ShowPlan = true, false
	return a.apply(ctx, o)
}

// session is everything one deploy or plan run needs: the loaded compose
// project, resolved secrets and open Servers.
type session struct {
	a          *app
	ver        string
	r          *composefile.Result
	store      *secrets.Store
	hosts      []plan.NamedHost
	hook       plan.HookFunc
	closeHosts func()
	red        interface{ Flush() error }
}

func (s *session) close() {
	if s.closeHosts != nil {
		s.closeHosts()
	}
	if s.red != nil {
		s.red.Flush()
	}
}

// resolveDeployVersion returns ver, or the git version of the App directory.
func (a *app) resolveDeployVersion(ver string) (string, error) {
	if ver != "" {
		return ver, nil
	}
	v, err := version.FromGit(a.dir)
	if err != nil {
		return "", fmt.Errorf("%w\nhint: commit your work or pass --version", err)
	}
	return v, nil
}

// openSession loads compose, resolves secrets and connects. readOnly skips
// the pre-connect Hook (plan must not run user scripts that deploy).
func (a *app) openSession(ctx context.Context, ver string, readOnly, refresh bool) (_ *session, err error) {
	u := a.ui
	step := u.Step("", "Load compose")
	r, err := a.compose(ctx)
	if err != nil {
		step.Fail(err, "run `yoho config check`")
		return nil, &silentError{err}
	}
	if refresh {
		hasBuild := false
		for _, svc := range r.Project.Services {
			hasBuild = hasBuild || svc.Build != nil
		}
		if !hasBuild {
			err = errors.New("nothing to refresh: no Service has build:")
			step.Fail(err, "add build: to a Service, or deploy without --refresh")
			return nil, &silentError{err}
		}
	}
	keys, _ := a.secretKeys(r)
	findings := composefile.Check(r, a.dest.Runtime, keys)
	findings = append(findings, composefile.CheckCloudflareSSH(a.destName, a.dest, r.Project)...)
	findings = append(findings, composefile.CheckBackupCoverage(a.destName, a.cfg.Backups, r)...)
	for _, f := range findings {
		u.Finding(f.Level, f.Service, f.Message)
	}
	if composefile.HasErrors(findings) {
		err = errors.New("compose check failed")
		step.Fail(err, "fix the errors above, then `yoho config check`")
		return nil, &silentError{err}
	}
	step.Done(ui.Count(len(r.Project.Services), "service", ""))

	step = u.Step("", "Resolve secrets")
	store, err := a.loadSecrets(ctx, step)
	if err != nil {
		step.Fail(err, secretsHint(err, "check .yoho/secrets and that op / bw / bws are signed in"))
		return nil, &silentError{err}
	}
	step.Done(ui.Count(len(store.Keys()), "key", ""))
	s := &session{a: a, ver: ver, r: r, store: store, red: store.Redactor(os.Stdout)}

	if !readOnly {
		if err = a.hookFunc(ver, "deploy", nil)(ctx, "pre-connect", nil); err != nil {
			s.close()
			return nil, err
		}
	}
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		s.close()
		return nil, err
	}
	s.hosts, s.closeHosts = hosts, closeHosts
	if !readOnly {
		s.hook = a.hookFunc(ver, "deploy", hosts)
	}
	return s, nil
}

// buildAndShip builds (unless skipBuild) and ships images, returning
// Service -> image reference for Services with a build section.
func (a *app) buildAndShip(ctx context.Context, s *session, o applyOptions) (map[string]string, error) {
	u := a.ui
	images := map[string]string{}
	if o.SkipBuild {
		for name, svc := range s.r.Project.Services {
			if svc.Build != nil {
				images[name] = build.ImageName(a.cfg.Registry, a.cfg.App, a.destName, name, s.ver)
			}
		}
		return images, nil
	}
	hosts, store, r, ver, hook := s.hosts, s.store, s.r, s.ver, s.hook
	var built []string
	engine := ""
	platform, err := deploymentPlatform(ctx, hosts, a.cfg.Builder.Platforms)
	if err != nil {
		return nil, err
	}
	// The Destination lock is held from before the build (deployLock).
	if err = hook(ctx, "pre-build", map[string]string{"YOHO_LOCK": hooks.LockHeld}); err != nil {
		return nil, err
	}
	bargs, benv, berr := store.BuildxArgs(a.cfg.Builder.Secrets)
	if berr != nil {
		return nil, berr
	}
	if o.Refresh {
		u.Info("Refreshing base images (pull, no cache)")
		u.Event("deploy.refresh", "", map[string]any{"refresh": true})
	}
	step := u.Step("", "Build images for %s", platform)
	if eng, why := build.ResolveEngine(a.cfg.Builder, runtime.GOOS, runtime.GOARCH, exec.LookPath, nil); a.cfg.Builder.Location == "" || a.cfg.Builder.Location == "local" {
		u.Info("  engine: %s (%s)", eng, why)
	}
	platforms := a.cfg.Builder.Platforms
	if len(platforms) == 0 {
		platforms = []string{platform}
	}
	var builder *remote.SSH
	var derr error
	var cacheTo []string
	hasBuild := false
	for _, svc := range r.Project.Services {
		if svc.Build != nil {
			hasBuild = true
			cacheTo = append(cacheTo, svc.Build.CacheTo...)
		}
	}
	if hasBuild {
		builder, derr = a.directBuilder(ctx, hosts, platforms, cacheTo)
	}
	if derr != nil {
		step.Fail(derr, "run `yoho builder check` for the Builder setup steps, or set builder.direct: auto")
		return nil, &silentError{derr}
	}
	if builder != nil {
		defer builder.Close()
	}
	if a.cfg.Builder.Location == "remote" && builder == nil {
		u.Info("  remote builder: each image is downloaded to this machine after it builds")
	}
	live := &buildLive{u: u, redact: store.RedactString}
	var progress func(build.BuildProgress)
	if !u.Verbose {
		progress = live.progress
	}
	imgs, berr := build.Images(ctx, build.Options{
		Refresh: o.Refresh, Dir: a.dir, App: a.cfg.App, Destination: a.destName, Version: ver,
		Project: r.Project, Builder: a.cfg.Builder, Registry: a.cfg.Registry, Direct: builder != nil,
		Platforms: platforms, BuildEnv: benv, BuildSecretArgs: bargs,
		Out: store.Redactor(step.Output()), Host: hosts[0].Host,
		OnSource: func(src build.SourceSummary) {
			u.Info("  source: %s (%d files, %s)", src.Root, src.Files, humanBytes(src.Bytes))
		},
		OnBuild:  live.started,
		OnBuilt:  live.built,
		Progress: progress,
	})
	if berr != nil {
		step.Fail(berr, "rerun with -v to see the full build output")
		return nil, &silentError{berr}
	}
	for svc, im := range imgs {
		images[svc] = im.Ref
		if im.Built {
			built = append(built, im.Ref)
			engine = im.Engine
		}
	}
	sort.Strings(built)
	step.Done(strings.Join(built, ", "))
	if o.Scan && len(built) > 0 {
		switch {
		case a.cfg.Builder.Location == "server":
			u.Info("scan skipped: images are built on the Server")
		case builder != nil:
			u.Info("scan skipped: images remain on the remote Builder")
		default:
			build.ScanImages(ctx, built, engine, platform, u, nil)
		}
	}

	if len(built) > 0 && a.cfg.Builder.Location == "server" && len(hosts) > 1 {
		// The image exists only on hosts[0]; the other Servers (Swarm workers,
		// extra compose Servers) need it too.
		step = u.Step("", "Ship %d image(s) from %s to %d other Server(s)", len(built), hosts[0].Name, len(hosts)-1)
		var others []remote.Host
		for _, h := range hosts[1:] {
			others = append(others, h.Host)
		}
		shipped, serr := build.ShipFromServer(ctx, hosts[0].Host, others, built, step.Output())
		if serr != nil {
			step.Fail(serr, "check Docker on the other Servers, or build with builder.location=local")
			return nil, &silentError{serr}
		}
		step.Done(fmt.Sprintf("%d loaded, %d already present", len(shipped), len(others)-len(shipped)))
	}
	if len(built) > 0 && builder != nil {
		step = u.Step("", "Ship %d image(s) from %s", len(built), a.cfg.Builder.Remote)
		var hs []remote.Host
		for _, h := range hosts {
			hs = append(hs, h.Host)
		}
		dres, serr := build.ShipDirect(ctx, builder, hs, built, platform, step.Output())
		if serr != nil {
			step.Fail(serr, "check that the Builder can ssh to each Server, or set builder.direct: false")
			return nil, &silentError{serr}
		}
		detail, fields := shipSummary(directResults(dres))
		step.DoneWith(detail, fields)
	} else if len(built) > 0 && a.cfg.Builder.Location != "server" {
		step = u.Step("", "Ship %d image(s)", len(built))
		var regPW string
		if a.cfg.Registry != nil && a.cfg.Registry.PasswordSecret != "" {
			regPW, _ = store.Get(a.cfg.Registry.PasswordSecret)
		}
		var hs []remote.Host
		for _, h := range hosts {
			hs = append(hs, h.Host)
		}
		res, perr := transport.Push(ctx, transport.PushOptions{
			Images: built, Hosts: hs, Mode: a.cfg.Transport.Mode, Registry: a.cfg.Registry,
			RegistryPassword: regPW, Out: step.Output(), Platform: platform, Engine: engine,
		})
		if perr != nil {
			step.Fail(perr, "try `transport: {mode: load}` or configure a registry")
			return nil, &silentError{perr}
		}
		detail, fields := shipSummary(res)
		step.DoneWith(detail, fields)
	}
	return images, nil
}

// directBuilder decides builder.direct for a location=remote build before
// it starts (build.UseDirect) and returns the connected Builder when images
// stay there and stream to the Servers; nil means they come back to this
// machine and ship from here. It errors only for builder.direct: true.
func (a *app) directBuilder(ctx context.Context, hosts []plan.NamedHost, platforms, cacheTo []string) (*remote.SSH, error) {
	b := a.cfg.Builder
	if b.Location != "remote" || b.Direct.Mode() == config.DirectOff {
		return nil, nil
	}
	// The Builder is reached directly, never through the Destination's
	// Cloudflare Tunnel.
	h, err := remote.NewSSH("builder", b.Remote, false, remote.SSHOptions{})
	if err != nil {
		return nil, err
	}
	var dsts []remote.Host
	for _, nh := range hosts {
		dsts = append(dsts, nh.Host)
	}
	use, reason, err := build.UseDirect(ctx, b, a.cfg.Transport.Mode, platforms, cacheTo, h, dsts)
	if !use {
		h.Close()
		if reason != "" {
			more := ""
			if build.IsProbeReason(reason) {
				more = "; run `yoho builder check` for setup steps"
			}
			a.ui.Info("  direct ship off (%s); images come back through this machine%s", reason, more)
		}
		return nil, err
	}
	a.ui.Info("  direct ship: images stay on %s and stream to the Servers", b.Remote)
	return h, nil
}

// directResults maps ShipDirect outcomes to Ship step results.
func directResults(dres []build.DirectResult) []transport.Result {
	res := make([]transport.Result, len(dres))
	for i, d := range dres {
		res[i] = transport.Result{Host: d.Host, Method: transport.MethodDirect, Bytes: d.Stats.Bytes, Duration: d.Stats.Duration}
		if d.Skipped {
			res[i].Method = transport.MethodSkip
		}
	}
	return res
}

// shipSummary renders the Ship step result: per Server the method, and for
// transfers that sent bytes, the size, duration and throughput so slow links
// are visible. fields["transfers"] lists the same for --json.
func shipSummary(res []transport.Result) (string, map[string]any) {
	var parts []string
	transfers := make([]map[string]any, 0, len(res))
	for _, r := range res {
		part := r.Host + " via " + r.Method
		if r.Bytes > 0 && r.Duration > 0 {
			part += ", " + humanBytes(r.Bytes) + " in " + roundDur(r.Duration) +
				" (" + humanBytes(int64(float64(r.Bytes)/r.Duration.Seconds())) + "/s)"
		}
		parts = append(parts, part)
		transfers = append(transfers, map[string]any{"host": r.Host, "method": r.Method, "bytes": r.Bytes, "duration_ms": r.Duration.Milliseconds()})
	}
	return strings.Join(parts, ", "), map[string]any{"transfers": transfers}
}

func roundDur(d time.Duration) string {
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// deploymentPlatform selects the image to ship from the Servers, not the order
// of builder.platforms. Transport currently ships one platform to all Servers.
func deploymentPlatform(ctx context.Context, hosts []plan.NamedHost, platforms []string) (string, error) {
	if len(hosts) == 0 {
		return "", errors.New("cannot select a deployment platform without a Server")
	}
	platform, err := build.ServerPlatform(ctx, hosts[0].Host)
	if err != nil {
		return "", err
	}
	for _, h := range hosts[1:] {
		p, err := build.ServerPlatform(ctx, h.Host)
		if err != nil {
			return "", err
		}
		if p != platform {
			return "", fmt.Errorf("Servers have different platforms: %s is %s, %s is %s; use separate Destinations for each architecture", hosts[0].Name, platform, h.Name, p)
		}
	}
	return pickPlatform(platform, platforms)
}

// pickPlatform selects the builder platform for a Server's platform.
func pickPlatform(platform string, platforms []string) (string, error) {
	if len(platforms) == 0 {
		return platform, nil
	}
	for _, p := range platforms {
		if p == platform || strings.HasPrefix(p, platform+"/") {
			return p, nil
		}
	}
	return "", fmt.Errorf("builder.platforms [%s] does not include Server platform %s; add %s or omit builder.platforms to detect it automatically", strings.Join(platforms, ", "), platform, platform)
}

// newDeploy builds the plan.Deploy for the session. forPlan works on a copy
// of the project (images set, build sections removed) and discards output, so
// the session's project can still be built afterwards.
func (a *app) newDeploy(s *session, images map[string]string, forPlan bool) (*plan.Deploy, error) {
	proj := s.r.Project
	var out io.Writer = io.Discard
	if forPlan {
		var err error
		if proj, err = proj.WithServicesTransform(func(_ string, sc types.ServiceConfig) (types.ServiceConfig, error) { return sc, nil }); err != nil {
			return nil, err
		}
	} else {
		out = s.store.Redactor(a.ui.Progress())
	}
	// Built are the Services Yoho built from a build: section; images also
	// holds the pulled image-only Services, which rollback must never retag.
	built := builtServices(proj, images)
	composefile.StripBuild(proj, images)
	svcSecrets, refs, err := serviceSecrets(s.store, s.r)
	if err != nil {
		return nil, err
	}
	return &plan.Deploy{
		App: a.cfg.App, Destination: a.destName, Version: s.ver, Performer: performer(),
		Servers: s.hosts, Project: proj, Ext: s.r.Ext, Built: built,
		ServiceSecrets: svcSecrets, SecretRefs: refs, Env: a.dest.Env, Subnet: a.dest.Subnet, Registry: a.cfg.Registry,
		Proxy: a.proxyConfig(), RetainReleases: a.cfg.RetainReleases, Hook: s.hook,
		Out: out,
	}, nil
}

// builtServices lists, sorted and never nil, the Services of p that have a
// build section and an image in images. Call it before StripBuild.
func builtServices(p *types.Project, images map[string]string) []string {
	out := []string{}
	for name, sc := range p.Services {
		if sc.Build != nil && images[name] != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// rollout runs the runtime's Deploy for d.
func (a *app) rollout(ctx context.Context, s *session, d *plan.Deploy, runtime plan.Runtime) (*release.Release, error) {
	step := a.ui.Step(s.hosts[0].Name, "Deploy %s (%s runtime)", shortVersion(s.ver), runtimeName(a.dest))
	rel, err := runtime.Deploy(ctx, d)
	if f, ok := d.Out.(interface{ Flush() error }); ok {
		f.Flush()
	}
	if err != nil {
		step.Fail(err, a.rolloutHint(ctx, err, "inspect per-Service failure facts, `yoho app ps`, and `yoho releases`; Services may already have changed; no automatic data/migration rollback"))
		return nil, &silentError{err}
	}
	step.Done()
	return rel, nil
}

// reconcileTunnel converges the Cloudflare Tunnel to config: ensure when
// configured, remove a leftover connector when it no longer is. Returns URL
// hints.
func (a *app) reconcileTunnel(ctx context.Context, s *session) (string, error) {
	u := a.ui
	t := a.proxyConfig().Tunnel
	owner := proxy.TunnelOwner(a.cfg.App, a.destName)
	urls := ""
	for _, h := range s.hosts {
		if t == nil {
			st, err := proxy.TunnelStatusOf(ctx, h.Host)
			if err != nil || !st.Exists() {
				continue
			}
			// The connector is per Server and shared by Apps: only remove one
			// this App Destination deployed.
			if !st.OwnedBy(owner) {
				u.Info("  Cloudflare Tunnel on %s is not managed by %s; leaving it (remove with `yoho tunnel down`)", h.Name, owner)
				continue
			}
			step := u.Step(h.Name, "Remove Cloudflare Tunnel (no longer in config)")
			if _, err := proxy.RemoveTunnelOwned(ctx, h.Host, owner, step.Output()); err != nil {
				step.Fail(err, "run `yoho tunnel down`")
				return urls, &silentError{err}
			}
			step.Done()
			continue
		}
		token, terr := tunnelToken(s.store, t)
		if terr != nil {
			// Leave the running connectors alone.
			return urls, terr
		}
		step := u.Step(h.Name, "Cloudflare Tunnel")
		st, terr := proxy.EnsureTunnelWith(ctx, h.Host, *t, token, step.Output(), a.tunnelOptions())
		if terr != nil {
			step.Fail(terr, "the App is deployed; fix the tunnel and run `yoho tunnel up`")
			return urls, &silentError{terr}
		}
		if st.URL != "" {
			step.Done(st.URL)
			urls += "\n  → " + st.URL + " (Quick Tunnel)"
		} else {
			step.Done("token tunnel → " + proxy.TunnelOrigin)
		}
	}
	return urls, nil
}

// tunnelToken resolves proxy.tunnel.token_secret. A configured but missing or
// empty secret is an error: an empty token would otherwise mean "Quick
// Tunnel" and could replace a managed connector with a public URL.
func tunnelToken(store *secrets.Store, t *config.TunnelConfig) (string, error) {
	if t == nil || t.TokenSecret == "" {
		return "", nil
	}
	var token string
	var ok bool
	if store != nil {
		token, ok = store.Get(t.TokenSecret)
	}
	if !ok || strings.TrimSpace(token) == "" {
		reason := "not found"
		if ok {
			reason = "is empty"
		}
		return "", fmt.Errorf("proxy.tunnel.token_secret %s %s\nhint: add %s to .yoho/secrets (the token from Cloudflare Zero Trust > Networks > Tunnels), or fix the secret name", t.TokenSecret, reason, t.TokenSecret)
	}
	return token, nil
}

// serviceSecrets resolves each Service's declared secrets from the store.
// Keys generated on the Server are left for the runtime to fill in.
func serviceSecrets(store *secrets.Store, r *composefile.Result) (map[string]map[string]string, map[string]string, error) {
	generated := map[string]bool{}
	for _, ext := range r.Ext {
		for k := range ext.Generate {
			generated[k] = true
		}
	}
	out := map[string]map[string]string{}
	refs := map[string]string{}
	for svc, ext := range r.Ext {
		var local config.SecretRefs
		for _, ref := range ext.Secrets {
			if _, ok := store.Get(ref.Key); !ok && generated[ref.Key] {
				continue
			}
			local = append(local, ref)
			refs[ref.Name] = store.Ref(ref.Key)
		}
		m, err := store.ForService(local)
		if err != nil {
			return nil, nil, fmt.Errorf("service %s: %w", svc, err)
		}
		if len(m) > 0 {
			out[svc] = m
		}
	}
	return out, refs, nil
}

func (a *app) proxyConfig() config.ProxyConfig {
	p := a.cfg.Proxy
	if a.dest.Proxy != nil {
		o := *a.dest.Proxy
		if o.Image != "" {
			p.Image = o.Image
		}
		if o.HTTPPort != nil {
			p.HTTPPort = o.HTTPPort
		}
		if o.HTTPSPort != nil {
			p.HTTPSPort = o.HTTPSPort
		}
		if o.Bind != "" {
			p.Bind = o.Bind
		}
		if o.Metrics {
			p.Metrics = true
		}
		if o.Tunnel != nil {
			p.Tunnel = o.Tunnel
		}
	}
	return p
}

// urls returns a hint with where proxied Services answer.
func (a *app) urls(r *composefile.Result, hosts []plan.NamedHost) string {
	var urls []string
	p := a.proxyConfig()
	for _, name := range sortedKeys(r.Ext) {
		ext := r.Ext[name]
		if ext.Proxy == nil {
			continue
		}
		scheme := "http"
		if ext.Proxy.TLS {
			scheme = "https"
		}
		if len(ext.Proxy.Hosts) > 0 {
			for _, h := range ext.Proxy.Hosts {
				urls = append(urls, scheme+"://"+h)
			}
			continue
		}
		port := 80
		if p.HTTPPort != nil {
			port = *p.HTTPPort
		}
		if port == 0 {
			urls = append(urls, "http://"+proxy.ContainerName+":80 (Server-internal, e.g. Cloudflare Tunnel)")
			continue
		}
		_, host, _, _ := remote.ParseTarget(hosts[0].Server.SSH)
		u := "http://" + host
		if port != 80 {
			u += fmt.Sprintf(":%d", port)
		}
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return ""
	}
	return "\n  → " + strings.Join(urls, "\n  → ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func shortVersion(v string) string { return version.Short(v) }

func runtimeName(d config.Destination) string {
	if d.Runtime == "" {
		return "compose"
	}
	return d.Runtime
}

func rollbackCmd(g *globals) *cobra.Command {
	var cutover, force, releaseSecrets bool
	c := &cobra.Command{
		Use:   "rollback VERSION | rollback --cutover [--force]",
		Short: "Redeploy a previous Release with zero downtime (volumes and migrations are not reverted)",
		Long: `Redeploy a previous Release with zero downtime: its images and compiled
compose. Volumes and database migrations are not reverted.

The Services keep the current secret values (the current Release's, and
generated values as they are now), so a rotated or leaked secret never
comes back. A rollback refuses when the Release reads a secret the current
Release does not have. --with-release-secrets runs the Release's own
secrets generation instead: values rotated since then come back.

With --cutover, undo the Destination's migration cutover instead: stop this
App's containers (never removed, volumes kept), start the old containers it
stopped, and restore the saved Cloudflare Tunnel ingress. It refuses when the
ingress changed since the cutover, unless --force.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cutover {
				return cobra.NoArgs(cmd, args)
			}
			if force {
				return errors.New("--force applies to --cutover only")
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if cutover && releaseSecrets {
				return errors.New("--with-release-secrets does not apply to --cutover")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if cutover {
				if a.scoped {
					return a.fail(&hintError{"rollback --cutover needs operator access", "ask the operator to run it"})
				}
				return a.rollbackCutover(ctx, force)
			}
			if a.scoped {
				if releaseSecrets {
					return a.fail(&hintError{"--with-release-secrets needs operator access", "roll back without it (the current secrets), or ask the operator"})
				}
				return a.scopedRollback(ctx, args[0])
			}
			return a.rollback(ctx, args[0], releaseSecrets)
		},
	}
	c.Flags().BoolVar(&cutover, "cutover", false, "Undo the migration cutover (start the old containers, restore the tunnel ingress)")
	c.Flags().BoolVar(&force, "force", false, "With --cutover: restore the saved ingress even when it changed since the cutover")
	c.Flags().BoolVar(&releaseSecrets, "with-release-secrets", false, "Run the Release's own secrets generation instead of the current values (rotated secrets come back)")
	addLockWaitFlag(c)
	return c
}

// rollback is `yoho rollback VERSION`: under the Destination lock, the
// runtime redeploys the Release with the current secrets (or, with
// releaseSecrets, its own).
func (a *app) rollback(ctx context.Context, v string, releaseSecrets bool) (err error) {
	// Kamal runs pre-connect for rollback too; the Version is the one asked
	// for (it is resolved once connected). No lock is held yet.
	if err := a.hookFunc(v, "rollback", nil)(ctx, "pre-connect", nil); err != nil {
		return err
	}
	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	// Converge (or resume a migration of) the App directory first: the
	// Release lookup reads it through the canonical path.
	if err := a.ensureAppDirs(ctx, hosts); err != nil {
		a.ui.Finished(err, "")
		return err
	}
	d := &plan.Deploy{App: a.cfg.App, Destination: a.destName, Performer: performer(), Servers: hosts,
		Proxy: a.proxyConfig(), Registry: a.cfg.Registry, RetainReleases: a.cfg.RetainReleases, Out: a.ui.Progress(),
		RollbackReleaseSecrets: releaseSecrets}
	target, err := a.resolveVersion(ctx, d, v)
	if err != nil {
		return err
	}
	// The runtime hooks run under the lock taken below.
	d.Hook = a.hookFunc(target, "rollback", hosts)
	a.ui.Title("Rolling back %s on %s to %s", a.cfg.App, a.destName, shortVersion(target))
	step := a.ui.Step(hosts[0].Name, "Acquire lock")
	unlock, err := a.acquireLock(ctx, hosts[0].Host, deploy.LockInfo{Version: target, Command: "rollback"})
	if err != nil {
		step.Fail(err, hintFor(err, "check that the Server is reachable"))
		a.ui.Finished(err, "")
		return &silentError{err}
	}
	step.Done()
	defer func() {
		if uerr := unlock(context.WithoutCancel(ctx)); uerr != nil && err == nil {
			err = uerr
		}
	}()
	a.ui.Warn("volumes and database migrations are not reverted")
	if releaseSecrets {
		a.ui.Warn("--with-release-secrets: the Services start with Release %s's own secret values; every secret rotated since (a replaced API key, a leaked password) comes back until the next deploy", shortVersion(target))
	}
	for _, w := range deploy.RollbackWarnings(ctx, hosts[0].Host, a.cfg.App, a.destName, target, releaseSecrets) {
		a.ui.Warn("%s", w)
	}
	step = a.ui.Step(hosts[0].Name, "Redeploy %s", shortVersion(target))
	rel, err := a.lockedRuntime().Rollback(ctx, d, target)
	if err != nil {
		step.Fail(err, hintFor(err, "see `yoho releases`"))
		a.ui.Finished(err, "")
		return &silentError{err}
	}
	detail := "current secrets"
	if rel.RollbackSecrets == deploy.RollbackSecretsRelease {
		detail = "the Release's own secrets"
	}
	if rel.SecretsGeneration != "" {
		detail += ", generation " + rel.SecretsGeneration
	}
	step.DoneWith(detail, map[string]any{"rollback_secrets": rel.RollbackSecrets, "secrets_generation": rel.SecretsGeneration, "secrets_from": rel.SecretsFrom})
	a.ui.Finished(nil, "Rolled back to "+shortVersion(rel.Version))
	return nil
}

// resolveVersion accepts a full version or a unique prefix.
func (a *app) resolveVersion(ctx context.Context, d *plan.Deploy, v string) (string, error) {
	rels, err := a.runtime().Releases(ctx, d)
	if err != nil {
		return "", err
	}
	versions := make([]string, len(rels))
	for i, r := range rels {
		versions[i] = r.Version
	}
	got, err := matchVersion(versions, v)
	if err != nil {
		return "", fmt.Errorf("%w on %s; see `yoho releases`", err, a.destName)
	}
	return got, nil
}

// matchVersion resolves v against versions: exact full match, then exact
// displayed short form, then unique prefix.
func matchVersion(versions []string, v string) (string, error) {
	for _, x := range versions {
		if x == v {
			return x, nil
		}
	}
	var short, prefix []string
	for _, x := range versions {
		if shortVersion(x) == v {
			short = append(short, x)
		}
		if strings.HasPrefix(x, v) {
			prefix = append(prefix, x)
		}
	}
	for _, m := range [][]string{short, prefix} {
		if len(m) == 1 {
			return m[0], nil
		}
		if len(m) > 1 {
			disp := make([]string, len(m))
			for i, x := range m {
				disp[i] = shortVersion(x)
			}
			return "", fmt.Errorf("version prefix %q is ambiguous: %s", v, strings.Join(disp, ", "))
		}
	}
	return "", fmt.Errorf("no Release %q", v)
}

func releasesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "releases",
		Short: "List Releases on the Server, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var rels []release.Release
			if a.scoped {
				if err := a.gateCall(ctx, "releases", nil, nil, nil, &rels); err != nil {
					return err
				}
			} else {
				hosts, closeHosts, err := a.connect(ctx)
				if err != nil {
					return err
				}
				defer closeHosts()
				if rels, err = a.runtime().Releases(ctx, &plan.Deploy{App: a.cfg.App, Destination: a.destName, Servers: hosts, Out: io.Discard}); err != nil {
					return err
				}
			}
			var rows [][]string
			cutover := false
			for _, r := range rels {
				cutover = cutover || r.Cutover != ""
			}
			for i, r := range rels {
				cur := ""
				if i == 0 && r.Status == "deployed" {
					cur = "*"
				}
				status := r.Status
				if r.Refreshed {
					status += " refreshed"
				}
				row := []string{cur, shortVersion(r.Version), status, r.DeployedAt.Local().Format("2006-01-02 15:04:05"), r.Performer, r.SecretsGeneration}
				if cutover {
					row = append(row, r.Cutover)
				}
				rows = append(rows, row)
			}
			header := []string{"CURRENT", "VERSION", "STATUS", "DEPLOYED", "BY", "SECRETS"}
			if cutover {
				header = append(header, "CUTOVER")
			}
			a.ui.Table(header, rows)
			return nil
		},
	}
}

// proxyHook runs `yoho proxy boot` Hooks: YOHO_COMMAND=proxy,
// YOHO_SUBCOMMAND=boot, without the Destination lock.
func (a *app) proxyHook(version string, hosts []plan.NamedHost) func(context.Context, string) error {
	return a.proxyHookFor("boot", version, hosts)
}

func proxyCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "proxy", Short: "Manage the shared kamal-proxy on the Server"}
	c.AddCommand(&cobra.Command{
		Use:   "boot",
		Short: "Start (or reconfigure) the Proxy",
		Long: `Start (or reconfigure) the Proxy on each Server, one Server at a time.

The pre-proxy-reboot and post-proxy-reboot Hooks run around each Server's
boot (YOHO_COMMAND=proxy, YOHO_SUBCOMMAND=boot, YOHO_HOSTS=that Server),
after pre-connect.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := a.proxyHook("", nil)(ctx, "pre-connect"); err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			for _, h := range hosts {
				hook := a.proxyHook(a.serverVersion(ctx, h.Host), []plan.NamedHost{h})
				if err := hook(ctx, "pre-proxy-reboot"); err != nil {
					return err
				}
				step := a.ui.Step(h.Name, "Boot %s", proxy.ContainerName)
				if err := proxy.Boot(ctx, h.Host, a.proxyConfig(), step.Output()); err != nil {
					step.Fail(err, "check that the Proxy ports are free on the Server")
					return &silentError{err}
				}
				step.Done()
				if err := hook(ctx, "post-proxy-reboot"); err != nil {
					return err
				}
			}
			return nil
		},
	})
	c.AddCommand(proxyUpgradeCmd(g), proxyStatusCmd(g))
	c.AddCommand(remoteShow(g, "logs", "Show recent Proxy logs", "docker logs --tail 200 "+proxy.ContainerName+" 2>&1"))
	return c
}

// appCmds adds ps / logs / stats / exec / run.
func appCmds(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Inspect the running App (ps, logs, stats, exec, run)"}
	c.AddCommand(&cobra.Command{
		Use: "ps", Short: "List the App's containers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if a.scoped {
				return a.gateCall(cmd.Context(), "ps", nil, nil, cmd.OutOrStdout(), nil)
			}
			if a.dest.Runtime == "swarm" {
				return a.remoteRun(cmd, "docker stack ps --no-trunc --format 'table {{.Name}}\\t{{.Node}}\\t{{.CurrentState}}\\t{{.Image}}\\t{{.Error}}' "+remote.Quote(a.project()))
			}
			return a.remoteRun(cmd, "docker compose -p "+remote.Quote(a.project())+" ps --format 'table {{.Service}}\\t{{.Name}}\\t{{.Status}}\\t{{.Image}}'")
		},
	})
	c.AddCommand(logsCmd(g), statsCmd(g))
	var execTerm termOptions
	execCmd := &cobra.Command{
		Use: "exec SERVICE [-i] [-t] -- COMMAND...", Short: "Run a command in a running Service container (-it for a console)",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			names, derr := a.serviceNames(cmd.Context())
			if derr != nil {
				a.ui.Warn("cannot list the App's Services, not checking %s: %v", args[0], derr)
			} else if err := checkServices(names, args[:1]); err != nil {
				return a.fail(err)
			}
			if err := execTerm.check(cmd); err != nil {
				return a.fail(err)
			}
			if a.dest.Runtime == "swarm" {
				err = a.swarmExec(cmd, args[0], args[1:], execTerm)
			} else {
				err = a.runTerminal(cmd, composeExecScript(a.project(), execTerm, args), execTerm)
			}
			return execError(err)
		},
	}
	addTermFlags(execCmd, &execTerm)
	c.AddCommand(execCmd, appRunCmd(g))
	return c
}

// execError is the result of `app exec`: the command's (or docker's) own
// stderr already reached the user, so a failure only carries its exit
// code, like ssh and docker exec. A Server ssh cannot reach still says so.
func execError(err error) error {
	var ee *remote.ExitError
	if _, ok := remote.AsConnectError(err); ok || !errors.As(err, &ee) {
		return err
	}
	return &silentError{&exitError{code: ee.Code, err: err}}
}

// swarmTaskContainer returns the id of a running task container of the stack
// Service on h (the node h connects to), or "".
func swarmTaskContainer(ctx context.Context, h remote.Host, project, svc string) (string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "docker ps -q --filter " + remote.Quote("label=com.docker.swarm.service.name="+project+"_"+svc) + " --filter status=running"})
	if err != nil {
		return "", fmt.Errorf("find %s task on %s: %w", svc, h.Name(), err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(id), nil
}

// swarmExec runs command in a running task container of Service. Swarm tasks
// can run on any node, so the Destination's Servers are tried in order and
// the command runs on the first node that has one.
func (a *app) swarmExec(cmd *cobra.Command, svc string, command []string, term termOptions) error {
	ctx := cmd.Context()
	var tried []string
	for _, name := range a.dest.Servers {
		srv := a.cfg.Servers[name]
		h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
		if err != nil {
			return err
		}
		id, err := swarmTaskContainer(ctx, h, a.project(), svc)
		if err == nil && id != "" {
			defer h.Close()
			script := "docker exec " + swarmExecFlags(term) + remote.Quote(id) + " " + remote.QuoteArgs(command...)
			if term.attached() {
				return commandExit(h.RunTerminal(ctx, script, term.tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()))
			}
			return h.Run(ctx, remote.Cmd{Script: script, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
		}
		h.Close()
		tried = append(tried, name)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("no running task of Service %s on %s\nhint: check `yoho app ps`; a task on a node outside this Destination's Servers cannot be reached", svc, strings.Join(tried, ", "))
}

func remoteShow(g *globals, use, short, script string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, RunE: func(cmd *cobra.Command, _ []string) error {
		a, err := g.load(cmd)
		if err != nil {
			return err
		}
		return a.remoteRun(cmd, script)
	}}
}

func (a *app) remoteRun(cmd *cobra.Command, script string) error {
	srv := a.cfg.Servers[a.dest.Servers[0]]
	h, err := remote.NewSSH(a.dest.Servers[0], srv.SSH, srv.Sudo, a.sshOptions())
	if err != nil {
		return err
	}
	defer h.Close()
	return h.Run(cmd.Context(), remote.Cmd{Script: script, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
}

var _ = release.ProjectName
