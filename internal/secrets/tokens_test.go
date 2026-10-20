package secrets

import (
	"bytes"
	"strings"
	"testing"
)

const (
	testTok = "yoho_token_4fK9aB3cD5eF7gH9iJ1kL3mN5oP7qR9sT1uV3wX5yZ7_aB3dE9"
	testDev = "yoho_device_Zz9Yy8Xx7Ww6Vv5Uu4Tt3Ss2Rr1Qq0Pp9Oo8Nn7Mm6L_Qw3rTy"
)

func TestRedactorAccessTokens(t *testing.T) {
	input := "token=" + testTok + " device: " + testDev + ". yoho_token_ alone, yoho_ok\n"
	want := "token=[REDACTED:yoho_token] device: [REDACTED:yoho_device]. yoho_token_ alone, yoho_ok\n"

	for cut := 0; cut <= len(input); cut++ {
		var out bytes.Buffer
		r := NewRedactor(&out)
		r.Write([]byte(input[:cut]))
		r.Write([]byte(input[cut:]))
		r.Close()
		if out.String() != want {
			t.Fatalf("cut %d: got %q want %q", cut, out.String(), want)
		}
	}

	var out bytes.Buffer
	r := NewRedactor(&out)
	for i := 0; i < len(input); i++ {
		r.Write([]byte{input[i]})
	}
	r.Close()
	if out.String() != want {
		t.Errorf("byte-wise: got %q want %q", out.String(), want)
	}
}

func TestRedactorAccessTokenWithSecrets(t *testing.T) {
	var out bytes.Buffer
	r := NewRedactor(&out, "s3cr3t-value")
	r.Write([]byte("a s3cr3t-value " + testTok + " b"))
	r.Close()
	if got, want := out.String(), "a *** [REDACTED:yoho_token] b"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if strings.Contains(out.String(), "4fK9") {
		t.Error("token leaked")
	}
}

func TestRedactorAccessTokenStreamsEagerly(t *testing.T) {
	var out bytes.Buffer
	r := NewRedactor(&out)
	r.Write([]byte("hello world\n"))
	if out.String() != "hello world\n" {
		t.Errorf("held back unrelated output: %q", out.String())
	}
	r.Write([]byte("x yoho_tok"))
	if out.String() != "hello world\nx " {
		t.Errorf("got %q", out.String())
	}
}

func tokenWithRandom(random string) string {
	return "yoho_token_" + random + "_aB3dE9"
}

func TestRedactorTokenRandomPartContainsYoho(t *testing.T) {
	randoms := []string{
		strings.Repeat("a", 39) + "yoho",
		"yoho" + strings.Repeat("b", 39),
		"yoyoho" + strings.Repeat("c", 17) + "yoho" + strings.Repeat("d", 16),
		strings.Repeat("e", 20) + "yoho" + strings.Repeat("f", 19),
	}
	for _, random := range randoms {
		if len(random) != 43 {
			t.Fatalf("bad test random %q (%d)", random, len(random))
		}
		tok := tokenWithRandom(random)
		input := "pre " + tok + " post yoho_"
		want := "pre [REDACTED:yoho_token] post yoho_"
		for cut := 0; cut <= len(input); cut++ {
			var out bytes.Buffer
			r := NewRedactor(&out)
			r.Write([]byte(input[:cut]))
			r.Write([]byte(input[cut:]))
			r.Close()
			if out.String() != want {
				t.Fatalf("random %q cut %d: got %q want %q", random, cut, out.String(), want)
			}
		}
		// Three-way splits at a stride.
		for a := 0; a < len(input); a += 3 {
			for b := a; b <= len(input); b += 5 {
				var out bytes.Buffer
				r := NewRedactor(&out)
				r.Write([]byte(input[:a]))
				r.Write([]byte(input[a:b]))
				r.Write([]byte(input[b:]))
				r.Close()
				if out.String() != want {
					t.Fatalf("random %q cuts %d,%d: got %q", random, a, b, out.String())
				}
			}
		}
	}
}

func TestAccessTokenHoldBounded(t *testing.T) {
	data := []byte(strings.Repeat("yoho_token_", 20))
	if h := accessTokenHold(data); h >= maxTokenLen {
		t.Errorf("hold %d not below max token length %d", h, maxTokenLen)
	}
	if h := accessTokenHold([]byte("hello world")); h != 0 {
		t.Errorf("unrelated hold = %d", h)
	}
}

func TestRedactStringRedactsAccessTokens(t *testing.T) {
	got := redactString("failed: "+testTok+" and "+testDev+" with s3cr3t-value", []string{"s3cr3t-value"})
	want := "failed: [REDACTED:yoho_token] and [REDACTED:yoho_device] with ***"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got := redactString("no secrets here yoho_token_", nil); got != "no secrets here yoho_token_" {
		t.Errorf("unrelated changed: %q", got)
	}
}

func TestAccessTokenRegexRejectsWrongLengths(t *testing.T) {
	good := tokenWithRandom(strings.Repeat("a", 43))
	if !accessTokenRe.MatchString(good) {
		t.Fatal("well-formed token not matched")
	}
	for name, s := range map[string]string{
		"short random":   "yoho_token_" + strings.Repeat("a", 42) + "_aB3dE9",
		"long random":    "yoho_token_" + strings.Repeat("a", 44) + "_aB3dE9",
		"short checksum": "yoho_token_" + strings.Repeat("a", 43) + "_aB3dE",
		"no separator":   "yoho_token_" + strings.Repeat("a", 44) + "aB3dE9",
		"bad kind":       "yoho_other_" + strings.Repeat("a", 43) + "_aB3dE9",
	} {
		if loc := accessTokenRe.FindStringIndex(s); loc != nil {
			t.Errorf("%s: unexpectedly matched %q", name, s[loc[0]:loc[1]])
		}
	}
	// Extra trailing checksum chars: the exact-length prefix still matches,
	// the extra byte stays visible but the secret body is masked.
	if !accessTokenRe.MatchString(good + "X") {
		t.Error("token followed by extra char should still redact its body")
	}
}
