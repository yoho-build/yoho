package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
	"github.com/yoho-build/yoho/internal/ui"
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

func TestPreviewRefusesVolumeDriversAndOptions(t *testing.T) {
	for _, spec := range []string{
		"{driver: nfs}",
		"{driver: local, driver_opts: {type: none, o: bind, device: /data/staging}}",
		"{driver_opts: {type: nfs, o: addr=server, device: ':/shared'}}",
	} {
		t.Run(spec, func(t *testing.T) {
			g := previewFixture(t, "")
			f := filepath.Join(filepath.Dir(g.configPath), "compose.yaml")
			writePreviewTestFile(t, f, "services:\n  web:\n    image: nginx\n    volumes: [data:/data]\nvolumes:\n  data: "+spec+"\n")
			a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.compose(context.Background()); err == nil || !strings.Contains(err.Error(), "data") {
				t.Fatalf("shared storage accepted: %v", err)
			}
		})
	}
}

func TestPreviewRefusesFixedNetworkAddresses(t *testing.T) {
	for _, spec := range []string{
		"services:\n  web:\n    image: nginx\nnetworks:\n  default:\n    ipam:\n      config: [{subnet: 10.2.0.0/24}]\n",
		"services:\n  web:\n    image: nginx\n    networks:\n      default: {ipv4_address: 10.2.0.2}\n",
		"services:\n  web:\n    image: nginx\n    networks:\n      default: {ipv6_address: 'fd00::2'}\n",
	} {
		t.Run(spec, func(t *testing.T) {
			g := previewFixture(t, "")
			writePreviewTestFile(t, filepath.Join(filepath.Dir(g.configPath), "compose.yaml"), spec)
			a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.compose(context.Background()); err == nil || !strings.Contains(err.Error(), "default") {
				t.Fatalf("fixed networking accepted: %v", err)
			}
		})
	}
}

func writePreviewTestFile(t *testing.T, file, data string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewTemplateUsesEffectiveOverlay(t *testing.T) {
	g := previewFixture(t, "")
	file := g.configPath
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// The raw file is invalid; the selected overlay supplies the valid setting.
	writePreviewTestFile(t, file, strings.Replace(string(b), "from: staging", "from: missing", 1))
	writePreviewTestFile(t, filepath.Join(filepath.Dir(file), "yoho.staging.yml"), "previews: {from: staging}\n")
	if _, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1"); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewTemplateRejectsEffectiveSwarm(t *testing.T) {
	g := previewFixture(t, "")
	// Select another Destination first so only the template's overlay introduces swarm.
	b, err := os.ReadFile(g.configPath)
	if err != nil {
		t.Fatal(err)
	}
	writePreviewTestFile(t, g.configPath, strings.Replace(string(b), "  staging: {servers:", "  production: {servers: [s]}\n  staging: {servers:", 1))
	writePreviewTestFile(t, filepath.Join(filepath.Dir(g.configPath), "yoho.staging.yml"), "previews: null\ndestinations:\n  staging: {runtime: swarm, app_dir: '', subnet: ''}\n")
	if _, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1"); err == nil {
		t.Fatal("effective Swarm template accepted")
	}
}

func TestPreviewScopedConfigCheck(t *testing.T) {
	g := previewFixture(t, "")
	t.Setenv("YOHO_ACCESS", "scoped")
	c := &cobra.Command{Use: "yoho"}
	c.AddCommand(configCmd(g))
	var output strings.Builder
	c.SetOut(&output)
	c.SetErr(&output)
	c.SilenceErrors = true
	c.SilenceUsage = true
	c.SetArgs([]string{"config", "check", "--offline"})
	err := c.ExecuteContext(context.Background())
	out := output.String()
	if err != nil {
		t.Fatalf("scoped offline check failed: %v\n%s", err, out)
	}
}

