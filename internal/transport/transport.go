// Package transport ships locally built images to Servers.
//
// Mode auto walks a ladder per Server, in parallel across Servers:
//  1. skip: every image is already on the Server with the same content;
//  2. pussh: `docker pussh` (unregistry) when the plugin is installed locally
//     and the Server's Docker uses the containerd image store;
//  3. load: `docker save | gzip` streamed over ssh into `docker load`;
//  4. registry: `docker push` locally, `docker pull` on the Server (only when
//     a registry is configured).
//
// Explicit modes (pussh, load, registry) use only that method after the
// skip check.
package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/remote"
)

// Methods reported per Server.
const (
	MethodSkip     = "skip"
	MethodPussh    = "pussh"
	MethodLoad     = "load"
	MethodRegistry = "registry"
)

// Command is a local process invocation.
type Command struct {
	Argv   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a local command.
type Exec func(ctx context.Context, c Command) error

// PushOptions configures Push.
type PushOptions struct {
	// Images are local image references (name:tag).
	Images []string
	Hosts  []remote.Host
	// Targets maps Host name -> ssh target [user@]host[:port] for pussh.
	// Default: Target() of *remote.SSH Hosts.
	Targets map[string]string
	// Mode: auto (default), pussh, load, registry.
	Mode     string
	Registry *config.Registry
	// RegistryPassword is the resolved registry.password_secret value. Sent
	// only on stdin to `docker login --password-stdin`.
	RegistryPassword string
	Out              io.Writer
	// Platform of the Servers, e.g. linux/amd64. Passed to `docker save
	// --platform` (Docker 28+) because a local containerd image store
	// otherwise exports the whole multi-platform index, which fails to load
	// when only one platform's layers are present.
	Platform string
	// Engine that built Images: docker (default) or container (Apple
	// container's store; see build.Image.Engine). container images ship by
	// load only: pussh and registry need them in the local Docker.
	Engine string
	// Exec runs local commands. Default os/exec.
	Exec Exec
}

// Result is the outcome for one Server.
type Result struct {
	Host   string
	Method string
}

// Push ships images to all Hosts in parallel and returns the method used
// per Host (same order as Hosts). Errors from all Hosts are joined.
func Push(ctx context.Context, o PushOptions) ([]Result, error) {
	if o.Exec == nil {
		o.Exec = osExec
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	switch o.Mode {
	case "":
		o.Mode = "auto"
	case "auto", MethodPussh, MethodLoad, MethodRegistry:
	default:
		return nil, fmt.Errorf("unknown transport mode %q", o.Mode)
	}
	if len(o.Images) == 0 {
		return nil, nil
	}
	if o.Engine == EngineContainer && (o.Mode == MethodPussh || o.Mode == MethodRegistry) {
		return nil, fmt.Errorf("transport mode %s needs images in the local Docker; images built with Apple container ship via load (set transport.mode: auto or load, or builder.engine: docker)", o.Mode)
	}
	p := &pusher{o: o, out: &syncWriter{w: o.Out}}
	ids, err := p.localIDs(ctx)
	if err != nil {
		return nil, err
	}
	p.ids = ids

	results := make([]Result, len(o.Hosts))
	errs := make([]error, len(o.Hosts))
	var wg sync.WaitGroup
	for i, h := range o.Hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := p.pushHost(ctx, h)
			results[i] = Result{Host: h.Name(), Method: m}
			if err != nil {
				errs[i] = fmt.Errorf("push images to %s: %w", h.Name(), err)
				return
			}
			if m == MethodSkip {
				fmt.Fprintf(p.out, "%s: images already present\n", h.Name())
			} else {
				fmt.Fprintf(p.out, "%s: images shipped via %s\n", h.Name(), m)
			}
		}()
	}
	wg.Wait()
	return results, errors.Join(errs...)
}

type pusher struct {
	o   PushOptions
	out io.Writer
	ids map[string]string // image -> Fingerprint

	pusshOnce sync.Once
	pusshOK   bool

	regOnce sync.Once
	regErr  error

	saveOnce     sync.Once
	saveHasPlatf bool
}

