package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/gate"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/transport"
	"github.com/yoho-build/yoho/internal/version"
)

// DefaultGateUser is the Unix user Members log in as.
const DefaultGateUser = "yoho-gate"

// scopedCommands run through the Gate, or never connect at all.
var scopedCommands = []string{
	"deploy", "rollback", "releases",
	"app ps", "app logs", "app stats",
	"tui", "ui",
	"config check", "config show",
}

// scopedCommand reports whether cmd (its path below the root) works with
// scoped access.
func scopedCommand(cmd *cobra.Command) bool {
	path := cmd.Name()
	for c := cmd.Parent(); c != nil && c.HasParent(); c = c.Parent() {
		path = c.Name() + " " + path
	}
	return slices.Contains(scopedCommands, path)
}

// scopedAccess reports whether dest is reached through the Gate: its
// access setting, overridden by YOHO_ACCESS (operator or scoped), so an
// operator and a team can share one Yoho file.
func scopedAccess(dest config.Destination, getenv func(string) string) (bool, error) {
	mode := dest.Access
	if v := strings.TrimSpace(getenv("YOHO_ACCESS")); v != "" {
		mode = v
	}
	switch mode {
	case "", "operator":
		return false, nil
	case "scoped":
		return true, nil
	}
	return false, fmt.Errorf("YOHO_ACCESS=%q must be operator or scoped", mode)
}

// sshOptions are the ssh options of the Destination's Servers.
func (a *app) sshOptions() remote.SSHOptions {
	var o remote.SSHOptions
	if a.dest.SSH != nil && a.dest.SSH.Via == "cloudflare" {
		o.CloudflareHostname = a.dest.SSH.Hostname
	}
	return o
}

func (a *app) gateUser() string {
	if a.dest.GateUser != "" {
		return a.dest.GateUser
	}
	return DefaultGateUser
}

// openGate connects to the Gate of the Destination's Server.
func (a *app) openGate() (*remote.Gate, string, error) {
	name := a.dest.Servers[0]
	srv := a.cfg.Servers[name]
	target, err := remote.WithUser(srv.SSH, a.gateUser())
	if err != nil {
		return nil, "", err
	}
	s, err := remote.NewSSH(name, target, false, a.sshOptions())
	if err != nil {
		return nil, "", err
	}
	return remote.NewGate(s, a.cfg.App, a.destName), target, nil
}

// gateCall runs one verb, reporting Gate refusals with their hints.
func (a *app) gateCall(ctx context.Context, verb string, args any, payload io.Reader, out io.Writer, result any) error {
	g, _, err := a.openGate()
	if err != nil {
		return a.fail(err)
	}
	defer g.Close()
	if err := g.Call(ctx, verb, args, payload, out, result); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return a.gateFail(err)
	}
	return nil
}

func (a *app) gateFail(err error) error {
	var ge *remote.GateError
	if errors.As(err, &ge) {
		return a.fail(&hintError{ge.Message, ge.Hint})
	}
	return a.fail(err)
}

// gateAttachAttempts / gateAttachDelay bound reconnects after a dropped
// SSH session (the tunnel may still be coming back). Tests shrink them.
var (
	gateAttachAttempts = 8
	gateAttachDelay    = 2 * time.Second
)

// gateCallOrAttach runs verb, and if the SSH session drops mid-rollout
// reconnects with attach so the client reads the worker's outcome.
func (a *app) gateCallOrAttach(ctx context.Context, g *remote.Gate, verb string, args any, payload io.Reader, out io.Writer, result any) error {
	err := g.Call(ctx, verb, args, payload, out, result)
	if !remote.IsDisconnect(err) {
		return err
	}
	a.ui.Warn("SSH session dropped; waiting for the rollout on the Server")
	switch x := args.(type) {
	case gate.DeployArgs:
		x.Attach, x.Archive = true, false
		args = x
	case gate.RollbackArgs:
		x.Attach = true
		args = x
	}
	var last error = err
	for i := 0; i < gateAttachAttempts; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gateAttachDelay):
		}
		g2, _, herr := a.hello(ctx, verb)
		if herr != nil {
			last = herr
			continue
		}
		last = g2.Call(ctx, verb, args, nil, out, result)
		g2.Close()
		if last == nil || !remote.IsDisconnect(last) {
			return last
		}
	}
	if last == nil {
		last = err
	}
	return last
}

