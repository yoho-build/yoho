package ui

import "testing"

func TestCount(t *testing.T) {
	for _, c := range []struct {
		n         int
		one, many string
		want      string
	}{
		{0, "key", "", "0 keys"},
		{1, "secret key", "", "1 secret key"},
		{2, "Service", "", "2 Services"},
		{1, "entry", "entries", "1 entry"},
		{3, "entry", "entries", "3 entries"},
	} {
		if got := Count(c.n, c.one, c.many); got != c.want {
			t.Errorf("Count(%d, %q, %q) = %q, want %q", c.n, c.one, c.many, got, c.want)
		}
	}
}
