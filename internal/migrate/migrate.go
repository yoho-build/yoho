package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Labels Yoho sets on compiled Services (internal/deploy).
const (
	labelApp         = "yoho.app"
	labelDestination = "yoho.destination"
	labelProject     = "com.docker.compose.project"
)

// Options configure a cutover.
type Options struct {
	App         string
	Destination string
	Migrate     config.Migrate
	// CF is the tunnel API; nil when migrate.tunnel is not set.
	CF CloudflareAPI
	// AllowEmpty accepts a stop selection that matches no running container.
	AllowEmpty bool
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Conflict is a running container that mounts claimed data.
type Conflict struct {
	Container string `json:"container"`
	// Claim is the claimed path or volume; Mount what the container mounts.
	Claim string `json:"claim"`
	Mount string `json:"mount"`
}

func (c Conflict) String() string {
	if c.Claim == c.Mount {
		return c.Container + " mounts " + c.Claim
	}
	return c.Container + " mounts " + c.Mount + " (claimed " + c.Claim + ")"
}

// Plan is what a preflight found. It changes nothing.
type Plan struct {
	o Options
	// Containers the cutover stops (running now, or saved in the Journal
	// when resuming).
	Containers []Container
	// Conflicts: containers outside the stop selection that mount claimed
	// data now; the claim check after stopping would fail.
	Conflicts []Conflict
	// Rules are the current ingress rules of the listed hostnames.
	Rules []IngressRule
}

// Preflight checks everything the cutover needs before it changes
// anything: the tunnel API token and configuration (every hostname has a
// rule), the old containers to stop, and that only those mount the claimed
// data. j is the Journal when resuming (its saved containers are used),
// else nil. The returned Plan holds what was found even when err is set.
func Preflight(ctx context.Context, h remote.Host, o Options, j *Journal) (*Plan, error) {
	p := &Plan{o: o}
	var errs []error
	if t := o.Migrate.Tunnel; t != nil {
		if o.CF == nil {
			return p, errors.New("migrate.tunnel is set but no Cloudflare API client was configured")
		}
		cfg, err := o.CF.GetConfig(ctx)
		if err != nil {
			errs = append(errs, err)
		} else if missing, err := MissingHostnames(cfg, t.Hostnames); err != nil {
			errs = append(errs, err)
		} else {
			if len(missing) > 0 {
				errs = append(errs, fmt.Errorf("tunnel has no ingress rule for %s; add it in Cloudflare (pointing at the old service) or remove it from migrate.tunnel.hostnames", strings.Join(missing, ", ")))
			}
			rules, _ := Rules(cfg)
			want := hostSet(t.Hostnames)
			for _, r := range rules {
				if r.Hostname != "" && want[strings.ToLower(r.Hostname)] {
					p.Rules = append(p.Rules, r)
				}
			}
		}
	}
	if j != nil {
		p.Containers = append([]Container(nil), j.Stopped...)
	} else {
		cs, err := matchContainers(ctx, h, o.Migrate.Stop)
		switch {
		case err != nil:
			errs = append(errs, err)
		case len(cs) == 0 && !o.AllowEmpty:
			errs = append(errs, errors.New("migrate.stop matches no running container; check the selectors with `docker ps`, or pass --cutover-allow-empty when the old stack is already stopped"))
		}
		p.Containers = cs
		stopping := map[string]bool{}
		for _, c := range cs {
			stopping[c.ID] = true
		}
		conflicts, err := claimConflicts(ctx, h, o.App, o.Destination, o.Migrate.Claim, stopping)
		if err != nil {
			errs = append(errs, err)
		}
		p.Conflicts = conflicts
		if len(conflicts) > 0 {
			errs = append(errs, conflictError(conflicts, "is not in migrate.stop"))
		}
	}
	return p, errors.Join(errs...)
}

func conflictError(cs []Conflict, why string) error {
	var parts []string
	for _, c := range cs {
		parts = append(parts, c.String())
	}
	return fmt.Errorf("claimed data is in use: %s; the container %s, so the old and new stacks would share data; add it to migrate.stop or stop it first", strings.Join(parts, "; "), why)
}

// Stop records the containers in the Journal, then stops them (docker stop
// by ID, never rm). Containers already stopped or gone are skipped, so a
// rerun is a no-op.
func (p *Plan) Stop(ctx context.Context, h remote.Host, j *Journal) error {
	if len(j.Stopped) == 0 {
		j.Stopped = append([]Container{}, p.Containers...)
	}
	if j.StoppedAt == nil {
		t := p.o.now().UTC()
		j.StoppedAt = &t
	}
	if err := j.Save(ctx, h); err != nil {
		return err
	}
	if len(j.Stopped) == 0 {
		return nil
	}
	var ids []string
	for _, c := range j.Stopped {
		ids = append(ids, remote.Quote(c.ID))
	}
	script := "set -eu\nfor id in " + strings.Join(ids, " ") + "; do\n" +
		"  if docker inspect --type container \"$id\" >/dev/null 2>&1; then docker stop \"$id\" >/dev/null; fi\n" +
		"done"
	if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
		return fmt.Errorf("stop old containers: %w", err)
	}
	return nil
}