// gateHint is the step hint when the Gate cannot be reached.
func (a *app) gateHint(target string) string {
	h := "check that `ssh " + target + "` reaches the Gate: the operator runs `yoho access install` and `yoho access key add` with your public key"
	if a.dest.SSH != nil {
		h += "; you are logged in to Cloudflare Access (or TUNNEL_SERVICE_TOKEN_ID/SECRET are set in CI)"
	}
	return h
}

// hello connects and checks the Member may run verb.
func (a *app) hello(ctx context.Context, verb string) (*remote.Gate, *gate.HelloResult, error) {
	g, target, err := a.openGate()
	if err != nil {
		return nil, nil, a.fail(err)
	}
	step := a.ui.Step(g.Name(), "Connect %s (scoped)", target)
	var h gate.HelloResult
	if err := g.Call(ctx, gate.VerbHello, nil, nil, nil, &h); err != nil {
		g.Close()
		var ge *remote.GateError
		if errors.As(err, &ge) {
			step.Fail(err, ge.Hint)
		} else {
			step.Fail(err, a.gateHint(target))
		}
		return nil, nil, &silentError{err}
	}
	if !slices.Contains(h.Verbs, verb) {
		g.Close()
		err := fmt.Errorf("Member %s may not %s %s/%s (allowed: %s)", h.Member, verb, a.cfg.App, a.destName, strings.Join(h.Verbs, ", "))
		step.Fail(err, "ask the operator: `yoho access add "+h.Member+" --verbs ...`")
		return nil, nil, &silentError{err}
	}
	step.Done("Member " + h.Member)
	return g, &h, nil
}

// scopedReleases fetches the Release list through the Gate.
func (a *app) scopedReleases(ctx context.Context, g *remote.Gate) ([]release.Release, error) {
	var rels []release.Release
	err := g.Call(ctx, gate.VerbReleases, nil, nil, nil, &rels)
	return rels, err
}

// scopedDeploy is `yoho deploy` through the Gate: build locally, ship the
// images to the Gate (or through the Registry), send the resolved compose
// project. Secrets are never resolved here; the Gate uses the values the
// operator pushed. Hooks run on this machine around the Gate call.
func (a *app) scopedDeploy(ctx context.Context, ver string, skipBuild bool) (err error) {
	u := a.ui
	start := time.Now()
	defer func() {
		if err != nil {
			u.Finished(err, "")
		}
	}()
	if loc := a.cfg.Builder.Location; loc != "" && loc != "local" {
		return fmt.Errorf("builder.location=%s is not available with scoped access\nhint: build on this machine (builder.location: local); the Gate only receives finished images", loc)
	}
	if ver, err = a.resolveDeployVersion(ver); err != nil {
		return err
	}
	if version.IsUncommitted(ver) {
		u.Warn("deploying uncommitted changes as %s", ver)
	}
	u.Title("Deploying %s@%s to %s (scoped)", a.cfg.App, shortVersion(ver), a.destName)

	step := u.Step("", "Load compose")
	r, err := a.compose(ctx)
	if err != nil {
		step.Fail(err, "run `yoho config check`")
		return &silentError{err}
	}
	// Secret values live on the Server; every referenced key is "known".
	var keys []string
	for _, e := range r.Ext {
		for _, ref := range e.Secrets {
			keys = append(keys, ref.Key)
		}
		for k := range e.Generate {
			keys = append(keys, k)
		}
	}
	findings := composefile.Check(r, "compose", keys)
	findings = append(findings, composefile.CheckCloudflareSSH(a.destName, a.dest, r.Project)...)
	for _, f := range findings {
		u.Finding(f.Level, f.Service, f.Message)
	}
	if composefile.HasErrors(findings) {
		err = errors.New("compose check failed")
		step.Fail(err, "fix the errors above, then `yoho config check`")
		return &silentError{err}
	}
	step.Done(fmt.Sprintf("%d services", len(r.Project.Services)))

	hook := a.hookFunc(ver, "deploy", nil)
	if err = hook(ctx, "pre-connect", nil); err != nil {
		return err
	}
	g, hello, err := a.hello(ctx, gate.VerbDeploy)
	if err != nil {
		return err
	}
	defer g.Close()
	hook = a.hookFunc(ver, "deploy", []plan.NamedHost{{Name: g.Name(), Server: config.Server{SSH: g.Target()}}})

	images := map[string]string{}
	var built []string
	for _, name := range sortedKeys(r.Project.Services) {
		if r.Project.Services[name].Build != nil {
			images[name] = build.ImageName(a.cfg.Registry, a.cfg.App, a.destName, name, ver)
		}
	}
	if !skipBuild && len(images) > 0 {
		if built, err = a.scopedBuild(ctx, r, ver, hello.Platform, hook); err != nil {
			return err
		}
	} else {
		for _, ref := range images {
			built = append(built, ref)
		}
		sort.Strings(built)
	}

	args := gate.DeployArgs{Version: ver, Performer: performer()}
	var payload io.Reader
	if len(built) > 0 {
		if payload, args.Pull, err = a.scopedShip(ctx, g, built, hello.Platform); err != nil {
			return err
		}
		args.Archive = payload != nil
		if c, ok := payload.(io.Closer); ok {
			defer c.Close() // stops docker save if the Gate refused early
		}
	}
	proj := r.Project
	args.Built = builtServices(proj, images)
	composefile.StripBuild(proj, images)
	compose, err := composefile.Resolved(proj)
	if err != nil {
		return err
	}
	args.Compose = string(compose)

	if err = hook(ctx, "pre-deploy", nil); err != nil {
		return err
	}
	step = u.Step(g.Name(), "Deploy %s through the Gate", shortVersion(ver))
	var rel release.Release
	if err := a.gateCallOrAttach(ctx, g, gate.VerbDeploy, args, payload, step.Output(), &rel); err != nil {
		var ge *remote.GateError
		hint := "see `yoho app ps` and `yoho releases`"
		if errors.As(err, &ge) && ge.Hint != "" {
			hint = ge.Hint
		}
		step.Fail(err, hint)
		return &silentError{err}
	}
	step.Done()
	if err = hook(ctx, "post-deploy", nil); err != nil {
		return err
	}
	u.Finished(nil, fmt.Sprintf("Deployed %s@%s to %s in %s", a.cfg.App, shortVersion(rel.Version), a.destName, time.Since(start).Round(100*time.Millisecond)))
	return nil
}

