package remote

import (
	"context"
	"fmt"
	"net"
)

// Dialer is a Host that can open network connections from the Server, e.g.
// to probe a container port. addr is host:port as seen from the Server
// (IPv6 bracketed, as net.JoinHostPort writes it).
//
// The ctx bounds only the dial; like net.Dialer, the returned connection
// outlives it. Close the connection when done.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

var (
	_ Dialer = (*SSH)(nil)
	_ Dialer = (*Local)(nil)
)

// Dial opens a connection to addr from Server h. It fails when h cannot dial
// (it does not implement Dialer).
func Dial(ctx context.Context, h Host, network, addr string) (net.Conn, error) {
	d, ok := h.(Dialer)
	if !ok {
		return nil, fmt.Errorf("dial %s on Server %s: this connection to the Server cannot open network connections", addr, h.Name())
	}
	return d.DialContext(ctx, network, addr)
}

// DialContext implements Dialer with net.Dialer: Local is the Server.
func (l *Local) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}