// CheckClaims fails when a running container other than this App
// Destination's mounts a claimed path or volume read-write: the path itself,
// a directory inside it or one above it.
func CheckClaims(ctx context.Context, h remote.Host, app, destination string, claim config.MigrateClaim) error {
	conflicts, err := claimConflicts(ctx, h, app, destination, claim, nil)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return conflictError(conflicts, "is still running after stop (a restart policy or the old platform may have started it again)")
	}
	return nil
}

// CheckClaims is CheckClaims for the Plan's App Destination.
func (p *Plan) CheckClaims(ctx context.Context, h remote.Host) error {
	return CheckClaims(ctx, h, p.o.App, p.o.Destination, p.o.Migrate.Claim)
}

// SwitchIngress repoints the listed hostnames to the configured service.
// The configuration before the switch is saved in the Journal before the
// PUT (once: a resumed switch keeps the original). An ingress that is
// already switched is not written again. It reports whether it wrote.
func (p *Plan) SwitchIngress(ctx context.Context, h remote.Host, j *Journal) (bool, error) {
	t := p.o.Migrate.Tunnel
	if t == nil {
		return false, nil
	}
	live, err := p.o.CF.GetConfig(ctx)
	if err != nil {
		return false, err
	}
	if len(j.TunnelBefore) == 0 {
		j.TunnelBefore = live
		j.Hostnames = append([]string(nil), t.Hostnames...)
		if err := j.Save(ctx, h); err != nil {
			return false, err
		}
	}
	want, err := RewriteIngress(live, t.Hostnames, t.TargetService())
	if err != nil {
		return false, err
	}
	wrote := false
	if Hash(want) != Hash(live) {
		if err := p.o.CF.PutConfig(ctx, want); err != nil {
			return false, err
		}
		wrote = true
	}
	after, err := p.o.CF.GetConfig(ctx)
	if err != nil {
		return wrote, err
	}
	if check, err := RewriteIngress(after, t.Hostnames, t.TargetService()); err != nil || Hash(check) != Hash(after) {
		return wrote, errors.New("Cloudflare did not keep the new ingress service; check the tunnel in Cloudflare Zero Trust")
	}
	j.TunnelAfterHash = Hash(after)
	return wrote, j.Save(ctx, h)
}

// Finish marks the Journal done.
func (p *Plan) Finish(ctx context.Context, h remote.Host, j *Journal) error {
	t := p.o.now().UTC()
	j.State, j.DoneAt = StateDone, &t
	return j.Save(ctx, h)
}

// DriftError refuses a rollback when the ingress changed since the cutover.
type DriftError struct{ Live, Want string }

func (e *DriftError) Error() string {
	return "the tunnel ingress changed since the cutover (configuration " + e.Live + ", expected " + e.Want + "); restoring the saved one would undo that change. Check it in Cloudflare, then rerun with --force"
}

// RollbackOptions configure Rollback.
type RollbackOptions struct {
	// Force restores the saved ingress even when it changed since.
	Force bool
	Now   func() time.Time
}

