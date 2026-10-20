// Package dash collects a read-only Snapshot of an App's Destination for
// `yoho tui` and `yoho ui`: host facts, the App's containers and the Proxy,
// and the Release history of every Server.
//
// Collection runs one POSIX sh script per Server over the existing remote
// Host. It only reads: docker ps, docker stats --no-stream, /proc and df.
// Nothing is installed or left running on the Server, and only Yoho-owned
// containers (the App's compose project or swarm stack, and yoho-proxy) are
// listed.
package dash

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Target is one Server of the Destination.
type Target struct {
	Name string
	SSH  string
	Host remote.Host
	// Reader, when set, collects this Server instead of running Script on
	// Host: scoped access reads through the Gate's dash and logs verbs.
	Reader Reader
}

// Reader collects one Server without a shell.
type Reader interface {
	// Server returns the Server's facts, containers and Releases.
	Server(ctx context.Context) (Server, error)
	// Logs returns the last lines of one of the App's containers.
	Logs(ctx context.Context, container string, lines int) (string, error)
}

// Source identifies what to collect.
type Source struct {
	App         string
	Destination string
	Runtime     string // compose | swarm
	Root        string // release.Root of the Servers
	Targets     []Target
	// Releases lists Releases on a Server. Nil skips Release history.
	Releases func(ctx context.Context, h remote.Host) ([]release.Release, error)
}

// Project is the compose project or swarm stack name of the App.
func (s *Source) Project() string { return release.ProjectName(s.App, s.Destination) }

// Snapshot is everything the dashboards show at one point in time.
type Snapshot struct {
	App         string    `json:"app"`
	Destination string    `json:"destination"`
	Runtime     string    `json:"runtime"`
	Project     string    `json:"project"`
	CollectedAt time.Time `json:"collected_at"`
	Servers     []Server  `json:"servers"`
}

// Server is one Server of the Destination.
type Server struct {
	Name       string      `json:"name"`
	SSH        string      `json:"ssh"`
	Error      string      `json:"error,omitempty"`
	Host       Host        `json:"host"`
	Proxy      *Container  `json:"proxy,omitempty"`
	Containers []Container `json:"containers"`
	Releases   []Release   `json:"releases"`
	Elapsed    float64     `json:"elapsed_seconds"`
}

// Host holds facts about the Server machine.
type Host struct {
	Hostname      string     `json:"hostname"`
	OS            string     `json:"os"`
	Kernel        string     `json:"kernel"`
	Arch          string     `json:"arch"`
	CPUs          int        `json:"cpus"`
	UptimeSeconds float64    `json:"uptime_seconds"`
	Load          [3]float64 `json:"load"`
	MemTotal      uint64     `json:"mem_total_bytes"`
	MemAvailable  uint64     `json:"mem_available_bytes"`
	Disks         []Disk     `json:"disks"`
	DockerVersion string     `json:"docker_version"`
	DockerError   string     `json:"docker_error,omitempty"`
}

// Disk is the filesystem that holds Path.
type Disk struct {
	Label string `json:"label"` // yoho | docker
	Path  string `json:"path"`
	Mount string `json:"mount"`
	Total uint64 `json:"total_bytes"`
	Used  uint64 `json:"used_bytes"`
	Avail uint64 `json:"avail_bytes"`
}

// Container is one Yoho-owned container with its latest resource usage.
type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"` // running | exited | ...
	// Completed is an exited container that finished with code 0 (a
	// one-off job or release command), not a failure.
	Completed  bool    `json:"completed"`
	Health     string  `json:"health"` // healthy | unhealthy | starting | ""
	Status     string  `json:"status"`
	Ports      string  `json:"ports"`
	RunningFor string  `json:"running_for"`
	CPUPercent float64 `json:"cpu_percent"`
	MemPercent float64 `json:"mem_percent"`
	MemUsage   string  `json:"mem_usage"`
	NetIO      string  `json:"net_io"`
	BlockIO    string  `json:"block_io"`
	PIDs       string  `json:"pids"`
}

// Release is a Release record without its secret fingerprints.
type Release struct {
	Version    string            `json:"version"`
	Status     string            `json:"status"`
	DeployedAt time.Time         `json:"deployed_at"`
	Performer  string            `json:"performer"`
	Current    bool              `json:"current"`
	Images     map[string]string `json:"images"`
}

