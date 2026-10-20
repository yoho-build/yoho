package accesstoken

import (
	"errors"
	"strings"
	"testing"
)

func TestNewParseRoundTrip(t *testing.T) {
	for _, kind := range []Kind{Token, Device} {
		seen := map[string]bool{}
		for i := 0; i < 200; i++ {
			s, err := New(kind)
			if err != nil {
				t.Fatal(err)
			}
			if seen[s] {
				t.Fatalf("duplicate %q", s)
			}
			seen[s] = true
			if !strings.HasPrefix(s, kind.Prefix()) {
				t.Fatalf("%q lacks prefix %q", s, kind.Prefix())
			}
			if got, err := Parse(s); err != nil || got != kind {
				t.Fatalf("Parse(%q) = %v, %v; want %v", s, got, err, kind)
			}
		}
	}
	if _, err := New(Kind(0)); err == nil {
		t.Error("New(0) should fail")
	}
}

func TestParseErrors(t *testing.T) {
	good, _ := New(Token)
	body := strings.TrimPrefix(good, tokenPrefix)

	// Flip one character in the random part to a different base62 char.
	flip := func(s string, at int) string {
		b := []byte(s)
		if b[at] == 'A' {
			b[at] = 'B'
		} else {
			b[at] = 'A'
		}
		return string(b)
	}
	// Swap the prefix kind without fixing the checksum.
	wrongKind := devicePrefix + body

	tests := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrMalformed},
		{"unknown prefix", "ghp_" + body, ErrMalformed},
		{"prefix only", tokenPrefix, ErrMalformed},
		{"truncated", good[:len(good)-1], ErrMalformed},
		{"too long", good + "A", ErrMalformed},
		{"bad separator", tokenPrefix + body[:randomLen] + "-" + body[randomLen+1:], ErrMalformed},
		{"bad char in random", tokenPrefix + "!" + body[1:], ErrMalformed},
		{"bad char in checksum", good[:len(good)-1] + "-", ErrMalformed},
		{"whitespace", good + "\n", ErrMalformed},
		{"typo first random char", flip(good, len(tokenPrefix)), ErrChecksum},
		{"typo middle random char", flip(good, len(tokenPrefix)+20), ErrChecksum},
		{"typo last random char", flip(good, len(tokenPrefix)+randomLen-1), ErrChecksum},
		{"typo checksum char", flip(good, len(good)-1), ErrChecksum},
		{"kind swapped", wrongKind, ErrChecksum},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.in)
			if !errors.Is(err, tc.want) {
				t.Errorf("Parse(%q) err = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

func TestEveryRandomCharTypoDetected(t *testing.T) {
	good, _ := New(Device)
	start := len(devicePrefix)
	for i := start; i < start+randomLen; i++ {
		for _, c := range []byte("0Az") {
			if good[i] == c {
				continue
			}
			b := []byte(good)
			b[i] = c
			if _, err := Parse(string(b)); !errors.Is(err, ErrChecksum) {
				t.Fatalf("pos %d -> %c not detected: %v", i, c, err)
			}
		}
	}
}

func TestHashAndMatch(t *testing.T) {
	a, _ := New(Token)
	b, _ := New(Token)
	h := Hash(a)
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("hash form %q", h)
	}
	if h != Hash(a) {
		t.Error("hash not deterministic")
	}
	if strings.Contains(h, a) {
		t.Error("hash contains plaintext")
	}
	tests := []struct {
		name      string
		plaintext string
		hash      string
		want      bool
	}{
		{"match", a, h, true},
		{"other token", b, h, false},
		{"empty plaintext", "", h, false},
		{"empty hash", a, "", false},
		{"unprefixed hash", a, strings.TrimPrefix(h, "sha256:"), false},
	}
	for _, tc := range tests {
		if got := ConstantTimeMatch(tc.plaintext, tc.hash); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestShort(t *testing.T) {
	tok, _ := New(Token)
	dev, _ := New(Device)
	if got, want := Short(tok), tokenPrefix+tok[len(tokenPrefix):len(tokenPrefix)+4]+"…"; got != want {
		t.Errorf("Short(token) = %q want %q", got, want)
	}
	if got := Short(dev); !strings.HasPrefix(got, devicePrefix) || !strings.HasSuffix(got, "…") || len(got) != len(devicePrefix)+4+len("…") {
		t.Errorf("Short(device) = %q", got)
	}
	if got := Short("not a token secret"); got != "…" {
		t.Errorf("Short(garbage) = %q", got)
	}
	// A mistyped token names its kind but echoes none of its characters.
	b := []byte(tok)
	if b[len(b)-1] == '0' {
		b[len(b)-1] = '1'
	} else {
		b[len(b)-1] = '0'
	}
	if got := Short(string(b)); got != InvalidShort {
		t.Errorf("Short(typo) = %q", got)
	}
}

func TestKindString(t *testing.T) {
	if Token.String() != "token" || Device.String() != "device" || Kind(9).String() != "unknown" {
		t.Error("Kind.String")
	}
}