// Rollback undoes a cutover: it stops this App Destination's containers
// (by compose project and yoho.* labels; never removed, volumes kept),
// starts the saved old containers and restores the saved ingress. Before
// changing anything it refuses when the live ingress is neither the one
// the cutover left nor the saved one, unless Force. A rolled-back Journal
// is a no-op.
func Rollback(ctx context.Context, h remote.Host, cf CloudflareAPI, j *Journal, o RollbackOptions) error {
	if j.State == StateRolledBack {
		return nil
	}
	restore := false
	if len(j.TunnelBefore) > 0 {
		if cf == nil {
			return errors.New("the cutover switched tunnel ingress but migrate.tunnel is no longer configured; restore the Destination's migrate.tunnel settings to roll back")
		}
		live, err := cf.GetConfig(ctx)
		if err != nil {
			return err
		}
		lh, bh := Hash(live), Hash(j.TunnelBefore)
		restore = lh != bh
		if restore && !o.Force && lh != j.TunnelAfterHash {
			want := j.TunnelAfterHash
			if want == "" {
				want = bh
			}
			return &DriftError{Live: lh, Want: want}
		}
	}
	project := release.ProjectName(j.App, j.Destination)
	stop := "set -eu\nids=$(docker ps -q --no-trunc --filter " + remote.Quote("label="+labelProject+"="+project) +
		" --filter " + remote.Quote("label="+labelApp+"="+j.App) + " --filter " + remote.Quote("label="+labelDestination+"="+j.Destination) + ")\n" +
		"[ -z \"$ids\" ] || docker stop $ids >/dev/null"
	if err := h.Run(ctx, remote.Cmd{Script: stop}); err != nil {
		return fmt.Errorf("stop %s: %w", project, err)
	}
	if len(j.Stopped) > 0 {
		var ids []string
		for _, c := range j.Stopped {
			ids = append(ids, remote.Quote(c.ID))
		}
		if err := h.Run(ctx, remote.Cmd{Script: "set -eu\ndocker start " + strings.Join(ids, " ") + " >/dev/null"}); err != nil {
			return fmt.Errorf("start old containers: %w", err)
		}
	}
	if restore {
		if err := cf.PutConfig(ctx, j.TunnelBefore); err != nil {
			return err
		}
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	t := now().UTC()
	j.State, j.RolledBackAt = StateRolledBack, &t
	return j.Save(ctx, h)
}

// matchContainers lists running containers matching any stop selector,
// sorted by name. Yoho's own containers are refused.
func matchContainers(ctx context.Context, h remote.Host, s config.MigrateStop) ([]Container, error) {
	var filters []string
	if s.ComposeProject != "" {
		filters = append(filters, "label="+labelProject+"="+s.ComposeProject)
	}
	for _, l := range s.Labels {
		filters = append(filters, "label="+l)
	}
	for _, n := range s.Containers {
		filters = append(filters, "name=^/"+regexp.QuoteMeta(n)+"$")
	}
	seen := map[string]Container{}
	var yoho []string
	for _, f := range filters {
		out, err := h.Output(ctx, remote.Cmd{Script: "docker ps --no-trunc --filter " + remote.Quote(f) + ` --format '{{.ID}}|{{.Names}}|{{.Label "` + labelApp + `"}}'`})
		if err != nil {
			return nil, fmt.Errorf("list old containers: %w", err)
		}
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
			if len(parts) < 2 || parts[0] == "" {
				continue
			}
			name := parts[1]
			if (len(parts) == 3 && parts[2] != "") || strings.HasPrefix(name, "yoho-") {
				yoho = append(yoho, name)
				continue
			}
			seen[parts[0]] = Container{ID: parts[0], Name: name}
		}
	}
	if len(yoho) > 0 {
		sort.Strings(yoho)
		return nil, fmt.Errorf("migrate.stop matches Yoho containers (%s); select only the old platform's containers", strings.Join(yoho, ", "))
	}
	out := make([]Container, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	return out, nil
}

// inspectFormat is one JSON line per running container.
const inspectFormat = `{"id":{{json .Id}},"name":{{json .Name}},"app":{{json (index .Config.Labels "` + labelApp + `")}},"destination":{{json (index .Config.Labels "` + labelDestination + `")}},"mounts":{{json .Mounts}}}`

type inspected struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	App         string `json:"app"`
	Destination string `json:"destination"`
	Mounts      []struct {
		Type   string `json:"Type"`
		Name   string `json:"Name"`
		Source string `json:"Source"`
		RW     bool   `json:"RW"`
	} `json:"mounts"`
}

// claimConflicts lists mounts of claimed data by running containers,
// except this App Destination's and those in skip (by ID).
func claimConflicts(ctx context.Context, h remote.Host, app, destination string, claim config.MigrateClaim, skip map[string]bool) ([]Conflict, error) {
	if len(claim.Paths) == 0 && len(claim.Volumes) == 0 {
		return nil, nil
	}
	script := "set -eu\nids=$(docker ps -q --no-trunc)\n[ -z \"$ids\" ] || docker inspect --format " + remote.Quote(inspectFormat) + " $ids"
	out, err := h.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return nil, fmt.Errorf("inspect running containers: %w (rerun: a container may have exited meanwhile)", err)
	}
	vols := map[string]bool{}
	for _, v := range claim.Volumes {
		vols[v] = true
	}
	var conflicts []Conflict
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c inspected
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("inspect running containers: unexpected output %q", line)
		}
		if skip[c.ID] || (c.App == app && c.Destination == destination) {
			continue
		}
		name := strings.TrimPrefix(c.Name, "/")
		for _, m := range c.Mounts {
			if !m.RW {
				// Read-only mounts (exporters, backup readers) cannot corrupt the data.
				continue
			}
			if m.Type == "volume" && vols[m.Name] {
				conflicts = append(conflicts, Conflict{Container: name, Claim: m.Name, Mount: m.Name})
				continue
			}
			if m.Source == "" || !strings.HasPrefix(m.Source, "/") {
				continue
			}
			src := path.Clean(m.Source)
			for _, p := range claim.Paths {
				if within(src, p) || within(p, src) {
					conflicts = append(conflicts, Conflict{Container: name, Claim: p, Mount: src})
				}
			}
		}
	}
	return conflicts, nil
}

func within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}
