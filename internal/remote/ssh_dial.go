package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stdioOpened is what `ssh -v` prints once the ControlMaster reports the
// stdio forward's channel open (OpenSSH mux.c, mux_client_request_stdio_fwd).
// Masters that wait for the Server's channel confirmation before replying
// (OpenSSH 8.x and later) make it mean "the Server connected to addr".
const stdioOpened = "master session id: "

// dialStderrMax bounds the ssh stderr kept for error messages.
const dialStderrMax = 16 << 10

// DialContext implements Dialer with ssh stdio forwarding through this
// Host's ControlMaster: `ssh <SSHArgs> -v -W addr -- <dest>`. Only "tcp"
// networks are supported. addr is resolved and connected by the Server.
//
// The dial returns once ssh reports the forward open, so a refused
// connection, or an sshd that forbids forwarding (AllowTcpForwarding no),
// fails the dial with a *ForwardError; ssh failing to reach the Server is a
// *ConnectError. Without that report (a non-OpenSSH client) the dial fails
// after ctx ends or ConnectTimeout passes.
//
// The connection is the ssh process's stdin and stdout pipes. Deadlines are
// the pipes' deadlines (os.File; supported on Unix only). Close ends the ssh
// process and waits for it.
func (s *SSH) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("dial %s %s on Server %s: only tcp works over ssh", network, addr, s.name)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil || strings.HasPrefix(addr, "-") {
		return nil, fmt.Errorf("dial %q on Server %s: want host:port", addr, s.name)
	}
	if err := s.ensureMaster(ctx); err != nil {
		return nil, err
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return nil, err
	}
	// The process outlives the dial's ctx; Close cancels it.
	procCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &sshConn{
		addr:   addr,
		in:     stdinW,
		out:    stdoutR,
		cancel: cancel,
		done:   make(chan struct{}),
		stderr: &dialStderr{ready: make(chan struct{})},
	}
	argv := append(s.SSHArgs(), "-v", "-W", addr, "--", s.Destination())
	go func() {
		err := s.opts.Exec(procCtx, argv, stdinR, stdoutW, c.stderr)
		// ssh is gone: let reads see EOF after the buffered data.
		stdinR.Close()
		stdoutW.Close()
		c.runErr = err
		close(c.done)
	}()

	wait := time.Duration(s.opts.ConnectTimeout) * time.Second
	if wait <= 0 {
		wait = 10 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-c.stderr.ready:
		return c, nil
	case <-c.done:
		select {
		case <-c.stderr.ready:
			return c, nil
		default:
		}
		c.Close()
		return nil, s.dialError(addr, c.runErr, c.stderr.text())
	case <-ctx.Done():
		c.Close()
		return nil, fmt.Errorf("dial %s on Server %s over ssh: %w", addr, s.name, ctx.Err())
	case <-timer.C:
		c.Close()
		return nil, fmt.Errorf("dial %s on Server %s over ssh: ssh did not report the forward open within %s", addr, s.name, wait)
	}
}

// ensureMaster makes sure the ControlMaster runs, so `ssh -W` is a mux
// client that reports the forward open. `-O check` only talks to the local
// control socket.
func (s *SSH) ensureMaster(ctx context.Context) error {
	check := append(s.SSHArgs(), "-O", "check", "--", s.Destination())
	if s.opts.Exec(ctx, check, nil, io.Discard, io.Discard) == nil {
		return nil
	}
	return s.Run(ctx, Cmd{Script: "true"})
}

// ForwardError is the Server failing to open a forwarded connection: nothing
// listens on Addr, or sshd forbids forwarding. It wraps ssh's ExitError.
type ForwardError struct {
	*ExitError
	// Addr is host:port as seen from the Server.
	Addr string
	// Line is ssh's message, e.g. "Stdio forwarding request failed:
	// Session open refused by peer".
	Line string
	// Hint is the next step.
	Hint string
}

func (e *ForwardError) Error() string {
	return "Server " + e.Host + " could not open " + e.Addr + " over ssh: " + e.Line
}

func (e *ForwardError) Unwrap() error { return e.ExitError }

