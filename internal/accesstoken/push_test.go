package accesstoken

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type call struct {
	name  string
	args  []string
	stdin string
}

func fake(t *testing.T, out string, err error) *[]call {
	t.Helper()
	var calls []call
	oldRun, oldLook := run, lookPath
	t.Cleanup(func() { run, lookPath = oldRun, oldLook })
	lookPath = func(string) (string, error) { return "/bin/x", nil }
	run = func(_ context.Context, name string, args []string, stdin string) ([]byte, error) {
		calls = append(calls, call{name, args, stdin})
		return []byte(out), err
	}
	return &calls
}

func TestParseTarget(t *testing.T) {
	ok := []struct {
		in   string
		want Target
	}{
		{"github:acme/app", Target{GitHub, "acme/app", ""}},
		{"github:acme/app@production", Target{GitHub, "acme/app", "production"}},
		{"gitlab:grp/proj", Target{GitLab, "grp/proj", ""}},
		{"gitlab:grp/sub/proj@staging", Target{GitLab, "grp/sub/proj", "staging"}},
	}
	for _, tc := range ok {
		got, err := ParseTarget(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
		if got.String() != tc.in {
			t.Errorf("String round trip %q != %q", got.String(), tc.in)
		}
	}
	for _, in := range []string{
		"", "acme/app", "bitbucket:a/b", "github:", "github:acme", "github:acme/",
		"github:a/b/c", "github:acme/app@", "github:/app", "gitlab:solo",
		"github:-x/app", "github:acme/app@-e", "github:acme/app env", "github:a/b@x@y",
	} {
		if _, err := ParseTarget(in); err == nil {
			t.Errorf("ParseTarget(%q) should fail", in)
		}
	}
}

func TestPushGitHub(t *testing.T) {
	calls := fake(t, "", nil)
	const secret = "yoho_token_SECRETVALUE_abcdef"
	tgt := Target{GitHub, "acme/app", "prod"}
	if err := Push(context.Background(), tgt, "YOHO_TOKEN", secret); err != nil {
		t.Fatal(err)
	}
	want := call{"gh", []string{"secret", "set", "YOHO_TOKEN", "-R", "acme/app", "--env", "prod"}, secret}
	if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], want) {
		t.Errorf("got %+v want %+v", *calls, want)
	}

	*calls = nil
	Push(context.Background(), Target{GitHub, "acme/app", ""}, "N", secret)
	if got := (*calls)[0].args; !reflect.DeepEqual(got, []string{"secret", "set", "N", "-R", "acme/app"}) {
		t.Errorf("no-env args %v", got)
	}
}

func TestPushGitLab(t *testing.T) {
	calls := fake(t, "", nil)
	const secret = "yoho_device_SECRETVALUE_abcdef"
	if err := Push(context.Background(), Target{GitLab, "grp/sub/proj", "prod"}, "YOHO_DEVICE", secret); err != nil {
		t.Fatal(err)
	}
	want := call{"glab", []string{"variable", "set", "YOHO_DEVICE", "-R", "grp/sub/proj", "--masked", "--protected", "--scope", "prod"}, secret}
	if !reflect.DeepEqual((*calls)[0], want) {
		t.Errorf("got %+v want %+v", (*calls)[0], want)
	}
}

func TestPushNeverPutsValueInArgv(t *testing.T) {
	const secret = "yoho_token_TOPSECRET_zzzzzz"
	for _, tgt := range []Target{{GitHub, "a/b", "e"}, {GitHub, "a/b", ""}, {GitLab, "g/p", "e"}, {GitLab, "g/p", ""}} {
		calls := fake(t, "", nil)
		if err := Push(context.Background(), tgt, "NAME", secret); err != nil {
			t.Fatal(err)
		}
		c := (*calls)[0]
		if strings.Contains(c.name+" "+strings.Join(c.args, " "), "TOPSECRET") {
			t.Errorf("%v: value leaked into argv %v", tgt, c.args)
		}
		if c.stdin != secret {
			t.Errorf("%v: stdin %q", tgt, c.stdin)
		}
	}
}

func TestPushVariable(t *testing.T) {
	calls := fake(t, "", nil)
	ctx := context.Background()
	if err := PushVariable(ctx, Target{GitHub, "a/b", "prod"}, "YOHO_URL", "https://x"); err != nil {
		t.Fatal(err)
	}
	if err := PushVariable(ctx, Target{GitLab, "g/p", ""}, "YOHO_URL", "https://x"); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"gh", []string{"variable", "set", "YOHO_URL", "--body", "https://x", "-R", "a/b", "--env", "prod"}, ""},
		{"glab", []string{"variable", "set", "YOHO_URL", "https://x", "-R", "g/p"}, ""},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("got %+v want %+v", *calls, want)
	}
}

func TestPushErrors(t *testing.T) {
	ctx := context.Background()

	fake(t, "", nil)
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	err := Push(ctx, Target{GitHub, "a/b", ""}, "N", "v")
	if err == nil || !strings.Contains(err.Error(), "gh not found") {
		t.Errorf("missing gh: %v", err)
	}
	err = Push(ctx, Target{GitLab, "g/p", ""}, "N", "v")
	if err == nil || !strings.Contains(err.Error(), "glab not found") {
		t.Errorf("missing glab: %v", err)
	}

	calls := fake(t, "HTTP 403 for yoho_token_LEAKY_abcdef", errors.New("exit status 1"))
	err = Push(ctx, Target{GitHub, "a/b", ""}, "N", "yoho_token_LEAKY_abcdef")
	if err == nil || strings.Contains(err.Error(), "LEAKY") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("command failure should be redacted: %v", err)
	}

	*calls = nil
	for _, name := range []string{"", "bad name", "-x", "a=b"} {
		if err := Push(ctx, Target{GitHub, "a/b", ""}, name, "v"); err == nil {
			t.Errorf("name %q should fail", name)
		}
	}
	if len(*calls) != 0 {
		t.Error("invalid names must not run commands")
	}
}
