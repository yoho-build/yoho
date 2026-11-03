package deploy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/remote"
)

func compiledLogging(t *testing.T, logDriver string) map[string]any {
	t.Helper()
	d := testDeploy(t, nil, nil)
	src := strings.Replace(testCompose, "  db:\n    image: postgres:17\n", "  db:\n    image: postgres:17\n    logging:\n      driver: journald\n      options: {tag: db}\n", 1)
	d.Project = loadProject(t, src)
	out, _, err := compile(d, "primary", planGenDir, nil, planKey, "", logDriver)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Logging map[string]any `yaml:"logging"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for name, s := range doc.Services {
		if s.Logging != nil {
			got[name] = s.Logging
		}
	}
	return got
}

func TestCompileDefaultLogging(t *testing.T) {
	want := map[string]any{"driver": "json-file", "options": map[string]any{"max-size": "10m", "max-file": "3"}}
	user := map[string]any{"driver": "journald", "options": map[string]any{"tag": "db"}}
	for _, driver := range []string{"", "json-file"} {
		got := compiledLogging(t, driver)
		for _, name := range []string{"web", "cloudflared"} {
			if !reflect.DeepEqual(got[name], want) {
				t.Errorf("daemon %q: %s logging = %v, want %v", driver, name, got[name], want)
			}
		}
		if !reflect.DeepEqual(got["db"], user) {
			t.Errorf("daemon %q: user logging changed: %v", driver, got["db"])
		}
	}
	// Another daemon driver is the operator's choice: no default.
	got := compiledLogging(t, "journald")
	if _, ok := got["web"]; ok {
		t.Errorf("daemon journald: web got logging %v", got["web"])
	}
	if !reflect.DeepEqual(got["db"], user) {
		t.Errorf("daemon journald: user logging changed: %v", got["db"])
	}
}

func TestCompileDefaultLoggingStable(t *testing.T) {
	a, _, err := compile(testDeploy(t, nil, nil), "primary", planGenDir, nil, planKey, "", "json-file")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := compile(testDeploy(t, nil, nil), "primary", planGenDir, nil, planKey, "", "json-file")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("compiled compose differs between identical compiles")
	}
}

// failHost fails every command, like a Server whose docker info errors.
type failHost struct{ remote.Local }

func (*failHost) Output(context.Context, remote.Cmd) (string, error) {
	return "", errors.New("docker info: permission denied")
}

func TestDockerLogDriverNeverFailsADeploy(t *testing.T) {
	got, err := DockerLogDriver(context.Background(), &failHost{})
	if err != nil || got != "" || !UseDefaultLogging(got) {
		t.Fatalf("unreadable driver must mean unknown (json-file default): %q %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DockerLogDriver(ctx, &failHost{}); err == nil {
		t.Fatal("a cancelled context must still stop the deploy")
	}
}
