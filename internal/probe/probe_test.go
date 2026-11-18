package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var dial DialFunc = (&net.Dialer{}).DialContext

func addrOf(t *testing.T, a net.Addr) (string, int) {
	t.Helper()
	ta := a.(*net.TCPAddr)
	return ta.IP.String(), ta.Port
}

func run(t *testing.T, srv *httptest.Server, c Check, h http.Header) error {
	t.Helper()
	ip, port := addrOf(t, srv.Listener.Addr())
	c.Port = port
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return Run(ctx, dial, ip, c, h)
}

// wsHandler answers an upgrade with status, headers and accept (computed
// when accept is "auto"), then holds the connection open sending data.
func wsHandler(status string, upgrade, connection, accept string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if accept == "auto" {
			accept = acceptKey(r.Header.Get("Sec-WebSocket-Key"))
		}
		fmt.Fprintf(brw, "HTTP/1.1 %s\r\nUpgrade: %s\r\nConnection: %s\r\nSec-WebSocket-Accept: %s\r\n\r\n", status, upgrade, connection, accept)
		brw.Flush()
		// A server that streams frames forever must not hold Run.
		for i := 0; i < 200; i++ {
			if _, err := conn.Write([]byte{0x81, 0x01, 'x'}); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ip, port := addrOf(t, ln.Addr())
	if err := Run(context.Background(), dial, ip, Check{Type: TCP, Port: port}, nil); err != nil {
		t.Errorf("open port: %v", err)
	}
	ln.Close()
	err = Run(context.Background(), dial, ip, Check{Type: TCP, Port: port}, nil)
	if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("tcp on 127.0.0.1:%d: ", port)) || !strings.Contains(err.Error(), "refused") {
		t.Errorf("closed port: %v", err)
	}
}

func TestTCPIPv6(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	defer ln.Close()
	ip, port := addrOf(t, ln.Addr())
	if err := Run(context.Background(), dial, ip, Check{Type: TCP, Port: port}, nil); err != nil {
		t.Error(err)
	}
}

func TestHTTP(t *testing.T) {
	var hits, redirected atomic.Int32
	var gotHost, gotHeader, gotURI atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotHost.Store(r.Host)
		gotHeader.Store(r.Header.Get("X-Probe"))
		gotURI.Store(r.RequestURI)
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("ok"))
		case "/down":
			http.Error(w, "down", http.StatusServiceUnavailable)
		case "/redirect":
			http.Redirect(w, r, "/target", http.StatusFound)
		case "/target":
			redirected.Add(1)
		case "/stream":
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
	}))
	defer srv.Close()

	if err := run(t, srv, Check{Type: HTTP, Path: "/ok?x=1"}, http.Header{"X-Probe": {"yes"}}); err != nil {
		t.Errorf("ok: %v", err)
	}
	if gotHost.Load() != "localhost" || gotHeader.Load() != "yes" || gotURI.Load() != "/ok?x=1" {
		t.Errorf("request host=%v header=%v uri=%v", gotHost.Load(), gotHeader.Load(), gotURI.Load())
	}
	if err := run(t, srv, Check{Type: HTTP, Path: "/ok", Host: "app.example.com"}, nil); err != nil || gotHost.Load() != "app.example.com" {
		t.Errorf("custom host: %v, %v", err, gotHost.Load())
	}
	if err := run(t, srv, Check{Type: HTTP}, nil); err != nil || gotURI.Load() != "/" {
		t.Errorf("default path: %v, %v", err, gotURI.Load())
	}

	_, port := addrOf(t, srv.Listener.Addr())
	err := run(t, srv, Check{Type: HTTP, Path: "/down"}, nil)
	if want := fmt.Sprintf("http /down on 127.0.0.1:%d: got 503 Service Unavailable, want 200-399", port); err == nil || err.Error() != want {
		t.Errorf("down: %v, want %s", err, want)
	}

	if err := run(t, srv, Check{Type: HTTP, Path: "/redirect"}, nil); err != nil {
		t.Errorf("redirect: %v", err)
	}
	if redirected.Load() != 0 {
		t.Error("redirect was followed")
	}

	start := time.Now()
	if err := run(t, srv, Check{Type: HTTP, Path: "/stream"}, nil); err != nil {
		t.Errorf("stream: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("stream: Run waited %s for the body", d)
	}
}

