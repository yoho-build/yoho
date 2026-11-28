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

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
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
	stopped, override, err := syncStopped(ctx, r.h, r.app, r.dest, plans)
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

// StopScript stops and removes the Service's containers (never one-offs or
// volumes) and prints "<id> <exit code>" per container. Removal, not only
// stop, keeps `restart: always` and a Docker restart from reviving them.
func StopScript(project, service string) string {
	return "set -eu\nids=$(docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+project) +
		" --filter " + remote.Quote("label=com.docker.compose.service="+service) +
		" --filter label=com.docker.compose.oneoff=False)\n" +
		`[ -n "$ids" ] || exit 0
docker stop $ids >/dev/null
for id in $ids; do printf '%s %s\n' "$id" "$(docker inspect -f '{{.State.ExitCode}}' "$id")"; done
docker rm $ids >/dev/null
`
}

// StopService records service as stopped on h, then stops and removes its
// containers. The record comes first, so an interrupted stop still keeps
// the Service down on the next deploy. Callers hold the Destination lock.
func StopService(ctx context.Context, h remote.Host, app, dest, service string, info StoppedEntry) (StopResult, error) {
	res := StopResult{Containers: []string{}, ExitCodes: map[string]int{}}
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
	out, err := h.Output(ctx, remote.Cmd{Script: StopScript(release.ProjectName(app, dest), service)})
	if err != nil {
		return res, fmt.Errorf("stop %s: %w", service, err)
	}
	for _, l := range lines(out) {
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

// StartError is a started Service that did not become ready. Its entry is
// already gone from StoppedFile.
type StartError struct {
	Service string
	Err     error
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Hint is the next step after a failed start.
func (e *StartError) Hint() string {
	return "`yoho app logs " + e.Service + "`; the Service stays marked started; rerun `yoho app start " + e.Service + "`"
}

// StartService removes service's stopped entry on d's Server, starts it from
// the current Release's compiled compose (other stopped Services stay down)
// and waits for its readiness. changed is false when it was not stopped.
// Callers hold the Destination lock.
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
	s, err := ReadStopped(ctx, h, d.App, d.Destination)
	if err != nil {
		return false, err
	}
	if _, changed = s[service]; changed {
		delete(s, service)
		if err := writeStopped(ctx, h, d.App, d.Destination, s); err != nil {
			return false, err
		}
	}
	r := newRunner(d, path.Join(release.Dir(d.App, d.Destination, ver), "compose.yaml"))
	r.phase = "service-start"
	if _, err := enforceStopped(ctx, r, plans); err != nil {
		return changed, err
	}
	r.logf("starting %s from Release %s", service, ver)
	if err := h.Run(ctx, remote.Cmd{Script: r.cc.cmd("up", "-d", "--no-deps", service)}); err != nil {
		return changed, &StartError{Service: service, Err: fmt.Errorf("compose up %s: %w", service, err)}
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

func partHost(parts map[string]*plan.Deploy, server string) remote.Host {
	if p := parts[server]; p != nil && len(p.Servers) > 0 {
		return p.Servers[0].Host
	}
	return nil
}