// saveArgv returns `docker save` argv, adding --platform when supported.
func (p *pusher) saveArgv(ctx context.Context, imgs []string) []string {
	argv := []string{"docker", "save"}
	if p.o.Platform == "" {
		return append(argv, imgs...)
	}
	p.saveOnce.Do(func() {
		var help bytes.Buffer
		_ = p.o.Exec(ctx, Command{Argv: []string{"docker", "save", "--help"}, Stdout: &help, Stderr: &help})
		p.saveHasPlatf = strings.Contains(help.String(), "--platform")
	})
	if p.saveHasPlatf {
		argv = append(argv, "--platform", p.o.Platform)
	}
	return append(argv, imgs...)
}

// Fingerprint identifies image content across image stores. Image IDs
// differ between a containerd store (manifest/index digest) and a classic
// overlay2 store (config digest), but platform, creation time and layer diff
// IDs are the same. range, not join: Docker 27's CLI decodes RootFS.Layers
// as []interface{} and join fails ("wrong type for value"). Apple container
// images get the same string from build.ParseContainerInspect.
const Fingerprint = `{{.Os}}/{{.Architecture}}|{{.Created}}|{{range .RootFS.Layers}}{{.}},{{end}}`

// localIDs returns each image's Fingerprint, for the Servers' platform when
// set (falls back to the default platform on Docker without --platform).
func (p *pusher) localIDs(ctx context.Context) (map[string]string, error) {
	if p.o.Engine == EngineContainer {
		return p.containerIDs(ctx)
	}
	inspect := func(extra ...string) ([]string, error) {
		var out bytes.Buffer
		argv := append(append([]string{"docker", "image", "inspect"}, extra...), "--format", Fingerprint)
		err := p.o.Exec(ctx, Command{Argv: append(argv, p.o.Images...), Stdout: &out, Stderr: io.Discard})
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if err == nil && len(lines) != len(p.o.Images) {
			err = fmt.Errorf("docker image inspect returned %d results for %d images", len(lines), len(p.o.Images))
		}
		return lines, err
	}
	var fps []string
	var err error
	if p.o.Platform != "" {
		fps, err = inspect("--platform", p.o.Platform)
	}
	if fps == nil || err != nil {
		if fps, err = inspect(); err != nil {
			return nil, fmt.Errorf("images not found locally (%s): %w", strings.Join(p.o.Images, ", "), err)
		}
	}
	res := map[string]string{}
	for i, img := range p.o.Images {
		res[img] = strings.TrimSpace(fps[i])
	}
	return res, nil
}

// missing returns images not present on h with the same content.
func (p *pusher) missing(ctx context.Context, h remote.Host) []string {
	var miss []string
	for _, img := range p.o.Images {
		fp, err := h.Output(ctx, remote.Cmd{Script: "docker image inspect --format " + remote.Quote(Fingerprint) + " " + remote.Quote(img) + " 2>/dev/null"})
		if err != nil || strings.TrimSpace(fp) != p.ids[img] {
			miss = append(miss, img)
		}
	}
	return miss
}

func (p *pusher) target(h remote.Host) string {
	if t := p.o.Targets[h.Name()]; t != "" {
		return t
	}
	if s, ok := h.(*remote.SSH); ok {
		return s.Target()
	}
	return ""
}

func (p *pusher) pushHost(ctx context.Context, h remote.Host) (string, error) {
	imgs := p.missing(ctx, h)
	if len(imgs) == 0 {
		return MethodSkip, nil
	}
	mode := p.o.Mode
	if p.o.Engine == EngineContainer {
		return MethodLoad, p.loadContainer(ctx, h, imgs)
	}
	var errs []error

	if mode == "auto" || mode == MethodPussh {
		err := p.pussh(ctx, h, imgs)
		if err == nil {
			return MethodPussh, nil
		}
		if mode == MethodPussh {
			return MethodPussh, err
		}
		fmt.Fprintf(p.out, "%s: pussh unavailable (%v), falling back to docker save | docker load\n", h.Name(), err)
		errs = append(errs, err)
	}
	if mode == "auto" || mode == MethodLoad {
		err := p.load(ctx, h, imgs)
		if err == nil {
			return MethodLoad, nil
		}
		if mode == MethodLoad || p.o.Registry == nil {
			return MethodLoad, err
		}
		fmt.Fprintf(p.out, "%s: docker load failed (%v), falling back to registry\n", h.Name(), err)
		errs = append(errs, err)
	}
	if err := p.registry(ctx, h, imgs); err != nil {
		return MethodRegistry, errors.Join(append(errs, err)...)
	}
	return MethodRegistry, nil
}