// scopedBuild builds the App's images on this machine for the Server's
// platform and returns the built references.
func (a *app) scopedBuild(ctx context.Context, r *composefile.Result, ver, serverPlatform string, hook plan.HookFunc) ([]string, error) {
	u := a.ui
	if serverPlatform == "" {
		return nil, errors.New("the Gate did not report the Server's platform")
	}
	platform, err := pickPlatform(serverPlatform, a.cfg.Builder.Platforms)
	if err != nil {
		return nil, err
	}
	if err := hook(ctx, "pre-build", nil); err != nil {
		return nil, err
	}
	var bargs []string
	var benv []string
	if len(a.cfg.Builder.Secrets) > 0 {
		// Build secrets (e.g. a package registry token) are the Member's own.
		store, err := a.loadSecrets(ctx, nil)
		if err != nil {
			return nil, err
		}
		if bargs, benv, err = store.BuildxArgs(a.cfg.Builder.Secrets); err != nil {
			return nil, err
		}
	}
	step := u.Step("", "Build images for %s", platform)
	eng, why := build.ResolveEngine(a.cfg.Builder, runtime.GOOS, runtime.GOARCH, exec.LookPath, nil)
	u.Info("  engine: %s (%s)", eng, why)
	imgs, err := build.Images(ctx, build.Options{
		Dir: a.dir, App: a.cfg.App, Destination: a.destName, Version: ver,
		Project: r.Project, Builder: a.cfg.Builder, Registry: a.cfg.Registry,
		Platforms: []string{platform}, BuildEnv: benv, BuildSecretArgs: bargs,
		Out: step.Output(),
	})
	if err != nil {
		step.Fail(err, "rerun with -v to see the full build output")
		return nil, &silentError{err}
	}
	var built []string
	for _, im := range imgs {
		if !im.Built {
			continue
		}
		if im.Engine == transport.EngineContainer {
			err := errors.New("images built with Apple container cannot be shipped through the Gate yet")
			step.Fail(err, "set builder.engine: docker")
			return nil, &silentError{err}
		}
		built = append(built, im.Ref)
	}
	sort.Strings(built)
	step.Done(strings.Join(built, ", "))
	return built, nil
}

