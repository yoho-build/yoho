package dash

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
)

// LogFilter narrows a logs read. The zero value shows everything.
type LogFilter struct {
	// Since and Until bound the logs in time, as docker's --since and
	// --until: a duration back from now (10m, 2h30m) or a timestamp
	// (2026-11-03T15:04:05Z, 2026-11-03, Unix seconds).
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Grep keeps lines containing this fixed string, matched on the Server.
	Grep string `json:"grep,omitempty"`
	// IgnoreCase matches Grep case-insensitively.
	IgnoreCase bool `json:"ignore_case,omitempty"`
	// Regex treats Grep as an extended regular expression (grep -E).
	Regex bool `json:"regex,omitempty"`
}

// MaxGrep bounds the length of a Grep pattern.
const MaxGrep = 512

// TimeHint is the hint for an invalid --since or --until.
const TimeHint = "use a duration like 30s, 10m or 2h, or a timestamp like 2026-11-03T15:04:05Z, 2026-11-03 or Unix seconds"

var (
	unixTime = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,9})?$`)
	// The layouts docker accepts for --since and --until (api/types/time).
	timeLayouts = []string{
		"2006-01-02", "2006-01-02Z07:00",
		"2006-01-02T15", "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02T15:04:05.999999999",
		"2006-01-02T15Z07:00", "2006-01-02T15:04Z07:00", time.RFC3339, time.RFC3339Nano,
	}
)

// CheckTime reports whether v is a --since or --until value docker accepts:
// a non-negative Go duration, Unix seconds, or a date or RFC 3339 timestamp.
func CheckTime(v string) error {
	if v == "" {
		return nil
	}
	bad := fmt.Errorf("invalid time %q", v)
	if len(v) > 40 {
		return bad
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d < 0 {
			return fmt.Errorf("invalid time %q: durations count back from now and must not be negative", v)
		}
		return nil
	}
	if unixTime.MatchString(v) {
		return nil
	}
	for _, l := range timeLayouts {
		if _, err := time.Parse(l, v); err == nil {
			return nil
		}
	}
	return bad
}

// Check validates f: times docker accepts and a single-line pattern.
func (f LogFilter) Check() error {
	if err := CheckTime(f.Since); err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if err := CheckTime(f.Until); err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	if len(f.Grep) > MaxGrep {
		return fmt.Errorf("--grep: pattern longer than %d bytes", MaxGrep)
	}
	if strings.ContainsAny(f.Grep, "\x00\r\n") {
		return errors.New("--grep: pattern must be a single line")
	}
	if (f.IgnoreCase || f.Regex) && f.Grep == "" {
		return errors.New("--ignore-case and --regex need --grep")
	}
	return nil
}

// AllLines as LogsOptions.Lines shows every line: no --tail.
const AllLines = -1

// MaxScan bounds a Grep read without Follow when LogsOptions.Scan asks
// for it: grep sees at most this many lines from the end of the logs.
const MaxScan = 100000

// LogsOptions is one logs read.
type LogsOptions struct {
	LogFilter
	// Lines is how many lines to show from the end: with Grep and without
	// Follow, the last matching lines. 0 shows none (with Follow, only new
	// lines); AllLines (any negative value) shows every line.
	Lines int
	// Scan, when positive, bounds a Grep read without Follow to the last
	// Scan lines before grep, so a search never reads a container's whole
	// log (the dashboards and the Gate pass MaxScan). 0 reads everything.
	Scan       int
	Follow     bool
	Timestamps bool
}

// ErrFollowUntil refuses Follow with Until: docker compose logs -f
// --until keeps following, so the read would never end.
var ErrFollowUntil = errors.New("--until cannot be combined with -f")

// FollowUntilHint is the hint for ErrFollowUntil.
const FollowUntilHint = "`--until` ends the output at a point in the past; drop -f, or use --since with -f"

// CheckFollow refuses an Until bound on a Follow read (ErrFollowUntil).
func CheckFollow(f LogFilter, follow bool) error {
	if follow && f.Until != "" {
		return ErrFollowUntil
	}
	return nil
}

// ExitBadPattern is the exit status of a logs script whose --grep -E
// pattern the Server's grep refuses.
const ExitBadPattern = 2

// grepFlags are o's grep matching flags, before -e PATTERN.
func grepFlags(o LogsOptions) string {
	g := ""
	if o.IgnoreCase {
		g += " -i"
	}
	if o.Regex {
		g += " -E"
	} else {
		g += " -F"
	}
	return g
}

// patternCheck refuses an extended regular expression the Server's grep
// cannot compile, before docker runs; otherwise grep's error would only
// close the pipe and the read would fail as docker's. BusyBox grep
// compiles on the first input line, hence one line of input; status 0 or
// 1 (a match or none) means the pattern is fine. A fixed string always is.
func patternCheck(o LogsOptions) string {
	if !o.Regex {
		return ""
	}
	return "prc=0\nperr=$(printf 'x\\n' | grep" + grepFlags(o) + " -e " + remote.Quote(o.Grep) + " 2>&1 >/dev/null) || prc=$?\n" +
		"if [ \"$prc\" -gt 1 ]; then echo \"invalid --grep pattern: $perr\" >&2; exit " + strconv.Itoa(ExitBadPattern) + "; fi\n"
}

// LogsScript returns a POSIX sh script running cmd (docker logs, docker
// compose logs or docker service logs, quoted) with o's flags before args
// (quoted). Grep filters on the Server; its pattern is quoted data. The
// caller has checked o.LogFilter.
func LogsScript(cmd, args string, o LogsOptions) string {
	s := cmd
	if o.Since != "" {
		s += " --since " + remote.Quote(o.Since)
	}
	if o.Until != "" {
		s += " --until " + remote.Quote(o.Until)
	}
	if o.Timestamps {
		s += " --timestamps"
	}
	if o.Follow {
		s += " -f"
	}
	// Without Follow, Grep reads everything (or the last Scan lines) and
	// keeps the last Lines matches.
	grepRead := o.Grep != "" && !o.Follow
	switch {
	case grepRead && o.Scan > 0:
		s += " --tail " + strconv.Itoa(o.Scan)
	case grepRead:
	case o.Lines >= 0:
		s += " --tail " + strconv.Itoa(o.Lines)
	}
	if args != "" {
		s += " " + args
	}
	s += " 2>&1"
	if o.Grep == "" {
		return s
	}
	pre := "set -eu\n" + patternCheck(o) + grepGuard(grepRead)
	g := "$gw grep"
	if o.Follow {
		// Print matches as they arrive; busybox grep lacks --line-buffered.
		pre += "lb=\nif printf 'x\\n' | grep --line-buffered x >/dev/null 2>&1; then lb=--line-buffered; fi\n"
		g += " $lb"
	}
	g = "{ " + g + grepFlags(o) + " -e " + remote.Quote(o.Grep) + " || echo \"g$?\" >&3; }"
	if grepRead && o.Lines >= 0 {
		g += " | tail -n " + strconv.Itoa(o.Lines)
	}
	// POSIX sh has no pipefail, and docker's own errors share stderr with the
	// container's, so grep would hide a failing docker. Carry docker's exit
	// status out on fd 3, from an &&/|| list that set -e leaves running,
	// and grep's after a "g" (1, nothing matched, is not an error; running
	// into the time limit is); matches go to the real stdout (fd 4).
	return pre + "exec 4>&1\n" +
		"rc=$( { { " + s + " && echo 0 >&3 || echo \"$?\" >&3; } | " + g + " >&4; } 3>&1 ) || :\n" +
		"drc=0 grc=0\n" +
		"for w in $rc; do case $w in g*) grc=${w#g} ;; *) drc=$w ;; esac; done\n" +
		"if [ -n \"$to\" ] && { [ \"$grc\" -eq 124 ] || [ \"$grc\" -eq 143 ]; }; then echo " + remote.Quote(GrepTimeoutMessage()) + " >&2; exit " + strconv.Itoa(ExitGrepTimeout) + "; fi\n" +
		"if [ \"$drc\" -ne 0 ]; then echo \"docker logs exited $drc; run without --grep to see its output\" >&2; exit \"$drc\"; fi\n"
}

// GrepLimit bounds, in seconds, how long grep may run in a logs read
// without Follow. A search reads at most MaxScan lines (where the caller
// asks), so only a pathological -E pattern comes near it, and it must not
// pin the Server's CPUs for hours. Follow reads have no limit: the
// client's leaving stops them (FollowGrepScript, the Gate's stdin watcher).
var GrepLimit = 60

// ExitGrepTimeout is the exit status of a logs script whose grep ran into
// GrepLimit.
const ExitGrepTimeout = 124

// GrepTimeoutMessage is the error of a search that ran into GrepLimit.
func GrepTimeoutMessage() string {
	return "--grep search took longer than " + strconv.Itoa(GrepLimit) + "s; narrow it with --since or -n"
}

// grepGuard sets gw, the words before grep: nice, so a search yields the
// CPU to the Apps, and with limit a time limit of GrepLimit seconds, also
// in to (empty without a timeout command: no limit then). GNU timeout
// needs --foreground to stay in the script's process group, which the
// Gate kills as a whole when its client leaves; BusyBox timeout has no
// such option and needs none, and BusyBox before 1.30 spells it -t SECS.
// On the limit grep exits 124 (GNU) or 143 (BusyBox: SIGTERM). BusyBox's
// watcher holds grep's pipes until it next looks, so a search there ends
// up to a second late.
func grepGuard(limit bool) string {
	s := "gw= to=\nif command -v nice >/dev/null 2>&1; then gw=nice; fi\n"
	if limit {
		n := strconv.Itoa(GrepLimit)
		s += "if timeout --foreground 1 true >/dev/null 2>&1; then to=\"timeout --foreground " + n + "\"\n" +
			"elif timeout 1 true >/dev/null 2>&1; then to=\"timeout " + n + "\"\n" +
			"elif timeout -t 1 true >/dev/null 2>&1; then to=\"timeout -t " + n + "\"\n" +
			"fi\n"
	}
	return s + "gw=\"$gw $to\"\n"
}

// FollowGrepScript is LogsScript for Follow with Grep when the script runs
// as an `sh -c` argument and ssh's stdin stays open for the whole command
// (remote.SSH.RunTerminal without a tty). Without a tty nothing tells
// docker the client left: docker writes only into grep's pipe, so a
// rarely matching pattern would keep `docker logs -f | grep` running on
// the Server forever. Here a watcher reads stdin; when the client exits or
// the connection drops, stdin ends and the script stops docker and grep.
// The script must not run through `sh -s`, where stdin is the script.
func FollowGrepScript(cmd, args string, o LogsOptions) string {
	s := LogsScript(cmd, args, LogsOptions{LogFilter: LogFilter{Since: o.Since, Until: o.Until}, Lines: o.Lines, Follow: true, Timestamps: o.Timestamps})
	s = strings.TrimSuffix(s, " 2>&1") // redirected to the fifo below, in order
	// nice, but no time limit: the watcher stops grep when the client leaves.
	g := "$gw grep $lb" + grepFlags(o) + " -e " + remote.Quote(o.Grep)
	return `set -eu
` + patternCheck(o) + grepGuard(false) + `lb=
if printf 'x\n' | grep --line-buffered x >/dev/null 2>&1; then lb=--line-buffered; fi
dp= gp= wp=
d=$(mktemp -d "${TMPDIR:-/tmp}/yoho-logs.XXXXXX")
trap 'kill $dp $gp $wp 2>/dev/null || :; rm -rf "$d"' EXIT
trap 'exit 130' INT HUP TERM
mkfifo "$d/p"
exec 3<&0
` + s + ` >"$d/p" 2>&1 & dp=$!
` + g + ` <"$d/p" & gp=$!
cat <&3 >/dev/null & wp=$!
while kill -0 "$wp" 2>/dev/null && kill -0 "$gp" 2>/dev/null; do sleep 1; done
rc=0
if ! kill -0 "$dp" 2>/dev/null; then wait "$dp" || rc=$?; fi
if [ "$rc" -ne 0 ]; then echo "docker logs exited $rc; run without --grep to see its output" >&2; exit "$rc"; fi
`
}
