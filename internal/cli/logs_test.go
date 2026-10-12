package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogsServices(t *testing.T) {
	three := []string{"db", "web", "worker"}
	cases := []struct {
		name      string
		names     []string
		args      []string
		all       bool
		swarm     bool
		want      string // joined; "<all>" for nil without error
		err, hint string
	}{
		{name: "single implicit", names: []string{"web"}, want: "web"},
		{name: "single implicit swarm", names: []string{"web"}, swarm: true, want: "web"},
		{name: "several", names: three, err: "name one or more: db, web, worker", hint: "--all"},
		{name: "several swarm", names: three, swarm: true, err: "name one: db, web, worker", hint: "yoho app logs db`"},
		{name: "named", names: three, args: []string{"worker", "web"}, want: "worker,web"},
		{name: "unknown", names: three, args: []string{"api"}, err: "unknown Service api; Services: db, web, worker"},
		{name: "all", names: three, all: true, want: "<all>"},
		{name: "all with args", names: three, args: []string{"web"}, all: true, err: "mutually exclusive"},
		{name: "all swarm", names: three, all: true, swarm: true, err: "--all is not supported", hint: "yoho app logs db`"},
		{name: "swarm several args", names: three, args: []string{"db", "web"}, swarm: true, err: "exactly one Service"},
		{name: "swarm one arg", names: three, args: []string{"web"}, swarm: true, want: "web"},
		{name: "no Services", err: "no Services Yoho deploys"},
		{name: "no Services named", args: []string{"web"}, err: "no Services Yoho deploys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := logsServices(c.names, c.args, c.all, c.swarm)
			if c.err != "" {
				var he *hintError
				if !errors.As(err, &he) || !strings.Contains(he.msg, c.err) || !strings.Contains(he.hint, c.hint) {
					t.Fatalf("err %v, want %q (hint %q)", err, c.err, c.hint)
				}
				if c.swarm && strings.Contains(he.hint, "--all") {
					t.Errorf("swarm hint suggests --all: %q", he.hint)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			s := strings.Join(got, ",")
			if got == nil {
				s = "<all>"
			}
			if s != c.want {
				t.Errorf("got %q, want %q", s, c.want)
			}
		})
	}
}

func TestStatsScript(t *testing.T) {
	cases := []struct {
		name                   string
		services               []string
		swarm, stream, jsonOut bool
		want, not              []string
	}{
		{
			name: "compose all live", stream: true,
			want: []string{"ids=$(docker compose -p 'shop-prod' ps -q)\n", `exec docker stats --format 'table {{.Name}}\t{{.CPUPerc}}`, " $ids\n"},
			not:  []string{"--no-stream"},
		},
		{
			name: "compose services snapshot", services: []string{"web", "it's"},
			want: []string{`ps -q 'web' 'it'\''s')`, "exec docker stats --no-stream --format 'table "},
		},
		{
			name: "json", services: []string{"web"}, jsonOut: true,
			want: []string{"--no-stream --format '{{json .}}' $ids"},
		},
		{
			name: "swarm all", swarm: true,
			want: []string{"ids=$(docker ps -q --filter 'label=com.docker.stack.namespace=shop-prod')"},
		},
		{
			name: "swarm services union", swarm: true, services: []string{"web", "db"},
			want: []string{"ids=$({ docker ps -q --filter 'label=com.docker.swarm.service.name=shop-prod_web'; docker ps -q --filter 'label=com.docker.swarm.service.name=shop-prod_db'; } | sort -u)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := statsScript("shop-prod", c.services, c.swarm, c.stream, c.jsonOut)
			if !strings.HasPrefix(s, "set -eu\n") || !strings.Contains(s, "if [ -z \"$ids\" ]; then exit 43; fi\n") {
				t.Errorf("missing guard:\n%s", s)
			}
			for _, w := range c.want {
				if !strings.Contains(s, w) {
					t.Errorf("missing %q in:\n%s", w, s)
				}
			}
			for _, n := range c.not {
				if strings.Contains(s, n) {
					t.Errorf("unexpected %q in:\n%s", n, s)
				}
			}
		})
	}
}

// appDir writes a Yoho file and compose file whose Server is unreachable, so
// any test that got as far as SSH would fail differently.
func appDir(t *testing.T, runtime, compose string) string {
	t.Helper()
	dir := t.TempDir()
	yoho := "app: shop\nservers:\n  primary:\n    ssh: yoho@203.0.113.10\ndestinations:\n  production:\n    servers: [primary]\n    runtime: " + runtime + "\n"
	if err := os.WriteFile(filepath.Join(dir, "yoho.yml"), []byte(yoho), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "yoho.yml")
}

func runApp(t *testing.T, path string, json bool, args ...string) (string, error) {
	t.Helper()
	g := &globals{configPath: path, json: json}
	c := appCmds(g)
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

func TestAppCommandsFailBeforeSSH(t *testing.T) {
	compose := "services:\n  web: {image: nginx, env_file: [missing.env]}\n  db: {image: postgres}\n  debug: {image: busybox, profiles: [debug]}\n"
	cases := []struct {
		runtime string
		json    bool
		args    []string
		want    []string
	}{
		{"compose", false, []string{"logs"}, []string{"✗ the App has several Services; name one or more: db, web", "hint: e.g. `yoho app logs db`, or --all"}},
		{"compose", true, []string{"logs"}, []string{`"event":"error"`, `"hint":"e.g. `}}, // NDJSON failure event
		{"compose", false, []string{"logs", "debug"}, []string{"unknown Service debug; Services: db, web"}},
		{"compose", false, []string{"logs", "--all", "web"}, []string{"mutually exclusive"}},
		{"swarm", false, []string{"logs"}, []string{"name one: db, web"}},
		{"swarm", false, []string{"logs", "--all"}, []string{"--all is not supported"}},
		{"compose", false, []string{"stats", "api"}, []string{"unknown Service api"}},
		{"compose", true, []string{"stats", "api"}, []string{`"event":"error"`, "unknown Service api"}},
	}
	for _, c := range cases {
		t.Run(c.runtime+" "+strings.Join(c.args, " "), func(t *testing.T) {
			out, err := runApp(t, appDir(t, c.runtime, compose), c.json, c.args...)
			var silent *silentError
			if !errors.As(err, &silent) {
				t.Fatalf("err %v, want a silentError; output:\n%s", err, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			if c.json && strings.Contains(out, "✗") {
				t.Errorf("human output in --json:\n%s", out)
			}
		})
	}
}

func TestLogsUnreadableComposeNeedsService(t *testing.T) {
	out, err := runApp(t, appDir(t, "compose", "services: [not, a, map]\n"), false, "logs")
	var silent *silentError
	if !errors.As(err, &silent) || !strings.Contains(out, "cannot list the App's Services") || !strings.Contains(out, "name a Service") {
		t.Fatalf("err %v; output:\n%s", err, out)
	}
}

func TestLiveStatsNeverWithoutTerminal(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		name           string
		noStream, json bool
		vars           map[string]string
	}{
		{"no terminal", false, false, nil},
		{"no-stream", true, false, nil},
		{"json", false, true, nil},
		{"CI", false, false, map[string]string{"CI": "true"}},
		{"dumb", false, false, map[string]string{"TERM": "dumb"}},
	} {
		if liveStats(c.noStream, c.json, nil, nil, env(c.vars)) {
			t.Errorf("%s: live view without a terminal", c.name)
		}
	}
}