// Collect gathers a Snapshot from every Target in parallel. A failing Server
// is reported in its Error field; Collect itself only fails on a cancelled ctx.
func Collect(ctx context.Context, src *Source) (*Snapshot, error) {
	snap := &Snapshot{
		App: src.App, Destination: src.Destination, Runtime: src.Runtime,
		Project: src.Project(), CollectedAt: time.Now().UTC(),
		Servers: make([]Server, len(src.Targets)),
	}
	var wg sync.WaitGroup
	for i, t := range src.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Swarm records Releases on the manager (the first Server) only.
			releases := src.Releases != nil && (i == 0 || src.Runtime != "swarm")
			snap.Servers[i] = collectServer(ctx, src, t, releases)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}

func collectServer(ctx context.Context, src *Source, t Target, releases bool) (s Server) {
	start := time.Now()
	s = Server{Name: t.Name, SSH: t.SSH, Containers: []Container{}, Releases: []Release{}}
	defer func() { s.Elapsed = time.Since(start).Seconds() }()
	if t.Reader != nil {
		got, err := t.Reader.Server(ctx)
		if err != nil {
			s.Error = err.Error()
			return s
		}
		got.Name, got.SSH = t.Name, t.SSH
		if got.Containers == nil {
			got.Containers = []Container{}
		}
		if got.Releases == nil || !releases {
			got.Releases = []Release{}
		}
		return got
	}
	out, err := t.Host.Output(ctx, remote.Cmd{Script: Script(src.Project(), src.Root)})
	if err != nil {
		s.Error = err.Error()
		return s
	}
	parsed := Parse(out)
	s.Host, s.Proxy, s.Containers = parsed.Host, parsed.Proxy, parsed.Containers
	if releases {
		rels, err := src.Releases(ctx, t.Host)
		if err != nil {
			s.Error = err.Error()
		}
		s.Releases = summarize(rels)
	}
	return s
}

// summarize drops secret fingerprints and marks the current Release the way
// `yoho releases` does: the newest one, when it deployed.
func summarize(rels []release.Release) []Release {
	out := make([]Release, 0, len(rels))
	for i, r := range rels {
		out = append(out, Release{
			Version: r.Version, Status: r.Status, DeployedAt: r.DeployedAt,
			Performer: r.Performer, Images: r.Images,
			Current: i == 0 && r.Status == "deployed",
		})
	}
	return out
}

// Section markers in the script output.
const (
	markHost   = "@@yoho-host"
	markPS     = "@@yoho-ps"
	markStats  = "@@yoho-stats"
	markDocker = "@@yoho-docker-error"
)

// Script is the read-only shell script that prints host facts, the App's
// containers (compose project or swarm stack) and yoho-proxy as JSON lines,
// and their resource usage.
func Script(project, root string) string {
	q := remote.Quote
	ps := "docker ps -a --format '{{json .}}'"
	return `set -eu
LC_ALL=C; export LC_ALL
echo ` + markHost + `
printf 'hostname=%s\n' "$(hostname 2>/dev/null || uname -n)"
printf 'os=%s\n' "$( (. /etc/os-release && printf '%s' "${PRETTY_NAME:-${NAME:-}}") 2>/dev/null || uname -s)"
printf 'kernel=%s\n' "$(uname -r)"
printf 'arch=%s\n' "$(uname -m)"
printf 'cpus=%s\n' "$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null || echo 0)"
if [ -r /proc/uptime ]; then printf 'uptime=%s\n' "$(cut -d' ' -f1 /proc/uptime)"; fi
if [ -r /proc/loadavg ]; then printf 'load=%s\n' "$(cut -d' ' -f1-3 /proc/loadavg)"; fi
if [ -r /proc/meminfo ]; then
  awk '/^MemTotal:/ {print "memtotal=" $2} /^MemAvailable:/ {print "memavail=" $2}' /proc/meminfo
fi
disk() {
  d=$2
  while [ ! -e "$d" ] && [ "$d" != / ]; do d=$(dirname "$d"); done
  df -Pk "$d" 2>/dev/null | awk -v l="$1" -v p="$2" 'NR==2 {print "disk=" l "\t" p "\t" $6 "\t" $2 "\t" $3 "\t" $4}' || :
}
disk yoho ` + q(root) + `
if v=$(docker version --format '{{.Server.Version}}' 2>/dev/null); then
  printf 'docker=%s\n' "$v"
  dr=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || :)
  if [ -n "$dr" ]; then disk docker "$dr"; fi
  echo ` + markPS + `
  { ` + ps + ` --filter ` + q("label=com.docker.compose.project="+project) + `
    ` + ps + ` --filter ` + q("label=com.docker.stack.namespace="+project) + `
    ` + ps + ` --filter ` + q("name=^/"+proxy.ContainerName+"$") + `
  } | sort -u
  ids=$( { docker ps -q --filter ` + q("label=com.docker.compose.project="+project) + `
    docker ps -q --filter ` + q("label=com.docker.stack.namespace="+project) + `
    docker ps -q --filter ` + q("name=^/"+proxy.ContainerName+"$") + `
  } | sort -u)
  echo ` + markStats + `
  if [ -n "$ids" ]; then docker stats --no-stream --format '{{json .}}' $ids 2>/dev/null || :; fi
else
  echo ` + markDocker + `
  docker version --format '{{.Server.Version}}' 2>&1 | head -n 3 || :
fi
`
}

