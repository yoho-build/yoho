package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/gate"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestLogsOptions(t *testing.T) {
	o, err := logsOptions(dash.LogsOptions{Lines: 100, LogFilter: dash.LogFilter{Since: "30s"}}, false, false)
	if err != nil || o.Lines != dash.AllLines {
		t.Errorf("--since without -n shows every line: %+v %v", o, err)
	}
	o, err = logsOptions(dash.LogsOptions{Lines: 20, LogFilter: dash.LogFilter{Grep: "x"}}, true, false)
	if err != nil || o.Lines != 20 {
		t.Errorf("-n is kept: %+v %v", o, err)
	}
	for _, c := range []struct {
		o     dash.LogsOptions
		swarm bool
		want  string
	}{
		{dash.LogsOptions{LogFilter: dash.LogFilter{Since: "banana"}}, false, "duration like 30s"},
		{dash.LogsOptions{LogFilter: dash.LogFilter{Until: "1h;id"}}, false, "duration like 30s"},
		{dash.LogsOptions{LogFilter: dash.LogFilter{IgnoreCase: true}}, false, "--grep ERROR -i"},
		{dash.LogsOptions{LogFilter: dash.LogFilter{Until: "1h"}}, true, "use --since"},
		{dash.LogsOptions{Lines: -2}, false, "-n -1"},
		// docker compose logs -f --until keeps following: refused.
		{dash.LogsOptions{Follow: true, LogFilter: dash.LogFilter{Until: "1h"}}, false, "use --since with -f"},
	} {
		_, err := logsOptions(c.o, true, c.swarm)
		var he *hintError
		if !errors.As(err, &he) || !strings.Contains(he.hint, c.want) {
			t.Errorf("%+v: %v", c.o, err)
		}
	}
}

// TestLogsErrorOnce: a bad -E pattern is reported once, with grep's
// message and the hint, from the captured stderr.
func TestLogsErrorOnce(t *testing.T) {
	var out bytes.Buffer
	a := &app{g: &globals{}, ui: ui.New(&out, ui.Human, false)}
	ee := &remote.ExitError{Code: dash.ExitBadPattern, Stderr: "invalid --grep pattern: grep: Unmatched ( or \\("}
	err := a.logsError(ee, dash.LogsOptions{LogFilter: dash.LogFilter{Grep: "(", Regex: true}})
	var se *silentError
	if !errors.As(err, &se) {
		t.Fatalf("err %v", err)
	}
	if got := out.String(); strings.Count(got, "invalid --grep pattern") != 1 || !strings.Contains(got, "Unmatched") || !strings.Contains(got, "drop -E") {
		t.Fatalf("output %q", got)
	}
	other := &remote.ExitError{Code: 1, Stderr: "docker logs exited 1"}
	if err := a.logsError(other, dash.LogsOptions{LogFilter: dash.LogFilter{Grep: "x"}}); err != other {
		t.Fatalf("other: %v", err)
	}
}

