package swarm

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCompileDefaultLogging(t *testing.T) {
	want := map[string]any{"driver": "json-file", "options": map[string]any{"max-size": "10m", "max-file": "3"}}
	_, doc := compileTest(t, func(in *compileInput) { in.LogDriver = "json-file" })
	for _, name := range []string{"web", "worker", "db", "cloudflared"} {
		if got := get(t, doc, "services", name, "logging"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s logging = %v, want %v", name, got, want)
		}
	}

	d := testDeploy(t, nil, nil)
	src := strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    logging:\n      driver: local\n", 1)
	d.Project = loadProject(t, src)
	c, err := compile(d, compileInput{PinHost: "node-1", HMACKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	var doc2 map[string]any
	if err := yaml.Unmarshal(c.YAML, &doc2); err != nil {
		t.Fatal(err)
	}
	if got := get(t, doc2, "services", "db", "logging"); !reflect.DeepEqual(got, map[string]any{"driver": "local"}) {
		t.Errorf("user logging changed: %v", got)
	}
	if got := get(t, doc2, "services", "web", "logging"); !reflect.DeepEqual(got, want) {
		t.Errorf("unknown daemon driver: web logging = %v, want default", got)
	}

	_, doc3 := compileTest(t, func(in *compileInput) { in.LogDriver = "syslog" })
	if _, ok := get(t, doc3, "services", "web").(map[string]any)["logging"]; ok {
		t.Error("daemon syslog: web got default logging")
	}
}
