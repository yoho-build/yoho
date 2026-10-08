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
		h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, remote.SSHOptions{})
		if err == nil {
			_, err = h.Output(ctx, remote.Cmd{Script: "docker version --format '{{.Server.Version}}'"})
		}
		if err != nil {
			step.Fail(err, "check `ssh "+srv.SSH+"` works without a password and the user can run docker (run `yoho setup`)")
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
		return swarmRuntime()
	}
	return deploy.Compose{}
}

// swarmRuntime is set by the swarm package wiring when available.
var swarmRuntime func() plan.Runtime

func (a *app) hookFunc(version, command string, hosts []plan.NamedHost) plan.HookFunc {
	var hs []string
	for _, h := range hosts {
		hs = append(hs, h.Server.SSH)
	}
	dir := a.cfg.Hooks.Path
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.dir, dir)
	}
	return hooks.New(dir, map[string]string{
		"YOHO_APP":         a.cfg.App,
		"YOHO_DESTINATION": a.destName,
		"YOHO_VERSION":     version,
		"YOHO_HOSTS":       strings.Join(hs, ","),
		"YOHO_COMMAND":     command,
		"YOHO_PERFORMER":   performer(),
	}, a.ui.Progress())
}

func deployCmd(g *globals) *cobra.Command {
	var ver string
	var skipBuild bool
	c := &cobra.Command{
		Use:   "deploy",
		Short: "Build, ship and deploy the App with zero downtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			err = a.deploy(cmd.Context(), ver, skipBuild)
			return err
		},
	}
	c.Flags().StringVar(&ver, "version", "", "Version to deploy (default: git SHA)")
	c.Flags().BoolVar(&skipBuild, "skip-build", false, "Use images already built for this version")
	return c
}