// TestLogsLinesAgree: -n means the same over SSH and through the Gate.
// -n 0 is no history (v0.5.9's --tail 0), -n -1 every line (the Gate
// bounds it), and without -n 100 lines, or with --since or --grep every
// selected line.
func TestLogsLinesAgree(t *testing.T) {
	for _, c := range []struct {
		name     string
		lines    int
		set      bool
		f        dash.LogFilter
		follow   bool
		tail     string // the docker flag over SSH; "" is none
		gate     int    // the lines the Gate shows
		wireLine string // the request's lines field
	}{
		{"unset", 100, false, dash.LogFilter{}, false, " --tail 100 ", 100, `"lines":100`},
		{"-n 0", 0, true, dash.LogFilter{}, false, " --tail 0 ", 0, `"lines":0`},
		{"-f -n 0", 0, true, dash.LogFilter{}, true, " -f --tail 0 ", 0, `"lines":0`},
		{"-n -1", -1, true, dash.LogFilter{}, false, "", gate.MaxLogLines, `"lines":10000`},
		{"--since", 100, false, dash.LogFilter{Since: "10m"}, false, "", gate.MaxLogLines, `"lines":10000`},
		{"--since -n 200", 200, true, dash.LogFilter{Since: "10m"}, false, " --tail 200 ", 200, `"lines":200`},
	} {
		o, err := logsOptions(dash.LogsOptions{Lines: c.lines, Follow: c.follow, LogFilter: c.f}, c.set, false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		s := appLogsScript("p", []string{"web"}, false, o)
		if c.tail == "" && strings.Contains(s, "--tail") || c.tail != "" && !strings.Contains(s, c.tail) {
			t.Errorf("%s over SSH: %s", c.name, s)
		}
		a := gate.LogsArgs{Lines: gateLines(o.Lines), Follow: o.Follow, LogFilter: o.LogFilter}
		if got := gate.LogLines(a); got != c.gate {
			t.Errorf("%s through the Gate: %d lines, want %d", c.name, got, c.gate)
		}
		if b, _ := json.Marshal(a); !strings.Contains(string(b), c.wireLine) {
			t.Errorf("%s request %s, want %s", c.name, b, c.wireLine)
		}
	}
	// --grep without -n: every matching line over SSH, no tail after grep.
	o, _ := logsOptions(dash.LogsOptions{Lines: 100, LogFilter: dash.LogFilter{Grep: "x"}}, false, false)
	if s := appLogsScript("p", []string{"web"}, false, o); strings.Contains(s, "tail") {
		t.Errorf("--grep without -n: %s", s)
	}
}

func TestOlderGateHint(t *testing.T) {
	ge := refusalHint(&remote.GateError{Message: `logs: bad arguments: json: unknown field "grep"`, Code: "invalid"})
	if !strings.Contains(ge.Hint, "older than this CLI") || !strings.Contains(ge.Hint, "yoho access install") {
		t.Errorf("hint: %+v", ge)
	}
	if ge := refusalHint(&remote.GateError{Message: "invalid Service name", Code: "invalid"}); ge.Hint != "" {
		t.Errorf("other refusals keep their hint: %+v", ge)
	}
}

func TestAppLogsScript(t *testing.T) {
	o := dash.LogsOptions{Lines: 50, Follow: true, Timestamps: true, LogFilter: dash.LogFilter{Since: "10m", Grep: `"; $(id)`, IgnoreCase: true}}
	got := appLogsScript("yoho-shop-production", []string{"web", "worker"}, false, o)
	if !strings.Contains(got, `docker compose -p 'yoho-shop-production' logs --since '10m' --timestamps -f --tail 50 'web' 'worker' 2>&1 && echo 0 >&3 || echo "$?" >&3; } | { $gw grep $lb -i -F -e '"; $(id)' || echo "g$?" >&3; } >&4;`) {
		t.Errorf("compose: %s", got)
	}
	got = appLogsScript("yoho-shop-production", []string{"web"}, true, dash.LogsOptions{Lines: 100})
	if got != "docker service logs --tail 100 'yoho-shop-production_web' 2>&1" {
		t.Errorf("swarm: %s", got)
	}
	got = appLogsScript("yoho-shop-production", nil, false, dash.LogsOptions{Lines: dash.AllLines, LogFilter: dash.LogFilter{Grep: "ERROR"}})
	if !strings.Contains(got, "{ { docker compose -p 'yoho-shop-production' logs 2>&1 && echo 0 >&3 || echo \"$?\" >&3; } | { $gw grep -F -e 'ERROR' || echo \"g$?\" >&3; } >&4; }") || !strings.Contains(got, "timeout --foreground 60") {
		t.Errorf("--all --grep: %q", got)
	}
}

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
			want: []string{"ids=$(docker compose -p 'shop-prod' ps -q)\n", `docker stats --format 'table {{.Name}}\t{{.CPUPerc}}`, " $ids </dev/null &\n", "trap 'exit 130' INT"},
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

// TestStatsClientProcess is a fake Docker client that ignores interrupts, as
// affected Docker versions do after cancelling stats collection.
func TestStatsClientProcess(t *testing.T) {
	if os.Getenv("YOHO_STATS_TEST_CLIENT") != "1" {
		return
	}
	signal.Ignore(os.Interrupt)
	_, _ = os.Stdout.WriteString("ready\n")
	// Bound its lifetime even if the supervisor test fails before cleanup.
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func statsTestDocker(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stats scripts require POSIX sh")
	}
	dir := t.TempDir()
	fake := `#!/bin/sh
if [ "$1" = compose ]; then
  printf '%s' "${STATS_TEST_IDS-client}"
  exit 0
fi
if [ -n "${STATS_TEST_EXIT-}" ]; then exit "$STATS_TEST_EXIT"; fi
exec "$STATS_TEST_BINARY" -test.run '^TestStatsClientProcess$'
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STATS_TEST_BINARY", binary)
	t.Setenv("YOHO_STATS_TEST_CLIENT", "1")
	return statsScript("shop-prod", nil, false, true, false)
}

func TestStatsScriptInterruptStopsClient(t *testing.T) {
	script := statsTestDocker(t)
	// Use a real pipe, not exec's copying goroutine: EOF proves the Docker
	// child has exited too, rather than only the supervising shell.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	defer func() { _ = cmd.Process.Kill() }()
	if err := r.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "ready\n" {
		t.Fatalf("client not ready: %q, %v", buf[:n], err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("interrupt exit = %v, want 130", err)
	}
	if _, err := r.Read(buf); err != io.EOF {
		t.Fatalf("Docker client survived shell exit: %v", err)
	}
}

func TestStatsScriptPreservesExitCode(t *testing.T) {
	for _, code := range []string{"0", "7", "43"} {
		t.Run(code, func(t *testing.T) {
			script := statsTestDocker(t)
			t.Setenv("STATS_TEST_EXIT", code)
			if code == "43" {
				t.Setenv("STATS_TEST_IDS", "")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			err := cmd.Run()
			if code == "0" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) || strconv.Itoa(ee.ExitCode()) != code {
				t.Fatalf("exit = %v, want %s", err, code)
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
