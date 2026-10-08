// Package build builds an App's Service images on the Builder.
//
// Builder locations:
//   - local (default): `docker buildx build --load` on this machine.
//   - remote: a buildx docker-container builder created on the build Server
//     with `docker buildx create ssh://<remote>`; the image is built there and
//     `--load` streams the result back into the local daemon, so the normal
//     image transport ships it to Servers afterwards.
//   - server: the source is rsynced to the Destination's Server
//     (<AppDir>/source) and built there with `docker build`, so no transport
//     is needed. Build secrets travel as env through remote.Cmd.Env.
package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Command is a local process invocation.
type Command struct {
	Argv []string
	// Extra KEY=value entries appended to os.Environ().
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a local command. Errors with ExitCode() report non-zero exits.
type Exec func(ctx context.Context, c Command) error

// Options configures Images.
type Options struct {
	// Dir is the directory of the Yoho file; relative build contexts resolve
	// against Project.WorkingDir, falling back to Dir.
	Dir         string
	App         string
	Destination string // location=server: picks <AppDir>/source
	Version     string
	Project     *types.Project
	Builder     config.Builder
	Registry    *config.Registry
	// Target platforms (e.g. linux/amd64). Default Builder.Platforms; when
	// both are empty buildx builds for its native platform.
	Platforms []string
	// KEY=value build secrets, set as env on the docker process (never argv).
	BuildEnv []string
	// Extra raw arguments for every build, e.g. ["--secret", "id=X,env=X"].
	BuildSecretArgs []string
	Out             io.Writer
	// Host is the Destination's Server (location=server).
	Host remote.Host
	// RsyncTarget overrides the rsync destination [user@]host[:port] for
	// location=server. Default: derived from Host when it is *remote.SSH;
	// a *remote.Local Host syncs to a local path.
	RsyncTarget string
	// SourceDir overrides the Server source directory for location=server.
	SourceDir string
	// Exec runs local commands. Default os/exec.
	Exec Exec
	// GOOS, GOARCH and LookPath feed ResolveEngine. Default: this machine
	// and exec.LookPath.
	GOOS, GOARCH string
	LookPath     func(string) (string, error)
}

// Image is the image a Service runs.
type Image struct {
	Ref string // name:tag
	// ID is the local (or Server, for location=server) image ID when built.
	ID string
	// Built is true for Services with a build section.
	Built bool
	// Engine that built the image (docker or container). container images
	// live in Apple container's store, not Docker's; transport must export
	// them with `container image save`.
	Engine string
}

// ImageName is <prefix>/<app>-<service>:<version>, lowercase. Prefix is
// registry.prefix, else registry.server, else "yoho".
func ImageName(reg *config.Registry, app, service, version string) string {
	prefix := "yoho"
	if reg != nil {
		if reg.Prefix != "" {
			prefix = strings.TrimSuffix(reg.Prefix, "/")
		} else if reg.Server != "" {
			prefix = strings.TrimSuffix(reg.Server, "/")
		}
	}
	return strings.ToLower(prefix + "/" + app + "-" + service + ":" + version)
}

// Images builds every Service with a build section and returns Service ->
// Image for all Services (unbuilt Services keep their compose image).
func Images(ctx context.Context, o Options) (map[string]Image, error) {
	if o.Project == nil {
		return nil, errors.New("build: no compose project")
	}
	if o.Exec == nil {
		o.Exec = osExec
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if len(o.Platforms) == 0 {
		o.Platforms = o.Builder.Platforms
	}
	if o.Dir == "" {
		o.Dir = o.Project.WorkingDir
	}
	res := map[string]Image{}
	var names []string
	for name, svc := range o.Project.Services {
		if svc.Build == nil {
			res[name] = Image{Ref: svc.Image}
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return res, nil
	}

	loc := o.Builder.Location
	if loc == "" {
		loc = "local"
	}
	var builder string
	engine := EngineDocker
	switch loc {
	case "local":
		var reason string
		engine, reason = o.resolveEngine(ctx)
		if engine == EngineContainer {
			for _, name := range names {
				if u := containerUnsupported(o.Project.Services[name].Build); len(u) > 0 {
					if o.Builder.Engine == EngineContainer {
						return nil, fmt.Errorf("build %s: container engine does not support %s; set builder.engine: docker", name, strings.Join(u, ", "))
					}
					engine, reason = EngineDocker, fmt.Sprintf("service %s uses %s, which container build lacks", name, strings.Join(u, ", "))
					break
				}
			}
		}
		fmt.Fprintf(o.Out, "Build engine: %s (%s)\n", engine, reason)
		if engine == EngineContainer {
			noteContainerUpdate(ctx, o.Exec)
		}
	case "remote":
		if o.Builder.Remote == "" {
			return nil, errors.New("builder.location=remote needs builder.remote")
		}
		b, err := ensureRemoteBuilder(ctx, o)
		if err != nil {
			return nil, err
		}
		builder = b
	case "server":
		if o.Host == nil {
			return nil, errors.New("builder.location=server needs a Server")
		}
		if err := syncSource(ctx, o); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown builder.location %q", loc)
	}

	for _, name := range names {
		svc := o.Project.Services[name]
		ref := ImageName(o.Registry, o.App, name, o.Version)
		fmt.Fprintf(o.Out, "Building %s (%s) on %s builder\n", name, ref, loc)
		var id string
		var err error
		switch {
		case loc == "server":
			id, err = buildOnServer(ctx, o, name, svc, ref)
		case engine == EngineContainer:
			id, err = buildContainer(ctx, o, name, svc, ref)
		default:
			id, err = buildLocal(ctx, o, name, svc, ref, builder)
		}
		if err != nil {
			return nil, fmt.Errorf("build %s: %w", name, err)
		}
		res[name] = Image{Ref: ref, ID: id, Built: true, Engine: engine}
	}
	return res, nil
}

func (o Options) resolveEngine(ctx context.Context) (string, string) {
	goos, goarch := o.GOOS, o.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	lp := o.LookPath
	if lp == nil {
		lp = exec.LookPath
	}
	return ResolveEngine(o.Builder, goos, goarch, lp, func() error { return ContainerStatus(ctx, o.Exec) })
}

// spec is a build resolved to paths on the machine running docker.
type spec struct {
	context    string
	dockerfile string // path, or "-" with inline on stdin
	inline     string
}

func (o Options) baseDir() string {
	if o.Project != nil && o.Project.WorkingDir != "" {
		return o.Project.WorkingDir
	}
	return o.Dir
}

func isRemoteContext(c string) bool {
	return strings.Contains(c, "://") || strings.HasPrefix(c, "git@")
}

func localSpec(o Options, b *types.BuildConfig) spec {
	ctxDir := b.Context
	if ctxDir == "" {
		ctxDir = "."
	}
	if !isRemoteContext(ctxDir) && !filepath.IsAbs(ctxDir) {
		ctxDir = filepath.Join(o.baseDir(), ctxDir)
	}
	s := spec{context: ctxDir}
	switch {
	case b.DockerfileInline != "":
		s.dockerfile, s.inline = "-", b.DockerfileInline
	case b.Dockerfile != "" && (filepath.IsAbs(b.Dockerfile) || isRemoteContext(ctxDir)):
		s.dockerfile = b.Dockerfile
	case b.Dockerfile != "":
		// compose resolves dockerfile against the context; docker -f against cwd.
		s.dockerfile = filepath.Join(ctxDir, b.Dockerfile)
	}
	return s
}

// buildArgs renders docker build flags (after "docker buildx build" or
// "docker build"). mapPath rewrites local file paths (secrets) for the
// machine running docker.
func buildArgs(o Options, service string, b *types.BuildConfig, s spec, ref string, mapPath func(string) (string, error)) ([]string, error) {
	a := []string{"--tag", ref}
	if s.dockerfile != "" {
		a = append(a, "--file", s.dockerfile)
	}
	for _, k := range sortedKeys(b.Args) {
		if v := b.Args[k]; v != nil {
			a = append(a, "--build-arg", k+"="+*v)
		} else {
			a = append(a, "--build-arg", k)
		}
	}
	a = append(a, labelArgs(o, service, b)...)
	if b.Target != "" {
		a = append(a, "--target", b.Target)
	}
	if b.Network != "" {
		a = append(a, "--network", b.Network)
	}
	if b.NoCache {
		a = append(a, "--no-cache")
	}
	if b.Pull {
		a = append(a, "--pull")
	}
	for _, c := range b.CacheFrom {
		a = append(a, "--cache-from", c)
	}
	for _, c := range b.CacheTo {
		a = append(a, "--cache-to", c)
	}
	for _, k := range sortedKeys(b.AdditionalContexts) {
		a = append(a, "--build-context", k+"="+b.AdditionalContexts[k])
	}
	for _, k := range b.SSH {
		if k.Path != "" {
			a = append(a, "--ssh", k.ID+"="+k.Path)
		} else {
			a = append(a, "--ssh", k.ID)
		}
	}
	sec, err := secretArgs(o, b, mapPath)
	if err != nil {
		return nil, err
	}
	a = append(a, sec...)
	a = append(a, o.BuildSecretArgs...)
	return append(a, s.context), nil
}

func labelArgs(o Options, service string, b *types.BuildConfig) []string {
	labels := map[string]string{}
	for k, v := range b.Labels {
		labels[k] = v
	}
	labels["yoho.app"] = o.App
	labels["yoho.service"] = service
	labels["yoho.version"] = o.Version
	var a []string
	for _, k := range sortedKeys(labels) {
		a = append(a, "--label", k+"="+labels[k])
	}
	return a
}

// secretArgs renders --secret flags; values are referenced by env name or
// file path, never inlined.
func secretArgs(o Options, b *types.BuildConfig, mapPath func(string) (string, error)) ([]string, error) {
	var a []string
	seen := map[string]bool{}
	for _, k := range o.Builder.Secrets {
		seen[k] = true
		a = append(a, "--secret", "id="+k+",env="+k)
	}
	for _, sec := range b.Secrets {
		id := sec.Target
		if id == "" {
			id = sec.Source
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		def, ok := o.Project.Secrets[sec.Source]
		switch {
		case !ok:
			return nil, fmt.Errorf("build secret %q is not defined in top-level secrets", sec.Source)
		case def.Environment != "":
			a = append(a, "--secret", "id="+id+",env="+def.Environment)
		case def.File != "":
			p := def.File
			if !filepath.IsAbs(p) {
				p = filepath.Join(o.baseDir(), p)
			}
			p, err := mapPath(p)
			if err != nil {
				return nil, fmt.Errorf("build secret %q: %w", sec.Source, err)
			}
			a = append(a, "--secret", "id="+id+",src="+p)
		default:
			return nil, fmt.Errorf("build secret %q: only environment or file secrets are supported", sec.Source)
		}
	}
	return a, nil
}

func buildLocal(ctx context.Context, o Options, service string, svc types.ServiceConfig, ref, builder string) (string, error) {
	s := localSpec(o, svc.Build)
	flags, err := buildArgs(o, service, svc.Build, s, ref, func(p string) (string, error) { return p, nil })
	if err != nil {
		return "", err
	}
	argv := []string{"docker", "buildx", "build", "--load"}
	if builder != "" {
		argv = append(argv, "--builder", builder)
	}
	if len(o.Platforms) > 0 {
		argv = append(argv, "--platform", strings.Join(o.Platforms, ","))
	}
	argv = append(argv, flags...)
	c := Command{Argv: argv, Env: o.BuildEnv, Stdout: o.Out, Stderr: o.Out}
	if s.inline != "" {
		c.Stdin = strings.NewReader(s.inline)
	}
	if err := o.Exec(ctx, c); err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := o.Exec(ctx, Command{Argv: []string{"docker", "image", "inspect", "--format", "{{.Id}}", ref}, Stdout: &out, Stderr: io.Discard}); err != nil {
		return "", nil // ID is best effort
	}
	return strings.TrimSpace(out.String()), nil
}

// RemoteBuilderName is the buildx builder Yoho creates for a remote target.
func RemoteBuilderName(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "yoho-remote-" + hex.EncodeToString(sum[:4])
}

func ensureRemoteBuilder(ctx context.Context, o Options) (string, error) {
	name := RemoteBuilderName(o.Builder.Remote)
	if err := o.Exec(ctx, Command{Argv: []string{"docker", "buildx", "inspect", name}, Stdout: io.Discard, Stderr: io.Discard}); err == nil {
		return name, nil
	}
	ep := o.Builder.Remote
	if !strings.HasPrefix(ep, "ssh://") {
		ep = "ssh://" + ep
	}
	argv := []string{"docker", "buildx", "create", "--name", name, "--driver", "docker-container", ep}
	if err := o.Exec(ctx, Command{Argv: argv, Stdout: o.Out, Stderr: o.Out}); err != nil {
		return "", fmt.Errorf("create remote builder %s: %w", ep, err)
	}
	return name, nil
}

// SourceDir is where location=server syncs the build source.
func SourceDir(app, destination string) string {
	return path.Join(release.AppDir(app, destination), "source")
}

func (o Options) sourceDir() string {
	if o.SourceDir != "" {
		return o.SourceDir
	}
	return SourceDir(o.App, o.Destination)
}

// RsyncArgv returns the rsync command syncing o.Dir to the Server.
func RsyncArgv(o Options) ([]string, error) {
	excl := append([]string{".git", "/.yoho/secrets*"}, o.Builder.Exclude...)
	argv := []string{"rsync", "-az", "--delete"}
	for _, e := range excl {
		argv = append(argv, "--exclude="+e)
	}
	src := strings.TrimSuffix(o.Dir, "/") + "/"
	dst := o.sourceDir() + "/"
	switch h := o.Host.(type) {
	case *remote.SSH:
		if o.RsyncTarget == "" {
			return append(argv, "-e", h.RshCommand(), src, h.Destination()+":"+dst), nil
		}
	case *remote.Local:
		if o.RsyncTarget == "" {
			return append(argv, src, dst), nil
		}
	}
	if o.RsyncTarget == "" {
		return nil, errors.New("builder.location=server: no rsync target for Server " + o.Host.Name())
	}
	user, host, port, err := remote.ParseTarget(o.RsyncTarget)
	if err != nil {
		return nil, err
	}
	rsh := "ssh -o BatchMode=yes"
	if port != 0 {
		rsh += fmt.Sprintf(" -p %d", port)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if user != "" {
		host = user + "@" + host
	}
	return append(argv, "-e", rsh, src, host+":"+dst), nil
}

func syncSource(ctx context.Context, o Options) error {
	if err := o.Host.Run(ctx, remote.Cmd{Script: "mkdir -p -m 0700 " + remote.Quote(o.sourceDir())}); err != nil {
		return err
	}
	argv, err := RsyncArgv(o)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Syncing source to %s:%s\n", o.Host.Name(), o.sourceDir())
	if err := o.Exec(ctx, Command{Argv: argv, Stdout: o.Out, Stderr: o.Out}); err != nil {
		return fmt.Errorf("rsync source: %w", err)
	}
	return nil
}

// ServerScript returns the remote script and stdin building one Service on
// the Server.
func ServerScript(o Options, service string, svc types.ServiceConfig, ref string) (string, io.Reader, error) {
	local := localSpec(o, svc.Build)
	if isRemoteContext(local.context) {
		return serverScriptFrom(o, service, svc, local, ref)
	}
	toRemote := func(p string) (string, error) {
		rel, err := filepath.Rel(o.Dir, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s is outside %s and is not synced to the Server", p, o.Dir)
		}
		return path.Join(o.sourceDir(), filepath.ToSlash(rel)), nil
	}
	var s spec
	var err error
	if s.context, err = toRemote(local.context); err != nil {
		return "", nil, err
	}
	s.dockerfile, s.inline = local.dockerfile, local.inline
	if s.dockerfile != "" && s.dockerfile != "-" {
		if s.dockerfile, err = toRemote(s.dockerfile); err != nil {
			return "", nil, err
		}
	}
	return serverScriptFrom(o, service, svc, s, ref, toRemote)
}

func serverScriptFrom(o Options, service string, svc types.ServiceConfig, s spec, ref string, mapPath ...func(string) (string, error)) (string, io.Reader, error) {
	mp := func(p string) (string, error) { return "", fmt.Errorf("%s cannot be used on the Server", p) }
	if len(mapPath) > 0 {
		mp = mapPath[0]
	}
	flags, err := buildArgs(o, service, svc.Build, s, ref, mp)
	if err != nil {
		return "", nil, err
	}
	script := "set -eu\nDOCKER_BUILDKIT=1 docker build " + remote.QuoteArgs(flags...)
	var stdin io.Reader
	if s.inline != "" {
		stdin = strings.NewReader(s.inline)
	}
	return script, stdin, nil
}

func buildOnServer(ctx context.Context, o Options, service string, svc types.ServiceConfig, ref string) (string, error) {
	script, stdin, err := ServerScript(o, service, svc, ref)
	if err != nil {
		return "", err
	}
	env, err := envMap(o.BuildEnv)
	if err != nil {
		return "", err
	}
	if err := o.Host.Run(ctx, remote.Cmd{Script: script, Stdin: stdin, Env: env, Stdout: o.Out, Stderr: o.Out}); err != nil {
		return "", err
	}
	id, err := o.Host.Output(ctx, remote.Cmd{Script: "docker image inspect --format '{{.Id}}' " + remote.Quote(ref)})
	if err != nil {
		return "", nil
	}
	return id, nil
}

func envMap(kv []string) (map[string]string, error) {
	if len(kv) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(kv))
	for _, e := range kv {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, errors.New("build env entry is not KEY=value")
		}
		m[k] = v
	}
	return m, nil
}

// ServerPlatform returns the Server's platform, e.g. linux/amd64.
func ServerPlatform(ctx context.Context, h remote.Host) (string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: `docker version -f '{{.Server.Os}}/{{.Server.Arch}}' 2>/dev/null || { printf '%s/' "$(uname -s | tr A-Z a-z)"; uname -m; }`})
	if err != nil {
		return "", fmt.Errorf("detect platform of %s: %w", h.Name(), err)
	}
	return NormalizePlatform(out)
}

// NormalizePlatform maps os/uname-arch to a docker platform.
func NormalizePlatform(s string) (string, error) {
	osName, arch, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || osName == "" || arch == "" {
		return "", fmt.Errorf("unrecognized platform %q", s)
	}
	switch arch {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64", "armv8l":
		arch = "arm64"
	case "armv7l", "armhf", "arm":
		arch = "arm/v7"
	case "armv6l":
		arch = "arm/v6"
	case "i386", "i686", "386":
		arch = "386"
	}
	return osName + "/" + arch, nil
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func osExec(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	return cmd.Run()
}