// Parsed is the decoded output of Script.
type Parsed struct {
	Host       Host
	Proxy      *Container
	Containers []Container
}

type psLine struct {
	ID, Names, Image, State, Status, Ports, Labels, RunningFor string
}

type statsLine struct {
	ID, Name, CPUPerc, MemPerc, MemUsage, NetIO, BlockIO, PIDs string
}

// Parse decodes Script output. Unknown or malformed lines are skipped so a
// Docker or OS variation degrades a field, not the whole dashboard.
func Parse(out string) Parsed {
	var p Parsed
	p.Host.Disks = []Disk{}
	p.Containers = []Container{}
	var section string
	var docker []string
	stats := map[string]statsLine{}
	var ps []psLine
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@@yoho-") {
			section = line
			continue
		}
		switch section {
		case markHost:
			hostLine(&p.Host, line)
		case markPS:
			var l psLine
			if json.Unmarshal([]byte(line), &l) == nil && l.Names != "" {
				ps = append(ps, l)
			}
		case markStats:
			var l statsLine
			if json.Unmarshal([]byte(line), &l) == nil && l.Name != "" {
				stats[strings.TrimPrefix(l.Name, "/")] = l
			}
		case markDocker:
			if s := strings.TrimSpace(line); s != "" {
				docker = append(docker, s)
			}
		}
	}
	if len(docker) > 0 {
		p.Host.DockerError = strings.Join(docker, " ")
	}
	for _, l := range ps {
		if l.State != "running" && strings.Contains(","+l.Labels, ",com.docker.swarm.task.id=") {
			continue // Swarm's retained history of replaced tasks
		}
		c := container(l)
		if st, ok := stats[c.Name]; ok {
			c.CPUPercent = percent(st.CPUPerc)
			c.MemPercent = percent(st.MemPerc)
			c.MemUsage, c.NetIO, c.BlockIO, c.PIDs = st.MemUsage, st.NetIO, st.BlockIO, st.PIDs
		}
		if c.Name == proxy.ContainerName {
			p.Proxy = &c
			continue
		}
		p.Containers = append(p.Containers, c)
	}
	sort.SliceStable(p.Containers, func(i, j int) bool {
		a, b := p.Containers[i], p.Containers[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	})
	return p
}

func hostLine(h *Host, line string) {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return
	}
	switch k {
	case "hostname":
		h.Hostname = v
	case "os":
		h.OS = v
	case "kernel":
		h.Kernel = v
	case "arch":
		h.Arch = v
	case "cpus":
		h.CPUs, _ = strconv.Atoi(strings.TrimSpace(v))
	case "uptime":
		h.UptimeSeconds, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
	case "load":
		for i, f := range strings.Fields(v) {
			if i < 3 {
				h.Load[i], _ = strconv.ParseFloat(f, 64)
			}
		}
	case "memtotal":
		kb, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		h.MemTotal = kb * 1024
	case "memavail":
		kb, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		h.MemAvailable = kb * 1024
	case "docker":
		h.DockerVersion = v
	case "disk":
		f := strings.Split(v, "\t")
		if len(f) != 6 {
			return
		}
		d := Disk{Label: f[0], Path: f[1], Mount: f[2]}
		d.Total = kib(f[3])
		d.Used = kib(f[4])
		d.Avail = kib(f[5])
		for _, e := range h.Disks {
			// The Docker root often shares the Yoho root's filesystem, as a
			// mount point of its own (bind mounts, overlay roots) or not.
			if e.Mount == d.Mount || e.Total == d.Total && near(e.Used, d.Used, d.Total/1000) {
				return
			}
		}
		h.Disks = append(h.Disks, d)
	}
}

