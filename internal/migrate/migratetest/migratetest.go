// Package migratetest fakes a Server's Docker and the Cloudflare tunnel
// configuration API for cutover tests. It understands only the scripts
// internal/migrate sends.
package migratetest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

// Mount is a container mount.
type Mount struct {
	Type   string `json:"Type"`
	Name   string `json:"Name,omitempty"`
	Source string `json:"Source"`
	RW     bool   `json:"RW"`
}

// PortBinding is a Docker published host-port binding.
type PortBinding struct {
	HostIP   string
	HostPort string
}

// Container is a fake container.
type Container struct {
	ID             string
	Name           string
	Labels         map[string]string
	Running        bool
	RestartPolicy  string
	Mounts         []Mount
	PortBindings   map[string][]PortBinding
	PublishedPorts map[string][]PortBinding
}

// Host is a fake Server running Docker.
type Host struct {
	mu         sync.Mutex
	Locked     bool
	Containers []*Container
	Files      map[string][]byte
	Modes      map[string]os.FileMode
	// Scripts and Env are every script and env sent, for leak checks.
	Scripts []string
	Env     []string
	// Ops records stop, start and restart-policy updates in order.
	Ops []string
	// AfterStop runs after a container is stopped (restart policies).
	AfterStop func(h *Host, c *Container)
}

// Name implements remote.Host.
func (h *Host) Name() string { return "s1" }

// Close implements remote.Host.
func (h *Host) Close() error { return nil }

// Container returns the container named name.
func (h *Host) Container(name string) *Container {
	for _, c := range h.Containers {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var quoted = regexp.MustCompile(`'([^']*)'`)

func args(line string) []string {
	var out []string
	for _, m := range quoted.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

func (c *Container) matches(filter string) bool {
	switch {
	case strings.HasPrefix(filter, "label="):
		k, v, hasV := strings.Cut(strings.TrimPrefix(filter, "label="), "=")
		got, ok := c.Labels[k]
		return ok && (!hasV || got == v)
	case strings.HasPrefix(filter, "name="):
		re, err := regexp.Compile(strings.TrimPrefix(filter, "name="))
		return err == nil && re.MatchString("/"+c.Name)
	}
	return false
}

func (h *Host) byID(id string) *Container {
	for _, c := range h.Containers {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (h *Host) stop(c *Container) {
	if !c.Running {
		return
	}
	c.Running = false
	h.Ops = append(h.Ops, "stop "+c.Name)
	if h.AfterStop != nil {
		h.AfterStop(h, c)
	}
}

// Run implements remote.Host.
func (h *Host) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

// Output implements remote.Host.
func (h *Host) Output(_ context.Context, c remote.Cmd) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Scripts = append(h.Scripts, c.Script)
	for k, v := range c.Env {
		h.Env = append(h.Env, k+"="+v)
	}
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		h.Env = append(h.Env, string(b))
	}
	if strings.Contains(c.Script, "/info.json; echo acquired") {
		if h.Locked {
			return "held\n", nil
		}
		h.Locked = true
		return "acquired\n", nil
	}
	if strings.HasPrefix(c.Script, "rm -rf ") && strings.Contains(c.Script, "/lock'") {
		h.Locked = false
		return "", nil
	}
	var out []string
	retired := strings.Contains(c.Script, "docker ps -aq")
	var selected []*Container
	for _, line := range strings.Split(c.Script, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "docker inspect --format") && strings.Contains(line, ".HostConfig.PortBindings"):
			running := strings.Contains(c.Script, "ids=$(docker ps -q")
			ids := args(line)
			for _, ct := range h.Containers {
				selected := running && ct.Running
				if !running {
					for _, id := range ids {
						if id == ct.ID {
							selected = true
						}
					}
				}
				if !selected {
					continue
				}
				published := ct.PublishedPorts
				if published == nil && ct.Running {
					published = ct.PortBindings
				}
				b, _ := json.Marshal(map[string]any{"id": ct.ID, "name": "/" + ct.Name, "app": ct.Labels["yoho.app"], "destination": ct.Labels["yoho.destination"], "project": ct.Labels["com.docker.compose.project"], "bindings": ct.PortBindings, "published": published})
				out = append(out, string(b))
			}
		case strings.HasPrefix(line, "ids=$(docker ps -aq"):
			filters := args(line)
			for _, ct := range h.Containers {
				all := true
				for _, f := range filters {
					all = all && ct.matches(f)
				}
				if all {
					selected = append(selected, ct)
				}
			}
		case strings.Contains(line, "docker inspect --format") && retired:
			for _, ct := range selected {
				name, count, _ := strings.Cut(ct.RestartPolicy, ":")
				n := 0
				if count != "" {
					fmt.Sscan(count, &n)
				}
				b, _ := json.Marshal(map[string]any{"id": ct.ID, "name": ct.Name, "restart_policy": map[string]any{"Name": name, "MaximumRetryCount": n}})
				out = append(out, string(b))
			}
		case strings.HasPrefix(line, "docker update"):
			as := args(line)
			policy := "no"
			if len(as) > 1 {
				policy = strings.TrimPrefix(as[0], "--restart=")
			}
			if len(as) > 0 {
				if ct := h.byID(as[len(as)-1]); ct != nil {
					ct.RestartPolicy = policy
					h.Ops = append(h.Ops, "update "+ct.Name+" "+policy)
				}
			}
		case strings.HasPrefix(line, "docker stop"):
			for _, id := range args(line) {
				if ct := h.byID(id); ct != nil {
					h.stop(ct)
				}
			}
		case strings.Contains(line, "docker inspect --format"):
			for _, ct := range h.Containers {
				if !ct.Running {
					continue
				}
				b, _ := json.Marshal(map[string]any{"id": ct.ID, "name": "/" + ct.Name, "app": ct.Labels["yoho.app"], "destination": ct.Labels["yoho.destination"], "mounts": ct.Mounts})
				out = append(out, string(b))
			}
		case strings.HasPrefix(line, "docker ps --no-trunc --filter"):
			f := args(line)[0]
			for _, ct := range h.Containers {
				if ct.Running && ct.matches(f) {
					out = append(out, ct.ID+"|"+ct.Name+"|"+ct.Labels["yoho.app"])
				}
			}
		case strings.HasPrefix(line, "for id in"):
			for _, id := range args(line) {
				if ct := h.byID(id); ct != nil {
					h.stop(ct)
				}
			}
		case strings.HasPrefix(line, "docker start"):
			for _, id := range args(line) {
				ct := h.byID(id)
				if ct == nil {
					return "", &remote.ExitError{Host: "s1", Code: 1, Stderr: "Error: No such container: " + id}
				}
				if !ct.Running {
					ct.Running = true
					h.Ops = append(h.Ops, "start "+ct.Name)
				}
			}
		case strings.HasPrefix(line, "ids=$(docker ps -q --no-trunc --filter"):
			filters := args(line)
			for _, ct := range h.Containers {
				all := ct.Running
				for _, f := range filters {
					all = all && ct.matches(f)
				}
				if all {
					h.stop(ct)
				}
			}
		}
	}
	return strings.Join(out, "\n"), nil
}

// WriteFile implements remote.Host.
func (h *Host) WriteFile(_ context.Context, p string, data []byte, mode os.FileMode, _ bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Files == nil {
		h.Files, h.Modes = map[string][]byte{}, map[string]os.FileMode{}
	}
	h.Files[p] = append([]byte(nil), data...)
	h.Modes[p] = mode
	return nil
}

// ReadFile implements remote.Host.
func (h *Host) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := h.Files[p]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("read %s: %w", p, fs.ErrNotExist)
}

