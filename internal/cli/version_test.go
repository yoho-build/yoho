package cli

import (
	"strings"
	"testing"
)

func TestMatchVersion(t *testing.T) {
	clean := "3df59abc227ac8bfb0000000000000000000000a"
	dirty := clean + "_uncommitted_d5d45ef8"
	other := "aaaa1111bbbb2222cccc3333dddd4444eeee5555"
	vs := []string{dirty, clean, other}

	for _, tc := range []struct{ in, want string }{
		{clean, clean},
		{dirty, dirty},
		{shortVersion(clean), clean},
		{shortVersion(dirty), dirty},
		{"aaaa11", other},
	} {
		got, err := matchVersion(vs, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("matchVersion(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}

	_, err := matchVersion(vs, "3df59")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), shortVersion(dirty)) || strings.Contains(err.Error(), clean) {
		t.Errorf("want ambiguous error with short forms, got %v", err)
	}
	if _, err := matchVersion(vs, "zzz"); err == nil {
		t.Error("want no-match error")
	}
}
