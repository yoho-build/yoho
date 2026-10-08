package swarm

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/plan"
	"github.com/yoho-dev/yoho/internal/release"
	"github.com/yoho-dev/yoho/internal/remote"
)

// fakeHost runs file work on the local filesystem (release.Root is a temp
// dir) and intercepts every script that calls docker, recording it (and its
// stdin) and answering from respond.
type fakeHost struct {
	name    string
	local   *remote.Local
	mu      sync.Mutex
	scripts []string
	stdins  []string
	respond func(script string) (string, error)
}

func (f *fakeHost) Name() string { return f.name }

func (f *fakeHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}

func (f *fakeHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !strings.Contains(c.Script, "docker") {
		if c.Stdout != nil {
			var buf bytes.Buffer
			c2 := c
			c2.Stdout = io.MultiWriter(&buf, c.Stdout)
			err := f.local.Run(ctx, c2)
			return strings.TrimSpace(buf.String()), err
		}
		return f.local.Output(ctx, c)
	}
	in := ""
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	f.mu.Lock()
	f.scripts = append(f.scripts, c.Script)
	f.stdins = append(f.stdins, in)
	f.mu.Unlock()
	if f.respond != nil {
		out, err := f.respond(c.Script)
		if c.Stdout != nil {
			io.WriteString(c.Stdout, out)
		}
		return out, err
	}
	return "", nil
}

func (f *fakeHost) WriteFile(ctx context.Context, p string, data []byte, mode os.FileMode, sudo bool) error {
	return f.local.WriteFile(ctx, p, data, mode, sudo)
}

func (f *fakeHost) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	return f.local.ReadFile(ctx, p, sudo)
}

func (f *fakeHost) Close() error { return nil }

func (f *fakeHost) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.scripts, "\n---\n")
}

func withRoot(t *testing.T) string {
	t.Helper()
	old := release.Root
	root := t.TempDir()
	release.Root = root
	t.Cleanup(func() { release.Root = old })
	return root
}

func fastPolling(t *testing.T) {
	t.Helper()
	old := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = old })
}

const testCompose = `
services:
  web:
    image: shop-web:v1
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:3000/up"]
    environment:
      PRICE: "$$5"
    depends_on: [db]
  worker:
    image: shop-web:v1
    command: ["bin/jobs"]
    scale: 2
    pull_policy: never
  db:
    image: postgres:17
    volumes:
      - pgdata:/var/lib/postgresql/data
  cloudflared:
    image: cloudflare/cloudflared:latest
    ports:
      - "127.0.0.1:9000:9000"
volumes:
  pgdata:
`

func loadProject(t *testing.T, src string) *types.Project {
	t.Helper()
	return loadProjectNamed(t, src, "yoho-shop-production")
}

func loadProjectNamed(t *testing.T, src, name string) *types.Project {
	t.Helper()
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  t.TempDir(),
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: []byte(src)}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) { o.SetProjectName(name, true) })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	webSecret    = "s3cr3t-key-base-VALUE"
	tunnelSecret = "tunnel-TOKEN-value-$x\"q"
)

func testDeploy(t *testing.T, h remote.Host, out io.Writer) *plan.Deploy {
	t.Helper()
	return &plan.Deploy{
		App: "shop", Destination: "production", Version: "v1", Performer: "alice",
		Servers: []plan.NamedHost{{Name: "primary", Server: config.Server{SSH: "yoho@203.0.113.10"}, Host: h}},
		Project: loadProject(t, testCompose),
		Ext: map[string]config.ServiceExt{
			"web": {
				Proxy:          &config.ServiceProxy{Hosts: []string{"shop.example.com"}, Port: 3000},
				Secrets:        config.SecretRefs{{Name: "DATABASE_PASSWORD", Key: "POSTGRES_PASSWORD"}, {Name: "SECRET_KEY_BASE", Key: "SECRET_KEY_BASE"}},
				ReleaseCommand: []string{"bin/rails", "db:migrate"},
			},
			"db":          {Stateful: true, Generate: map[string]string{"POSTGRES_PASSWORD": "password32"}},
			"cloudflared": {Secrets: config.SecretRefs{{Name: "TUNNEL_TOKEN", Key: "TUNNEL_TOKEN"}}, SecretsAsEnv: true},
		},
		ServiceSecrets: map[string]map[string]string{
			"web":         {"SECRET_KEY_BASE": webSecret},
			"cloudflared": {"TUNNEL_TOKEN": tunnelSecret},
		},
		SecretRefs:     map[string]string{"SECRET_KEY_BASE": "op://vault/shop/skb"},
		RetainReleases: 2,
		Out:            out,
	}
}
