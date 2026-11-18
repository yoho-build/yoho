// Package servicecheck runs a Service's x-yoho.check (see config.ServiceCheck)
// against its containers: after a deploy until the check passes or its
// timeout ends (Wait), and once for `yoho app ps` and the dashboards (Probe).
//
// The check runs from the Server: each container's address comes from
// docker inspect (after checking the container belongs to the compose
// project) and the connection opens through remote.Dial, so nothing is
// installed on the Server or needed in the image. It only connects; it never
// changes a container.
package servicecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/probe"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Timing of Wait and Probe; tests shrink them.
var (
	// PollInterval is the wait between failed attempts of Wait.
	PollInterval = time.Second
	// AttemptTimeout bounds one attempt of Wait.
	AttemptTimeout = 5 * time.Second
	// ProbeTimeout bounds one probe of a Probe pass.
	ProbeTimeout = 2 * time.Second
	// PassTimeout bounds a whole Probe pass, docker commands included.
	PassTimeout = 6 * time.Second
)

// maxProbes bounds the concurrent probes of a Probe pass: each one is an
// ssh -W session, and sshd allows 10 per connection by default.
const maxProbes = 4

// Container is one container to check.
type Container struct {
	ID string
	// Name is the container name, for messages.
	Name string
	// Addr is the IP address the check connects to.
	Addr string
}

// label returns the container's name, or its short ID.
func (c Container) label() string {
	if c.Name != "" {
		return c.Name
	}
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}

// inspectFormat prints, per container and separated by |: ID, name, compose
// project label, network mode, then name=IPv4 for each network. Fields can
// be empty, so they are not separated by spaces.
const inspectFormat = `{{.Id}}|{{.Name}}|{{index .Config.Labels "com.docker.compose.project"}}|{{.HostConfig.NetworkMode}}{{range $k, $v := .NetworkSettings.Networks}}|{{$k}}={{$v.IPAddress}}{{end}}`

// Addresses inspects ids and returns them, in order, with the address a
// check connects to. Every container must carry the compose project label
// project. The address is the IPv4 address on one of the project's own
// networks (<project>_*), else on any other network the container joined;
// a container with network_mode host is checked on 127.0.0.1.
func Addresses(ctx context.Context, h remote.Host, project string, ids []string) ([]Container, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := h.Output(ctx, remote.Cmd{Script: "docker inspect --format " + remote.Quote(inspectFormat) + " " + remote.QuoteArgs(ids...)})
	if err != nil {
		return nil, fmt.Errorf("inspect containers: %w", err)
	}
	var res []Container
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "|")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		c := Container{ID: f[0], Name: strings.TrimPrefix(f[1], "/")}
		if f[2] != project {
			return nil, fmt.Errorf("container %s does not belong to compose project %s", c.label(), project)
		}
		c.Addr = pickAddr(project, f[3], f[4:])
		res = append(res, c)
	}
	if len(res) != len(ids) {
		return nil, fmt.Errorf("inspect containers: got %d of %d", len(res), len(ids))
	}
	return res, nil
}

// pickAddr chooses the address of a container from its network mode and
// name=IP pairs.
func pickAddr(project, mode string, nets []string) string {
	if mode == "host" {
		return "127.0.0.1"
	}
	var own, other []string
	for _, kv := range nets {
		name, ip, ok := strings.Cut(kv, "=")
		if !ok || ip == "" || strings.Contains(ip, ":") {
			continue
		}
		if strings.HasPrefix(name, project+"_") {
			own = append(own, name+"="+ip)
		} else {
			other = append(other, name+"="+ip)
		}
	}
	sort.Strings(own)
	sort.Strings(other)
	for _, kv := range append(own, other...) {
		_, ip, _ := strings.Cut(kv, "=")
		return ip
	}
	return ""
}

// Wait runs c against every container in ids (of compose project project)
// until each passes, retrying every PollInterval until c's timeout ends.
// It returns the last failure of the first container that does not pass.
func Wait(ctx context.Context, h remote.Host, project string, ids []string, c config.ServiceCheck) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("no running containers to check")
	}
	ctx, cancel := context.WithTimeout(ctx, c.TimeoutDuration())
	defer cancel()
	cs, err := Addresses(ctx, h, project, ids)
	if err != nil {
		return err
	}
	pc := c.Probe()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return remote.Dial(ctx, h, network, addr)
	}
	for _, ct := range cs {
		if ct.Addr == "" {
			return fmt.Errorf("container %s has no IPv4 address on a network", ct.label())
		}
		err := probe.Poll(ctx, PollInterval, func(ctx context.Context) error {
			actx, acancel := context.WithTimeout(ctx, AttemptTimeout)
			defer acancel()
			return probe.Run(actx, probe.DialFunc(dial), ct.Addr, pc, nil)
		})
		if err != nil {
			return fmt.Errorf("container %s: %w", ct.label(), err)
		}
	}
	return nil
}

