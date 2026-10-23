package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/transport"
	"github.com/yoho-build/yoho/internal/version"
)

var (
	serviceRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	versionRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// maxLogLines bounds a non-following logs request.
const maxLogLines = 10000

func (c *call) project() string { return release.ProjectName(c.req.App, c.req.Destination) }

// settings reads the operator Settings, required for deploy and rollback.
func (c *call) settings() (*Settings, error) {
	b, err := os.ReadFile(SettingsPath(c.req.App, c.req.Destination))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &remote.GateError{
			Message: "the operator has not pushed settings and secrets for scoped deploys of " + c.req.App + "/" + c.req.Destination,
			Hint:    "ask the operator to run `yoho access secrets push -d " + c.req.Destination + "`",
			Code:    "failed",
		}
	}
	if err != nil {
		return nil, err
	}
	return ParseSettings(b)
}

// HelloResult is what hello returns.
type HelloResult struct {
	Protocol    int        `json:"protocol"`
	Version     string     `json:"version"`
	Member      string     `json:"member"`
	App         string     `json:"app"`
	Destination string     `json:"destination"`
	Verbs       []string   `json:"verbs"`
	Platform    string     `json:"platform,omitempty"`
	Apps        []AppGrant `json:"apps"`
	// Rollouts: deploy and rollback accept Rollout and Attach, so a client
	// can reconnect after its SSH session drops.
	Rollouts bool `json:"rollouts,omitempty"`
}

func hello(ctx context.Context, c *call) (any, error) {
	members, err := loadMembers(c.req.App, c.req.Destination)
	if err != nil {
		return nil, err
	}
	res := HelloResult{
		Protocol: remote.GateProtocol, Version: c.o.Version, Member: c.o.Member,
		App: c.req.App, Destination: c.req.Destination, Verbs: members.Members[c.o.Member].Verbs,
		Apps: grantsOf(c.o.Member), Rollouts: true,
	}
	if p, err := build.ServerPlatform(ctx, c.host); err == nil {
		res.Platform = p
	}
	return res, nil
}

func ps(ctx context.Context, c *call) (any, error) {
	if _, err := c.ownContainers(ctx); err != nil {
		return nil, err
	}
	script := "docker compose -p " + remote.Quote(c.project()) + " ps -a --format 'table {{.Service}}\\t{{.Name}}\\t{{.Status}}\\t{{.Image}}'"
	return nil, c.host.Run(ctx, remote.Cmd{Script: script, Stdout: c.fw})
}

// ServicesArgs selects Services.
type ServicesArgs struct {
	Services []string `json:"services,omitempty"`
}

func checkServices(svcs []string) error {
	for _, s := range svcs {
		if !serviceRe.MatchString(s) {
			return invalid(fmt.Sprintf("invalid Service name %q", s))
		}
	}
	return nil
}

// StatsArgs are the stats arguments.
type StatsArgs struct {
	ServicesArgs
	JSON bool `json:"json,omitempty"`
}

// StatsFormat is the table `yoho app stats` prints.
const StatsFormat = `table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}`

