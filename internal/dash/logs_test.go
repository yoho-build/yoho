package dash

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckTime(t *testing.T) {
	for _, ok := range []string{"", "30s", "10m", "2h30m", "1.5h", "0", "1700000000", "1700000000.123456789",
		"2026-11-03", "2026-11-03T15", "2026-11-03T15:04", "2026-11-03T15:04:05", "2026-11-03T15:04:05.5",
		"2026-11-03T15:04:05Z", "2026-11-03T15:04:05+01:00", "2026-11-03T15:04:05.123Z", "2026-11-03Z", "2026-11-03T15:04-07:00"} {
		if err := CheckTime(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"banana", "-5m", "10x", "10m;id", "$(id)", "2026-13-01", "2026-11-03 15:04:05", "--tail=1", "10m ", strings.Repeat("1", 50)} {
		if err := CheckTime(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestLogFilterCheck(t *testing.T) {
	for _, bad := range []LogFilter{{Since: "x"}, {Until: "x"}, {Grep: "a\nb"}, {Grep: "a\x00"}, {IgnoreCase: true}, {Regex: true}, {Grep: strings.Repeat("a", MaxGrep+1)}} {
		if bad.Check() == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
	if err := (LogFilter{Since: "1h", Until: "10m", Grep: `it's "x"; $(id)`, IgnoreCase: true, Regex: true}).Check(); err != nil {
		t.Error(err)
	}
}

// grepScript is LogsScript's shape with a grep filter: docker's exit
// status leaves the pipeline on fd 3, matches go to fd 4.
func grepScript(docker, grep string) string {
	return grepScriptChecked("", docker, grep)
}

// grepScriptChecked is grepScript after the -E pattern check check. grep
// runs under nice and the time limit and leaves its status on fd 3 after
// a "g"; a tail after it stays outside.
func grepScriptChecked(check, docker, grep string) string {
	grep, tail, _ := strings.Cut(grep, " | tail")
	if tail != "" {
		tail = " | tail" + tail
	}
	guard := "gw= to=\nif command -v nice >/dev/null 2>&1; then gw=nice; fi\n" +
		"if timeout --foreground 1 true >/dev/null 2>&1; then to=\"timeout --foreground 60\"\n" +
		"elif timeout 1 true >/dev/null 2>&1; then to=\"timeout 60\"\n" +
		"elif timeout -t 1 true >/dev/null 2>&1; then to=\"timeout -t 60\"\n" +
		"fi\ngw=\"$gw $to\"\n"
	return "set -eu\n" + check + guard + "exec 4>&1\nrc=$( { { " + docker + " && echo 0 >&3 || echo \"$?\" >&3; } | { $gw " + grep + " || echo \"g$?\" >&3; }" + tail + " >&4; } 3>&1 ) || :\n" +
		"drc=0 grc=0\nfor w in $rc; do case $w in g*) grc=${w#g} ;; *) drc=$w ;; esac; done\n" +
		"if [ -n \"$to\" ] && { [ \"$grc\" -eq 124 ] || [ \"$grc\" -eq 143 ]; }; then echo '--grep search took longer than 60s; narrow it with --since or -n' >&2; exit 124; fi\n" +
		"if [ \"$drc\" -ne 0 ]; then echo \"docker logs exited $drc; run without --grep to see its output\" >&2; exit \"$drc\"; fi\n"
}

func TestLogsScript(t *testing.T) {
	for _, c := range []struct {
		o    LogsOptions
		want string
	}{
		{LogsOptions{Lines: 100}, "docker logs --tail 100 'c' 2>&1"},
		{LogsOptions{Lines: AllLines}, "docker logs 'c' 2>&1"},
		{LogsOptions{}, "docker logs --tail 0 'c' 2>&1"},
		{LogsOptions{Follow: true}, "docker logs -f --tail 0 'c' 2>&1"},
		{LogsOptions{Lines: 5, Follow: true, Timestamps: true, LogFilter: LogFilter{Since: "10m", Until: "2026-11-03T15:04:05Z"}},
			"docker logs --since '10m' --until '2026-11-03T15:04:05Z' --timestamps -f --tail 5 'c' 2>&1"},
		{LogsOptions{Lines: 20, LogFilter: LogFilter{Grep: "it's"}},
			grepScript("docker logs 'c' 2>&1", "grep -F -e 'it'\\''s' | tail -n 20")},
		{LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "-v", IgnoreCase: true, Regex: true}},
			grepScriptChecked("prc=0\nperr=$(printf 'x\\n' | grep -i -E -e '-v' 2>&1 >/dev/null) || prc=$?\n"+
				"if [ \"$prc\" -gt 1 ]; then echo \"invalid --grep pattern: $perr\" >&2; exit 2; fi\n",
				"docker logs 'c' 2>&1", "grep -i -E -e '-v'")},
		// A bounded search: grep reads the last Scan lines, then keeps Lines.
		{LogsOptions{Lines: 200, Scan: MaxScan, LogFilter: LogFilter{Grep: "x"}},
			grepScript("docker logs --tail 100000 'c' 2>&1", "grep -F -e 'x' | tail -n 200")},
		{LogsOptions{Lines: AllLines, Scan: MaxScan, LogFilter: LogFilter{Grep: "x"}},
			grepScript("docker logs --tail 100000 'c' 2>&1", "grep -F -e 'x'")},
		{LogsOptions{Lines: 0, LogFilter: LogFilter{Grep: "x"}},
			grepScript("docker logs 'c' 2>&1", "grep -F -e 'x' | tail -n 0")},
		// Scan does not apply without grep or with Follow.
		{LogsOptions{Lines: 7, Scan: MaxScan}, "docker logs --tail 7 'c' 2>&1"},
	} {
		if got := LogsScript("docker logs", "'c'", c.o); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.o, got, c.want)
		}
	}
	f := LogsScript("docker logs", "'c'", LogsOptions{Lines: 10, Follow: true, LogFilter: LogFilter{Grep: "x"}})
	if !strings.Contains(f, "-f --tail 10 'c' 2>&1 && echo 0 >&3 || echo \"$?\" >&3; } | { $gw grep $lb -F -e 'x' || echo \"g$?\" >&3; } >&4;") || !strings.Contains(f, "--line-buffered") {
		t.Errorf("follow keeps docker's tail and line-buffers grep: %s", f)
	}
	// A follow runs grep under nice but with no time limit: the client's
	// leaving stops it.
	for _, s := range []string{f, FollowGrepScript("docker logs", "'c'", LogsOptions{LogFilter: LogFilter{Grep: "x"}})} {
		if !strings.Contains(s, "then gw=nice; fi") || !strings.Contains(s, "$gw grep $lb") || strings.Contains(s, "timeout") {
			t.Errorf("follow: nice, no limit: %s", s)
		}
	}
}

// TestLogsScriptRuns runs the scripts with a fake docker: the pattern is
// data (no injection), -n counts matches, and no match is not an error.
func TestLogsScriptRuns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	fake := "docker() { printf '%s\\n' 'tick 1' 'ERROR boom 1' 'error low' \"it's \\\"q\\\"; \\$(touch pwned)\" 'ERROR boom 2'; }\n"
	run := func(o LogsOptions) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", fake+LogsScript("docker logs", "'c'", o))
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%+v: %v\n%s", o, err, out)
		}
		if _, err := exec.Command("test", "-e", cmd.Dir+"/pwned").CombinedOutput(); err == nil {
			t.Fatalf("pattern was executed")
		}
		return string(out)
	}
	if got := run(LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "ERROR"}}); got != "ERROR boom 1\nERROR boom 2\n" {
		t.Errorf("grep: %q", got)
	}
	if got := run(LogsOptions{Lines: 1, LogFilter: LogFilter{Grep: "ERROR"}}); got != "ERROR boom 2\n" {
		t.Errorf("last match: %q", got)
	}
	if got := run(LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "error", IgnoreCase: true}}); strings.Count(got, "\n") != 3 {
		t.Errorf("-i: %q", got)
	}
	if got := run(LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: `"; $(touch pwned)`}}); !strings.Contains(got, "it's") {
		t.Errorf("literal: %q", got)
	}
	if got := run(LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "boom [0-9]$", Regex: true}}); strings.Count(got, "\n") != 2 {
		t.Errorf("regex: %q", got)
	}
	if got := run(LogsOptions{Follow: true, LogFilter: LogFilter{Grep: "nothing"}}); got != "" {
		t.Errorf("no match: %q", got)
	}
	if got := run(LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "a.c"}}); got != "" {
		t.Errorf("fixed string by default: %q", got)
	}
}

