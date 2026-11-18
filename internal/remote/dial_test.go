package remote

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const openedLine = "OpenSSH_10.0p2, LibreSSL 3.3.6\r\ndebug1: mux_client_request_stdio_fwd: master session id: 2\r\n"

// dialExec fakes ssh for DialContext: `-O check` succeeds unless
// noMaster, `-W` runs forward with the process's stdio.
type dialExec struct {
	mu       sync.Mutex
	calls    [][]string
	noMaster bool
	forward  func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error
}

func (f *dialExec) exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	f.mu.Unlock()
	switch {
	case slices.Contains(argv, "check"):
		if f.noMaster {
			return exitCode(255)
		}
		return nil
	case slices.Contains(argv, "-W"):
		return f.forward(ctx, stdin, stdout, stderr)
	}
	return nil
}

func (f *dialExec) ssh(t *testing.T) *SSH {
	t.Helper()
	s, err := NewSSH("web1", "deploy@web", false, SSHOptions{ControlDir: t.TempDir(), Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func echo(_ context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	io.WriteString(stderr, openedLine)
	_, err := io.Copy(stdout, stdin)
	return err
}

func TestSSHDialArgvAndStdio(t *testing.T) {
	f := &dialExec{forward: echo}
	s := f.ssh(t)
	c, err := s.DialContext(context.Background(), "tcp", "[fd00::5]:8080")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}
	if got := c.RemoteAddr().String(); got != "[fd00::5]:8080" {
		t.Errorf("RemoteAddr = %q", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("write after close: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 2 {
		t.Fatalf("calls = %q", f.calls)
	}
	check := f.calls[0]
	if want := []string{"-O", "check", "--", "deploy@web"}; !reflect.DeepEqual(check[len(check)-4:], want) {
		t.Errorf("check argv = %q", check)
	}
	w := f.calls[1]
	if !reflect.DeepEqual(w[:len(s.SSHArgs())], s.SSHArgs()) {
		t.Errorf("-W argv does not start with SSHArgs: %q", w)
	}
	if want := []string{"-v", "-W", "[fd00::5]:8080", "--", "deploy@web"}; !reflect.DeepEqual(w[len(w)-5:], want) {
		t.Errorf("-W argv = %q", w)
	}
}

func TestSSHDialStartsMaster(t *testing.T) {
	f := &dialExec{noMaster: true, forward: echo}
	c, err := f.ssh(t).DialContext(context.Background(), "tcp", "10.0.0.5:80")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 3 || slices.Contains(f.calls[1], "-W") || slices.Contains(f.calls[1], "-O") || !slices.Contains(f.calls[2], "-W") {
		t.Errorf("want check, a session, then -W; got %q", f.calls)
	}
}

func TestSSHDialFailures(t *testing.T) {
	failWith := func(stderr string) func(context.Context, io.Reader, io.Writer, io.Writer) error {
		return func(_ context.Context, _ io.Reader, _, e io.Writer) error {
			io.WriteString(e, "OpenSSH_10.0p2\r\ndebug1: mux_client_request_session\r\n"+stderr)
			return exitCode(255)
		}
	}
	t.Run("refused", func(t *testing.T) {
		f := &dialExec{forward: failWith("Stdio forwarding request failed: Session open refused by peer\r\n")}
		_, err := f.ssh(t).DialContext(context.Background(), "tcp", "10.0.0.5:80")
		var fe *ForwardError
		if !errors.As(err, &fe) {
			t.Fatalf("err = %v (%T)", err, err)
		}
		if fe.Addr != "10.0.0.5:80" || fe.Line != "Stdio forwarding request failed: Session open refused by peer" || !strings.Contains(fe.Hint, "AllowTcpForwarding") {
			t.Errorf("ForwardError = %+v", fe)
		}
		if strings.Contains(err.Error(), "debug1") {
			t.Errorf("debug output in error: %v", err)
		}
	})
	t.Run("prohibited", func(t *testing.T) {
		f := &dialExec{forward: failWith("channel 0: open failed: administratively prohibited: open failed\r\nstdio forwarding failed\r\n")}
		_, err := f.ssh(t).DialContext(context.Background(), "tcp", "10.0.0.5:80")
		var fe *ForwardError
		if !errors.As(err, &fe) || !strings.Contains(fe.Hint, "forbids forwarding") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("connect", func(t *testing.T) {
		f := &dialExec{forward: failWith("ssh: connect to host web port 22: Connection refused\r\n")}
		_, err := f.ssh(t).DialContext(context.Background(), "tcp", "10.0.0.5:80")
		if _, ok := AsConnectError(err); !ok {
			t.Fatalf("err = %v (%T)", err, err)
		}
	})
	t.Run("no confirmation", func(t *testing.T) {
		f := &dialExec{forward: func(ctx context.Context, _ io.Reader, _, _ io.Writer) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := f.ssh(t).DialContext(ctx, "tcp", "10.0.0.5:80")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad input", func(t *testing.T) {
		s := (&dialExec{forward: echo}).ssh(t)
		for _, c := range [][2]string{{"udp", "10.0.0.5:53"}, {"tcp", "10.0.0.5"}, {"tcp", "-oProxyCommand=x:1"}} {
			if _, err := s.DialContext(context.Background(), c[0], c[1]); err == nil {
				t.Errorf("%s %s: no error", c[0], c[1])
			}
		}
	})
}

func TestSSHConnEOFAndDeadline(t *testing.T) {
	f := &dialExec{forward: func(_ context.Context, _ io.Reader, out, e io.Writer) error {
		io.WriteString(e, openedLine)
		io.WriteString(out, "hello")
		return nil
	}}
	c, err := f.ssh(t).DialContext(context.Background(), "tcp", "10.0.0.5:80")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "hello" {
		t.Errorf("ReadAll = %q, %v", b, err)
	}
	c.Close()

	f = &dialExec{forward: echo}
	c, err = f.ssh(t).DialContext(context.Background(), "tcp", "10.0.0.5:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read past deadline: %v", err)
	}
}

func TestLocalDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.WriteString(c, "hi")
			c.Close()
		}
	}()
	c, err := Dial(context.Background(), &Local{}, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if b, _ := io.ReadAll(c); string(b) != "hi" {
		t.Errorf("read %q", b)
	}
}

// plainHost is a Host that cannot dial.
type plainHost struct{ Host }

func (plainHost) Name() string { return "web1" }

func TestDialNotDialer(t *testing.T) {
	_, err := Dial(context.Background(), plainHost{}, "tcp", "10.0.0.5:80")
	if err == nil || !strings.Contains(err.Error(), "web1") {
		t.Fatalf("err = %v", err)
	}
}