// near reports whether a and b differ by at most tol (two df calls on one
// filesystem see slightly different usage).
func near(a, b, tol uint64) bool {
	if a > b {
		a, b = b, a
	}
	return b-a <= tol
}

func kib(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return n * 1024
}

func percent(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%")), 64)
	return f
}

func container(l psLine) Container {
	c := Container{
		ID: l.ID, Name: strings.TrimPrefix(l.Names, "/"), Image: l.Image,
		State: l.State, Status: l.Status, Ports: l.Ports, RunningFor: l.RunningFor,
	}
	labels := map[string]string{}
	for _, kv := range strings.Split(l.Labels, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			labels[k] = v
		}
	}
	switch {
	case labels["com.docker.compose.service"] != "":
		c.Service = labels["com.docker.compose.service"]
	case labels["com.docker.swarm.service.name"] != "":
		svc := labels["com.docker.swarm.service.name"]
		if ns := labels["com.docker.stack.namespace"]; ns != "" {
			svc = strings.TrimPrefix(svc, ns+"_")
		}
		c.Service = svc
	case c.Name == proxy.ContainerName:
		c.Service = "proxy"
	}
	st := strings.ToLower(l.Status)
	c.Completed = c.State == "exited" && strings.HasPrefix(st, "exited (0)")
	switch {
	case strings.Contains(st, "(healthy)"):
		c.Health = "healthy"
	case strings.Contains(st, "(unhealthy)"):
		c.Health = "unhealthy"
	case strings.Contains(st, "(health: starting)"):
		c.Health = "starting"
	}
	return c
}

// LogsScript prints the last lines of a container's logs with timestamps.
// The caller must check that name is a container of the Snapshot.
func LogsScript(name string, lines int) string {
	if lines <= 0 || lines > 5000 {
		lines = 200
	}
	return "docker logs --timestamps --tail " + strconv.Itoa(lines) + " " + remote.Quote(name) + " 2>&1"
}

// HasContainer reports whether name is a container (or the Proxy) of the
// Server in snap, so only Yoho-owned containers can be read.
func HasContainer(snap *Snapshot, server, name string) bool {
	if snap == nil {
		return false
	}
	for _, s := range snap.Servers {
		if s.Name != server {
			continue
		}
		if s.Proxy != nil && s.Proxy.Name == name {
			return true
		}
		for _, c := range s.Containers {
			if c.Name == name {
				return true
			}
		}
	}
	return false
}

// Logs reads the last lines of a Yoho-owned container on server.
func Logs(ctx context.Context, src *Source, snap *Snapshot, server, name string, lines int) (string, error) {
	if !HasContainer(snap, server, name) {
		return "", fmt.Errorf("unknown container %q on %q", name, server)
	}
	for _, t := range src.Targets {
		if t.Name == server {
			if t.Reader != nil {
				return t.Reader.Logs(ctx, name, lines)
			}
			return t.Host.Output(ctx, remote.Cmd{Script: LogsScript(name, lines)})
		}
	}
	return "", fmt.Errorf("unknown Server %q", server)
}

// Live caches the latest Snapshot so several viewers (browser tabs, the TUI
// and its key presses) share one collection per MaxAge instead of each
// running the Server script.
type Live struct {
	Src    *Source
	MaxAge time.Duration

	mu   sync.Mutex
	last *Snapshot
}

// Snapshot returns the cached Snapshot when younger than MaxAge, else
// collects a fresh one. Concurrent callers wait for the same collection.
func (l *Live) Snapshot(ctx context.Context, fresh bool) (*Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !fresh && l.last != nil && time.Since(l.last.CollectedAt) < l.MaxAge {
		return l.last, nil
	}
	snap, err := Collect(ctx, l.Src)
	if err != nil {
		return nil, err
	}
	l.last = snap
	return snap, nil
}

// Logs reads the last lines of a container listed by the latest Snapshot.
func (l *Live) Logs(ctx context.Context, server, name string, lines int) (string, error) {
	snap, err := l.Snapshot(ctx, false)
	if err != nil {
		return "", err
	}
	if !HasContainer(snap, server, name) {
		// The container may be newer than the cached Snapshot.
		if snap, err = l.Snapshot(ctx, true); err != nil {
			return "", err
		}
	}
	return Logs(ctx, l.Src, snap, server, name, lines)
}