// scopedShip returns the image archive to stream with the deploy request
// (nil when the Server has every image), or, with transport.mode registry,
// pushes the images and returns them for the Gate to pull.
func (a *app) scopedShip(ctx context.Context, g *remote.Gate, built []string, platform string) (io.Reader, []string, error) {
	step := a.ui.Step("", "Ship %d image(s)", len(built))
	if a.cfg.Transport.Mode == transport.MethodRegistry {
		if a.cfg.Registry == nil {
			err := errors.New("transport.mode registry needs a registry")
			step.Fail(err, "configure registry, or use transport.mode auto")
			return nil, nil, &silentError{err}
		}
		if err := transport.PushRegistry(ctx, built, step.Output()); err != nil {
			step.Fail(err, "log in to the registry with `docker login`")
			return nil, nil, &silentError{err}
		}
		step.Done("pushed; the Server pulls them")
		return nil, built, nil
	}
	fps, err := transport.LocalFingerprints(ctx, built, platform)
	if err != nil {
		step.Fail(err, "")
		return nil, nil, &silentError{err}
	}
	var missing []string
	if err := g.Call(ctx, "images", gate.ImagesArgs{Images: fps}, nil, nil, &missing); err != nil {
		step.Fail(err, "")
		return nil, nil, &silentError{err}
	}
	if len(missing) == 0 {
		step.Done("already on the Server")
		return nil, nil, nil
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(transport.SaveArchive(ctx, missing, platform, pw)) }()
	step.Done(fmt.Sprintf("%d streamed with the deploy", len(missing)))
	return pr, nil, nil
}

// scopedRollback is `yoho rollback` through the Gate.
func (a *app) scopedRollback(ctx context.Context, v string) error {
	g, _, err := a.hello(ctx, gate.VerbRollback)
	if err != nil {
		a.ui.Finished(err, "")
		return err
	}
	defer g.Close()
	// The Gate resolves a short version itself, so rolling back needs only
	// the rollback grant (not releases).
	target := v
	a.ui.Title("Rolling back %s on %s to %s (scoped)", a.cfg.App, a.destName, shortVersion(target))
	a.ui.Warn("volumes and database migrations are not reverted")
	step := a.ui.Step(g.Name(), "Redeploy %s", shortVersion(target))
	var rel release.Release
	if err := a.gateCallOrAttach(ctx, g, gate.VerbRollback, gate.RollbackArgs{Version: target, Performer: performer()}, nil, step.Output(), &rel); err != nil {
		step.Fail(err, hintFor(err, "see `yoho releases`"))
		a.ui.Finished(err, "")
		return &silentError{err}
	}
	step.Done()
	a.ui.Finished(nil, "Rolled back to "+shortVersion(rel.Version))
	return nil
}

// scopedStats prints docker stats through the Gate. The Gate has no
// terminal, so the live view polls snapshots.
func (a *app) scopedStats(cmd *cobra.Command, services []string, live bool) error {
	ctx := cmd.Context()
	args := gate.StatsArgs{ServicesArgs: gate.ServicesArgs{Services: services}, JSON: a.g.json}
	if !live {
		return a.gateCall(ctx, gate.VerbStats, args, nil, cmd.OutOrStdout(), nil)
	}
	g, _, err := a.openGate()
	if err != nil {
		return a.fail(err)
	}
	defer g.Close()
	for {
		var buf strings.Builder
		err := g.Call(ctx, gate.VerbStats, args, nil, &buf, nil)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return a.gateFail(err)
		}
		fmt.Fprint(cmd.OutOrStdout(), "\x1b[H\x1b[2J"+buf.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// gateReader is a dash.Reader through the Gate.
type gateReader struct{ g *remote.Gate }

func (r gateReader) Server(ctx context.Context) (dash.Server, error) {
	var s dash.Server
	err := r.g.Call(ctx, gate.VerbDash, nil, nil, nil, &s)
	return s, err
}

func (r gateReader) Logs(ctx context.Context, container string, lines int) (string, error) {
	var b strings.Builder
	err := r.g.Call(ctx, gate.VerbLogs, gate.LogsArgs{Container: container, Lines: lines, Timestamps: true}, nil, &b, nil)
	return b.String(), err
}

// scopedDashSource is dashSource through the Gate.
func (a *app) scopedDashSource(cmd *cobra.Command, interval time.Duration) (*dash.Live, func(), error) {
	g, _, err := a.hello(cmd.Context(), gate.VerbDash)
	if err != nil {
		return nil, nil, err
	}
	src := &dash.Source{App: a.cfg.App, Destination: a.destName, Runtime: "compose", Root: a.root(),
		Targets: []dash.Target{{Name: a.dest.Servers[0], SSH: g.Target(), Reader: gateReader{g}}},
		// Releases come with the dash verb.
		Releases: func(context.Context, remote.Host) ([]release.Release, error) { return nil, nil },
	}
	return &dash.Live{Src: src, MaxAge: interval * 4 / 5}, func() { g.Close() }, nil
}
