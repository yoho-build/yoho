package secrets

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
)

// injectRunner is a fakeRunner that also answers op inject from values,
// keyed by "<account>|<ref>".
type injectRunner struct {
	*fakeRunner
	values map[string]string
	fail   *fakeAnswer
}

var tplRef = regexp.MustCompile(`\{\{ (.*?) \}\}`)

func (f *injectRunner) RunInput(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(argv, " "))
	if f.fail != nil {
		return nil, []byte(f.fail.stderr), f.fail.err
	}
	account := ""
	if len(argv) == 4 && argv[2] == "--account" {
		account = argv[3]
	}
	var missing bool
	out := tplRef.ReplaceAllStringFunc(string(stdin), func(m string) string {
		v, ok := f.values[account+"|"+tplRef.FindStringSubmatch(m)[1]]
		missing = missing || !ok
		return v
	})
	if missing {
		return nil, []byte(`[ERROR] "x" isn't an item`), errors.New("exit status 1")
	}
	return []byte(out), nil, nil
}

func (f *injectRunner) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestLoadBatchesOpReads(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "FROM_FILE=$(op read op://P/app/token)\nQUOTED=\"$(op read --account work 'op://P/My Item/key')\"\nDERIVED=\"pg://$(op read -n op://P/db/password)\"\n")
	f := &injectRunner{fakeRunner: &fakeRunner{}, values: map[string]string{
		"|op://P/db/password":     "dbpass",
		"|op://P/app/token":       "tok\n",
		"|op://P/tls/pem":         "-----BEGIN-----\nabc\n-----END-----\n",
		"|op://P/odd/value":       "{{ op://P/db/password }}",
		"work|op://P/My Item/key": "workkey",
		"work|op://P/w/two":       "two",
	}}
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"vault": {Type: "op"}, "work": {Type: "op", Account: "work"}},
		Values: map[string]config.SecretValue{
			"DB":  {Provider: "vault", Ref: "op://P/db/password"},
			"PEM": {Provider: "vault", Ref: "op://P/tls/pem"},
			"ODD": {Provider: "vault", Ref: "op://P/odd/value"},
			"TWO": {Provider: "work", Ref: "op://P/w/two"},
		},
	}
	s, err := Load(context.Background(), dir, "production", cfg, LoadOptions{Runner: f, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DB": "dbpass", "PEM": "-----BEGIN-----\nabc\n-----END-----\n", "ODD": "{{ op://P/db/password }}", "TWO": "two",
		"FROM_FILE": "tok", "QUOTED": "workkey", "DERIVED": "pg://dbpass",
	}
	for k, w := range want {
		if v, _ := s.Get(k); v != w {
			t.Errorf("%s = %q want %q", k, v, w)
		}
	}
	if f.count("op inject") != 2 || f.count("op inject --account work") != 1 || f.count("op read") != 0 || f.count("sh ") != 0 {
		t.Errorf("calls %v", f.calls)
	}
}

func TestLoadOpBatchFallsBackPerReference(t *testing.T) {
	f := &injectRunner{fakeRunner: &fakeRunner{answers: map[string]fakeAnswer{
		"op read --no-newline op://P/a/x":    {out: "a"},
		"op read --no-newline op://P/gone/x": {stderr: `[ERROR] "gone" isn't an item`, err: errors.New("exit status 1")},
	}}, values: map[string]string{"|op://P/a/x": "a"}}
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"v": {Type: "op"}},
		Values:    map[string]config.SecretValue{"A": {Provider: "v", Ref: "op://P/a/x"}, "GONE": {Provider: "v", Ref: "op://P/gone/x"}},
	}
	var warns []string
	_, err := Load(context.Background(), t.TempDir(), "", cfg, LoadOptions{Runner: f, Getenv: noEnv, Warn: func(m string) { warns = append(warns, m) }})
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Class != NotFound || !strings.Contains(err.Error(), "secret GONE") || strings.Contains(err.Error(), "secret A:") {
		t.Fatalf("err %v", err)
	}
	if f.count("op inject") != 1 || f.count("op read") != 2 || len(warns) != 1 {
		t.Errorf("calls %v warns %v", f.calls, warns)
	}
}

func TestLoadOpBatchStopsWhenNotSignedIn(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "B=$(op read op://P/b/x)\n")
	f := &injectRunner{fakeRunner: &fakeRunner{}, fail: &fakeAnswer{stderr: "[ERROR] You are not currently signed in.", err: errors.New("exit status 1")}}
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"v": {Type: "op"}},
		Values:    map[string]config.SecretValue{"A": {Provider: "v", Ref: "op://P/a/x"}},
	}
	_, err := Load(context.Background(), dir, "", cfg, LoadOptions{Runner: f, Getenv: noEnv})
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Class != NotSignedIn || !strings.Contains(err.Error(), "secret A") {
		t.Fatalf("err %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls %v", f.calls)
	}
}

func TestLoadOpBatchSkipsFilesSettingOpEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "OP_ACCOUNT=work\nA=$(op read op://P/a/x)\nB=$(op read op://P/b/x)\n")
	f := &injectRunner{fakeRunner: &fakeRunner{answers: map[string]fakeAnswer{
		"sh -c op read op://P/a/x": {out: "a\n"},
		"sh -c op read op://P/b/x": {out: "b\n"},
	}}}
	s, err := Load(context.Background(), dir, "", config.SecretsConfig{}, LoadOptions{Runner: f, Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get("B"); v != "b" || f.count("op inject") != 0 {
		t.Errorf("B=%q calls %v", v, f.calls)
	}
}

func TestParseOpRead(t *testing.T) {
	for _, c := range []struct {
		cmd     string
		account string
		ref     string
		ok      bool
	}{
		{"op read op://V/i/f", "", "op://V/i/f", true},
		{"op read --no-newline op://V/i/f", "", "op://V/i/f", true},
		{"  op  read -n  'op://V/My Item/f' ", "", "op://V/My Item/f", true},
		{`op read --account my "op://V/i/f?attribute=otp"`, "my", "op://V/i/f?attribute=otp", true},
		{"op read --account=my op://V/i/f", "my", "op://V/i/f", true},
		{"op read op://$VAULT/i/f", "", "", false},
		{`op read "op://${V}/i/f"`, "", "", false},
		{"op read op://V/i/f | tr a b", "", "", false},
		{"op read op://V/i/f; rm x", "", "", false},
		{"op read --out-file x op://V/i/f", "", "", false},
		{"op read --account op://V/i/f", "", "", false},
		{"OP_ACCOUNT=x op read op://V/i/f", "", "", false},
		{"op item get x", "", "", false},
		{"op read", "", "", false},
		{"op read 'op://V/i/f", "", "", false},
	} {
		r, ok := parseOpRead(c.cmd)
		if ok != c.ok || ok && (r.account != c.account || r.ref != c.ref) {
			t.Errorf("%q: got %+v %v", c.cmd, r, ok)
		}
	}
}
