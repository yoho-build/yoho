package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-dev/yoho/internal/config"
)

// fakeRunner answers by joined argv; records calls.
type fakeRunner struct {
	mu      sync.Mutex
	answers map[string]fakeAnswer
	calls   []string
	envs    [][]string
}

type fakeAnswer struct {
	out, stderr string
	err         error
}

func (f *fakeRunner) Run(_ context.Context, argv []string, env []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := strings.Join(argv, " ")
	f.calls = append(f.calls, k)
	f.envs = append(f.envs, env)
	a, ok := f.answers[k]
	if !ok {
		return nil, []byte("no such command"), errors.New("exit status 127")
	}
	return []byte(a.out), []byte(a.stderr), a.err
}

func noEnv(string) (string, bool) { return "", false }

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, ".yoho", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseDotenv(t *testing.T) {
	src := `# comment
export A=plain value   # trailing comment
B='single $A "x" # not comment'
C="dq \"q\" \n tab\t \$A $A ${A}x"
D=url#fragment
E=
F="multi
line"
  G = spaced
H=$(echo "a)b" 'c(d') tail
I="$(printf '%s' "x")"
J=$5 $ ${not valid}
`
	entries, err := parseDotenv(".yoho/secrets", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]part{}
	lines := map[string]int{}
	for _, e := range entries {
		got[e.key] = e.parts
		lines[e.key] = e.line
	}
	want := map[string][]part{
		"A": {{litPart, "plain value"}},
		"B": {{litPart, `single $A "x" # not comment`}},
		"C": {{litPart, "dq \"q\" \n tab\t $A "}, {varPart, "A"}, {litPart, " "}, {varPart, "A"}, {litPart, "x"}},
		"D": {{litPart, "url#fragment"}},
		"E": nil,
		"F": {{litPart, "multi\nline"}},
		"G": {{litPart, "spaced"}},
		"H": {{cmdPart, `echo "a)b" 'c(d'`}, {litPart, " tail"}},
		"I": {{cmdPart, `printf '%s' "x"`}},
		"J": {{litPart, "$5 $ ${not valid}"}},
	}
	if !reflect.DeepEqual(got, want) {
		for k := range want {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("%s: got %#v want %#v", k, got[k], want[k])
			}
		}
	}
	if lines["F"] != 7 || lines["G"] != 9 {
		t.Errorf("line numbers: %v", lines)
	}
}

func TestParseDotenvErrors(t *testing.T) {
	for _, src := range []string{
		"A='open",
		"A=\"open",
		"A=$(echo",
		"=x",
		"A x",
		"A='x' junk",
	} {
		if _, err := parseDotenv("f", []byte(src)); err == nil {
			t.Errorf("%q: want error", src)
		} else if !strings.HasPrefix(err.Error(), "f:1:") {
			t.Errorf("%q: error lacks position: %v", src, err)
		}
	}
}