func TestWebSocket(t *testing.T) {
	cases := []struct {
		name    string
		handler http.Handler
		want    string // error suffix; "" means success
	}{
		{"ok", wsHandler("101 Switching Protocols", "websocket", "Upgrade", "auto"), ""},
		{"case-insensitive", wsHandler("101 Switching Protocols", "WebSocket", "keep-alive, upgrade", "auto"), ""},
		{"200", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }), ": got 200 OK, want 101"},
		{"wrong accept", wsHandler("101 Switching Protocols", "websocket", "Upgrade", "bm90IHRoZSBrZXk="), `: got Sec-WebSocket-Accept "bm90IHRoZSBrZXk="`},
		{"no upgrade", wsHandler("101 Switching Protocols", "h2c", "Upgrade", "auto"), `: got Upgrade "h2c", want websocket`},
		{"no connection", wsHandler("101 Switching Protocols", "websocket", "keep-alive", "auto"), `: got Connection "keep-alive", want upgrade`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			start := time.Now()
			err := run(t, srv, Check{Type: WebSocket, Path: "/sync"}, nil)
			if d := time.Since(start); d > time.Second {
				t.Errorf("Run took %s", d)
			}
			_, port := addrOf(t, srv.Listener.Addr())
			prefix := fmt.Sprintf("websocket /sync on 127.0.0.1:%d", port)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("err = %v", err)
			case c.want != "" && (err == nil || !strings.HasPrefix(err.Error(), prefix+c.want)):
				t.Errorf("err = %v, want %s%s", err, prefix, c.want)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept, never answer
		}
	}()
	ip, port := addrOf(t, ln.Addr())
	for _, typ := range []string{HTTP, WebSocket} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		err := Run(ctx, dial, ip, Check{Type: typ, Port: port}, nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no answer in time") {
			t.Errorf("%s: err = %v", typ, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %s", typ, d)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		c  Check
		ok bool
	}{
		{Check{Type: TCP, Port: 5432}, true},
		{Check{Type: HTTP, Port: 80}, true},
		{Check{Type: HTTP, Port: 80, Path: "/up?full=1", Host: "app.example.com:443"}, true},
		{Check{Type: WebSocket, Port: 8080, Path: "/sync"}, true},
		{Check{Port: 80}, false},
		{Check{Type: "grpc", Port: 80}, false},
		{Check{Type: TCP, Port: 0}, false},
		{Check{Type: TCP, Port: 70000}, false},
		{Check{Type: TCP, Port: 80, Path: "/"}, false},
		{Check{Type: HTTP, Port: 80, Path: "health"}, false},
		{Check{Type: HTTP, Port: 80, Path: "//evil.example/x"}, false},
		{Check{Type: HTTP, Port: 80, Path: "/a b"}, false},
		{Check{Type: HTTP, Port: 80, Path: "/a\r\nX: y"}, false},
		{Check{Type: HTTP, Port: 80, Host: "a b"}, false},
		{Check{Type: HTTP, Port: 80, Host: "a/b"}, false},
	}
	for _, c := range cases {
		if err := c.c.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: err = %v, want ok=%v", c.c, err, c.ok)
		}
	}
	if err := Run(context.Background(), dial, "127.0.0.1", Check{Type: HTTP, Port: 80}, http.Header{"Bad Name": {"x"}}); err == nil {
		t.Error("invalid header name accepted")
	}
	if err := Run(context.Background(), dial, "", Check{Type: TCP, Port: 80}, nil); err == nil {
		t.Error("empty ip accepted")
	}
}

func TestPoll(t *testing.T) {
	n := 0
	err := Poll(context.Background(), time.Millisecond, func(context.Context) error {
		n++
		if n < 3 {
			return fmt.Errorf("attempt %d", n)
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("err = %v after %d attempts", err, n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	n = 0
	err = Poll(ctx, 5*time.Millisecond, func(context.Context) error {
		n++
		return fmt.Errorf("attempt %d", n)
	})
	if err == nil || err.Error() != fmt.Sprintf("attempt %d", n) || n < 2 {
		t.Errorf("err = %v after %d attempts", err, n)
	}
}
