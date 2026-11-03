package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/remote"
)

func compiledLogging(t *testing.T, logDriver DaemonLogging) map[string]any {
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
	for _, driver := range []DaemonLogging{{}, {Driver: "json-file"}} {
		got := compiledLogging(t, driver)
		for _, name := range []string{"web", "cloudflared"} {
			if !reflect.DeepEqual(got[name], want) {
				t.Errorf("daemon %+v: %s logging = %v, want %v", driver, name, got[name], want)
			}
		}
		if !reflect.DeepEqual(got["db"], user) {
			t.Errorf("daemon %+v: user logging changed: %v", driver, got["db"])
		}
	}
	// Another daemon driver is the operator's choice: no default.
	got := compiledLogging(t, DaemonLogging{Driver: "journald"})
	if _, ok := got["web"]; ok {
		t.Errorf("daemon journald: web got logging %v", got["web"])
	}
	// A json-file daemon with its own log-opts keeps them: no default.
	got = compiledLogging(t, DaemonLogging{Driver: "json-file", Opts: true})
	if _, ok := got["web"]; ok {
		t.Errorf("daemon log-opts: web got logging %v", got["web"])
	}
	if !reflect.DeepEqual(got["db"], user) {
		t.Errorf("daemon journald: user logging changed: %v", got["db"])
	}
}

func TestCompileDefaultLoggingStable(t *testing.T) {
	a, _, err := compile(testDeploy(t, nil, nil), "primary", planGenDir, nil, planKey, "", DaemonLogging{Driver: "json-file"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := compile(testDeploy(t, nil, nil), "primary", planGenDir, nil, planKey, "", DaemonLogging{Driver: "json-file"})
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

func TestDockerLogDriverFailureStopsTheDeploy(t *testing.T) {
	_, err := DockerLogDriver(context.Background(), &failHost{})
	var le *LogDriverError
	if !errors.As(err, &le) {
		t.Fatalf("unreadable driver must be a LogDriverError, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DockerLogDriver(ctx, &failHost{}); !errors.As(err, &le) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context must stop the deploy: %v", err)
	}
	if l, err := DockerLogDriver(context.Background(), nil); err != nil || !l.UseDefault() {
		t.Fatalf("nil Host = unknown, json-file default: %+v %v", l, err)
	}
}

func TestParseLogProbe(t *testing.T) {
	for _, tc := range []struct {
		out        string
		want       DaemonLogging
		useDefault bool
	}{
		{"json-file\n", DaemonLogging{Driver: "json-file"}, true},
		{"json-file\nlog-opts\n", DaemonLogging{Driver: "json-file", Opts: true}, false},
		{"json-file\nunreadable\n", DaemonLogging{Driver: "json-file", Opts: true}, false},
		{"local\n", DaemonLogging{Driver: "local"}, false},
	} {
		got := parseLogProbe(tc.out)
		if got != tc.want || got.UseDefault() != tc.useDefault {
			t.Errorf("%q: %+v (default %v), want %+v", tc.out, got, got.UseDefault(), tc.want)
		}
	}
}

// TestLogProbeScript runs the probe with a fake docker and a rootless
// daemon.json.
func TestLogProbeScript(t *testing.T) {
	if b, err := os.ReadFile("/etc/docker/daemon.json"); err == nil && strings.Contains(string(b), `"log-opts"`) {
		t.Skip("this machine's /etc/docker/daemon.json sets log-opts")
	}
	dir := t.TempDir()
	bin := dir + "/bin"
	cfg := dir + "/cfg/docker"
	for _, d := range []string{bin, cfg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(bin+"/docker", []byte("#!/bin/sh\necho json-file\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func() DaemonLogging {
		t.Helper()
		cmd := exec.Command("sh", "-c", logProbe)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir, "XDG_CONFIG_HOME=" + dir + "/cfg"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return parseLogProbe(string(out))
	}
	if l := run(); !l.UseDefault() {
		t.Fatalf("no daemon.json log-opts: %+v", l)
	}
	if err := os.WriteFile(cfg+"/daemon.json", []byte(`{"log-driver":"json-file","log-opts":{"max-size":"100m"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if l := run(); l.UseDefault() || !l.Opts {
		t.Fatalf("daemon.json log-opts must skip the default: %+v", l)
	}
}