// deploy is `yoho deploy`: apply --auto-approve without printing the plan.
func (a *app) deploy(ctx context.Context, ver string, skipBuild bool) error {
	return a.apply(ctx, applyOptions{Version: ver, SkipBuild: skipBuild, AutoApprove: true})
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
func (a *app) openSession(ctx context.Context, ver string, readOnly bool) (_ *session, err error) {
	u := a.ui
	step := u.Step("", "Load compose")
	r, err := a.compose(ctx)
	if err != nil {
		step.Fail(err, "run `yoho config check`")
		return nil, &silentError{err}
	}
	keys, _ := a.secretKeys(r)
	findings := composefile.Check(r, a.dest.Runtime, keys)
	for _, f := range findings {
		u.Finding(f.Level, f.Service, f.Message)
	}
	if composefile.HasErrors(findings) {
		err = errors.New("compose check failed")
		step.Fail(err, "fix the errors above, then `yoho config check`")
		return nil, &silentError{err}
	}
	step.Done(fmt.Sprintf("%d services", len(r.Project.Services)))

	step = u.Step("", "Resolve secrets")
	store, err := a.loadSecrets(ctx)
	if err != nil {
		step.Fail(err, "check .yoho/secrets and that op / bw / bws are signed in")
		return nil, &silentError{err}
	}
	step.Done(fmt.Sprintf("%d keys", len(store.Keys())))
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
func (a *app) buildAndShip(ctx context.Context, s *session, skipBuild bool) (map[string]string, error) {
	u := a.ui
	images := map[string]string{}
	if skipBuild {
		for name, svc := range s.r.Project.Services {
			if svc.Build != nil {
				images[name] = build.ImageName(a.cfg.Registry, a.cfg.App, name, s.ver)
			}
		}
		return images, nil
	}
	hosts, store, r, ver, hook := s.hosts, s.store, s.r, s.ver, s.hook
	var built []string
	engine := ""
	platform := ""
	var err error
	if len(a.cfg.Builder.Platforms) == 0 {
		if platform, err = build.ServerPlatform(ctx, hosts[0].Host); err != nil {
			return nil, err
		}
	} else {
		platform = a.cfg.Builder.Platforms[0]
	}
	if err = hook(ctx, "pre-build", nil); err != nil {
		return nil, err
	}
	bargs, benv, berr := store.BuildxArgs(a.cfg.Builder.Secrets)
	if berr != nil {
		return nil, berr
	}
	step := u.Step("", "Build images for %s", platform)
	if eng, why := build.ResolveEngine(a.cfg.Builder, runtime.GOOS, runtime.GOARCH, exec.LookPath, nil); a.cfg.Builder.Location == "" || a.cfg.Builder.Location == "local" {
		u.Info("  engine: %s (%s)", eng, why)
	}
	platforms := a.cfg.Builder.Platforms
	if len(platforms) == 0 {
		platforms = []string{platform}
	}
	imgs, berr := build.Images(ctx, build.Options{
		Dir: a.dir, App: a.cfg.App, Destination: a.destName, Version: ver,
		Project: r.Project, Builder: a.cfg.Builder, Registry: a.cfg.Registry,
		Platforms: platforms, BuildEnv: benv, BuildSecretArgs: bargs,
		Out: store.Redactor(step.Output()), Host: hosts[0].Host,
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

	if len(built) > 0 && a.cfg.Builder.Location != "server" {
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
		var methods []string
		for _, r := range res {
			methods = append(methods, r.Host+" via "+r.Method)
		}
		step.Done(strings.Join(methods, ", "))
	}
	return images, nil
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
	composefile.StripBuild(proj, images)
	svcSecrets, refs, err := serviceSecrets(s.store, s.r)
	if err != nil {
		return nil, err
	}
	return &plan.Deploy{
		App: a.cfg.App, Destination: a.destName, Version: s.ver, Performer: performer(),
		Servers: s.hosts, Project: proj, Ext: s.r.Ext,
		ServiceSecrets: svcSecrets, SecretRefs: refs, Env: a.dest.Env, Registry: a.cfg.Registry,
		Proxy: a.proxyConfig(), RetainReleases: a.cfg.RetainReleases, Hook: s.hook,
		Out: out,
	}, nil
}

// rollout runs the runtime's Deploy for d.
func (a *app) rollout(ctx context.Context, s *session, d *plan.Deploy) (*release.Release, error) {
	step := a.ui.Step(s.hosts[0].Name, "Deploy %s (%s runtime)", shortVersion(s.ver), runtimeName(a.dest))
	rel, err := a.runtime().Deploy(ctx, d)
	if f, ok := d.Out.(interface{ Flush() error }); ok {
		f.Flush()
	}
	if err != nil {
		step.Fail(err, "previous Release keeps serving; see `yoho releases` and rerun with -v")
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
	urls := ""
	for _, h := range s.hosts {
		if t == nil {
			st, err := proxy.TunnelStatusOf(ctx, h.Host)
			if err != nil || !st.Exists() {
				continue
			}
			step := u.Step(h.Name, "Remove Cloudflare Tunnel (no longer in config)")
			if err := proxy.RemoveTunnel(ctx, h.Host, step.Output()); err != nil {
				step.Fail(err, "run `yoho tunnel down`")
				return urls, &silentError{err}
			}
			step.Done()
			continue
		}
		token := ""
		if t.TokenSecret != "" {
			token, _ = s.store.Get(t.TokenSecret)
		}
		step := u.Step(h.Name, "Cloudflare Tunnel")
		st, terr := proxy.EnsureTunnel(ctx, h.Host, *t, token, step.Output())
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

func shortVersion(v string) string {
	if i := strings.Index(v, version.UncommittedMarker); i >= 0 {
		base := v[:i]
		if len(base) > 12 {
			base = base[:12]
		}
		return base + v[i:]
	}
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

func runtimeName(d config.Destination) string {
	if d.Runtime == "" {
		return "compose"
	}
	return d.Runtime
}

func rollbackCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rollback VERSION",
		Short: "Redeploy a previous Release with zero downtime (volumes and migrations are not reverted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			d := &plan.Deploy{App: a.cfg.App, Destination: a.destName, Performer: performer(), Servers: hosts,
				Proxy: a.proxyConfig(), RetainReleases: a.cfg.RetainReleases, Out: a.ui.Progress()}
			target, err := a.resolveVersion(ctx, d, args[0])
			if err != nil {
				return err
			}
			a.ui.Title("Rolling back %s on %s to %s", a.cfg.App, a.destName, shortVersion(target))
			a.ui.Warn("volumes and database migrations are not reverted")
			step := a.ui.Step(hosts[0].Name, "Redeploy %s", shortVersion(target))
			rel, err := a.runtime().Rollback(ctx, d, target)
			if err != nil {
				step.Fail(err, "see `yoho releases`")
				a.ui.Finished(err, "")
				return &silentError{err}
			}
			step.Done()
			a.ui.Finished(nil, "Rolled back to "+shortVersion(rel.Version))
			return nil
		},
	}
}

// resolveVersion accepts a full version or a unique prefix.
func (a *app) resolveVersion(ctx context.Context, d *plan.Deploy, v string) (string, error) {
	rels, err := a.runtime().Releases(ctx, d)
	if err != nil {
		return "", err
	}
	var match []string
	for _, r := range rels {
		if r.Version == v {
			return v, nil
		}
		if strings.HasPrefix(r.Version, v) {
			match = append(match, r.Version)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return "", fmt.Errorf("no Release %q on %s; see `yoho releases`", v, a.destName)
	}
	return "", fmt.Errorf("version prefix %q is ambiguous: %s", v, strings.Join(match, ", "))
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
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			rels, err := a.runtime().Releases(ctx, &plan.Deploy{App: a.cfg.App, Destination: a.destName, Servers: hosts, Out: io.Discard})
			if err != nil {
				return err
			}
			var rows [][]string
			for i, r := range rels {
				cur := ""
				if i == 0 && r.Status == "deployed" {
					cur = "*"
				}
				rows = append(rows, []string{cur, shortVersion(r.Version), r.Status, r.DeployedAt.Local().Format("2006-01-02 15:04:05"), r.Performer, r.SecretsGeneration})
			}
			a.ui.Table([]string{"CURRENT", "VERSION", "STATUS", "DEPLOYED", "BY", "SECRETS"}, rows)
			return nil
		},
	}
}

func proxyCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "proxy", Short: "Manage the shared kamal-proxy on the Server"}
	c.AddCommand(&cobra.Command{
		Use:   "boot",
		Short: "Start (or reconfigure) the Proxy",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer closeHosts()
			for _, h := range hosts {
				step := a.ui.Step(h.Name, "Boot %s", proxy.ContainerName)
				if err := proxy.Boot(cmd.Context(), h.Host, a.proxyConfig(), step.Output()); err != nil {
					step.Fail(err, "check that the Proxy ports are free on the Server")
					return &silentError{err}
				}
				step.Done()
			}
			return nil
		},
	})
	c.AddCommand(remoteShow(g, "status", "Show the Proxy container and its routes",
		"docker ps --filter name=^/"+proxy.ContainerName+"$ --format '{{.Names}}\\t{{.Status}}\\t{{.Ports}}'; docker exec "+proxy.ContainerName+" kamal-proxy list"))
	c.AddCommand(remoteShow(g, "logs", "Show recent Proxy logs", "docker logs --tail 200 "+proxy.ContainerName+" 2>&1"))
	return c
}

