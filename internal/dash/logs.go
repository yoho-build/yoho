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

// LogsOptions is one logs read.
type LogsOptions struct {
	LogFilter
	// Lines is how many lines to show from the end: with Grep and without
	// Follow, the last matching lines. 0 shows all.
	Lines      int
	Follow     bool
	Timestamps bool
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
	// Without Follow, Grep reads everything and keeps the last matches.
	tailAfter := o.Grep != "" && !o.Follow && o.Lines > 0
	if o.Lines > 0 && !tailAfter {
		s += " --tail " + strconv.Itoa(o.Lines)
	}
	if args != "" {
		s += " " + args
	}
	s += " 2>&1"
	if o.Grep == "" {
		return s
	}
	g := "grep"
	pre := "set -eu\n"
	if o.Follow {
		// Print matches as they arrive; busybox grep lacks --line-buffered.
		pre += "lb=\nif printf 'x\\n' | grep --line-buffered x >/dev/null 2>&1; then lb=--line-buffered; fi\n"
		g += " $lb"
	}
	if o.IgnoreCase {
		g += " -i"
	}
	if o.Regex {
		g += " -E"
	} else {
		g += " -F"
	}
	g += " -e " + remote.Quote(o.Grep)
	if tailAfter {
		g += " | tail -n " + strconv.Itoa(o.Lines)
	}
	// POSIX sh has no pipefail, and docker's own errors share stderr with the
	// container's, so grep would hide a failing docker. Carry docker's exit
	// status out on fd 3, from an &&/|| list that set -e leaves running;
	// matches go to the real stdout (fd 4). grep exiting 1 (nothing
	// matched) is not an error.
	return pre + "exec 4>&1\n" +
		"rc=$( { { " + s + " && echo 0 >&3 || echo \"$?\" >&3; } | " + g + " >&4; } 3>&1 ) || :\n" +
		"if [ \"${rc:-0}\" -ne 0 ]; then echo \"docker logs exited $rc; run without --grep to see its output\" >&2; exit \"$rc\"; fi\n"
}