func TestPreviewRefusesConfiguredHostCollisions(t *testing.T) {
	for _, location := range []string{"template", "other-service", "other-destination"} {
		t.Run(location, func(t *testing.T) {
			g := previewFixture(t, "")
			dir := filepath.Dir(g.configPath)
			switch location {
			case "template":
				b, err := os.ReadFile(filepath.Join(dir, "compose.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				writePreviewTestFile(t, filepath.Join(dir, "compose.yaml"), strings.Replace(string(b), "original.example.com", "PR-1.preview.example.com", 1))
			case "other-service":
				writePreviewTestFile(t, filepath.Join(dir, "compose.staging.yaml"), "services:\n  api:\n    image: nginx\n    x-yoho:\n      proxy: {hosts: [web-pr-1.preview.example.com], port: 80}\n")
				b, err := os.ReadFile(g.configPath)
				if err != nil {
					t.Fatal(err)
				}
				writePreviewTestFile(t, g.configPath, strings.Replace(string(b), "{name}.preview", "{service}-{name}.preview", 1))
			case "other-destination":
				b, err := os.ReadFile(g.configPath)
				if err != nil {
					t.Fatal(err)
				}
				writePreviewTestFile(t, g.configPath, strings.Replace(string(b), "  staging: {servers:", "  production: {servers: [s]}\n  staging: {servers:", 1))
				writePreviewTestFile(t, filepath.Join(dir, "compose.production.yaml"), "services:\n  web:\n    x-yoho:\n      proxy: {hosts: !override [pr-1.preview.example.com], port: 80}\n")
			}
			a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.compose(context.Background()); err == nil || !strings.Contains(err.Error(), "configured") {
				t.Fatalf("configured host collision accepted: %v", err)
			}
		})
	}
}

func TestPreviewNameFlagOnlyOnUpAndDown(t *testing.T) {
	c := previewCmd(&globals{})
	for _, sub := range c.Commands() {
		has := sub.Flags().Lookup("name") != nil || c.PersistentFlags().Lookup("name") != nil
		if has != (sub.Name() != "list") {
			t.Errorf("%s --name available=%v", sub.Name(), has)
		}
	}
}

func TestPreviewDownJSONErrorTerminates(t *testing.T) {
	g := previewFixture(t, "")
	g.json = true
	c := previewCmd(g)
	var out strings.Builder
	c.SetOut(&out)
	c.SetErr(&out)
	c.SilenceErrors = true
	c.SilenceUsage = true
	c.SetArgs([]string{"down", "--name", "pr-1"}) // Confirmation fails before SSH.
	if err := c.ExecuteContext(context.Background()); err == nil {
		t.Fatal("missing --yes accepted")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if last["event"] != "finished" || last["error"] == nil {
		t.Fatalf("unterminated error output: %s", out.String())
	}
}

func TestPreviewChecksDirectoryBeforeConvergenceWrites(t *testing.T) {
	for _, kind := range []string{"unmarked", "app-dir-symlink"} {
		t.Run(kind, func(t *testing.T) {
			oldRoot := release.Root
			release.Root = t.TempDir()
			t.Cleanup(func() { release.Root = oldRoot })
			a := &app{cfg: &config.Config{App: "shop"}, destName: "preview-pr-1", previewName: "pr-1", ui: ui.Discard()}
			dir := release.AppDir(a.cfg.App, a.destName)
			target := dir
			if kind == "app-dir-symlink" {
				target = t.TempDir()
			}
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			writePreviewTestFile(t, filepath.Join(target, "hmac.key"), "keep")
			if kind == "app-dir-symlink" {
				// A normal App directory link could otherwise be migrated by EnsureAppDir.
				writePreviewTestFile(t, filepath.Join(target, ".yoho-app.json"), safepath.Marker(a.cfg.App, a.destName))
				if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.ensureAppDirs(context.Background(), []plan.NamedHost{{Name: "s", Host: &remote.Local{}}}); err == nil {
				t.Fatal("unmarked preview adopted during App directory convergence")
			}
			if _, err := os.Lstat(filepath.Join(release.Root, "appdirs")); !os.IsNotExist(err) {
				t.Fatalf("convergence wrote before refusal: %v", err)
			}
			if kind == "app-dir-symlink" {
				if got, err := os.Readlink(dir); err != nil || got != target {
					t.Fatalf("App directory moved before refusal: %q %v", got, err)
				}
			}
		})
	}
}

func TestPreviewKeepsSelectedOverlaySettingsWithTemplateOverlay(t *testing.T) {
	g := previewFixture(t, "")
	b, err := os.ReadFile(g.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "  staging: {servers:", "  production: {servers: [s]}\n  staging: {servers:", 1)
	s = strings.Replace(s, "from: staging", "from: missing", 1)
	writePreviewTestFile(t, g.configPath, s)
	writePreviewTestFile(t, filepath.Join(filepath.Dir(g.configPath), "yoho.production.yml"), "previews: {from: staging, host: '{name}.effective.example.com'}\n")
	a, err := g.loadPreview(&cobra.Command{Use: "up"}, "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if a.templateName != "staging" || a.cfg.Previews.Host != "{name}.effective.example.com" || a.dest.Env["FROM_TEMPLATE"] != "yes" {
		t.Fatalf("lost effective settings or template overlay: %+v %+v", a.cfg.Previews, a.dest)
	}
}

func TestPreviewDefaultsToTemplateWithoutProduction(t *testing.T) {
	g := previewFixture(t, "")
	t.Setenv("YOHO_DESTINATION", "")
	b, err := os.ReadFile(g.configPath)
	if err != nil {
		t.Fatal(err)
	}
	writePreviewTestFile(t, g.configPath, strings.Replace(string(b), "  staging: {servers:", "  qa: {servers: [s]}\n  staging: {servers:", 1))
	a, err := g.loadPreviewTemplate(&cobra.Command{Use: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if a.destName != "staging" || a.dest.Env["FROM_TEMPLATE"] != "yes" {
		t.Fatalf("template Destination and overlay not selected: %s %+v", a.destName, a.dest)
	}
}

func TestPreviewCollisionUsesTemplateProjectInterpolation(t *testing.T) {
	g := previewFixture(t, "")
	f := filepath.Join(filepath.Dir(g.configPath), "compose.yaml")
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	writePreviewTestFile(t, f, strings.Replace(string(b), "original.example.com", `"${COMPOSE_PROJECT_NAME}.preview.example.com"`, 1))
	// This preview hostname equals the template's configured hostname, but
	// differs from the hostname produced by loading Compose as the preview.
	a, err := g.loadPreview(&cobra.Command{Use: "up"}, "yoho-shop-staging")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.compose(context.Background())
	if err == nil || !strings.Contains(err.Error(), "yoho-shop-staging.preview.example.com is already configured on staging/web") {
		t.Fatalf("template's interpolated hostname collision accepted: %v", err)
	}
}
