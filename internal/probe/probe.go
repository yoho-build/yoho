// Package probe checks that a Service answers on a port: a TCP connect, an
// HTTP GET, or a WebSocket upgrade. Run makes one attempt over a caller's
// dial function, so the same check runs from the operator's machine (through
// ssh, remote.Dial) and on the Server (the Gate, net.Dialer). Poll retries an
// attempt until its context ends.
package probe

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Check types.
const (
	TCP       = "tcp"
	HTTP      = "http"
	WebSocket = "websocket"
)

// DefaultHost is the Host header http and websocket checks send when Check
// sets none.
const DefaultHost = "localhost"

// maxHeader bounds the response bytes read: status line and headers.
const maxHeader = 64 << 10

// Check is one readiness probe of a Service port.
type Check struct {
	// Type is TCP, HTTP or WebSocket.
	Type string `json:"type"`
	// Port is the container port.
	Port int `json:"port"`
	// Path is the request path (and query) for HTTP and WebSocket. Default "/".
	Path string `json:"path,omitempty"`
	// Host is the Host header for HTTP and WebSocket. Default DefaultHost.
	Host string `json:"host,omitempty"`
	// Want2xx makes an HTTP check pass only on 200-299, not 200-399.
	Want2xx bool `json:"-"`
}

// DialFunc opens a connection, like net.Dialer.DialContext or remote.Dial
// bound to a Host.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Validate reports a Check that cannot run.
func (c Check) Validate() error {
	switch c.Type {
	case TCP, HTTP, WebSocket:
	case "":
		return errors.New("check type is required: tcp, http or websocket")
	default:
		return fmt.Errorf("check type %q is not tcp, http or websocket", c.Type)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("check port %d is not between 1 and 65535", c.Port)
	}
	if c.Type == TCP {
		if c.Path != "" || c.Host != "" {
			return errors.New("check path and host only apply to http and websocket checks")
		}
		return nil
	}
	if c.Path != "" {
		u, err := url.ParseRequestURI(c.Path)
		if err != nil || !strings.HasPrefix(c.Path, "/") || strings.HasPrefix(c.Path, "//") || u.Host != "" || strings.ContainsAny(c.Path, "# \t\r\n") {
			return fmt.Errorf("check path %q must be an absolute path such as /health", c.Path)
		}
	}
	if c.Host != "" && (strings.ContainsAny(c.Host, "/?#@\\") || strings.IndexFunc(c.Host, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0) {
		return fmt.Errorf("check host %q is not a hostname", c.Host)
	}
	return nil
}

func (c Check) path() string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}

func (c Check) host() string {
	if c.Host == "" {
		return DefaultHost
	}
	return c.Host
}

// String names the check, e.g. "http /health" or "tcp".
func (c Check) String() string {
	if c.Type == TCP {
		return TCP
	}
	return c.Type + " " + c.path()
}

// Run makes one attempt of c against ip (an IPv4 or IPv6 address, or a
// hostname, as dial resolves it) on c.Port, bounded by ctx. Callers add the
// timeout and the retries (Poll).
//
//   - tcp: the connection opens.
//   - http: GET c.Path answers 200-399 (200-299 with Want2xx). Redirects
//     are not followed; only the status line and headers are read.
//   - websocket: the upgrade answers 101 with a valid Sec-WebSocket-Accept;
//     the connection closes right after the headers.
//
// header adds request headers to http and websocket checks (the Host header
// comes from c.Host). Errors read like
// "websocket /sync on 172.18.0.5:8080: got 200 OK, want 101".
func Run(ctx context.Context, dial DialFunc, ip string, c Check, header http.Header) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if ip == "" {
		return fmt.Errorf("%s on port %d: no address to probe", c, c.Port)
	}
	addr := net.JoinHostPort(ip, strconv.Itoa(c.Port))
	fail := func(err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("%s on %s: no answer in time: %w", c, addr, context.DeadlineExceeded)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s on %s: %w", c, addr, ctx.Err())
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Err != nil {
			err = op.Err
		}
		return fmt.Errorf("%s on %s: %w", c, addr, err)
	}

	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	if c.Type == TCP {
		return nil
	}
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+c.path(), nil)
	if err != nil {
		return fail(err)
	}
	req.Host = c.host()
	for k, vs := range header {
		if k == "" || strings.IndexFunc(k, func(r rune) bool { return r <= ' ' || r >= 0x7f || strings.ContainsRune("\"(),/:;<=>?@[\\]{}", r) }) >= 0 {
			return fmt.Errorf("%s: header name %q is not valid", c, k)
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "yoho-probe")
	}
	var key string
	if c.Type == WebSocket {
		key, err = newKey()
		if err != nil {
			return fail(err)
		}
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Key", key)
		req.Header.Set("Sec-WebSocket-Version", "13")
	} else {
		req.Close = true
	}
	if err := req.Write(conn); err != nil {
		return fail(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, maxHeader)), req)
	if err != nil {
		return fail(err)
	}
	if c.Type == HTTP {
		hi, want := 399, "200-399"
		if c.Want2xx {
			hi, want = 299, "2xx"
		}
		if resp.StatusCode < 200 || resp.StatusCode > hi {
			return fail(fmt.Errorf("got %s, want %s", resp.Status, want))
		}
		return nil
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fail(fmt.Errorf("got %s, want 101", resp.Status))
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		return fail(fmt.Errorf("got Upgrade %q, want websocket", resp.Header.Get("Upgrade")))
	}
	if !hasToken(resp.Header.Values("Connection"), "upgrade") {
		return fail(fmt.Errorf("got Connection %q, want upgrade", strings.Join(resp.Header.Values("Connection"), ", ")))
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		return fail(fmt.Errorf("got Sec-WebSocket-Accept %q, want %q", got, acceptKey(key)))
	}
	return nil
}

// Poll calls attempt until it succeeds or ctx ends, waiting interval between
// attempts, and returns the last attempt's error. It always makes one
// attempt. Bound each attempt inside attempt when it needs its own timeout.
func Poll(ctx context.Context, interval time.Duration, attempt func(context.Context) error) error {
	for {
		err := attempt(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(interval):
		}
	}
}

func newKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// acceptKey is RFC 6455's Sec-WebSocket-Accept for key.
func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func hasToken(values []string, token string) bool {
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}
