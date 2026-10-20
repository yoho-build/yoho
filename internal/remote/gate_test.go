package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestValidHostname(t *testing.T) {
	for _, ok := range []string{"ssh.example.com", "a-b.c1.example.co", "X.Y"} {
		if !ValidHostname(ok) {
			t.Errorf("%s: want valid", ok)
		}
	}
	for _, bad := range []string{"", "localhost", "-a.example.com", "a-.example.com", "a..b", "a.b;id", "a.b c", "a.b%h", "$(id).example.com", "a.b/c", strings.Repeat("a", 64) + ".com"} {
		if ValidHostname(bad) {
			t.Errorf("%q: want invalid", bad)
		}
	}
}

func TestCloudflareProxyCommand(t *testing.T) {
	f := &fakeExec{}
	s, err := NewSSH("web1", "deb@home", false, SSHOptions{ControlDir: t.TempDir(), Exec: f.exec, CloudflareHostname: "ssh.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(s.SSHArgs(), " ")
	if !strings.Contains(args, "-o ProxyCommand=cloudflared access ssh --hostname ssh.example.com") {
		t.Fatalf("args: %s", args)
	}
	plain, _ := NewSSH("web1", "deb@home", false, SSHOptions{ControlDir: t.TempDir(), Exec: f.exec})
	if s.controlPath == plain.controlPath {
		t.Fatal("a tunneled master must not share the direct one's socket")
	}
	if _, err := NewSSH("web1", "deb@home", false, SSHOptions{Exec: f.exec, CloudflareHostname: "x.com;id"}); err == nil {
		t.Fatal("want error for an invalid hostname")
	}
	old := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	defer func() { lookPath = old }()
	if _, err := NewSSH("web1", "deb@home", false, SSHOptions{CloudflareHostname: "ssh.example.com"}); err == nil || !strings.Contains(err.Error(), "cloudflared is not installed") {
		t.Fatalf("missing cloudflared: %v", err)
	}
}

func TestWithUser(t *testing.T) {
	for in, want := range map[string]string{
		"deb@1.2.3.4":      "yoho-gate@1.2.3.4",
		"host:2222":        "yoho-gate@host:2222",
		"root@[::1]:22":    "yoho-gate@[::1]:22",
		"ssh://deb@a.b.cc": "yoho-gate@a.b.cc",
	} {
		got, err := WithUser(in, "yoho-gate")
		if err != nil || got != want {
			t.Errorf("%s: got %q %v, want %s", in, got, err, want)
		}
	}
	if _, err := WithUser("host", "-oProxyCommand=x"); err == nil {
		t.Fatal("want error for an option-like user")
	}
}

func TestChunks(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), maxChunk/5) // two chunks
	var stream bytes.Buffer
	stream.WriteString("{}\n")
	if err := writeChunks(&stream, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	stream.WriteString("tail")
	req := stream.Bytes()[3:]
	r := bytes.NewReader(req)
	got, err := io.ReadAll(GatePayload(r))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("payload: %v, %d bytes", err, len(got))
	}
	if rest, _ := io.ReadAll(r); string(rest) != "tail" {
		t.Fatalf("payload read past its end: %q", rest)
	}
	if _, err := io.ReadAll(GatePayload(bytes.NewReader([]byte{0, 0, 0, 9, 'x'}))); err == nil {
		t.Fatal("want error for a truncated chunk")
	}
}

func TestReadGateRequest(t *testing.T) {
	line, err := EncodeGateRequest("shop", "production", "logs", map[string]any{"lines": 5})
	if err != nil {
		t.Fatal(err)
	}
	req, rest, err := ReadGateRequest(io.MultiReader(bytes.NewReader(line), strings.NewReader("payload")))
	if err != nil || req.Verb != "logs" || string(req.Args) != `{"lines":5}` {
		t.Fatalf("%+v %v", req, err)
	}
	if b, _ := io.ReadAll(rest); string(b) != "payload" {
		t.Fatalf("rest %q", b)
	}
	if _, _, err := ReadGateRequest(strings.NewReader(`{"protocol":2}` + "\n")); err == nil || !strings.Contains(err.Error(), "protocol 2") {
		t.Fatalf("protocol: %v", err)
	}
	if _, _, err := ReadGateRequest(strings.NewReader("")); err == nil {
		t.Fatal("want error for empty input")
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	fw.Write([]byte("step 1\n"))
	fw.Result(map[string]string{"version": "abc"})
	var out bytes.Buffer
	d := &frameDecoder{out: &out}
	// Split mid-frame like a pipe would.
	b := buf.Bytes()
	d.Write(b[:5])
	d.Write(b[5:])
	if err := d.finish(); err != nil || out.String() != "step 1\n" || string(d.result) != `{"version":"abc"}` {
		t.Fatalf("out %q result %s err %v", out.String(), d.result, err)
	}
}

func TestGateCallReportsErrorFrame(t *testing.T) {
	f := &fakeExec{}
	s := newTestSSH(t, "yoho-gate@host", false, f)
	s.opts.Exec = func(_ context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) error {
		if !strings.HasSuffix(strings.Join(argv, " "), "-T -- yoho-gate@host yoho-gate ps") {
			t.Errorf("argv %v", argv)
		}
		NewFrameWriter(stdout).Fail(&GateError{Message: "no", Hint: "ask", Code: "denied"})
		return exitErr(2)
	}
	err := NewGate(s, "shop", "production").Call(context.Background(), "ps", nil, nil, nil, nil)
	var ge *GateError
	if !errors.As(err, &ge) || ge.Hint != "ask" {
		t.Fatalf("got %v", err)
	}
}

type exitErr int

func (e exitErr) Error() string { return "exit" }
func (e exitErr) ExitCode() int { return int(e) }

// TestGateCallReturnsWhenSSHExits is the QA hang: a real child process that
// answers (or fails) without reading stdin must not leave Call waiting on
// its own stdin writer.
func TestGateCallReturnsWhenSSHExits(t *testing.T) {
	for name, script := range map[string]string{
		"result":  `printf '%s\n' '{"result":{"ok":true}}'; sleep 0.1`,
		"refused": `echo 'Permission denied (publickey).' >&2; exit 255`,
		"error":   `printf '%s\n' '{"error":{"message":"no"}}'; exit 2`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestSSH(t, "yoho-gate@host", false, &fakeExec{})
			s.opts.Exec = func(ctx context.Context, _ []string, stdin io.Reader, stdout, stderr io.Writer) error {
				return osExec(ctx, []string{"sh", "-c", script}, stdin, stdout, stderr)
			}
			done := make(chan error, 1)
			go func() {
				var res struct{ OK bool }
				done <- NewGate(s, "shop", "production").Call(context.Background(), "deploy", map[string]string{"a": "b"}, strings.NewReader("payload"), nil, &res)
			}()
			select {
			case err := <-done:
				if name == "result" && err != nil {
					t.Fatalf("result: %v", err)
				}
				if name != "result" && err == nil {
					t.Fatal("want error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Call hung after ssh exited")
			}
		})
	}
}
