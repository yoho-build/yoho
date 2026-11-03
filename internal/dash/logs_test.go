package dash

import (
	"os/exec"
	"strings"
	"testing"
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

func TestLogsScript(t *testing.T) {
	for _, c := range []struct {
		o    LogsOptions
		want string
	}{
		{LogsOptions{Lines: 100}, "docker logs --tail 100 'c' 2>&1"},
		{LogsOptions{}, "docker logs 'c' 2>&1"},
		{LogsOptions{Lines: 5, Follow: true, Timestamps: true, LogFilter: LogFilter{Since: "10m", Until: "2026-11-03T15:04:05Z"}},
			"docker logs --since '10m' --until '2026-11-03T15:04:05Z' --timestamps -f --tail 5 'c' 2>&1"},
		{LogsOptions{Lines: 20, LogFilter: LogFilter{Grep: "it's"}},
			"set -eu\ndocker logs 'c' 2>&1 | grep -F -e 'it'\\''s' | tail -n 20\n"},
		{LogsOptions{LogFilter: LogFilter{Grep: "-v", IgnoreCase: true, Regex: true}},
			"set -eu\ndocker logs 'c' 2>&1 | grep -i -E -e '-v' || [ \"$?\" -eq 1 ]\n"},
	} {
		if got := LogsScript("docker logs", "'c'", c.o); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.o, got, c.want)
		}
	}
	f := LogsScript("docker logs", "'c'", LogsOptions{Lines: 10, Follow: true, LogFilter: LogFilter{Grep: "x"}})
	if !strings.Contains(f, "-f --tail 10 'c' 2>&1 | grep $lb -F -e 'x' || [") || !strings.Contains(f, "--line-buffered") {
		t.Errorf("follow keeps docker's tail and line-buffers grep: %s", f)
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
	if got := run(LogsOptions{LogFilter: LogFilter{Grep: "ERROR"}}); got != "ERROR boom 1\nERROR boom 2\n" {
		t.Errorf("grep: %q", got)
	}
	if got := run(LogsOptions{Lines: 1, LogFilter: LogFilter{Grep: "ERROR"}}); got != "ERROR boom 2\n" {
		t.Errorf("last match: %q", got)
	}
	if got := run(LogsOptions{LogFilter: LogFilter{Grep: "error", IgnoreCase: true}}); strings.Count(got, "\n") != 3 {
		t.Errorf("-i: %q", got)
	}
	if got := run(LogsOptions{LogFilter: LogFilter{Grep: `"; $(touch pwned)`}}); !strings.Contains(got, "it's") {
		t.Errorf("literal: %q", got)
	}
	if got := run(LogsOptions{LogFilter: LogFilter{Grep: "boom [0-9]$", Regex: true}}); strings.Count(got, "\n") != 2 {
		t.Errorf("regex: %q", got)
	}
	if got := run(LogsOptions{Follow: true, LogFilter: LogFilter{Grep: "nothing"}}); got != "" {
		t.Errorf("no match: %q", got)
	}
	if got := run(LogsOptions{LogFilter: LogFilter{Grep: "a.c"}}); got != "" {
		t.Errorf("fixed string by default: %q", got)
	}
}