// Status is the result of one check of one container.
type Status struct {
	// Service is the container's Service.
	Service string `json:"service"`
	// Check names the check, e.g. "websocket /sync on port 8080".
	Check string `json:"check"`
	// State is ok, failed, or unknown (the check could not run).
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// Status states.
const (
	OK      = "ok"
	Failed  = "failed"
	Unknown = "unknown"
)

// String is "ok", "failed: <reason>" or "unknown: <reason>".
func (s Status) String() string {
	if s.Error == "" {
		return s.State
	}
	return s.State + ": " + s.Error
}

// planEntry is the part of a Release's plan.json that Current reads.
type planEntry struct {
	Name  string               `json:"name"`
	Check *config.ServiceCheck `json:"check,omitempty"`
}

// Current returns the checks of the current Release of app/dest by Service
// (nil when there is no current Release or no Service has a check).
func Current(ctx context.Context, h remote.Host, app, dest string) (map[string]config.ServiceCheck, error) {
	b, err := h.ReadFile(ctx, path.Join(release.AppDir(app, dest), "current", "plan.json"), false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var plans []planEntry
	if err := json.Unmarshal(b, &plans); err != nil {
		return nil, fmt.Errorf("current plan.json: %w", err)
	}
	var out map[string]config.ServiceCheck
	for _, p := range plans {
		if p.Check == nil {
			continue
		}
		if out == nil {
			out = map[string]config.ServiceCheck{}
		}
		out[p.Name] = *p.Check
	}
	return out, nil
}

// Probe runs, once, the current Release's checks against the containers of
// app/dest on h, and returns their Status by container name; a container
// that is not running has failed. It returns nil when no Service of the
// current Release has a check, and a non-nil map otherwise. Each probe has
// ProbeTimeout and the pass PassTimeout; up to maxProbes run concurrently.
func Probe(ctx context.Context, h remote.Host, app, dest string) (map[string]Status, error) {
	ctx, cancel := context.WithTimeout(ctx, PassTimeout)
	defer cancel()
	checks, err := Current(ctx, h, app, dest)
	if err != nil || len(checks) == 0 {
		return nil, err
	}
	project := release.ProjectName(app, dest)
	out, err := h.Output(ctx, remote.Cmd{Script: "docker ps -a --no-trunc --filter " + remote.Quote("label=com.docker.compose.project="+project) +
		` --format '{{.ID}}|{{.Names}}|{{.Label "com.docker.compose.service"}}|{{.Label "com.docker.compose.oneoff"}}|{{.State}}'`})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	st := map[string]Status{}
	var ids, services []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "|")
		if len(f) < 5 || f[0] == "" {
			continue
		}
		if strings.EqualFold(f[3], "true") {
			continue // compose run one-off
		}
		c, ok := checks[f[2]]
		if !ok {
			continue
		}
		if f[4] != "running" {
			st[f[1]] = Status{Service: f[2], Check: c.String(), State: Failed, Error: "not running"}
			continue
		}
		ids = append(ids, f[0])
		services = append(services, f[2])
	}
	if len(ids) == 0 {
		return st, nil
	}
	cs, err := Addresses(ctx, h, project, ids)
	if err != nil {
		return nil, err
	}
	res := make([]Status, len(cs))
	sem := make(chan struct{}, maxProbes)
	var wg sync.WaitGroup
	for i, ct := range cs {
		c := checks[services[i]]
		res[i] = Status{Service: services[i], Check: c.String()}
		if _, ok := h.(remote.Dialer); !ok {
			res[i].State, res[i].Error = Unknown, "this connection to the Server cannot open network connections"
			continue
		}
		if ct.Addr == "" {
			res[i].State, res[i].Error = Failed, "no IPv4 address on a network"
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				res[i].State, res[i].Error = Unknown, "the check pass ran out of time"
				return
			}
			defer func() { <-sem }()
			pctx, pcancel := context.WithTimeout(ctx, ProbeTimeout)
			defer pcancel()
			dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
				return remote.Dial(ctx, h, network, addr)
			}
			if err := probe.Run(pctx, probe.DialFunc(dial), ct.Addr, c.Probe(), nil); err != nil {
				res[i].State, res[i].Error = Failed, err.Error()
				var fe *remote.ForwardError
				if errors.As(err, &fe) && strings.Contains(fe.Hint, "forbids forwarding") {
					res[i].State = Unknown
				}
				return
			}
			res[i].State = OK
		})
	}
	wg.Wait()
	for i, ct := range cs {
		st[ct.Name] = res[i]
	}
	return st, nil
}

// AppendColumn adds a CHECK column to a `docker compose ps` table whose
// second column is the container name: each container's Status, "-" for
// containers without a check. Other lines are kept as they are.
func AppendColumn(table string, st map[string]Status) string {
	ls := strings.Split(strings.TrimRight(table, "\n"), "\n")
	width := 0
	for _, l := range ls {
		width = max(width, utf8.RuneCountInString(strings.TrimRight(l, " ")))
	}
	var b strings.Builder
	for i, l := range ls {
		l = strings.TrimRight(l, " ")
		col := ""
		f := strings.Fields(l)
		switch {
		case i == 0 && len(f) > 1 && f[1] == "NAME":
			col = "CHECK"
		case len(f) > 1:
			col = "-"
			if s, ok := st[f[1]]; ok {
				col = s.String()
			}
		}
		b.WriteString(l)
		if col != "" {
			b.WriteString(strings.Repeat(" ", width-utf8.RuneCountInString(l)+3))
			b.WriteString(col)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
