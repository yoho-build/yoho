package deploy

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/plan"
	"github.com/yoho-dev/yoho/internal/release"
	"github.com/yoho-dev/yoho/internal/remote"
)

// dockerHost runs file and shell work on the local filesystem (remote.Local
// inside release.Root = t.TempDir()) but intercepts every script that calls
// docker, recording it and answering from respond.
type dockerHost struct {
	local   *remote.Local
	mu      sync.Mutex
	scripts []string // docker scripts only, in order
	respond func(script string) (string, error)
}

func (f *dockerHost) Name() string { return "s1" }

func (f *dockerHost) isDocker(s string) bool { return strings.Contains(s, "docker") }

func (f *dockerHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}

func (f *dockerHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !f.isDocker(c.Script) {
		if c.Stdout != nil {
			var buf bytes.Buffer
			c2 := c
			c2.Stdout = io.MultiWriter(&buf, c.Stdout)
			err := f.local.Run(ctx, c2)
			return strings.TrimSpace(buf.String()), err
		}
		return f.local.Output(ctx, c)
	}
	f.mu.Lock()
	f.scripts = append(f.scripts, c.Script)
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(c.Script)
	}
	return "", nil
}

func (f *dockerHost) WriteFile(ctx context.Context, p string, data []byte, mode os.FileMode, sudo bool) error {
	return f.local.WriteFile(ctx, p, data, mode, sudo)
}

func (f *dockerHost) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	return f.local.ReadFile(ctx, p, sudo)
}

func (f *dockerHost) Close() error { return nil }

func (f *dockerHost) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.scripts, "\n---\n")
}

// withRoot points release.Root at a temp dir for the test.
func withRoot(t *testing.T) string {
	t.Helper()
	old := release.Root
	root := t.TempDir()
	release.Root = root
	t.Cleanup(func() { release.Root = old })
	return root
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
  db:
    image: postgres:17
    volumes:
      - pgdata:/var/lib/postgresql/data
  cloudflared:
    image: cloudflare/cloudflared:latest
    networks: [default, yoho]
volumes:
  pgdata:
networks:
  yoho:
    external: true
`

func loadProject(t *testing.T, src string) *types.Project {
	t.Helper()
	p, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir:  t.TempDir(),
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: []byte(src)}},
		Environment: types.Mapping{},
	}, func(o *loader.Options) { o.SetProjectName("shop", true) })
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
