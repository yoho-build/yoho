package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
)

// StoppedFile records the Services `yoho app stop` stopped on a Server. It is
// Server state, not a Release file: deploy, apply and rollback keep a stopped
// Service down until `yoho app start` removes its entry (ADR 0021).
const StoppedFile = "stopped.json"

// StoppedOverrideFile is the Compose override derived from StoppedFile for
// the Release being run: every stopped Service of that Release at zero
// replicas. Yoho passes it as an extra -f; it is removed when empty.
const StoppedOverrideFile = "stopped.compose.yaml"

// StoppedEntry says who stopped a Service, when, and why.
type StoppedEntry struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// Stopped maps Service names to their stop records.
type Stopped map[string]StoppedEntry

// Describe is "who, when[: reason]" for plans and `app ps`.
func (e StoppedEntry) Describe() string {
	s := e.By
	if !e.At.IsZero() {
		s += ", " + e.At.UTC().Format("2006-01-02 15:04 UTC")
	}
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

func stoppedPath(app, dest string) string {
	return path.Join(release.AppDir(app, dest), StoppedFile)
}

func stoppedOverridePath(app, dest string) string {
	return path.Join(release.AppDir(app, dest), StoppedOverrideFile)
}

// ReadStopped reads the stopped Services on h; empty when none. Read-only.
func ReadStopped(ctx context.Context, h remote.Host, app, dest string) (Stopped, error) {
	b, err := h.ReadFile(ctx, stoppedPath(app, dest), false)
	if errors.Is(err, os.ErrNotExist) {
		return Stopped{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", StoppedFile, err)
	}
	s := Stopped{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", StoppedFile, err)
	}
	return s, nil
}

func writeStopped(ctx context.Context, h remote.Host, app, dest string, s Stopped) error {
	if len(s) == 0 {
		return h.Run(ctx, remote.Cmd{Script: "set -eu\nrm -f -- " + remote.Quote(stoppedPath(app, dest))})
	}
	return writeJSON(ctx, h, stoppedPath(app, dest), s)
}

// stoppedOverride is the Compose override for the stopped Services present
// in plans; nil when there are none. A Service absent from the Release must
// not appear: Compose would read it as a new Service without an image.
func stoppedOverride(s Stopped, plans []servicePlan) []byte {
	type zero struct {
		Scale  int `yaml:"scale"`
		Deploy struct {
			Replicas int `yaml:"replicas"`
		} `yaml:"deploy"`
	}
	services := map[string]zero{}
	for _, sp := range plans {
		if _, ok := s[sp.Name]; ok {
			services[sp.Name] = zero{}
		}
	}
	if len(services) == 0 {
		return nil
	}
	b, _ := yaml.Marshal(map[string]any{"services": services})
	return b
}

// syncStopped writes (or removes) the derived override for the Release
// whose plans are being run and returns the stopped Services among them
// and the override path ("" when none). Callers hold the Destination lock.
func syncStopped(ctx context.Context, h remote.Host, app, dest string, plans []servicePlan) (map[string]bool, string, error) {
	s, err := ReadStopped(ctx, h, app, dest)
	if err != nil {
		return nil, "", err
	}
	return writeStoppedOverride(ctx, h, app, dest, s, plans)
}

// writeStoppedOverride writes (or removes) the override of s for plans.
func writeStoppedOverride(ctx context.Context, h remote.Host, app, dest string, s Stopped, plans []servicePlan) (map[string]bool, string, error) {
	override := stoppedOverride(s, plans)
	p := stoppedOverridePath(app, dest)
	if override == nil {
		if err := h.Run(ctx, remote.Cmd{Script: "set -eu\nrm -f -- " + remote.Quote(p)}); err != nil {
			return nil, "", fmt.Errorf("remove %s: %w", StoppedOverrideFile, err)
		}
		return map[string]bool{}, "", nil
	}
	if err := h.WriteFile(ctx, p, override, 0o600, false); err != nil {
		return nil, "", fmt.Errorf("write %s: %w", StoppedOverrideFile, err)
	}
	stopped := map[string]bool{}
	for _, sp := range plans {
		if _, ok := s[sp.Name]; ok {
			stopped[sp.Name] = true
		}
	}
	return stopped, p, nil
}

// applyStopped returns a copy of plans with every stopped Service at zero
// replicas and no release command or check: nothing starts or waits for it.
// The Release's plan.json keeps the declared values.
func applyStopped(plans []servicePlan, stopped map[string]bool) []servicePlan {
	if len(stopped) == 0 {
		return plans
	}
	out := slices.Clone(plans)
	for i := range out {
		if stopped[out[i].Name] {
			out[i].Replicas = 0
			out[i].ReleaseCommand = nil
			out[i].Check = nil
		}
	}
	return out
}

// enforceStopped syncs the override for plans and points r at it.
func enforceStopped(ctx context.Context, r *runner, plans []servicePlan) ([]servicePlan, error) {
	s, err := ReadStopped(ctx, r.h, r.app, r.dest)
	if err != nil {
		return nil, err
	}
	return enforceStoppedSet(ctx, r, s, plans)
}

// enforceStoppedSet is enforceStopped for the stopped set s.
func enforceStoppedSet(ctx context.Context, r *runner, s Stopped, plans []servicePlan) ([]servicePlan, error) {
	stopped, override, err := writeStoppedOverride(ctx, r.h, r.app, r.dest, s, plans)
	if err != nil {
		return nil, err
	}
	r.cc.overrides = nil
	if override != "" {
		r.cc.overrides = []string{override}
		names := sortedKeys(stopped)
		r.logf("keeping %s stopped (`yoho app stop`); `yoho app start` runs it again", strings.Join(names, ", "))
	}
	return applyStopped(plans, stopped), nil
}

// currentPlans reads the current Release's Version and saved plan on h.
func currentPlans(ctx context.Context, h remote.Host, app, dest string) (string, []servicePlan, error) {
	ver, _, err := CurrentServices(ctx, h, app, dest)
	if err != nil {
		return "", nil, err
	}
	b, err := h.ReadFile(ctx, path.Join(release.Dir(app, dest, ver), "plan.json"), false)
	if err != nil {
		return "", nil, err
	}
	var plans []servicePlan
	if err := json.Unmarshal(b, &plans); err != nil {
		return "", nil, fmt.Errorf("parse plan.json of Release %s: %w", ver, err)
	}
	return ver, plans, nil
}

// StopResult is what `yoho app stop` did on one Server.
type StopResult struct {
	// Changed is false when the Service was already stopped and had no
	// containers left.
	Changed bool `json:"changed"`
	// Containers are the removed container IDs.
	Containers []string `json:"containers"`
	// ExitCodes are the removed containers' exit codes after docker stop,
	// by container ID. A clean Postgres shutdown exits 0.
	ExitCodes map[string]int `json:"exit_codes"`
	// AfterStop is StopOptions.AfterStop's output by container ID.
	AfterStop map[string]string `json:"-"`
}

// Clean reports whether every stopped container exited 0.
func (r StopResult) Clean() bool {
	for _, code := range r.ExitCodes {
		if code != 0 {
			return false
		}
	}
	return true
}

// StopOptions change how StopService stops a Service; the zero value is
// plain `yoho app stop`.
type StopOptions struct {
	// AfterStop is a POSIX sh snippet run once per stopped container, with
	// its ID in $id, after docker stop and before docker rm, while the
	// container's volumes are still attached to it. Its stdout is returned
	// in StopResult.AfterStop. When it fails, no container is removed and
	// the stop fails.
	AfterStop string
}

// StopScript stops and removes the Service's containers (never one-offs or
// volumes) and prints "<id> <exit code>" per container. Removal, not only
// stop, keeps `restart: always` and a Docker restart from reviving them.
// The yoho.app and yoho.destination labels keep it to this App
// Destination's containers: two of them can share a compose project name.
func StopScript(app, dest, service string) string {
	return stopScript(app, dest, service, "")
}

func stopScript(app, dest, service, afterStop string) string {
	s := "set -eu\nids=$(docker ps -aq " + serviceFilters(app, dest, service) +
		" --filter label=com.docker.compose.oneoff=False)\n" +
		`[ -n "$ids" ] || exit 0
docker stop $ids >/dev/null
for id in $ids; do printf '%s %s\n' "$id" "$(docker inspect -f '{{.State.ExitCode}}' "$id")"; done
`
	if afterStop != "" {
		s += "for id in $ids; do\n  out=$(\n" + afterStop + "\n) || { echo \"check of stopped container $id failed; containers kept\" >&2; exit 1; }\n" +
			"  printf '%s\\n' \"$out\" | sed \"s/^/$id| /\"\ndone\n"
	}
	return s + "docker rm $ids >/dev/null\n"
}

// serviceFilters are the docker ps filters of service's containers of this
// App Destination.
func serviceFilters(app, dest, service string) string {
	return "--filter " + remote.Quote("label=com.docker.compose.project="+release.ProjectName(app, dest)) +
		" --filter " + remote.Quote("label=com.docker.compose.service="+service) +
		" --filter " + remote.Quote("label="+LabelApp+"="+app) +
		" --filter " + remote.Quote("label="+LabelDestination+"="+dest)
}

// RunningContainers are the IDs of service's running containers of this
// App Destination on h. Read-only.
func RunningContainers(ctx context.Context, h remote.Host, app, dest, service string) ([]string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\ndocker ps -q " + serviceFilters(app, dest, service)})
	if err != nil {
		return nil, fmt.Errorf("list running containers of %s: %w", service, err)
	}
	return lines(out), nil
}

// StopService records service as stopped on h, then stops and removes its
// containers. The record comes first, so an interrupted stop still keeps
// the Service down on the next deploy. Callers hold the Destination lock.
func StopService(ctx context.Context, h remote.Host, app, dest, service string, info StoppedEntry, opts ...StopOptions) (StopResult, error) {
	var o StopOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	res := StopResult{Containers: []string{}, ExitCodes: map[string]int{}, AfterStop: map[string]string{}}
	s, err := ReadStopped(ctx, h, app, dest)
	if err != nil {
		return res, err
	}
	_, was := s[service]
	if !was {
		s[service] = info
		if err := writeStopped(ctx, h, app, dest, s); err != nil {
			return res, err
		}
	}
	if _, plans, err := currentPlans(ctx, h, app, dest); err == nil {
		if _, _, err := syncStopped(ctx, h, app, dest, plans); err != nil {
			return res, err
		}
	}
	out, err := h.Output(ctx, remote.Cmd{Script: stopScript(app, dest, service, o.AfterStop)})
	if err != nil {
		return res, fmt.Errorf("stop %s: %w", service, err)
	}
	for _, l := range lines(out) {
		if id, text, ok := strings.Cut(l, "| "); ok && !strings.Contains(id, " ") {
			if res.AfterStop[id] != "" {
				res.AfterStop[id] += "\n"
			}
			res.AfterStop[id] += text
			continue
		}
		id, code, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(code))
		if err != nil {
			return res, fmt.Errorf("stop %s: exit code of %s: %q", service, id, code)
		}
		res.Containers = append(res.Containers, id)
		res.ExitCodes[id] = n
	}
	res.Changed = !was || len(res.Containers) > 0
	return res, nil
}

// StartError is a started Service that did not come up or did not become
// ready. StillStopped says its entry is still in StoppedFile (compose up
// failed); otherwise the entry is gone.
type StartError struct {
	Service      string
	StillStopped bool
	Err          error
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Hint is the next step after a failed start.
func (e *StartError) Hint() string {
	if e.StillStopped {
		return "`yoho app logs " + e.Service + "`; the Service stays marked stopped; rerun `yoho app start " + e.Service + "`"
	}
	return "`yoho app logs " + e.Service + "`; the Service stays marked started; rerun `yoho app start " + e.Service + "`"
}

// StartService starts service from the current Release's compiled compose
// on d's Server (other stopped Services stay down), removes its stopped
// entry once compose up succeeded, and waits for its readiness. changed is
// false when it was not stopped. Callers hold the Destination lock.
func StartService(ctx context.Context, d *plan.Deploy, service string) (changed bool, err error) {
	h := d.Servers[0].Host
	ver, plans, err := currentPlans(ctx, h, d.App, d.Destination)
	if err != nil {
		return false, err
	}
	var sp *servicePlan
	for i := range plans {
		if plans[i].Name == service {
			sp = &plans[i]
		}
	}
	if sp == nil {
		return false, fmt.Errorf("Service %s is not in the current Release %s", service, ver)
	}
	if err := checkOwnership(ctx, h, d.App, d.Destination, []servicePlan{*sp}); err != nil {
		return false, err
	}
	s, err := ReadStopped(ctx, h, d.App, d.Destination)
	if err != nil {
		return false, err
	}
	_, changed = s[service]
	rest := Stopped{}
	for name, e := range s {
		if name != service {
			rest[name] = e
		}
	}
	r := newRunner(d, path.Join(release.Dir(d.App, d.Destination, ver), "compose.yaml"))
	r.phase = "service-start"
	if _, err := enforceStoppedSet(ctx, r, rest, plans); err != nil {
		return false, err
	}
	r.logf("starting %s from Release %s", service, ver)
	if err := h.Run(ctx, remote.Cmd{Script: r.cc.cmd("up", "-d", "--no-deps", service)}); err != nil {
		return false, &StartError{Service: service, StillStopped: changed, Err: fmt.Errorf("compose up %s: %w", service, err)}
	}
	if changed {
		if err := writeStopped(ctx, h, d.App, d.Destination, rest); err != nil {
			return changed, err
		}
	}
	r.phase = "plain-readiness"
	if err := r.waitServices(ctx, []servicePlan{*sp}, "complete"); err != nil {
		return changed, &StartError{Service: service, Err: err}
	}
	return changed, nil
}

// StoppedProviderError explains a Connection readiness failure whose
// provider is stopped on its Server; nil when it is not.
func StoppedProviderError(ctx context.Context, h remote.Host, app, dest, connection, provider, server string) error {
	if h == nil {
		return nil
	}
	s, err := ReadStopped(ctx, h, app, dest)
	if err != nil {
		return nil
	}
	if _, ok := s[provider]; !ok {
		return nil
	}
	return fmt.Errorf("connection %s provider %s is stopped on %s (`yoho app stop`); start it or switch the Connection", connection, provider, server)
}

// stoppedOr returns StoppedProviderError when the provider is stopped on
// its Server, else err.
func stoppedOr(ctx context.Context, h remote.Host, d *plan.Deploy, connection, provider, server string, err error) error {
	if se := StoppedProviderError(ctx, h, d.App, d.Destination, connection, provider, server); se != nil {
		return fmt.Errorf("%w: %w", se, err)
	}
	return err
}

// connectionReady checks Connection cn's providers from Server name. A
// stopped provider fails with StoppedProviderError, a same-Server one too
// (checked before it is skipped); a cross-Server provider must accept TCP
// on its private address.
func connectionReady(ctx context.Context, d *plan.Deploy, from remote.Host, hostOf func(string) remote.Host, cn string, c config.Connection, providers []string, name string) error {
	for _, provider := range providers {
		if provider == name {
			if se := StoppedProviderError(ctx, hostOf(provider), d.App, d.Destination, cn, c.Service, provider); se != nil {
				return se
			}
			continue
		}
		if e := roles.Ready(ctx, from, d.PrivateAddresses[provider], c.Port); e != nil {
			return stoppedOr(ctx, hostOf(provider), d, cn, c.Service, provider, fmt.Errorf("connection %s from %s: %w", c.Service, name, e))
		}
	}
	return nil
}

func partHost(parts map[string]*plan.Deploy, server string) remote.Host {
	if p := parts[server]; p != nil && len(p.Servers) > 0 {
		return p.Servers[0].Host
	}
	return nil
}