// TestLogsScriptGrepKeepsDockerFailure: grep must not hide a failing docker
// (POSIX sh has no pipefail), while no match on a working docker is fine.
func TestLogsScriptGrepKeepsDockerFailure(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(fake string, o LogsOptions) (string, int) {
		t.Helper()
		out, err := exec.Command("sh", "-c", fake+LogsScript("docker logs", "'c'", o)).CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	failing := "docker() { echo 'Error response from daemon: No such container: c' >&2; return 3; }\n"
	for _, o := range []LogsOptions{{Lines: AllLines, LogFilter: LogFilter{Grep: "x"}}, {Lines: 5, LogFilter: LogFilter{Grep: "x"}}, {Follow: true, LogFilter: LogFilter{Grep: "x"}}} {
		out, code := run(failing, o)
		if code != 3 || !strings.Contains(out, "docker logs exited 3") {
			t.Errorf("%+v: failing docker must fail (exit %d): %q", o, code, out)
		}
	}
	working := "docker() { printf '%s\\n' 'a' 'b'; }\n"
	if out, code := run(working, LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "zzz"}}); code != 0 || out != "" {
		t.Errorf("no match is not an error: %d %q", code, out)
	}
	if out, code := run(working, LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: "b"}}); code != 0 || out != "b\n" {
		t.Errorf("match: %d %q", code, out)
	}
}