// pussh needs the local plugin, an ssh target and the containerd image store
// on the Server (unregistry stores blobs in containerd).
func (p *pusher) pussh(ctx context.Context, h remote.Host, imgs []string) error {
	p.pusshOnce.Do(func() {
		p.pusshOK = p.o.Exec(ctx, Command{Argv: []string{"docker", "pussh", "--help"}, Stdout: io.Discard, Stderr: io.Discard}) == nil
	})
	if !p.pusshOK {
		return errors.New("docker pussh plugin not installed")
	}
	t := p.target(h)
	if t == "" {
		return errors.New("no ssh target")
	}
	ds, err := h.Output(ctx, remote.Cmd{Script: "docker info --format '{{json .DriverStatus}}'"})
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	if !strings.Contains(ds, "io.containerd.snapshotter") {
		return errors.New("Server Docker does not use the containerd image store")
	}
	for _, img := range imgs {
		if err := p.o.Exec(ctx, Command{Argv: []string{"docker", "pussh", img, t}, Stdout: p.out, Stderr: p.out}); err != nil {
			return fmt.Errorf("docker pussh %s: %w", img, err)
		}
	}
	return nil
}

// LoadScript is the Server side of save|load. docker load detects gzip.
const LoadScript = "docker load"

// load streams `docker save imgs | gzip` into `docker load` on h.
func (p *pusher) load(ctx context.Context, h remote.Host, imgs []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	saveErr := make(chan error, 1)
	go func() {
		gz, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		var stderr bytes.Buffer
		err := p.o.Exec(ctx, Command{Argv: p.saveArgv(ctx, imgs), Stdout: gz, Stderr: &stderr})
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			err = fmt.Errorf("docker save: %w %s", err, strings.TrimSpace(stderr.String()))
		}
		pw.CloseWithError(err)
		saveErr <- err
	}()
	runErr := h.Run(ctx, remote.Cmd{Script: LoadScript, Stdin: pr})
	// Unblock save if load ended early.
	pr.CloseWithError(errors.New("docker load ended"))
	if runErr != nil {
		cancel()
		<-saveErr
		return runErr
	}
	return <-saveErr
}

func (p *pusher) registry(ctx context.Context, h remote.Host, imgs []string) error {
	r := p.o.Registry
	if r == nil {
		return errors.New("no registry configured")
	}
	for _, img := range imgs {
		if !strings.HasPrefix(img, strings.TrimSuffix(firstNonEmpty(r.Prefix, r.Server), "/")+"/") {
			return fmt.Errorf("image %s is not under registry %s", img, firstNonEmpty(r.Prefix, r.Server))
		}
	}
	p.regOnce.Do(func() {
		if err := p.login(ctx, nil); err != nil {
			p.regErr = err
			return
		}
		for _, img := range p.o.Images {
			if err := p.o.Exec(ctx, Command{Argv: []string{"docker", "push", img}, Stdout: p.out, Stderr: p.out}); err != nil {
				p.regErr = fmt.Errorf("docker push %s: %w", img, err)
				return
			}
		}
	})
	if p.regErr != nil {
		return p.regErr
	}
	if err := p.login(ctx, h); err != nil {
		return err
	}
	script := "set -eu"
	for _, img := range imgs {
		script += "\ndocker pull " + remote.Quote(img)
	}
	return h.Run(ctx, remote.Cmd{Script: script, Stdout: p.out, Stderr: p.out})
}

// login runs docker login locally (h nil) or on h, password on stdin only.
func (p *pusher) login(ctx context.Context, h remote.Host) error {
	r := p.o.Registry
	if r.Username == "" || p.o.RegistryPassword == "" {
		return nil // public or already logged in
	}
	server := r.Server
	if server == "" {
		server, _, _ = strings.Cut(r.Prefix, "/")
	}
	args := []string{"docker", "login", server, "--username", r.Username, "--password-stdin"}
	if h == nil {
		var stderr bytes.Buffer
		if err := p.o.Exec(ctx, Command{Argv: args, Stdin: strings.NewReader(p.o.RegistryPassword), Stdout: io.Discard, Stderr: &stderr}); err != nil {
			return fmt.Errorf("docker login %s: %w %s", server, err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	return h.Run(ctx, remote.Cmd{Script: remote.QuoteArgs(args...) + " >/dev/null", Stdin: strings.NewReader(p.o.RegistryPassword)})
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func osExec(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	return cmd.Run()
}
