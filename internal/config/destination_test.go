package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDestNameDefault(t *testing.T) {
	// YOHO_DESTINATION is applied by the CLI before DestName. Config does not
	// read the environment; an explicit name that is not defined is an error.
	cases := []struct {
		name    string
		dests   []string
		ask     string
		want    string
		wantErr string
	}{
		{name: "production present", dests: []string{"staging", "production"}, ask: "", want: "production"},
		{name: "production absent", dests: []string{"qa", "staging"}, ask: "", wantErr: "several Destinations (production not defined), choose one with -d: qa, staging"},
		{name: "only one", dests: []string{"staging"}, ask: "", want: "staging"},
		{name: "explicit", dests: []string{"production", "staging"}, ask: "staging", want: "staging"},
		{name: "missing", dests: []string{"production", "staging"}, ask: "dev", wantErr: `unknown Destination "dev" (have: production, staging)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Destinations: map[string]Destination{}}
			for _, n := range tc.dests {
				c.Destinations[n] = Destination{Servers: []string{"primary"}}
			}
			got, err := c.DestName(tc.ask)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("got %q err %v", got, err)
				}
				if _, derr := c.Dest(tc.ask); derr == nil || derr.Error() != tc.wantErr {
					t.Fatalf("Dest err %v", derr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v", got, err)
			}
			d, err := c.Dest(tc.ask)
			if err != nil || !reflect.DeepEqual(d, c.Destinations[tc.want]) {
				t.Fatalf("Dest %+v err %v", d, err)
			}
		})
	}
}

func writeYoho(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const destBase = `
app: shop
servers:
  primary:
    ssh: u@h
destinations:
  production:
    servers: [primary]
    env: {A: "1", B: "2"}
  staging:
    servers: [primary]
    env: {A: "1", B: "2"}
compose: [a.yaml, b.yaml]
proxy:
  image: example/proxy:1
  http_port: 8080
`

func TestLoadForDestinationMerge(t *testing.T) {
	dir := t.TempDir()
	base := writeYoho(t, dir, "yoho.yml", destBase)
	overlay := writeYoho(t, dir, "yoho.staging.yml", `
destinations:
  staging:
    env: {B: "9", C: "3"}
    servers: [other]
servers:
  other: {ssh: o@h}
compose: [c.yaml]
proxy: {image: example/proxy:2}
retain_releases: 2
`)
	cfg, dest, gotOverlay, err := LoadForDestination(base, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if dest != "staging" || gotOverlay != overlay {
		t.Fatalf("dest %q overlay %q", dest, gotOverlay)
	}
	if !reflect.DeepEqual(cfg.Compose, []string{"c.yaml"}) {
		t.Errorf("compose list replaced: %#v", cfg.Compose)
	}
	st := cfg.Destinations["staging"]
	if !reflect.DeepEqual(st.Env, map[string]string{"A": "1", "B": "9", "C": "3"}) {
		t.Errorf("env merge: %#v", st.Env)
	}
	if !reflect.DeepEqual(st.Servers, []string{"other"}) {
		t.Errorf("servers list replaced: %#v", st.Servers)
	}
	if cfg.Destinations["production"].Env["B"] != "2" {
		t.Errorf("production env changed: %#v", cfg.Destinations["production"].Env)
	}
	if _, ok := cfg.Servers["primary"]; !ok {
		t.Error("base server dropped")
	}
	if cfg.Servers["other"].SSH != "o@h" {
		t.Errorf("overlay server: %+v", cfg.Servers["other"])
	}
	if cfg.Proxy.Image != "example/proxy:2" || cfg.Proxy.HTTPPort == nil || *cfg.Proxy.HTTPPort != 8080 {
		t.Errorf("proxy merge: %+v", cfg.Proxy)
	}
	if cfg.RetainReleases != 2 || cfg.Builder.Location != "local" {
		t.Errorf("defaults after merge: retain %d builder %q", cfg.RetainReleases, cfg.Builder.Location)
	}

	cfg, dest, gotOverlay, err = LoadForDestination(base, "")
	if err != nil {
		t.Fatal(err)
	}
	if dest != "production" || gotOverlay != "" {
		t.Fatalf("default dest %q overlay %q", dest, gotOverlay)
	}
	if !reflect.DeepEqual(cfg.Compose, []string{"a.yaml", "b.yaml"}) {
		t.Errorf("base compose: %#v", cfg.Compose)
	}
}

func TestLoadForDestinationProductionOverlay(t *testing.T) {
	dir := t.TempDir()
	base := writeYoho(t, dir, "yoho.yml", destBase)
	overlay := writeYoho(t, dir, "yoho.production.toml", `
[destinations.production.env]
B = "toml"
`)
	cfg, dest, gotOverlay, err := LoadForDestination(base, "")
	if err != nil {
		t.Fatal(err)
	}
	if dest != "production" || gotOverlay != overlay {
		t.Fatalf("dest %q overlay %q", dest, gotOverlay)
	}
	if cfg.Destinations["production"].Env["A"] != "1" || cfg.Destinations["production"].Env["B"] != "toml" {
		t.Errorf("toml merge: %#v", cfg.Destinations["production"].Env)
	}
}

func TestLoadForDestinationUnknownOverlayKey(t *testing.T) {
	dir := t.TempDir()
	base := writeYoho(t, dir, "yoho.yml", destBase)
	overlay := writeYoho(t, dir, "yoho.staging.json", `{"nope": true}`)
	_, _, _, err := LoadForDestination(base, "staging")
	if err == nil || !strings.Contains(err.Error(), overlay) || !strings.Contains(err.Error(), `unknown key "nope"`) {
		t.Fatalf("got %v", err)
	}
}

func TestLoadForDestinationMultipleOverlays(t *testing.T) {
	dir := t.TempDir()
	base := writeYoho(t, dir, "yoho.yml", destBase)
	writeYoho(t, dir, "yoho.staging.yml", "retain_releases: 2\n")
	writeYoho(t, dir, "yoho.staging.yaml", "retain_releases: 3\n")
	_, _, _, err := LoadForDestination(base, "staging")
	if err == nil || !strings.Contains(err.Error(), "several Destination overlays") || !strings.Contains(err.Error(), "yoho.staging.yml") || !strings.Contains(err.Error(), "yoho.staging.yaml") {
		t.Fatalf("got %v", err)
	}
}

func TestDestNameNoDestinations(t *testing.T) {
	c := &Config{Servers: map[string]Server{"web2": {}, "web1": {}}}
	_, err := c.DestName("")
	want := "no Destinations defined: add destinations.production.servers: [web1, web2] to the Yoho file"
	if err == nil || err.Error() != want {
		t.Fatalf("err %v", err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "yoho.yaml")
	if err := os.WriteFile(p, []byte("servers:\n  web1:\n    ssh: u@h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = LoadForDestination(p, "")
	if err == nil || !strings.Contains(err.Error(), "servers: [web1] to "+p) {
		t.Fatalf("err %v", err)
	}
}
