package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"
)

type call struct {
	argv  []string
	stdin string
}

type fakeExec struct {
	calls []call
	// respond sets stdout and the returned error per call index.
	respond func(i int, c call, stdout io.Writer) error
}

func (f *fakeExec) exec(_ context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) error {
	c := call{argv: argv}
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		c.stdin = string(b)
	}
	f.calls = append(f.calls, c)
	if f.respond != nil {
		return f.respond(len(f.calls)-1, c, stdout)
	}
	return nil
}

type exitCode int

func (e exitCode) Error() string { return "exit" }
func (e exitCode) ExitCode() int { return int(e) }

func newTestSSH(t *testing.T, target string, sudo bool, f *fakeExec) *SSH {
	t.Helper()
	s, err := NewSSH("web1", target, sudo, SSHOptions{ControlDir: t.TempDir(), Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in         string
		user, host string
		port       int
		bad        bool
	}{
		{in: "example.com", host: "example.com"},
		{in: "deploy@example.com:2222", user: "deploy", host: "example.com", port: 2222},
		{in: "ssh://root@10.0.0.1", user: "root", host: "10.0.0.1"},
		{in: "[::1]:22", host: "::1", port: 22},
		{in: "u@[fe80::1]", user: "u", host: "fe80::1"},
		{in: "-oProxyCommand=x", bad: true},
		{in: "host:abc", bad: true},
		{in: "host;rm", bad: true},
		{in: "@host", bad: true},
	}
	for _, c := range cases {
		u, h, p, err := ParseTarget(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil || u != c.user || h != c.host || p != c.port {
			t.Errorf("%q: got %q %q %d %v", c.in, u, h, p, err)
		}
	}
}

func TestSSHArgsAndControlPath(t *testing.T) {
	f := &fakeExec{}
	s := newTestSSH(t, "deploy@example.com:2222", false, f)
	if s.Target() != "deploy@example.com:2222" || s.Destination() != "deploy@example.com" {
		t.Fatalf("target %q dest %q", s.Target(), s.Destination())
	}
	a := strings.Join(s.SSHArgs(), " ")
	for _, want := range []string{"ControlMaster=auto", "ControlPersist=60", "BatchMode=yes", "ServerAliveInterval=15", "-p 2222"} {
		if !strings.Contains(a, want) {
			t.Errorf("args missing %q: %s", want, a)
		}
	}
	// Default control dir must stay well under the 104-byte socket limit.
	d, err := NewSSH("x", "a-very-long-user-name@a-very-long-host-name.example.internal.corp:2222", false, SSHOptions{Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.controlPath)+20 > 104 {
		t.Errorf("control path too long: %d %s", len(d.controlPath), d.controlPath)
	}
}

func TestRunScriptOnStdin(t *testing.T) {
	f := &fakeExec{}
	s := newTestSSH(t, "example.com", false, f)
	err := s.Run(context.Background(), Cmd{Script: "echo hi", Env: map[string]string{"TOKEN": "s3cr'et", "A": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]
	argv := strings.Join(c.argv, " ")
	if strings.Contains(argv, "s3cr") || strings.Contains(argv, "echo hi") {
		t.Fatalf("script or secret on argv: %s", argv)
	}
	if c.argv[len(c.argv)-1] != "sh -s" || c.argv[len(c.argv)-2] != "example.com" || c.argv[len(c.argv)-3] != "--" {
		t.Fatalf("argv tail: %v", c.argv)
	}
	want := "{\nexport A='1'\nexport TOKEN='s3cr'\\''et'\necho hi\n} </dev/null\n"
	if c.stdin != want {
		t.Fatalf("stdin:\n%q\nwant\n%q", c.stdin, want)
	}
}

func TestRunWithStdinAndEnvUsesEnvFile(t *testing.T) {
	f := &fakeExec{respond: func(i int, c call, out io.Writer) error {
		if i == 0 {
			io.WriteString(out, "/tmp/yoho-env.abc\n")
		}
		return nil
	}}
	s := newTestSSH(t, "example.com", false, f)
	err := s.Run(context.Background(), Cmd{Script: "docker load", Stdin: strings.NewReader("DATA"), Env: map[string]string{"PW": "hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls: %d", len(f.calls))
	}
	if !strings.Contains(f.calls[0].stdin, "export PW='hunter2'") {
		t.Fatalf("env not on stdin: %q", f.calls[0].stdin)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.argv, " "), "hunter2") {
			t.Fatal("secret on argv")
		}
	}
	last := f.calls[1]
	rc := last.argv[len(last.argv)-1]
	if rc != "sh -c "+Quote(". '/tmp/yoho-env.abc'; rm -f '/tmp/yoho-env.abc'\ndocker load") {
		t.Fatalf("remote cmd: %s", rc)
	}
	if last.stdin != "DATA" {
		t.Fatalf("stdin %q", last.stdin)
	}
}

func TestSudo(t *testing.T) {
	f := &fakeExec{}
	s := newTestSSH(t, "example.com", true, f)
	_ = s.Run(context.Background(), Cmd{Script: "id", Sudo: true})
	rc := f.calls[0].argv[len(f.calls[0].argv)-1]
	if !strings.Contains(rc, "sudo -n sh -s") {
		t.Fatalf("no sudo: %s", rc)
	}
	_ = s.Run(context.Background(), Cmd{Script: "cat", Stdin: strings.NewReader("x"), Sudo: true})
	rc = f.calls[1].argv[len(f.calls[1].argv)-1]
	if !strings.Contains(rc, `sudo -n sh -c "$1"`) || !strings.HasSuffix(rc, " yoho 'cat'") {
		t.Fatalf("sudo stdin form: %s", rc)
	}
	// Host not configured for sudo: Cmd.Sudo is a no-op.
	f2 := &fakeExec{}
	s2 := newTestSSH(t, "example.com", false, f2)
	_ = s2.Run(context.Background(), Cmd{Script: "id", Sudo: true})
	if rc := f2.calls[0].argv[len(f2.calls[0].argv)-1]; rc != "sh -s" {
		t.Fatalf("unexpected sudo: %s", rc)
	}
}

func TestExitErrorAndReadFileMissing(t *testing.T) {
	f := &fakeExec{respond: func(i int, c call, out io.Writer) error { return exitCode(exitNotExist) }}
	s := newTestSSH(t, "example.com", false, f)
	_, err := s.ReadFile(context.Background(), "/nope", false)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	f.respond = func(int, call, io.Writer) error { return exitCode(3) }
	err = s.Run(context.Background(), Cmd{Script: "false"})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || ee.Host != "web1" {
		t.Fatalf("got %v", err)
	}
}

// The generated scripts must actually work under a local sh.
func TestScriptsWithLocalShell(t *testing.T) {
	dir := t.TempDir()
	local := func(_ context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
		// Replace "ssh ... -- dest" with a local sh -c running the remote command.
		rc := argv[len(argv)-1]
		if argv[len(argv)-2] == "-O" || strings.Contains(strings.Join(argv, " "), " -O ") {
			return nil
		}
		c := exec.Command("sh", "-c", rc)
		c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
		return c.Run()
	}
	s, err := NewSSH("loc", "localhost", false, SSHOptions{ControlDir: t.TempDir(), Exec: local})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := dir + "/a b/c.txt"
	if err := s.WriteFile(ctx, p, []byte("hello\n"), 0o640, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFile(ctx, p, false)
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("read %q %v", got, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := s.ReadFile(ctx, dir+"/missing", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	// Commands reading stdin must not eat the rest of the script.
	out, err := s.Output(ctx, Cmd{Script: "cat\necho after $K", Env: map[string]string{"K": "v w"}})
	if err != nil || out != "after v w" {
		t.Fatalf("output %q %v", out, err)
	}
	// Env with Stdin uses the env file, which is removed after sourcing.
	out, err = s.Output(ctx, Cmd{Script: `echo "$K"; cat`, Stdin: strings.NewReader("-in"), Env: map[string]string{"K": "x'y"}})
	if err != nil || out != "x'y\n-in" {
		t.Fatalf("output %q %v", out, err)
	}
	var buf bytes.Buffer
	err = s.Run(ctx, Cmd{Script: "echo oops >&2; exit 7", Stderr: &buf})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 7 || ee.Stderr != "oops" || buf.String() != "oops\n" {
		t.Fatalf("err %v buf %q", err, buf.String())
	}
}

func TestE2ELocalhost(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	target := os.Getenv("YOHO_E2E_SSH")
	if target == "" {
		target = "localhost"
	}
	s, err := NewSSH("e2e", target, false, SSHOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	out, err := s.Output(ctx, Cmd{Script: "echo $X", Env: map[string]string{"X": "ok"}})
	if err != nil || out != "ok" {
		t.Fatalf("%q %v", out, err)
	}
	p := "/tmp/yoho-e2e-" + strconv.Itoa(os.Getpid()) + "/f"
	defer s.Run(ctx, Cmd{Script: "rm -rf " + Quote(path.Dir(p))})
	if err := s.WriteFile(ctx, p, []byte("data"), 0o600, false); err != nil {
		t.Fatal(err)
	}
	if b, err := s.ReadFile(ctx, p, false); err != nil || string(b) != "data" {
		t.Fatalf("%q %v", b, err)
	}
	if out, err := s.Output(ctx, Cmd{Script: "cat; echo \"$X\"", Stdin: strings.NewReader("in-"), Env: map[string]string{"X": "y'z"}}); err != nil || out != "in-y'z" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := s.ReadFile(ctx, "/nonexistent-yoho", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// Close must use "-O stop" so sessions owned by other yoho processes sharing
// the ControlMaster are not killed ("-O exit" would).
func TestCloseUsesStop(t *testing.T) {
	f := &fakeExec{}
	s := newTestSSH(t, "deploy@example.com", false, f)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls: %d", len(f.calls))
	}
	argv := f.calls[0].argv
	j := strings.Join(argv, " ")
	if !strings.Contains(j, " -O stop ") || strings.Contains(j, " -O exit") {
		t.Fatalf("argv %v", argv)
	}
}

// YOHO_E2E=1 YOHO_E2E_SSH=user@host: closing one Host must not interrupt a
// running command on another Host sharing the same ControlMaster.
func TestE2ECloseKeepsOtherSessions(t *testing.T) {
	target := os.Getenv("YOHO_E2E_SSH")
	if os.Getenv("YOHO_E2E") != "1" || target == "" {
		t.Skip("YOHO_E2E=1 and YOHO_E2E_SSH not set")
	}
	dir, err := os.MkdirTemp("", "yoho")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	a, err := NewSSH("a", target, false, SSHOptions{ControlDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSSH("b", target, false, SSHOptions{ControlDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Run(ctx, Cmd{Script: "true"}); err != nil { // start the master
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, Cmd{Script: "sleep 5"}) }()
	time.Sleep(time.Second)
	_ = b.Close()
	if err := <-done; err != nil {
		t.Fatalf("running command interrupted by Close on another Host: %v", err)
	}
}
