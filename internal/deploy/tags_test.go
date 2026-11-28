package deploy

import (
	"context"
	"io"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

func withTags(d *plan.Deploy, server string, tags ...string) *plan.Deploy {
	ext := d.Ext["web"]
	ext.Tags = map[string]config.ServiceTag{
		"eu":     {Env: map[string]string{"REGION": "eu", "PRICE": "eu-override"}},
		"canary": {Env: map[string]string{"FEATURE_X": "1"}},
		"b":      {Env: map[string]string{"NODE_NAME": "beta"}},
	}
	d.Ext["web"] = ext
	d.Servers[0].Name = server
	d.Servers[0].Server.Tags = tags
	return d
}

func TestCompileTagEnvPerServer(t *testing.T) {
	envOf := func(d *plan.Deploy) map[string]string {
		t.Helper()
		out, _, _ := compileTest(t, d)
		var doc compiledDoc
		if err := yaml.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Services["web"].Environment
	}
	a := withTags(testDeploy(t, nil, nil), "a", "eu", "canary")
	env := envOf(a)
	if env["REGION"] != "eu" || env["FEATURE_X"] != "1" || env["PRICE"] != "eu-override" || env["NODE_NAME"] != "" {
		t.Fatalf("a: %v", env)
	}
	if v := a.Project.Services["web"].Environment["REGION"]; v != nil {
		t.Fatal("input project mutated")
	}
	b := withTags(testDeploy(t, nil, nil), "b")
	env = envOf(b)
	if env["NODE_NAME"] != "beta" || env["REGION"] != "" || env["PRICE"] == "eu-override" {
		t.Fatalf("b: %v", env)
	}
	// Yoho still sets its own YOHO_ environment.
	if env["YOHO_SERVICE"] != "web" {
		t.Fatalf("b: %v", env)
	}
}

func TestDiffTagEnvOnlyOnMatchingServer(t *testing.T) {
	h, _, _ := deployed(t)
	// The deployed Server is "primary"; a tag it does not carry changes nothing.
	d := withTags(testDeploy(t, h, io.Discard), "primary", "canary-off")
	cs, err := Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Action != plan.ActionNoop {
			t.Fatalf("untagged Server changed: %+v", c)
		}
	}
	d = withTags(testDeploy(t, h, io.Discard), "primary", "eu")
	cs, err = Compose{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	m := byKey(cs)
	if c := m["service web"]; c.Action == plan.ActionNoop || len(c.Reasons) == 0 || c.Reasons[0] != "config changed: environment" {
		t.Fatalf("web %+v", c)
	}
	if c := m["service db"]; c.Action != plan.ActionNoop {
		t.Fatalf("db %+v", c)
	}
}