// appCmds adds ps / logs / exec.
func appCmds(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Inspect the running App (ps, logs, exec)"}
	c.AddCommand(&cobra.Command{
		Use: "ps", Short: "List the App's containers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if a.dest.Runtime == "swarm" {
				return a.remoteRun(cmd, "docker stack ps --no-trunc --format 'table {{.Name}}\\t{{.Node}}\\t{{.CurrentState}}\\t{{.Image}}\\t{{.Error}}' "+remote.Quote(a.project()))
			}
			return a.remoteRun(cmd, "docker compose -p "+remote.Quote(a.project())+" ps --format 'table {{.Service}}\\t{{.Name}}\\t{{.Status}}\\t{{.Image}}'")
		},
	})
	var follow bool
	var tail int
	logs := &cobra.Command{
		Use: "logs [SERVICE...]", Short: "Show App logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			s := fmt.Sprintf("docker compose -p %s logs --tail %d", remote.Quote(a.project()), tail)
			if follow {
				s += " -f"
			}
			if len(args) > 0 {
				s += " " + remote.QuoteArgs(args...)
			}
			if a.dest.Runtime == "swarm" {
				if len(args) != 1 {
					return errors.New("swarm: name exactly one Service, e.g. `yoho app logs web`")
				}
				s = fmt.Sprintf("docker service logs --tail %d", tail)
				if follow {
					s += " -f"
				}
				s += " " + remote.Quote(a.project()+"_"+args[0])
			}
			return a.remoteRun(cmd, s+" 2>&1")
		},
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "Follow")
	logs.Flags().IntVarP(&tail, "lines", "n", 100, "Lines from the end")
	c.AddCommand(logs)
	c.AddCommand(&cobra.Command{
		Use: "exec SERVICE -- COMMAND...", Short: "Run a command in a running Service container",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.remoteRun(cmd, "docker compose -p "+remote.Quote(a.project())+" exec -T "+remote.QuoteArgs(args...))
		},
	})
	return c
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
	h, err := remote.NewSSH(a.dest.Servers[0], srv.SSH, srv.Sudo, remote.SSHOptions{})
	if err != nil {
		return err
	}
	defer h.Close()
	return h.Run(cmd.Context(), remote.Cmd{Script: script, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()})
}

var _ = release.ProjectName
