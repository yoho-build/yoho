package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// KeepWithin is a parsed backups.targets.<n>.keep_within: a calendar
// duration in restic's --keep-within syntax (years, months, days, hours).
type KeepWithin struct {
	Years, Months, Days, Hours int
}

var keepWithinRe = regexp.MustCompile(`^(?:([0-9]+)y)?(?:([0-9]+)m)?(?:([0-9]+)d)?(?:([0-9]+)h)?$`)

// ParseKeepWithin parses "30d", "12h", "1y6m", ... (units in the order y, m,
// d, h; m is months, as in restic).
func ParseKeepWithin(s string) (KeepWithin, error) {
	m := keepWithinRe.FindStringSubmatch(s)
	if s == "" || m == nil {
		return KeepWithin{}, fmt.Errorf("%q is not a duration like 30d, 12h or 1y6m (units y, m for months, d, h, in that order)", s)
	}
	var k KeepWithin
	for i, p := range []*int{&k.Years, &k.Months, &k.Days, &k.Hours} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil || n > 100000 {
			return KeepWithin{}, fmt.Errorf("%q: number too large", s)
		}
		*p = n
	}
	if k == (KeepWithin{}) {
		return KeepWithin{}, errors.New("must be longer than zero")
	}
	return k, nil
}

// Cutoff is the oldest time kept relative to newest: a Backup taken at or
// after Cutoff(newest) is within the duration. Same arithmetic as restic.
func (k KeepWithin) Cutoff(newest time.Time) time.Time {
	return newest.AddDate(-k.Years, -k.Months, -k.Days).Add(-time.Duration(k.Hours) * time.Hour)
}

// String is the restic --keep-within argument.
func (k KeepWithin) String() string {
	s := ""
	for _, u := range []struct {
		n    int
		unit string
	}{{k.Years, "y"}, {k.Months, "m"}, {k.Days, "d"}, {k.Hours, "h"}} {
		if u.n > 0 {
			s += strconv.Itoa(u.n) + u.unit
		}
	}
	return s
}