func (s *SSH) dialError(addr string, runErr error, stderr string) error {
	var ec interface{ ExitCode() int }
	if runErr == nil || !errors.As(runErr, &ec) || ec.ExitCode() < 0 {
		if runErr == nil {
			runErr = errors.New("ssh exited before the forward opened")
		}
		return fmt.Errorf("dial %s on Server %s over ssh: %w", addr, s.name, runErr)
	}
	ee := &ExitError{Host: s.name, Script: "ssh -W " + addr, Code: ec.ExitCode(), Stderr: stderr}
	if line := sshFailure(ee); line != "" {
		return &ConnectError{ExitError: ee, Target: s.Target(), Line: line, Hint: s.connectHint()}
	}
	line := lastLine(stderr, errors.New("ssh exited with code "+strconv.Itoa(ee.Code)))
	hint := "check that something listens on " + addr + " on the Server, and that the Server's sshd allows forwarding (AllowTcpForwarding yes or local in sshd_config): probes and checks open connections through ssh"
	if strings.Contains(stderr, "administratively prohibited") {
		hint = "the Server's sshd forbids forwarding; set AllowTcpForwarding yes (or local) in sshd_config for this user and reload sshd: probes and checks open connections through ssh"
	}
	return &ForwardError{ExitError: ee, Addr: addr, Line: line, Hint: hint}
}

// sshConn is a net.Conn over an `ssh -W` process's stdio.
type sshConn struct {
	addr   string
	in     *os.File // ssh's stdin
	out    *os.File // ssh's stdout
	cancel context.CancelFunc
	stderr *dialStderr

	done   chan struct{} // closed when ssh exited; runErr is set
	runErr error

	closing   atomic.Bool
	closeOnce sync.Once
}

func (c *sshConn) Read(p []byte) (int, error) {
	n, err := c.out.Read(p)
	switch {
	case errors.Is(err, os.ErrClosed):
		err = net.ErrClosed
	case err == io.EOF && !c.closing.Load():
		// stdout reaches EOF only after ssh exited.
		<-c.done
		var ec interface{ ExitCode() int }
		if errors.As(c.runErr, &ec) && ec.ExitCode() > 0 {
			err = fmt.Errorf("connection to %s over ssh ended: %s", c.addr, lastLine(c.stderr.text(), c.runErr))
		}
	}
	return n, err
}

func (c *sshConn) Write(p []byte) (int, error) {
	n, err := c.in.Write(p)
	if errors.Is(err, os.ErrClosed) {
		err = net.ErrClosed
	}
	return n, err
}

// Close ends the ssh process and waits for it. It always returns nil.
func (c *sshConn) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		c.in.Close()
		c.cancel()
		<-c.done
		c.out.Close()
	})
	return nil
}

func (c *sshConn) LocalAddr() net.Addr  { return sshAddr("ssh") }
func (c *sshConn) RemoteAddr() net.Addr { return sshAddr(c.addr) }

func (c *sshConn) SetDeadline(t time.Time) error {
	return errors.Join(c.out.SetReadDeadline(t), c.in.SetWriteDeadline(t))
}
func (c *sshConn) SetReadDeadline(t time.Time) error  { return c.out.SetReadDeadline(t) }
func (c *sshConn) SetWriteDeadline(t time.Time) error { return c.in.SetWriteDeadline(t) }

// sshAddr is an address seen through ssh.
type sshAddr string

func (a sshAddr) Network() string { return "ssh" }
func (a sshAddr) String() string  { return string(a) }

// dialStderr collects ssh -v's stderr and signals ready on stdioOpened.
type dialStderr struct {
	mu    sync.Mutex
	buf   []byte
	open  bool
	ready chan struct{}
}

func (d *dialStderr) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf = append(d.buf, p...)
	if !d.open && bytes.Contains(d.buf, []byte(stdioOpened)) {
		d.open = true
		close(d.ready)
	}
	if len(d.buf) > dialStderrMax {
		d.buf = d.buf[len(d.buf)-dialStderrMax:]
	}
	return len(p), nil
}

// text is stderr without ssh -v's debug lines and version banner.
func (d *dialStderr) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var keep []string
	for _, l := range strings.Split(string(d.buf), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "debug") || strings.HasPrefix(l, "OpenSSH_") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

func lastLine(stderr string, err error) string {
	if stderr == "" {
		return err.Error()
	}
	lines := strings.Split(stderr, "\n")
	return lines[len(lines)-1]
}
