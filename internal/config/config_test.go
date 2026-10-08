package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const yamlSrc = `
app: shop
servers:
  web1:
    ssh: deploy@1.2.3.4:22
    labels: {zone: a}
destinations:
  production:
    servers: [web1]
    env:
      TAG: v1
builder:
  location: remote
  remote: build@10.0.0.2
backups:
  targets:
    s3: {repository: "s3:bucket/x", password_secret: RESTIC_PW}
  jobs:
    nightly: {destination: production, target: s3, schedule: "*-*-* 03:00:00"}
secrets:
  providers:
    op: {type: op}
  values:
    DB_PASSWORD: {provider: op, ref: "op://v/i/f"}
`

const tomlSrc = `
app = "shop"

[servers.web1]
ssh = "deploy@1.2.3.4:22"
labels = { zone = "a" }

[destinations.production]
servers = ["web1"]
env = { TAG = "v1" }

[builder]
location = "remote"
remote = "build@10.0.0.2"

[backups.targets.s3]
repository = "s3:bucket/x"
password_secret = "RESTIC_PW"

[backups.jobs.nightly]
destination = "production"
target = "s3"
schedule = "*-*-* 03:00:00"

[secrets.providers.op]
type = "op"

[secrets.values.DB_PASSWORD]
provider = "op"
ref = "op://v/i/f"
`

const jsonSrc = `{
  "app": "shop",
  "servers": {"web1": {"ssh": "deploy@1.2.3.4:22", "labels": {"zone": "a"}}},
  "destinations": {"production": {"servers": ["web1"], "env": {"TAG": "v1"}}},
  "builder": {"location": "remote", "remote": "build@10.0.0.2"},
  "backups": {
    "targets": {"s3": {"repository": "s3:bucket/x", "password_secret": "RESTIC_PW"}},
    "jobs": {"nightly": {"destination": "production", "target": "s3", "schedule": "*-*-* 03:00:00"}}
  },
  "secrets": {"providers": {"op": {"type": "op"}}, "values": {"DB_PASSWORD": {"provider": "op", "ref": "op://v/i/f"}}}
}`

const jsoncSrc = `// Yoho file
{
  /* block */
  "app": "shop",
  "servers": {"web1": {"ssh": "deploy@1.2.3.4:22", "labels": {"zone": "a"},},},
  "destinations": {"production": {"servers": ["web1"], "env": {"TAG": "v1"}}},
  "builder": {"location": "remote", "remote": "build@10.0.0.2"},
  "backups": {
    "targets": {"s3": {"repository": "s3:bucket/x", "password_secret": "RESTIC_PW"}},
    "jobs": {"nightly": {"destination": "production", "target": "s3", "schedule": "*-*-* 03:00:00"}}
  },
  "secrets": {"providers": {"op": {"type": "op"}}, "values": {"DB_PASSWORD": {"provider": "op", "ref": "op://v/i/f"}}},
}`

func TestParseAllFormatsEquivalent(t *testing.T) {
	var ref *Config
	for name, src := range map[string]string{"yaml": yamlSrc, "toml": tomlSrc, "json": jsonSrc, "jsonc": jsoncSrc} {
		c, err := Parse([]byte(src), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ref == nil {
			ref = c
			continue
		}
		if !reflect.DeepEqual(ref, c) {
			t.Errorf("%s differs from yaml:\n%+v\n%+v", name, c, ref)
		}
	}
	if ref.Destinations["production"].Runtime != "compose" || ref.Transport.Mode != "auto" ||
		ref.RetainReleases != 5 || ref.Setup.User != "yoho" || ref.Hooks.Path != ".yoho/hooks" {
		t.Errorf("defaults not applied: %+v", ref)
	}
	if errs := Validate(ref); len(errs) != 0 {
		t.Errorf("unexpected validation errors: %v", errs)
	}
}

func TestUnknownKeyPath(t *testing.T) {
	cases := map[string]string{
		"app: a\nservers: {s: {ssh: x, bogus: 1}}\ndestinations: {d: {servers: [s]}}\n": `servers.s.bogus`,
		"app: a\nnope: 1\n": `"nope"`,
		"app: a\ndestinations: {d: {servers: [s], proxy: {htpp_port: 1}}}\n": `destinations.d.proxy.htpp_port`,
		`{"app":"a","backups":{"jobs":{"j":{"x":1}}}}`:                       `backups.jobs.j.x`,
	}
	for src, want := range cases {
		_, err := Parse([]byte(src), "t")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %s, got %v", want, err)
		}
	}
}

