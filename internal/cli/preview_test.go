package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
)

func previewFixture(t *testing.T, extra string) *globals {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"yoho.yml": `app: shop
servers:
  s: {ssh: user@example.com}
destinations:
  staging: {servers: [s], app_dir: /data/shop, subnet: 10.1.1.0/24}
previews: {from: staging, host: '{name}.preview.example.com'}
proxy: {tunnel: {token_secret: TUNNEL}}
` + extra,
		"yoho.staging.yml": "destinations:\n  staging:\n    env: {FROM_TEMPLATE: yes}\n",
		"compose.yaml": `services:
  web:
    image: nginx
    x-yoho:
      proxy: {hosts: [original.example.com], port: 80, tls: true}
      backup: {volumes: [data]}
    volumes: [data:/data]
volumes:
  data: {name: staging-data}
`,
		"compose.staging.yaml":  "services:\n  web:\n    environment: {OVERLAY: staging}\n",
		".yoho/secrets":         "COMMON=common\nOVERRIDE=common\n",
		".yoho/secrets.staging": "OVERRIDE=staging\n",
	}
	for name, data := range files {
		f := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(f), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := release.Root
	t.Cleanup(func() { release.Root = old })
	t.Setenv("YOHO_ACCESS", "operator")
	return &globals{configPath: filepath.Join(dir, "yoho.yml")}
}

func TestPreviewUsesTemplateOverlayAndSecrets(t *testing.T) {
	g := previewFixture(t, "")
	a, err := g.loadPreview(&cobra.Command{Use: "up"}, "PR/123")
	if err != nil {
		t.Fatal(err)
	}
	if a.destName != "preview-pr-123" || a.dest.AppDir != "" || a.dest.Subnet != "" || a.dest.Env["FROM_TEMPLATE"] != "yes" || a.dest.Servers[0] != "s" {
		t.Fatalf("derived: %+v", a.dest)
	}
	if len(a.cfg.Backups.Jobs) > 0 || a.proxyConfig().Tunnel != nil {
		t.Fatal("inherited Backup jobs or Tunnel")
	}
	r, err := a.compose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := r.Ext["web"]
	if e.Backup != nil || !e.Proxy.TLS || e.Proxy.Hosts[0] != "pr-123.preview.example.com" {
		t.Fatalf("extension: %+v", e)
	}
	if r.Project.Volumes["data"].Name != a.project()+"_data" {
		t.Fatal("template volume reused")
	}
	if *r.Project.Services["web"].Environment["OVERLAY"] != "staging" {
		t.Fatal("compose template overlay lost")
	}
	s, err := a.loadSecrets(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("OVERRIDE"); v != "staging" {
		t.Fatal("template secrets lost")
	}
	if _, err := a.secretKeys(r); err != nil {
		t.Fatal(err)
	}
	p := a.previewRecord(&session{r: r, ver: "v1"})
	if err := p.Validate(a.cfg.App, a.destName); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewConfiguredCollisionRefused(t *testing.T) {
	g := previewFixture(t, "")
	// Put the collision in the template overlay.
	overlay := filepath.Join(filepath.Dir(g.configPath), "yoho.staging.yml")
	if err := os.WriteFile(overlay, []byte("destinations:\n  preview-pr-1: {servers: [s]}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"up", "down"} {
		_, err := g.loadPreview(&cobra.Command{Use: verb}, "pr-1")
		if err == nil || !strings.Contains(err.Error(), "collides") {
			t.Fatalf("%s: %v", verb, err)
		}
	}
}

func TestPreviewMultipleHostsNeedServicePattern(t *testing.T) {
	g := previewFixture(t, "")
	a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.compose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.Ext["api"] = config.ServiceExt{Proxy: &config.ServiceProxy{Hosts: []string{"api.example.com"}}}
	if err := a.preparePreviewCompose(r); err == nil || !strings.Contains(err.Error(), "{service}") {
		t.Fatalf("missing pattern hint: %v", err)
	}
	a.cfg.Previews.Host = "{service}-{name}.example.com"
	if err := a.preparePreviewCompose(r); err != nil {
		t.Fatal(err)
	}
	if r.Ext["api"].Proxy.Hosts[0] != "api-pr-1.example.com" {
		t.Fatal("wrong service host")
	}
}

func TestConfigCheckValidatesPreviewTemplate(t *testing.T) {
	g := previewFixture(t, "")
	// A second proxied Service needs {service}, even if normal compose routing
	// itself is valid. Check must fail before any SSH connection or provider run.
	f := filepath.Join(filepath.Dir(g.configPath), "compose.staging.yaml")
	if err := os.WriteFile(f, []byte(`services:
  api:
    image: nginx
    x-yoho:
      proxy: {hosts: [api.example.com], port: 80}
`), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd := &cobra.Command{Use: "check"}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	err := runConfigCheck(cmd, g, true)
	if err == nil || !strings.Contains(out.String(), "{service}") {
		t.Fatalf("check did not validate preview hosts: %v\n%s", err, out.String())
	}
}

func TestPreviewRefusesSharedData(t *testing.T) {
	for _, tc := range []struct{ name, compose, want string }{
		{"external volume", "services:\n  web:\n    image: nginx\n    volumes: [data:/data]\nvolumes:\n  data: {external: true}\n", "external"},
		{"writable bind", "services:\n  web:\n    image: nginx\n    volumes: [/data/staging:/data]\n", "writable bind"},
		{"fixed container", "services:\n  web:\n    image: nginx\n    container_name: staging-web\n", "container_name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := previewFixture(t, "")
			f := filepath.Join(filepath.Dir(g.configPath), "compose.yaml")
			if err := os.WriteFile(f, []byte(tc.compose), 0600); err != nil {
				t.Fatal(err)
			}
			a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.compose(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("shared data accepted: %v", err)
			}
		})
	}
}