func TestLoadRealShell(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", `TOKEN=$(printf 'abc\n\n')
URL="https://u:$TOKEN@h"
FROM_ENV=$(printf %s "$TOKEN-x")
`)
	s, err := Load(context.Background(), dir, "", config.SecretsConfig{}, LoadOptions{Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	check := map[string]string{"TOKEN": "abc", "URL": "https://u:abc@h", "FROM_ENV": "abc-x"}
	for k, want := range check {
		if v, _ := s.Get(k); v != want {
			t.Errorf("%s = %q want %q", k, v, want)
		}
	}
	if s.Ref("URL") != "file:.yoho/secrets" {
		t.Errorf("ref %q", s.Ref("URL"))
	}
}

func TestLoadCommandFailureRedactsStderr(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "A=hunter22\nB=$(echo leaked hunter22 >&2; exit 3)\n")
	_, err := Load(context.Background(), dir, "", config.SecretsConfig{}, LoadOptions{Getenv: noEnv})
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if strings.Contains(msg, "hunter22") || !strings.Contains(msg, "leaked ***") || !strings.Contains(msg, ".yoho/secrets:2: B") {
		t.Fatalf("bad error: %s", msg)
	}
}

func TestFileKeysDoesNotRunCommands(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	writeFile(t, dir, "secrets", "A=$(touch "+marker+")\n")
	writeFile(t, dir, "secrets.production", "B=1\nA=2\n")
	keys, err := FileKeys(dir, "production")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"A", "B"}) {
		t.Errorf("keys %v", keys)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("FileKeys ran a command")
	}
}

func TestLoadPrecedenceAndProviders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "COMMON=c\nOVER=common\nFILE_WINS=file\nDERIVED=\"pg://$DB\"\n")
	writeFile(t, dir, "secrets.production", "OVER=prod\n")
	f := &fakeRunner{answers: map[string]fakeAnswer{
		"op read --no-newline --account my op://P/db/password": {out: "dbpass"},
		"bw get password github":                               {out: "ghpass\n"},
		"bw get item app":                                      {out: `{"login":{"username":"bob"},"fields":[{"name":"api","value":"apikey"}]}`},
		"bws secret get 123 -o json":                           {out: `{"id":"123","value":"bwsval"}`},
		"helper x":                                             {out: "helped\n"},
	}}
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{
			"vault": {Type: "op", Account: "my"},
			"bw":    {Type: "bw"},
			"sm":    {Type: "bws"},
			"cmd":   {Type: "command", Command: []string{"helper"}},
		},
		Values: map[string]config.SecretValue{
			"DB":        {Provider: "vault", Ref: "op://P/db/password"},
			"DB_AGAIN":  {Provider: "vault", Ref: "op://P/db/password"},
			"GH":        {Provider: "bw", Ref: "github"},
			"API":       {Provider: "bw", Ref: "app/api"},
			"USER":      {Provider: "bw", Ref: "app/username"},
			"SM":        {Provider: "sm", Ref: "123"},
			"CMD":       {Provider: "cmd", Ref: "x"},
			"FILE_WINS": {Provider: "vault", Ref: "op://never"},
			"STAGING":   {Provider: "vault", Ref: "op://never", Destinations: []string{"staging"}},
		},
	}
	var warns []string
	s, err := Load(context.Background(), dir, "production", cfg, LoadOptions{
		Runner: f,
		Getenv: func(k string) (string, bool) { return "tok", k == "BWS_ACCESS_TOKEN" },
		Warn:   func(m string) { warns = append(warns, m) },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"COMMON": "c", "OVER": "prod", "FILE_WINS": "file", "DERIVED": "pg://dbpass",
		"DB": "dbpass", "DB_AGAIN": "dbpass", "GH": "ghpass", "API": "apikey", "USER": "bob",
		"SM": "bwsval", "CMD": "helped",
	}
	for k, w := range want {
		if v, ok := s.Get(k); !ok || v != w {
			t.Errorf("%s = %q want %q", k, v, w)
		}
	}
	if _, ok := s.Get("STAGING"); ok {
		t.Error("STAGING should be filtered by destination")
	}
	if len(s.Keys()) != len(want) {
		t.Errorf("keys %v", s.Keys())
	}
	if s.Ref("DB") != "vault:op://P/db/password" || s.Ref("OVER") != "file:.yoho/secrets.production" {
		t.Errorf("refs %q %q", s.Ref("DB"), s.Ref("OVER"))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "FILE_WINS") {
		t.Errorf("warns %v", warns)
	}
	counts := map[string]int{}
	for _, c := range f.calls {
		counts[c]++
	}
	if counts["op read --no-newline --account my op://P/db/password"] != 1 || counts["bw get item app"] != 1 {
		t.Errorf("cache not used: %v", f.calls)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "op://never") {
			t.Errorf("overridden/filtered ref was read: %s", c)
		}
	}
}

func TestLoadProviderErrors(t *testing.T) {
	f := &fakeRunner{answers: map[string]fakeAnswer{
		"op read --no-newline op://bad": {out: "partialsecret", stderr: "[ERROR] item not found partialsecret", err: errors.New("exit status 1")},
	}}
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"v": {Type: "op"}, "sm": {Type: "bws"}},
		Values: map[string]config.SecretValue{
			"A": {Provider: "v", Ref: "op://bad"},
			"B": {Provider: "sm", Ref: "id"},
		},
	}
	_, err := Load(context.Background(), t.TempDir(), "", cfg, LoadOptions{Runner: f, Getenv: noEnv})
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	for _, want := range []string{`secret A: provider "v" (op) ref "op://bad"`, "item not found ***", `secret B: provider "sm" (bws)`, "BWS_ACCESS_TOKEN"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "partialsecret") {
		t.Errorf("value leaked: %s", msg)
	}

	cfg.Values = map[string]config.SecretValue{"C": {Provider: "nope", Ref: "x"}}
	if _, err := Load(context.Background(), t.TempDir(), "", cfg, LoadOptions{Runner: f}); err == nil || !strings.Contains(err.Error(), `unknown provider "nope"`) {
		t.Errorf("err %v", err)
	}
}

func TestLoadShellEnvIncludesProviderValues(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "secrets", "X=$(use-it)\n")
	f := &fakeRunner{answers: map[string]fakeAnswer{
		"cmd r":        {out: "pv"},
		"sh -c use-it": {out: "ok"},
	}}
	cfg := config.SecretsConfig{
		Providers: map[string]config.SecretProvider{"c": {Type: "command", Command: []string{"cmd"}}},
		Values:    map[string]config.SecretValue{"P": {Provider: "c", Ref: "r"}},
	}
	if _, err := Load(context.Background(), dir, "", cfg, LoadOptions{Runner: f, Getenv: noEnv}); err != nil {
		t.Fatal(err)
	}
	last := f.envs[len(f.envs)-1]
	if !reflect.DeepEqual(last, []string{"P=pv"}) {
		t.Errorf("env %v", last)
	}
}

func TestParseRefsAndForService(t *testing.T) {
	m, err := ParseRefs(map[string]any{"DATABASE_PASSWORD": "DB", "API": "API"})
	if err != nil {
		t.Fatal(err)
	}
	want := config.SecretRefs{{Name: "API", Key: "API"}, {Name: "DATABASE_PASSWORD", Key: "DB"}}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("map form %v", m)
	}
	l, err := ParseRefs([]any{"API", "DATABASE_PASSWORD:DB"})
	if err != nil || !reflect.DeepEqual(l, want) {
		t.Errorf("list form %v %v", l, err)
	}
	for _, bad := range []any{[]any{"A", "A:B"}, []any{1}, "x", []any{"bad-name"}, map[string]any{"A": 1}} {
		if _, err := ParseRefs(bad); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}

	s := New(map[string]string{"DB": "pw", "API": "k", "OTHER": "o"})
	got, err := s.ForService(want)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"API": "k", "DATABASE_PASSWORD": "pw"}) {
		t.Errorf("ForService %v %v", got, err)
	}
	_, err = s.ForService(config.SecretRefs{{Name: "A", Key: "ZED"}, {Name: "B", Key: "MISSING"}, {Name: "C", Key: "ZED"}})
	if err == nil || !strings.Contains(err.Error(), "missing secrets: MISSING, ZED (") {
		t.Errorf("err %v", err)
	}
}

func TestRedactorBoundaries(t *testing.T) {
	secret := "s3cr3t-value"
	var out bytes.Buffer
	r := NewRedactor(&out, secret, "abc") // "abc" is too short to redact
	input := "a s3cr3t-value b s3cr3t-value abc " + base64.StdEncoding.EncodeToString([]byte(secret)) +
		" " + url.QueryEscape("p@ss w/rd") + " end s3cr3"
	r2out := &bytes.Buffer{}
	r2 := NewRedactor(r2out, secret, "p@ss w/rd")
	// Every split point, byte by byte.
	for i := 0; i < len(input); i++ {
		r2.Write([]byte{input[i]})
	}
	r2.Flush()
	want := "a *** b *** abc *** *** end s3cr3"
	if r2out.String() != want {
		t.Errorf("byte-wise:\n got %q\nwant %q", r2out.String(), want)
	}
	for cut := 0; cut <= len(input); cut++ {
		out.Reset()
		r = NewRedactor(&out, secret, "p@ss w/rd")
		r.Write([]byte(input[:cut]))
		r.Write([]byte(input[cut:]))
		r.Close()
		if out.String() != want {
			t.Fatalf("cut %d: got %q", cut, out.String())
		}
	}
}

func TestRedactorStreamsEagerly(t *testing.T) {
	var out bytes.Buffer
	r := NewRedactor(&out, "secretvalue")
	r.Write([]byte("hello world\n"))
	if out.String() != "hello world\n" {
		t.Errorf("held back unrelated output: %q", out.String())
	}
	r.Write([]byte("x secr"))
	if out.String() != "hello world\nx " {
		t.Errorf("got %q", out.String())
	}
}

func TestRedactorLongestAndStoreLabels(t *testing.T) {
	var out bytes.Buffer
	s := New(map[string]string{"SHORT": "abcd", "LONG": "abcdefgh", "ML": "line1\nline2"})
	r := s.Redactor(&out)
	r.Write([]byte("abcdef"))
	r.Write([]byte("gh abcd! {\"k\":\"line1\\nline2\"}"))
	r.Flush()
	want := `[REDACTED:LONG] [REDACTED:SHORT]! {"k":"[REDACTED:ML]"}`
	if out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

func TestBuildxArgs(t *testing.T) {
	s := New(map[string]string{"NPM_TOKEN": "tok", "GH": "g"})
	args, env, err := s.BuildxArgs([]string{"NPM_TOKEN", "GH"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args, []string{"--secret", "id=NPM_TOKEN,env=NPM_TOKEN", "--secret", "id=GH,env=GH"}) {
		t.Errorf("args %v", args)
	}
	if !reflect.DeepEqual(env, []string{"NPM_TOKEN=tok", "GH=g"}) {
		t.Errorf("env %v", env)
	}
	for _, a := range args {
		if strings.Contains(a, "tok") && !strings.Contains(a, "NPM_TOKEN") {
			t.Errorf("value in argv: %v", args)
		}
	}
	if _, _, err := s.BuildxArgs([]string{"NOPE"}); err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("err %v", err)
	}
	if _, _, err := s.BuildxArgs([]string{"a=b"}); err == nil {
		t.Error("want invalid name error")
	}
}

func TestFindInterpolatedSecrets(t *testing.T) {
	yaml := `services:
  web:
    environment:
      A: $DB_PASSWORD
      B: ${API_KEY:-fallback}
      C: ${TOKEN?required}
      D: $$NOT_INTERPOLATED
      E: ${IMAGE_TAG}
      F: "${API_KEY}"
      G: $DB_PASSWORD_HASH
`
	got := FindInterpolatedSecrets([]byte(yaml), []string{"DB_PASSWORD", "API_KEY", "TOKEN", "NOT_INTERPOLATED"})
	if !reflect.DeepEqual(got, []string{"API_KEY", "DB_PASSWORD", "TOKEN"}) {
		t.Errorf("got %v", got)
	}
}

func TestDescribeAndReveal(t *testing.T) {
	s := New(map[string]string{"B": "two words", "A": "x"})
	d := Describe(s, []byte("k"))
	if len(d) != 2 || d[0].Key != "A" || d[0].Length != 1 || d[0].Ref != "inline" || d[0].Fingerprint != Fingerprint([]byte("k"), "x") {
		t.Errorf("describe %+v", d)
	}
	if Describe(s, nil)[0].Fingerprint != "" {
		t.Error("fingerprint without key")
	}
	var out bytes.Buffer
	if err := Reveal(&out, s, nil, false); !errors.Is(err, ErrNotTTY) || out.Len() != 0 {
		t.Errorf("non-TTY reveal: %v %q", err, out.String())
	}
	if err := Reveal(&out, s, nil, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "A=x\nB='two words'\n" {
		t.Errorf("reveal %q", out.String())
	}
	if err := Reveal(&out, s, []string{"Z"}, true); err == nil {
		t.Error("want missing error")
	}
}

func TestQuoteValueRoundTrip(t *testing.T) {
	for _, v := range []string{"plain", "", "a b", "it's", "multi\nline $X \"q\" \\", "#hash"} {
		entries, err := parseDotenv("f", []byte("K="+quoteValue(v)+"\n"))
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		got, err := evalEntry(context.Background(), entries[0], nil, nil, noEnv)
		if err != nil || got != v {
			t.Errorf("%q round-tripped to %q (%v)", v, got, err)
		}
	}
}

func TestGenerate(t *testing.T) {
	lens := map[string]int{"password32": 32, "password64": 64, "hex32": 64, "hex64": 128, "base64_32": 44, "base64_64": 88}
	for kind, n := range lens {
		v, err := Generate(kind)
		if err != nil || len(v) != n {
			t.Errorf("%s: len %d err %v", kind, len(v), err)
		}
	}
	if _, err := Generate("nope"); err == nil {
		t.Error("want error")
	}
	if Fingerprint([]byte("a"), "v") == Fingerprint([]byte("b"), "v") || len(Fingerprint(nil, "v")) != 16 {
		t.Error("fingerprint")
	}
}