// TestFollowGrepScriptStopsWhenStdinCloses: the client leaving (stdin EOF)
// must stop docker and grep, even when nothing ever matches.
func TestFollowGrepScriptStopsWhenStdinCloses(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	fake := "docker() { i=0; while :; do i=$((i+1)); echo \"tick $i\"; sleep 0.1; done; }\n"
	cmd := exec.Command("sh", "-c", fake+FollowGrepScript("docker logs", "'c'", LogsOptions{Lines: 1, LogFilter: LogFilter{Grep: "tick 3"}}))
	cmd.Env = append(cmd.Environ(), "TMPDIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	stdin.Close() // the ssh client went away
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("script kept running after stdin closed")
	}
	if !strings.Contains(out.String(), "tick 3\n") || strings.Contains(out.String(), "tick 4\n") {
		t.Errorf("matches: %q", out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "yoho-logs.*")); len(left) > 0 {
		t.Errorf("temp dir left: %v", left)
	}
}

// TestLogsScriptBadPattern: an -E pattern the Server's grep refuses fails
// before docker runs, as an invalid pattern (exit 2), not as docker's
// failure; a valid one that matches nothing, or not the probe line, runs.
// Run it under BusyBox too: CGO_ENABLED=0 go test -c, then
// docker run --rm -v "$PWD/dash.test:/t:ro" alpine:3 /t -test.run BadPattern
func TestLogsScriptBadPattern(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	fake := "docker() { : >" + dir + "/ran; printf '%s\\n' 'a1' 'b2' 'c3'; }\n"
	run := func(script string) (string, int) {
		t.Helper()
		os.Remove(filepath.Join(dir, "ran"))
		cmd := exec.Command("sh", "-c", fake+script)
		cmd.Env = append(cmd.Environ(), "TMPDIR="+dir)
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	for _, f := range []LogFilter{{Grep: "a(", Regex: true}, {Grep: "a[", Regex: true, IgnoreCase: true}} {
		for name, script := range map[string]string{
			"read":   LogsScript("docker logs", "'c'", LogsOptions{Lines: 10, LogFilter: f}),
			"follow": LogsScript("docker logs", "'c'", LogsOptions{Lines: 10, Follow: true, LogFilter: f}),
			"stdin":  FollowGrepScript("docker logs", "'c'", LogsOptions{Lines: 10, LogFilter: f}),
		} {
			out, code := run(script)
			if code != ExitBadPattern || !strings.Contains(out, "invalid --grep pattern") || strings.Contains(out, "docker logs exited") {
				t.Errorf("%s %q: exit %d %q", name, f.Grep, code, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
				t.Errorf("%s %q: docker ran", name, f.Grep)
			}
		}
	}
	for grep, want := range map[string]string{"^(a|c)[0-9]$": "a1\nc3\n", "^zzz$": "", "x": ""} {
		out, code := run(LogsScript("docker logs", "'c'", LogsOptions{Lines: AllLines, LogFilter: LogFilter{Grep: grep, Regex: true}}))
		if code != 0 || out != want {
			t.Errorf("%q: exit %d %q", grep, code, out)
		}
	}
}

// fakeGrepDir returns a directory with a grep that, for the pattern SLOW,
// drains its input and then hangs (as a pathological -E pattern would),
// writing its pid to pidFile; otherwise it runs the real grep. With
// tools, the directory also links them, so PATH can be the directory
// alone.
func fakeGrepDir(t *testing.T, pidFile string, tools ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := func(name string) string {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skip("no " + name)
		}
		return p
	}
	script := "#!/bin/sh\nfor a; do if [ \"$a\" = SLOW ]; then " + path("cat") + " >/dev/null; echo $$ >" + pidFile + "; exec " + path("sleep") + " 30; fi; done\nexec " + path("grep") + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "grep"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if err := os.Symlink(path(tool), filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// alive reports whether pid runs (a zombie does not).
func alive(pid string) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	_, rest, _ := strings.Cut(string(b), ") ")
	return !strings.HasPrefix(rest, "Z")
}

// TestLogsScriptTimeLimit: a search without Follow stops at GrepLimit with
// a clear error, not docker's, and leaves no grep running; without a
// timeout command it still runs, unbounded. Run it under BusyBox too, as
// TestLogsScriptBadPattern says.
func TestLogsScriptTimeLimit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	old := GrepLimit
	GrepLimit = 1
	t.Cleanup(func() { GrepLimit = old })
	pidFile := filepath.Join(t.TempDir(), "pid")
	fake := "docker() { i=0; while [ $i -lt 500 ]; do i=$((i+1)); echo 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab'; done; }\n"
	run := func(path string, o LogsOptions) (string, int, time.Duration) {
		t.Helper()
		cmd := exec.Command("sh", "-c", fake+LogsScript("docker logs", "'c'", o))
		cmd.Env = append(os.Environ(), "PATH="+path)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code, time.Since(start)
	}
	dir := fakeGrepDir(t, pidFile)
	for _, lines := range []int{AllLines, 10} {
		os.Remove(pidFile)
		out, code, took := run(dir+":"+os.Getenv("PATH"), LogsOptions{Lines: lines, Scan: MaxScan, LogFilter: LogFilter{Grep: "SLOW"}})
		if code != ExitGrepTimeout || !strings.Contains(out, "--grep search took longer than 1s; narrow it with --since or -n") || strings.Contains(out, "docker logs exited") || took > 10*time.Second {
			t.Fatalf("lines %d: exit %d after %s: %q", lines, code, took, out)
		}
		pid, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		if p := strings.TrimSpace(string(pid)); alive(p) {
			t.Errorf("lines %d: grep %s still runs", lines, p)
		}
	}
	// Without nice and timeout (PATH has only grep and tail), a search runs.
	bare := fakeGrepDir(t, pidFile, "tail")
	if out, code, _ := run(bare, LogsOptions{Lines: 2, Scan: MaxScan, LogFilter: LogFilter{Grep: "b$", Regex: true}}); code != 0 || strings.Count(out, "\n") != 2 {
		t.Errorf("without timeout: exit %d %q", code, out)
	}
	// A failing docker still fails as docker, under the limit.
	failing := "docker() { echo 'No such container: c' >&2; return 3; }\n"
	out, err := exec.Command("sh", "-c", failing+LogsScript("docker logs", "'c'", LogsOptions{Lines: 5, LogFilter: LogFilter{Grep: "x"}})).CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 || !strings.Contains(string(out), "docker logs exited 3") {
		t.Errorf("failing docker: %v %q", err, out)
	}
}