func TestTypeErrorHasField(t *testing.T) {
	_, err := Parse([]byte("app: a\nretain_releases: many\n"), "t")
	if err == nil || !strings.Contains(err.Error(), "retain_releases") {
		t.Errorf("got %v", err)
	}
}

func TestValidateMessages(t *testing.T) {
	c, err := Parse([]byte(`
app: Bad_App
servers: {a: {ssh: x}, b: {ssh: y}}
destinations:
  prod: {servers: [a, b]}
  stg: {servers: [ghost]}
  sw: {servers: [a, b], runtime: swarm}
builder: {location: remote}
backups:
  jobs: {j: {destination: nope, target: nowhere}}
secrets:
  values: {K: {provider: missing, ref: r}}
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, e := range Validate(c) {
		all = append(all, e.Error())
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{
		"app", "Roles are not supported yet; use runtime: swarm for several Servers",
		`unknown Server "ghost"`, "builder.remote is required",
		`unknown Destination "nope"`, `unknown Backup Target "nowhere"`, `unknown provider "missing"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "destinations.sw") {
		t.Errorf("swarm with several Servers must be valid:\n%s", joined)
	}
}

func TestDest(t *testing.T) {
	c := &Config{Destinations: map[string]Destination{"p": {Servers: []string{"a"}}}}
	if _, err := c.Dest(""); err != nil {
		t.Error(err)
	}
	if _, err := c.Dest("x"); err == nil {
		t.Error("want error")
	}
	c.Destinations["q"] = Destination{}
	if _, err := c.Dest(""); err == nil {
		t.Error("want error for ambiguous")
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	if _, err := Find(dir); err == nil {
		t.Error("want not found")
	}
	os.WriteFile(filepath.Join(dir, "yoho.toml"), []byte("app='a'"), 0o644)
	p, err := Find(dir)
	if err != nil || filepath.Base(p) != "yoho.toml" {
		t.Fatal(p, err)
	}
	os.WriteFile(filepath.Join(dir, "yoho.yml"), []byte("app: a"), 0o644)
	if _, err := Find(dir); err == nil {
		t.Error("want error for several")
	}
}

func TestSecretRefs(t *testing.T) {
	var e ServiceExt
	if err := json.Unmarshal([]byte(`{"secrets":{"Z_NAME":"B","A_NAME":"A"}}`), &e); err != nil {
		t.Fatal(err)
	}
	want := SecretRefs{{"A_NAME", "A"}, {"Z_NAME", "B"}}
	if !reflect.DeepEqual(e.Secrets, want) {
		t.Errorf("map form: %v", e.Secrets)
	}
	var l ServiceExt
	if err := json.Unmarshal([]byte(`{"secrets":["DB","PW:DB_PASSWORD"]}`), &l); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Secrets, SecretRefs{{"DB", "DB"}, {"PW", "DB_PASSWORD"}}) {
		t.Errorf("list form: %v", l.Secrets)
	}
	b, _ := json.Marshal(l.Secrets)
	if string(b) != `{"DB":"DB","PW":"DB_PASSWORD"}` {
		t.Errorf("marshal: %s", b)
	}
	if err := json.Unmarshal([]byte(`{"secrets":"x"}`), &e); err == nil {
		t.Error("want error")
	}
	if err := json.Unmarshal([]byte(`{"secrets":[":K"]}`), &e); err == nil {
		t.Error("want error for empty name")
	}
}

func TestDecodeServiceExtUnknown(t *testing.T) {
	_, err := DecodeServiceExt(map[string]any{"proxy": map[string]any{"hots": []any{"a"}}})
	if err == nil || !strings.Contains(err.Error(), "proxy.hots") {
		t.Errorf("got %v", err)
	}
}

func TestSchema(t *testing.T) {
	b, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"destinations"`) || !strings.Contains(string(b), "2020-12") {
		t.Error("schema lacks destinations or version")
	}
	b, err = ServiceExtSchema()
	if err != nil || !json.Valid(b) {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"oneOf"`) {
		t.Error("SecretRefs oneOf missing from x-yoho schema")
	}
}

func TestValidateRoot(t *testing.T) {
	c, err := Parse([]byte(`
app: a
servers: {a: {ssh: x, root: rel}, b: {ssh: y}, c: {ssh: z, root: /srv/yoho}}
destinations:
  d: {servers: [b, c], runtime: swarm}
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, e := range Validate(c) {
		all = append(all, e.Error())
	}
	j := strings.Join(all, "\n")
	if !strings.Contains(j, "must be an absolute path") || !strings.Contains(j, "share the same root") {
		t.Errorf("got:\n%s", j)
	}
}
