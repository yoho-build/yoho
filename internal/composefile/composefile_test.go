package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func msgs(fs []Finding, level string) string {
	var s []string
	for _, f := range fs {
		if f.Level == level {
			s = append(s, f.Service+": "+f.Message)
		}
	}
	return strings.Join(s, "\n")
}

func TestLoadGood(t *testing.T) {
	dir := fixtureDir(t, "compose.yaml")
	r, err := Load(context.Background(), dir, nil, map[string]string{"TAG": "v2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Project.Name != "shop" {
		t.Errorf("name %q", r.Project.Name)
	}
	web := r.Ext["web"]
	if web.Proxy == nil || web.Proxy.Port != 3000 || len(web.Secrets) != 2 || web.Secrets[0].Name != "DB_PASSWORD" {
		t.Errorf("web ext: %+v", web)
	}
	db := r.Ext["db"]
	if !db.Stateful || len(db.Secrets) != 2 || db.Secrets[1].Key != "DB_PASSWORD" || db.Generate["PEPPER"] != "hex32" {
		t.Errorf("db ext: %+v", db)
	}
	if got := *r.Project.Services["web"].Environment["TAG"]; got != "v2" {
		t.Errorf("TAG %q", got)
	}
	for _, rt := range []string{"compose", "swarm"} {
		fs := Check(r, rt, []string{"RAILS_KEY", "DB_PASSWORD"})
		if HasErrors(fs) {
			t.Errorf("%s: unexpected errors:\n%s", rt, msgs(fs, LevelError))
		}
	}
	// swarm notes that build is fine
	if !strings.Contains(msgs(Check(r, "swarm", nil), LevelWarning), "builds images first") {
		t.Error("swarm build note missing")
	}
}

func TestEnvFile(t *testing.T) {
	dir := fixtureDir(t, "compose.yaml")
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TAG=fromdotenv\nDB_PASSWORD=x\n"), 0o600)
	r, err := Load(context.Background(), dir, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := *r.Project.Services["web"].Environment["TAG"]; got != "fromdotenv" {
		t.Errorf("TAG %q", got)
	}
	if strings.Join(r.EnvFileKeys, ",") != "DB_PASSWORD,TAG" {
		t.Errorf("keys %v", r.EnvFileKeys)
	}
	if !strings.Contains(msgs(Check(r, "compose", []string{"DB_PASSWORD"}), LevelWarning), ".env contains secret key DB_PASSWORD") {
		t.Error("expected .env warning")
	}
}

func TestCheckBad(t *testing.T) {
	dir := fixtureDir(t, "bad.yaml")
	r, err := Load(context.Background(), dir, nil, map[string]string{"DB_PASSWORD": "x"}, "bad")
	if err != nil {
		t.Fatal(err)
	}
	fs := Check(r, "swarm", []string{"DB_PASSWORD"})
	errs := msgs(fs, LevelError)
	for _, want := range []string{
		"${DB_PASSWORD} interpolates a secret key",
		`unknown secret key "NOPE"`,
		"must not publish ports", "must not set container_name",
		"no healthcheck",
		`unknown kind "rot13"`, "release_command must not be empty",
		"rejects: volumes_from",
		`"other" is not a named volume`,
		"no deploy.placement.constraints",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing error %q in:\n%s", want, errs)
		}
	}
	warns := msgs(fs, LevelWarning)
	for _, want := range []string{"ignores: privileged, container_name", "depends_on is ignored"} {
		if !strings.Contains(warns, want) {
			t.Errorf("missing warning %q in:\n%s", want, warns)
		}
	}
	if strings.Count(errs, "interpolates") != 1 {
		t.Errorf("$$ escape must not count:\n%s", errs)
	}
	// compose runtime: no healthcheck is a warning, unmarked volume a warning.
	cf := Check(r, "compose", []string{"DB_PASSWORD"})
	if !strings.Contains(msgs(cf, LevelWarning), "no healthcheck") || !strings.Contains(msgs(cf, LevelWarning), "named volume") {
		t.Errorf("compose warnings:\n%s", msgs(cf, LevelWarning))
	}
}

func TestStatefulProxy(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`
services:
  db:
    image: pg
    healthcheck: {test: ["CMD", "true"]}
    x-yoho: {stateful: true, proxy: {}}
`), 0o644)
	r, err := Load(context.Background(), dir, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msgs(Check(r, "compose", nil), LevelError), "stateful service cannot be proxied") {
		t.Error("want stateful+proxy error")
	}
}

func TestUnknownExtKey(t *testing.T) {
	dir := fixtureDir(t, "unknown.yaml")
	_, err := Load(context.Background(), dir, nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), "prxy") {
		t.Errorf("got %v", err)
	}
}

func TestInterpolatedVars(t *testing.T) {
	got := strings.Join(InterpolatedVars([]byte("a: $A ${B} ${C:-x} ${D?err} $$E $${F}")), ",")
	if got != "A,B,C,D" {
		t.Errorf("got %s", got)
	}
}

func TestStripBuild(t *testing.T) {
	dir := fixtureDir(t, "compose.yaml")
	r, err := Load(context.Background(), dir, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	StripBuild(r.Project, map[string]string{"web": "shop-web:abc123"})
	w := r.Project.Services["web"]
	if w.Build != nil || w.Image != "shop-web:abc123" {
		t.Errorf("%+v", w)
	}
}