// Cloudflare fakes the tunnel configuration endpoint.
type Cloudflare struct {
	*httptest.Server
	APIToken string
	Account  string
	Tunnel   string
	mu       sync.Mutex
	config   json.RawMessage
	// Puts counts PUT requests; Bodies holds every request body.
	Puts   int
	Bodies []string
	// Headers holds the Authorization header of every request.
	Headers []string
}

// IngressConfig is a typical remotely managed tunnel configuration: two
// App hostnames on the old stack, another App, and the catch-all.
const IngressConfig = `{"ingress":[` +
	`{"hostname":"app.example.com","service":"http://old-web:3000","originRequest":{"noTLSVerify":true}},` +
	`{"hostname":"api.example.com","path":"/v1","service":"http://old-api:8080"},` +
	`{"hostname":"other.example.com","service":"http://other:80"},` +
	`{"service":"http_status:404"}],"warp-routing":{"enabled":false}}`

// NewCloudflare starts a fake API accepting apiToken and serving config.
func NewCloudflare(t testing.TB, apiToken, config string) *Cloudflare {
	cf := &Cloudflare{APIToken: apiToken, Account: "acct0123", Tunnel: "5d1c0f5e-0000-4000-8000-00000000abcd", config: json.RawMessage(config)}
	cf.Server = httptest.NewServer(http.HandlerFunc(cf.serve))
	t.Cleanup(cf.Close)
	return cf
}

// TunnelToken is a connector token naming the fake account and tunnel.
func (cf *Cloudflare) TunnelToken() string {
	b, _ := json.Marshal(map[string]string{"a": cf.Account, "t": cf.Tunnel, "s": "dHVubmVsLXNlY3JldC12YWx1ZQ=="})
	return base64.StdEncoding.EncodeToString(b)
}

// Config returns the stored configuration.
func (cf *Cloudflare) Config() json.RawMessage {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return cf.config
}

// SetConfig replaces the stored configuration (someone edited it).
func (cf *Cloudflare) SetConfig(c string) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	cf.config = json.RawMessage(c)
}

func (cf *Cloudflare) serve(w http.ResponseWriter, r *http.Request) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	cf.Bodies = append(cf.Bodies, string(body))
	cf.Headers = append(cf.Headers, r.Header.Get("Authorization"))
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+cf.APIToken {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"result":null}`)
		return
	}
	if r.URL.Path != "/accounts/"+cf.Account+"/cfd_tunnel/"+cf.Tunnel+"/configurations" {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":7003,"message":"Could not route"}]}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var req struct {
			Config json.RawMessage `json:"config"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Config) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"success":false,"errors":[{"code":1001,"message":"bad config"}]}`)
			return
		}
		cf.config = req.Config
		cf.Puts++
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res, _ := json.Marshal(map[string]any{"tunnel_id": cf.Tunnel, "version": cf.Puts + 1, "config": cf.config, "source": "cloudflare"})
	fmt.Fprintf(w, `{"success":true,"errors":[],"messages":[],"result":%s}`, res)
}