func stats(ctx context.Context, c *call) (any, error) {
	var a StatsArgs
	if err := c.args(&a); err != nil {
		return nil, err
	}
	if err := checkServices(a.Services); err != nil {
		return nil, err
	}
	c.audit["services"] = a.Services
	format := StatsFormat
	if a.JSON {
		format = "{{json .}}"
	}
	// Container IDs pinned after the ownership check.
	all, err := c.ownContainers(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ct := range all {
		if ct[2] == "running" && (len(a.Services) == 0 || slices.Contains(a.Services, ct[1])) {
			ids = append(ids, ct[0])
		}
	}
	if len(ids) == 0 {
		return nil, &remote.GateError{Message: "no running containers for " + c.req.App, Hint: "check `yoho app ps`", Code: "no-containers"}
	}
	return nil, c.host.Run(ctx, remote.Cmd{Script: "exec docker stats --no-stream --format " + remote.Quote(format) + " " + remote.QuoteArgs(ids...), Stdout: c.fw})
}

// LogsArgs are the logs arguments. Container names one container of the
// App (the dashboards); otherwise Services (none: every Service).
type LogsArgs struct {
	ServicesArgs
	Container  string `json:"container,omitempty"`
	Lines      int    `json:"lines,omitempty"`
	Follow     bool   `json:"follow,omitempty"`
	Timestamps bool   `json:"timestamps,omitempty"`
}

func logs(ctx context.Context, c *call) (any, error) {
	var a LogsArgs
	if err := c.args(&a); err != nil {
		return nil, err
	}
	if err := checkServices(a.Services); err != nil {
		return nil, err
	}
	if a.Lines <= 0 {
		a.Lines = 100
	}
	if a.Lines > maxLogLines {
		a.Lines = maxLogLines
	}
	c.audit["services"], c.audit["lines"], c.audit["follow"] = a.Services, a.Lines, a.Follow
	if a.Follow {
		c.watchStdin()
	}
	flags := " --tail " + strconv.Itoa(a.Lines)
	if a.Follow {
		flags += " -f"
	}
	if a.Timestamps {
		flags += " --timestamps"
	}
	var script string
	if a.Container != "" {
		c.audit["container"] = a.Container
		if !containerRe.MatchString(a.Container) {
			return nil, invalid(fmt.Sprintf("invalid container name %q", a.Container))
		}
		// Only the App's own containers: the Proxy and other Apps' logs
		// carry other Apps' traffic.
		info, err := c.host.Output(ctx, remote.Cmd{Script: "docker inspect --type container -f " +
			remote.Quote(`{{.Id}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "`+deploy.LabelApp+`"}}|{{index .Config.Labels "`+deploy.LabelDestination+`"}}`) +
			" " + remote.Quote(a.Container) + " 2>/dev/null || true"})
		if err != nil {
			return nil, err
		}
		id, owner, _ := strings.Cut(strings.TrimSpace(info), "|")
		if id == "" || owner != c.project()+"|"+c.req.App+"|"+c.req.Destination {
			return nil, denied("container "+a.Container+" is not part of "+c.req.App+"/"+c.req.Destination, "")
		}
		script = "docker logs" + flags + " " + remote.Quote(id) + " 2>&1"
	} else {
		if _, err := c.ownContainers(ctx); err != nil {
			return nil, err
		}
		script = "docker compose -p " + remote.Quote(c.project()) + " logs" + flags
		if len(a.Services) > 0 {
			script += " " + remote.QuoteArgs(a.Services...)
		}
		script += " 2>&1"
	}
	err := c.host.Run(ctx, remote.Cmd{Script: script, Stdout: c.fw})
	if a.Follow && ctx.Err() != nil {
		return nil, nil
	}
	return nil, err
}

func dashVerb(ctx context.Context, c *call) (any, error) {
	if _, err := c.ownContainers(ctx); err != nil {
		return nil, err
	}
	name := c.host.Name()
	if s, err := c.settings(); err == nil && s.Server != "" {
		name = s.Server
	}
	src := &dash.Source{
		App: c.req.App, Destination: c.req.Destination, Runtime: "compose", Root: release.Root,
		Targets: []dash.Target{{Name: name, Host: c.host}},
		Releases: func(ctx context.Context, h remote.Host) ([]release.Release, error) {
			return deploy.ListReleases(ctx, h, c.req.App, c.req.Destination)
		},
	}
	snap, err := dash.Collect(ctx, src)
	if err != nil {
		return nil, err
	}
	return snap.Servers[0], nil
}

func releasesVerb(ctx context.Context, c *call) (any, error) {
	rels, err := deploy.ListReleases(ctx, c.host, c.req.App, c.req.Destination)
	if err != nil {
		return nil, err
	}
	for i := range rels {
		rels[i].Secrets = nil // fingerprints stay on the Server
	}
	if rels == nil {
		rels = []release.Release{}
	}
	return rels, nil
}

// ImagesArgs asks which built images the Server lacks.
type ImagesArgs struct {
	// Image reference -> transport.Fingerprint of the local image.
	Images map[string]string `json:"images"`
}

// imagePrefix is the repository prefix of every image Yoho builds for the
// App Destination (build.ImageName without Service and Version).
func imagePrefix(s *Settings, app, dest string) string {
	n := build.ImageName(s.Registry, app, dest, "x", "v")
	return strings.TrimSuffix(n, "x:v")
}

func imagesVerb(ctx context.Context, c *call) (any, error) {
	var a ImagesArgs
	if err := c.args(&a); err != nil {
		return nil, err
	}
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	prefix := imagePrefix(s, c.req.App, c.req.Destination)
	missing := []string{}
	for _, ref := range sortedKeys(a.Images) {
		if !strings.HasPrefix(ref, prefix) {
			return nil, denied(fmt.Sprintf("image %s is not one Yoho builds for %s/%s (%s...)", ref, c.req.App, c.req.Destination, prefix), "")
		}
		fp, err := c.host.Output(ctx, remote.Cmd{Script: "docker image inspect --format " + remote.Quote(transport.Fingerprint) + " " + remote.Quote(ref) + " 2>/dev/null || true"})
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(fp) != a.Images[ref] {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// DeployArgs is a scoped deploy: the resolved compose project (images set,
// build sections removed, see composefile.Resolved) and what the CLI built.
type DeployArgs struct {
	Version string `json:"version"`
	Compose string `json:"compose"`
	// Services Yoho built for this Version.
	Built []string `json:"built"`
	// An image archive (docker save, gzipped) follows the request line.
	Archive bool `json:"archive,omitempty"`
	// Built images to pull from the Registry instead.
	Pull []string `json:"pull,omitempty"`
	// Performer as the CLI sees it (user@host), recorded next to the Member.
	Performer string `json:"performer,omitempty"`
	// Rollout is an id the CLI picks for this deploy (sent when hello
	// reports Rollouts); Attach reconnects to that rollout, running or
	// finished, instead of starting a new one.
	Rollout string `json:"rollout,omitempty"`
	Attach  bool   `json:"attach,omitempty"`
}

func deployVerb(ctx context.Context, c *call) (any, error) {
	var a DeployArgs
	if err := c.args(&a); err != nil {
		return nil, err
	}
	c.audit["version"], c.audit["built"], c.audit["archive"] = a.Version, a.Built, a.Archive
	c.audit["compose_sha256"] = deploy.SHA256Hex([]byte(a.Compose))
	if !versionRe.MatchString(a.Version) {
		return nil, invalid(fmt.Sprintf("invalid version %q", a.Version))
	}
	for _, svc := range a.Built {
		if !serviceRe.MatchString(svc) {
			return nil, invalid(fmt.Sprintf("invalid Service name %q", svc))
		}
	}
	if !c.o.RolloutWorker {
		if res, err, ok := c.checkRollout(ctx, VerbDeploy, a.Rollout, a.Attach); ok {
			return res, err
		}
	}
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	if err := collisions(c.req.App, c.req.Destination, a.Built); err != nil {
		return nil, err
	}
	r, pol, err := c.vet(ctx, s, []byte(a.Compose), a.Version, a.Built)
	if err != nil {
		return nil, err
	}
	pol.Harden(r.Project)
	if err := c.checkImages(ctx, pol, r.Project); err != nil {
		return nil, err
	}
	if err := c.checkVolumes(ctx, r.Project); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, ref := range pol.Built {
		allowed[NormalizeRef(ref)] = true
	}
	if a.Archive {
		if err := c.loadArchive(ctx, s.Limits, allowed); err != nil {
			return nil, err
		}
	}
	for _, ref := range a.Pull {
		if !allowed[NormalizeRef(ref)] {
			return nil, denied("pull "+ref+": not an image built for this deploy", "")
		}
		fmt.Fprintf(c.fw, "pulling %s\n", ref)
		if err := c.host.Run(ctx, remote.Cmd{Script: "docker pull -q " + remote.Quote(ref), Stdout: io.Discard, Stderr: c.fw}); err != nil {
			return nil, fmt.Errorf("pull %s: %w", ref, err)
		}
	}
	if err := c.pullImages(ctx, pol, r.Project); err != nil {
		return nil, err
	}
	for _, svc := range sortedKeys(pol.Built) {
		ref := pol.Built[svc]
		if err := c.host.Run(ctx, remote.Cmd{Script: "docker image inspect " + remote.Quote(ref) + " >/dev/null 2>&1"}); err != nil {
			return nil, &remote.GateError{Message: "image " + ref + " is not on the Server", Hint: "deploy again; the CLI ships missing images", Code: "failed"}
		}
	}

	if !c.o.RolloutWorker {
		id, err := c.startRollout(VerbDeploy, a.Version, a.Rollout, a)
		if err != nil {
			return nil, err
		}
		return c.attachRollout(ctx, id)
	}
	return c.finishDeploy(ctx, s, r, a)
}

// finishDeploy is the detached worker: secrets, compose rollout, vetted record.
func (c *call) finishDeploy(ctx context.Context, s *Settings, r *composefile.Result, a DeployArgs) (any, error) {
	svcSecrets, gen, err := storedSecrets(c.req.App, c.req.Destination, r.Ext)
	if err != nil {
		return nil, err
	}
	c.audit["secrets_generation"] = gen
	var values []string
	for _, m := range svcSecrets {
		for _, v := range m {
			values = append(values, v)
		}
	}
	out := secrets.NewRedactor(c.fw, values...)
	defer out.Flush()
	c.clearStaleLock()
	d := &plan.Deploy{
		App: c.req.App, Destination: c.req.Destination, Version: a.Version, Performer: c.performer(a.Performer),
		Servers: []plan.NamedHost{c.namedHost(s)}, Project: r.Project, Ext: r.Ext, Built: slices.Sorted(slices.Values(a.Built)),
		ServiceSecrets: svcSecrets, Registry: s.Registry, Proxy: s.Proxy, RetainReleases: s.RetainReleases, Out: out,
		Scoped: c.scoped(nil),
	}
	rel, err := composeDeploy(ctx, d)
	if err != nil {
		return nil, secrets.RedactError(lockedError(err), values...)
	}
	if err := c.recordVetted(ctx, a, rel); err != nil {
		fmt.Fprintf(c.fw, "warning: %v (rolling back to %s through the Gate will be refused)\n", err, a.Version)
	}
	rel.Secrets = nil
	return rel, nil
}

// composeDeploy is deploy.Compose.Deploy; tests replace it to drop the client mid-rollout.
var composeDeploy = func(ctx context.Context, d *plan.Deploy) (*release.Release, error) {
	return deploy.Compose{}.Deploy(ctx, d)
}

var composeRollback = func(ctx context.Context, d *plan.Deploy, version string) (*release.Release, error) {
	return deploy.Compose{}.Rollback(ctx, d, version)
}

// vet loads a Member's resolved compose project and checks it against the
// policy for version (Built: the Services built for it).
func (c *call) vet(ctx context.Context, s *Settings, compose []byte, version string, built []string) (*composefile.Result, Policy, error) {
	pol := Policy{Project: c.project(), Built: map[string]string{}, Routes: s.Routes, Reserved: reservedPrefixes(s), Limits: s.Limits}
	if err := CheckRaw(compose); err != nil {
		return nil, pol, refused(err)
	}
	r, err := composefile.LoadResolved(ctx, compose, release.AppDir(c.req.App, c.req.Destination), c.project())
	if err != nil {
		return nil, pol, invalid("compose: " + err.Error())
	}
	for _, svc := range built {
		pol.Built[svc] = build.ImageName(s.Registry, c.req.App, c.req.Destination, svc, version)
	}
	if err := pol.Check(r.Project, r.Ext); err != nil {
		return nil, pol, refused(err)
	}
	return r, pol, nil
}

// proxyNetwork is the App's own network to the Proxy.
func (c *call) proxyNetwork() string { return c.project() + "-proxy" }

// scoped is the runtime's view of this Gate run.
func (c *call) scoped(vet func([]byte, *release.Release) error) *plan.Scoped {
	return &plan.Scoped{PID: os.Getpid(), Boot: c.o.Boot, ProxyNetwork: c.proxyNetwork(), Vet: vet}
}

func (c *call) clearStaleLock() {
	if ok, err := ClearStaleLock(c.req.App, c.req.Destination, c.o.Boot); err != nil {
		fmt.Fprintf(c.fw, "warning: %v\n", err)
	} else if ok {
		fmt.Fprintf(c.fw, "cleared the deploy lock of a Gate run that is no longer running\n")
		c.audit["cleared_stale_lock"] = true
	}
}

// Vetted records what the Gate checked for a Release it deployed, so a
// rollback can check it again: the Member's resolved compose and built
// Services, and the compiled compose.yaml the runtime wrote.
type Vetted struct {
	Version       string   `json:"version"`
	Compose       string   `json:"compose"`
	Built         []string `json:"built"`
	ComposeSHA256 string   `json:"compose_sha256"`
}

// vettedPath is <access>/vetted/<version>.json (root-owned).
func vettedPath(app, dest, version string) string {
	return filepath.Join(release.AccessDir(app, dest), "vetted", version+".json")
}

func (c *call) recordVetted(ctx context.Context, a DeployArgs, rel *release.Release) error {
	b, err := json.Marshal(Vetted{Version: a.Version, Compose: a.Compose, Built: slices.Sorted(slices.Values(a.Built)), ComposeSHA256: rel.ComposeSHA256})
	if err != nil {
		return err
	}
	p := vettedPath(c.req.App, c.req.Destination, a.Version)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	// Forget Releases that retention removed.
	rels, err := deploy.ListReleases(ctx, c.host, c.req.App, c.req.Destination)
	if err != nil {
		return nil
	}
	keep := map[string]bool{}
	for _, r := range rels {
		keep[r.Version+".json"] = true
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.Remove(filepath.Join(filepath.Dir(p), e.Name()))
		}
	}
	return nil
}

// refused wraps policy errors with the scoped-access hint.
func refused(err error) error {
	return &remote.GateError{
		Message: "the Gate refused this deploy:\n  " + strings.ReplaceAll(err.Error(), "\n", "\n  "),
		Hint:    "scoped access only deploys what stays inside the App; ask the operator to deploy this change",
		Code:    "denied",
	}
}

func (c *call) performer(client string) string {
	p := c.o.Member + " (gate)"
	if client = strings.TrimSpace(client); client != "" && len(client) < 128 && !strings.ContainsAny(client, "\n\r") {
		p += " " + client
	}
	return p
}

func (c *call) namedHost(s *Settings) plan.NamedHost {
	name := s.Server
	if name == "" {
		name = c.host.Name()
	}
	return plan.NamedHost{Name: name, Host: c.host}
}

// loadArchive stores the image archive that follows the request (bounded
// in size and time, one upload per App Destination at a time), checks that
// it only names images built for this deploy, and loads it.
func (c *call) loadArchive(ctx context.Context, l Limits, allowed map[string]bool) error {
	l = l.WithDefaults()
	access := release.AccessDir(c.req.App, c.req.Destination)
	lock, err := os.OpenFile(filepath.Join(access, "upload.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("image upload: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return &remote.GateError{Message: "another image upload for " + c.req.App + "/" + c.req.Destination + " is in progress", Hint: "wait for it to finish and deploy again", Code: "locked"}
	}
	// Holding the lock, leftovers of uploads that died are stale.
	if old, _ := filepath.Glob(filepath.Join(access, ".gate-upload-*")); len(old) > 0 {
		for _, o := range old {
			os.RemoveAll(o)
		}
	}
	dir, err := os.MkdirTemp(access, ".gate-upload-")
	if err != nil {
		return fmt.Errorf("image upload: %w", err)
	}
	defer os.RemoveAll(dir)
	f, err := os.OpenFile(filepath.Join(dir, "images.tar"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(c.fw, "receiving images\n")
	n, err := receive(f, remote.GatePayload(c.payload), l.MaxUploadBytes, time.Duration(l.UploadTimeoutSeconds)*time.Second)
	c.audit["archive_bytes"] = n
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tags, err := ArchiveTags(f, l.MaxUploadBytes)
	if errors.Is(err, ErrArchiveTooLarge) {
		return tooLarge(l.MaxUploadBytes)
	}
	if err != nil {
		return invalid(err.Error())
	}
	for _, t := range tags {
		if !allowed[t] {
			return denied("the image archive names "+t+", which is not an image built for this deploy", "")
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	fmt.Fprintf(c.fw, "loading %d image(s)\n", len(tags))
	return c.host.Run(ctx, remote.Cmd{Script: transport.LoadScript, Stdin: f, Stdout: io.Discard})
}

func tooLarge(max int64) error {
	return &remote.GateError{
		Message: "the image archive is larger than the " + byteSize(max) + " the Gate accepts",
		Hint:    "ship smaller images, use a Registry, or ask the operator to raise limits.max_upload_bytes",
		Code:    "denied",
	}
}

func byteSize(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// receive copies at most max bytes of src to dst within timeout.
func receive(dst io.Writer, src io.Reader, max int64, timeout time.Duration) (int64, error) {
	type res struct {
		n   int64
		err error
	}
	ch := make(chan res, 1)
	go func() {
		n, err := io.Copy(dst, io.LimitReader(src, max+1))
		ch <- res{n, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return r.n, fmt.Errorf("receive images: %w", r.err)
		}
		if r.n > max {
			return r.n, tooLarge(max)
		}
		return r.n, nil
	case <-time.After(timeout):
		return 0, &remote.GateError{Message: fmt.Sprintf("the image upload did not finish within %s", timeout), Hint: "use a Registry for large images, or ask the operator to raise limits.upload_timeout_seconds", Code: "failed"}
	}
}

// storedSecrets builds the Services' secrets from the generation the
// operator's last `yoho access secrets push` stored (release.AccessSecretsFile),
// never from the newest generation: every deploy writes one with only what
// it declared. Members never send values. Generated keys are left to the
// runtime.
func storedSecrets(app, dest string, ext map[string]config.ServiceExt) (map[string]map[string]string, string, error) {
	gen, err := accessGeneration(app, dest)
	if err != nil {
		return nil, "", err
	}
	var stored map[string]map[string]string
	if gen != "" {
		if stored, err = deploy.ReadSecretsGeneration(release.SecretsDir(app, dest, gen)); err != nil {
			return nil, "", fmt.Errorf("read secrets generation %s: %w", gen, err)
		}
	}
	generated := map[string]bool{}
	for _, e := range ext {
		for k := range e.Generate {
			generated[k] = true
		}
	}
	out := map[string]map[string]string{}
	var missing []string
	for _, svc := range sortedKeys(ext) {
		for _, ref := range ext[svc].Secrets {
			if v, ok := stored[svc][ref.Name]; ok {
				if out[svc] == nil {
					out[svc] = map[string]string{}
				}
				out[svc][ref.Name] = v
				continue
			}
			if generated[ref.Key] {
				continue
			}
			missing = append(missing, svc+"/"+ref.Name)
		}
	}
	if len(missing) > 0 {
		where := "the operator has not pushed secrets"
		if gen != "" {
			where = "the operator's last push lacks them"
		}
		return nil, gen, &remote.GateError{
			Message: "secrets " + strings.Join(missing, ", ") + " are not stored for scoped deploys (" + where + ")",
			Hint:    "ask the operator to run `yoho access secrets push -d " + dest + "`",
			Code:    "failed",
		}
	}
	return out, gen, nil
}

// accessGeneration reads release.AccessSecretsFile ("" when absent).
func accessGeneration(app, dest string) (string, error) {
	b, err := os.ReadFile(release.AccessSecretsFile(app, dest))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	gen := strings.TrimSpace(string(b))
	if !deploy.ValidGeneration(gen) {
		return "", fmt.Errorf("%s names %q, which is not a secrets generation", release.AccessSecretsFile(app, dest), gen)
	}
	return gen, nil
}

// RollbackArgs selects the Release to redeploy: a version or a unique
// prefix of one.
type RollbackArgs struct {
	Version   string `json:"version"`
	Performer string `json:"performer,omitempty"`
	// Rollout and Attach as in DeployArgs.
	Rollout string `json:"rollout,omitempty"`
	Attach  bool   `json:"attach,omitempty"`
}

func rollbackVerb(ctx context.Context, c *call) (any, error) {
	var a RollbackArgs
	if err := c.args(&a); err != nil {
		return nil, err
	}
	c.audit["version"] = a.Version
	if !versionRe.MatchString(a.Version) {
		return nil, invalid(fmt.Sprintf("invalid version %q", a.Version))
	}
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	rels, err := deploy.ListReleases(ctx, c.host, c.req.App, c.req.Destination)
	if err != nil {
		return nil, err
	}
	version, err := matchRelease(rels, a.Version)
	if err != nil {
		return nil, err
	}
	c.audit["version"] = version
	if !c.o.RolloutWorker {
		if res, err, ok := c.checkRollout(ctx, VerbRollback, a.Rollout, a.Attach); ok {
			return res, err
		}
		a.Version = version
		id, err := c.startRollout(VerbRollback, version, a.Rollout, a)
		if err != nil {
			return nil, err
		}
		return c.attachRollout(ctx, id)
	}
	c.clearStaleLock()
	d := &plan.Deploy{
		App: c.req.App, Destination: c.req.Destination, Performer: c.performer(a.Performer),
		Servers: []plan.NamedHost{c.namedHost(s)}, Registry: s.Registry, Proxy: s.Proxy,
		RetainReleases: s.RetainReleases, Out: c.fw,
		Scoped: c.scoped(func(compose []byte, rel *release.Release) error { return c.vetRollback(ctx, s, version, compose, rel) }),
	}
	rel, err := composeRollback(ctx, d, version)
	if err != nil {
		return nil, lockedError(err)
	}
	rel.Secrets = nil
	return rel, nil
}

// matchRelease resolves a version: exact, then the displayed short form
// (version.Short), then a unique prefix.
func matchRelease(rels []release.Release, v string) (string, error) {
	var short, match []string
	for _, r := range rels {
		if r.Version == v {
			return v, nil
		}
		if version.Short(r.Version) == v {
			short = append(short, r.Version)
		}
		if strings.HasPrefix(r.Version, v) {
			match = append(match, r.Version)
		}
	}
	if len(short) == 1 {
		return short[0], nil
	}
	if len(short) > 1 {
		match = short
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return "", invalid("no Release " + v + "; see `yoho releases`")
	default:
		return "", invalid(fmt.Sprintf("%s matches %d Releases (%s); give more of the version", v, len(match), strings.Join(match, ", ")))
	}
}

// vetRollback checks a Release again before the runtime redeploys it
// (inside the deploy lock): it must be one the Gate deployed, its compiled
// compose must be the one recorded then, and the Member's compose must pass
// today's policy. Releases the operator deployed can do anything an
// operator can, so Members cannot roll back to them.
func (c *call) vetRollback(ctx context.Context, s *Settings, version string, compose []byte, rel *release.Release) error {
	b, err := os.ReadFile(vettedPath(c.req.App, c.req.Destination, version))
	if err != nil {
		return denied("Release "+version+" was not deployed through the Gate, so Members cannot roll back to it", "ask the operator to roll back, or deploy that version again")
	}
	var v Vetted
	if err := json.Unmarshal(b, &v); err != nil || v.Version != version {
		return denied("the Gate's record of Release "+version+" is unreadable", "deploy that version again")
	}
	if v.ComposeSHA256 == "" || v.ComposeSHA256 != deploy.SHA256Hex(compose) || (rel.ComposeSHA256 != "" && rel.ComposeSHA256 != v.ComposeSHA256) {
		return denied("Release "+version+" on the Server is not the one the Gate deployed", "deploy that version again")
	}
	if !slices.Equal(slices.Sorted(slices.Values(rel.Built)), v.Built) {
		return denied("Release "+version+" built other Services than the Gate recorded", "deploy that version again")
	}
	_, pol, err := c.vet(ctx, s, []byte(v.Compose), version, v.Built)
	if err != nil {
		return err
	}
	// The runtime runs the compiled file, hardened under the limits of
	// back then: check it against today's.
	compiled, err := composefile.LoadResolved(ctx, compose, release.AppDir(c.req.App, c.req.Destination), c.project())
	if err != nil {
		return denied("Release "+version+": its compose.yaml does not load: "+err.Error(), "deploy that version again")
	}
	if err := pol.CheckCompiled(compiled.Project); err != nil {
		return &remote.GateError{
			Message: "Release " + version + " breaks today's limits:\n  " + strings.ReplaceAll(err.Error(), "\n", "\n  "),
			Hint:    "deploy that version again; a new deploy applies today's limits",
			Code:    "denied",
		}
	}
	return checkCompiledNetworks(compose, c.proxyNetwork())
}

// checkCompiledNetworks refuses a compiled compose that joins an external
// network other than the App's own Proxy network (Releases from before
// per-App networks joined the shared one).
func checkCompiledNetworks(compose []byte, own string) error {
	var doc struct {
		Networks map[string]struct {
			Name     string `yaml:"name"`
			External bool   `yaml:"external"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(compose, &doc); err != nil {
		return fmt.Errorf("compiled compose: %w", err)
	}
	for k, n := range doc.Networks {
		if n.External && n.Name != own {
			return denied(fmt.Sprintf("the Release joins the shared network %s (%s)", n.Name, k), "deploy that version again; new Releases get their own network")
		}
	}
	return nil
}
